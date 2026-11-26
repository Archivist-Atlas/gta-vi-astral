//go:build windows

package hle

import (
	"bytes"
	"compress/zlib"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/Archivist-Atlas/gta-vi-astral/pkg/formats"
	"github.com/Archivist-Atlas/gta-vi-astral/pkg/gpu"
)

// dstorageState implémente un moteur DirectStorage console (XCRDAPI.dll)
// adossé à VirtualFS avec lecture réelle de fichiers .rpf et décompression zlib.
type dsPendingPageReq struct {
	opts    uint64
	dstPtr  uintptr
	pageOff int
	dstSize uint32
	srcPtr  uintptr
	srcOff  uint64
	srcSize uint32
}

type dstorageState struct {
	vfs          *VirtualFS
	mu           sync.Mutex
	files        map[uintptr]*os.File
	statuses     map[uintptr]*dstorageStatus
	pendingPages map[uintptr][]dsPendingPageReq
	readCount     atomic.Uint64
	bytesRead     atomic.Uint64
	pageWrites    atomic.Uint64
	bcpackDecoded atomic.Uint64
	bcpackWarns   atomic.Int32
}

type dstorageStatus struct {
	mu      sync.Mutex
	results []uint32
}

func bindDirectStorage(t *LiveTable, vfs *VirtualFS) {
	ds := &dstorageState{
		vfs:          vfs,
		files:        make(map[uintptr]*os.File),
		statuses:     make(map[uintptr]*dstorageStatus),
		pendingPages: make(map[uintptr][]dsPendingPageReq),
	}

	cbOK := syscall.NewCallback(func() uintptr { return hresultOK })

	// Vtables partagées O(1) pour IDStorageFileX, IDStorageStatusArrayX et IDStorageQueueX
	dsFileProto := NewCOMObject("IDStorageFileX", 16, map[int]uintptr{
		3: syscall.NewCallback(func(this uintptr) uintptr { // Close
			ds.mu.Lock()
			if fh, ok := ds.files[this]; ok {
				_ = fh.Close()
				delete(ds.files, this)
			}
			ds.mu.Unlock()
			return 0
		}),
		4: syscall.NewCallback(func(this, infoPtr uintptr) uintptr { // GetFileInformation (52 octets)
			ds.mu.Lock()
			fh := ds.files[this]
			ds.mu.Unlock()
			if fh == nil || infoPtr == 0 {
				return hresultInvalidArg
			}
			st, err := fh.Stat()
			if err != nil {
				return 0x80070002
			}
			b := unsafe.Slice((*byte)(osPtr(infoPtr)), 52)
			for i := range b {
				b[i] = 0
			}
			sz := uint64(st.Size())
			*(*uint32)(osPtr(infoPtr + 0)) = 0x80 // FILE_ATTRIBUTE_NORMAL
			*(*uint32)(osPtr(infoPtr + 32)) = uint32(sz >> 32)
			*(*uint32)(osPtr(infoPtr + 36)) = uint32(sz & 0xFFFFFFFF)
			return hresultOK
		}),
	})
	dsFileVtable := *(*uintptr)(osPtr(dsFileProto))
	newDSFile := func(f *os.File) uintptr {
		obj, _, _ := k32VirtualAlloc.Call(0, 0x20, 0x3000, 0x04)
		if obj != 0 {
			*(*uintptr)(osPtr(obj)) = dsFileVtable
			ds.mu.Lock()
			ds.files[obj] = f
			ds.mu.Unlock()
		}
		return obj
	}

	dsStatusProto := NewCOMObject("IDStorageStatusArrayX", 16, map[int]uintptr{
		3: syscall.NewCallback(func(this, index uintptr) uintptr { return 1 }), // IsComplete -> TRUE
		4: syscall.NewCallback(func(this, index uintptr) uintptr { // GetHResult
			idx := int(uint32(index))
			ds.mu.Lock()
			s := ds.statuses[this]
			ds.mu.Unlock()
			if s != nil && idx >= 0 && idx < len(s.results) {
				return uintptr(s.results[idx])
			}
			return hresultOK
		}),
	})
	dsStatusVtable := *(*uintptr)(osPtr(dsStatusProto))
	newDSStatus := func(capacity int) uintptr {
		if capacity < 1 {
			capacity = 1
		}
		st := &dstorageStatus{results: make([]uint32, capacity)}
		obj, _, _ := k32VirtualAlloc.Call(0, 0x20, 0x3000, 0x04)
		if obj != 0 {
			*(*uintptr)(osPtr(obj)) = dsStatusVtable
			ds.mu.Lock()
			ds.statuses[obj] = st
			ds.mu.Unlock()
		}
		return obj
	}

	execDSReq := func(opts uint64, dstPtr uintptr, pageOff int, dstSize uint32, srcPtr uintptr, srcOff uint64, srcSize uint32) {
		if dstPtr == 0 || dstSize == 0 || srcPtr == 0 {
			return
		}
		const (
			optZlib    = uint64(1)
			optBCPack  = uint64(0xF << 1)
			optPageDst = uint64(1 << 21)
			optMemSrc  = uint64(1 << 22)
		)
		var raw []byte
		if opts&optMemSrc != 0 {
			if srcSize > 0 && srcSize <= (64<<20) {
				raw = make([]byte, srcSize)
				copy(raw, unsafe.Slice((*byte)(osPtr(srcPtr)), srcSize))
			}
		} else {
			ds.mu.Lock()
			fh := ds.files[srcPtr]
			ds.mu.Unlock()
			if fh != nil && srcSize > 0 {
				raw = make([]byte, srcSize)
				n, _ := fh.ReadAt(raw, int64(srcOff))
				raw = raw[:n]
			} else if srcSize > 0 && srcSize <= (64<<20) {
				raw = make([]byte, srcSize)
				copy(raw, unsafe.Slice((*byte)(osPtr(srcPtr)), srcSize))
			}
		}
		if len(raw) == 0 {
			slog.Warn("IDStorageQueueX!EnqueueRequest raw vide", "opts", opts, "srcSize", srcSize, "dstSize", dstSize)
			return
		}
		bcFmt := (opts & optBCPack) >> 1
		isPageDst := (opts & optPageDst) != 0
		var dst []byte
		if isPageDst {
			dst = make([]byte, dstSize)
		} else {
			dst = unsafe.Slice((*byte)(osPtr(dstPtr)), dstSize)
		}
		var written int
		if bcFmt != 0 {
			// Ordre matériel Xbox Series : ZLIB d'abord (si combiné), puis BCPack (BC1/BC3/BC4/BC5/BC6H/BC7)
			packed := raw
			if opts&optZlib != 0 {
				if zr, err := zlib.NewReader(bytes.NewReader(raw)); err == nil {
					if dec, rerr := io.ReadAll(zr); rerr == nil && len(dec) > 0 {
						packed = dec
					}
					_ = zr.Close()
				}
			}
			if err := formats.DecodeBCPack(uint8(bcFmt), packed, dst, nil); err == nil {
				written = int(dstSize)
				ds.bytesRead.Add(uint64(written))
				if n := ds.bcpackDecoded.Add(1); n == 1 || n%250 == 0 {
					slog.Info("IDStorageQueueX!BCPack décodage matériel pur Go actif", "count", n, "bcFmt", bcFmt, "srcSize", len(packed), "dstSize", dstSize)
				}
			} else {
				if ds.bcpackWarns.Add(1) <= 5 {
					slog.Warn("IDStorageQueueX!EnqueueRequest BCPack erreur décodage (repli zéro)", "bcFmt", bcFmt, "srcSize", len(packed), "dstSize", dstSize, "err", err)
				}
				clear(dst)
				written = int(dstSize)
			}
		} else if opts&optZlib != 0 { // ZLIB (RFC 1950)
			if zr, err := zlib.NewReader(bytes.NewReader(raw)); err == nil {
				var rerr error
				written, rerr = io.ReadFull(zr, dst)
				_ = zr.Close()
				ds.bytesRead.Add(uint64(written))
				if rerr != nil && rerr != io.EOF && rerr != io.ErrUnexpectedEOF {
					slog.Warn("IDStorageQueueX!EnqueueRequest zlib read err", "err", rerr, "written", written, "dstSize", dstSize)
				}
			} else {
				slog.Warn("IDStorageQueueX!EnqueueRequest zlib err", "err", err, "srcSize", srcSize, "dstSize", dstSize)
			}
		} else {
			written = copy(dst, raw)
			ds.bytesRead.Add(uint64(written))
		}
		if isPageDst && written > 0 {
			nPages := (pageOff + int(dstSize) + int(xmemPageSize) - 1) / int(xmemPageSize)
			if nPages > 0 && nPages <= 4096 {
				pages := unsafe.Slice((*uintptr)(osPtr(dstPtr)), nPages)
				if xmemWritePhysical(pages, pageOff, dst[:written]) {
					if pw := ds.pageWrites.Add(1); pw == 1 || pw%250 == 0 {
						slog.Info("IDStorageQueueX!PAGE_DESTINATION écriture pages physiques active", "count", pw, "pages", nPages, "written", written)
					}
				} else {
					slog.Warn("IDStorageQueueX!PAGE_DESTINATION échec écriture pages physiques", "pages", nPages, "page0", pages[0], "written", written)
				}
			}
		}
		rc := ds.readCount.Add(1)
		if rc%25 == 0 {
			gpu.ReportDirectStorageRead(rc)
		}
		if rc <= 5 || rc%500 == 0 {
			slog.Info("IDStorageQueueX!EnqueueRequest", "count", rc, "opts", opts, "srcSize", srcSize, "dstSize", dstSize, "written", written)
		}
	}
	flushPendingPages := func(q uintptr) {
		ds.mu.Lock()
		reqs := ds.pendingPages[q]
		if len(reqs) > 0 {
			delete(ds.pendingPages, q)
		}
		ds.mu.Unlock()
		for _, r := range reqs {
			execDSReq(r.opts, r.dstPtr, r.pageOff, r.dstSize, r.srcPtr, r.srcOff, r.srcSize)
		}
	}

	dsQueueProto := NewCOMObject("IDStorageQueueX", 32, map[int]uintptr{
		3: syscall.NewCallback(func(this, reqPtr uintptr) uintptr { // EnqueueRequest
			if reqPtr == 0 {
				return 0
			}
			opts := *(*uint64)(osPtr(reqPtr + 0x00))
			dstPtr := *(*uintptr)(osPtr(reqPtr + 0x08))
			pageOff := int(*(*uint64)(osPtr(reqPtr+0x10)) & 0xFFFF)
			dstSize := *(*uint32)(osPtr(reqPtr + 0x18))
			srcPtr := *(*uintptr)(osPtr(reqPtr + 0x20))
			srcOff := *(*uint64)(osPtr(reqPtr + 0x28))
			srcSize := *(*uint32)(osPtr(reqPtr + 0x30))

			if dstPtr == 0 || dstSize == 0 || srcPtr == 0 {
				slog.Warn("IDStorageQueueX!EnqueueRequest ignoré (null/zero)", "opts", opts, "dst", dstPtr, "dstSize", dstSize, "src", srcPtr, "srcSize", srcSize)
				return 0
			}
			// Les requêtes PAGE_DESTINATION peuvent avoir leur tableau de pages rempli entre EnqueueRequest et Submit
			if opts&(1<<21) != 0 {
				ds.mu.Lock()
				ds.pendingPages[this] = append(ds.pendingPages[this], dsPendingPageReq{
					opts: opts, dstPtr: dstPtr, pageOff: pageOff, dstSize: dstSize,
					srcPtr: srcPtr, srcOff: srcOff, srcSize: srcSize,
				})
				ds.mu.Unlock()
				return 0
			}
			execDSReq(opts, dstPtr, 0, dstSize, srcPtr, srcOff, srcSize)
			return 0
		}),
		4: syscall.NewCallback(func(this, statusArr, index uintptr) uintptr { // EnqueueStatus (0x20)
			flushPendingPages(this)
			return hresultOK
		}),
		5: syscall.NewCallback(func(this, fence, val uintptr) uintptr { // EnqueueSignal (0x28)
			flushPendingPages(this)
			if fence >= 0x10000 {
				vtbl := *(*uintptr)(osPtr(fence))
				if vtbl >= 0x10000 {
					fn := *(*uintptr)(osPtr(vtbl + 10*8)) // ID3D12Fence::Signal (slot 10)
					if fn >= 0x10000 {
						_, _, _ = syscall.SyscallN(fn, fence, val)
					}
				}
			}
			slog.Info("IDStorageQueueX!EnqueueSignal", "fence", fence, "val", uint64(val))
			return hresultOK
		}),
		6: syscall.NewCallback(func(this uintptr) uintptr { // Submit (0x30)
			flushPendingPages(this)
			return hresultOK
		}),
		8:  cbOK, // Close (0x40)
		10: cbOK, // Submit / ErrorRecord fallback
	})
	dsQueueVtable := *(*uintptr)(osPtr(dsQueueProto))
	newDSQueue := func() uintptr {
		obj, _, _ := k32VirtualAlloc.Call(0, 0x20, 0x3000, 0x04)
		if obj != 0 {
			*(*uintptr)(osPtr(obj)) = dsQueueVtable
		}
		return obj
	}

	cbCreateQueue := syscall.NewCallback(func(this, descPtr, iidPtr, outPtr uintptr) uintptr {
		slog.Info("IDStorageFactoryX!CreateQueue -> IDStorageQueueX")
		if outPtr != 0 {
			*(*uintptr)(osPtr(outPtr)) = newDSQueue()
		}
		return hresultOK
	})

	dstorageFactoryObj := NewCOMObject("IDStorageFactoryX", 32, map[int]uintptr{
		3:  cbCreateQueue, // CreateQueue (2020)
		4:  syscall.NewCallback(func(this, pathPtr, iidPtr, outPtr uintptr) uintptr { // OpenFile
			orig := readUTF16Str(pathPtr)
			hostPath := orig
			if ds.vfs != nil {
				if mapped, ok := ds.vfs.ResolveRead(orig); ok {
					hostPath = mapped
				}
			}
			slog.Info("IDStorageFactoryX!OpenFile", "path", orig)
			fh, err := os.Open(hostPath)
			if err != nil {
				slog.Warn("IDStorageFactoryX!OpenFile échec ouverture", "path", orig, "err", err)
				return 0x80070002 // ERROR_FILE_NOT_FOUND
			}
			if outPtr != 0 {
				*(*uintptr)(osPtr(outPtr)) = newDSFile(fh)
			}
			return hresultOK
		}),
		5: syscall.NewCallback(func(this, capacity, namePtr, iidPtr, outPtr uintptr) uintptr { // CreateStatusArray
			if outPtr != 0 {
				*(*uintptr)(osPtr(outPtr)) = newDSStatus(int(uint32(capacity)))
			}
			return hresultOK
		}),
		6:  cbOK,          // SetCpuAffinity
		7:  cbOK,          // SetDebugFlags
		8:  cbOK,          // SetStagingBufferSize (0x40)
		9:  cbOK,          // GetCompressionCodec
		10: cbCreateQueue, // CreateQueue (2026 Xbox Series, 0x50)
	})

	cbGetFactory := syscall.NewCallback(func(iidPtr, outPtr uintptr) uintptr {
		slog.Info("XCRDAPI!DStorageGetFactory -> IDStorageFactoryX COM actif", "iid", FormatGUID(iidPtr))
		if outPtr != 0 {
			*(*uintptr)(osPtr(outPtr)) = dstorageFactoryObj
		}
		return hresultOK
	})
	t.bind("xcrdapi.dll", "DStorageGetFactory", cbGetFactory)
	t.bind("dstorage_xs.dll", "DStorageGetFactory", cbGetFactory)
}
