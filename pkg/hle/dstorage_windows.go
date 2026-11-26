//go:build windows

package hle

import (
	"bytes"
	"compress/zlib"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/Archivist-Atlas/gta-vi-astral/pkg/formats"
	"github.com/Archivist-Atlas/gta-vi-astral/pkg/gpu"
)

const hresultPending uint32 = 0x8000000A

type dsCmdKind uint8

const (
	dsCmdRead dsCmdKind = iota
	dsCmdStatus
	dsCmdSignal
)

// dsPendingCmd mémorise une commande DirectStorage en attente jusqu'à Submit()
// ou auto-submit à capacity/2 (en ordre FIFO strict).
type dsPendingCmd struct {
	kind             dsCmdKind
	opts             uint64
	dstPtr           uintptr
	pageOff          int
	dstSize          uint32
	srcPtr           uintptr
	srcOff           uint64
	srcSize          uint32
	intermediateSize uint32
	statusArr        uintptr
	statusIdx        int
	fence            uintptr
	fenceVal         uintptr
}

type dsQueueState struct {
	mu          sync.Mutex
	cond        *sync.Cond
	capacity    int
	unsubmitted []dsPendingCmd
	submitted   [][]dsPendingCmd
	closed      bool
	done        chan struct{}
}

type dsChunkCache struct {
	srcPtr  uintptr
	opts    uint64
	srcOff  uint64
	srcSize uint32
	decoded []byte
}

type dstorageState struct {
	vfs           *VirtualFS
	mu            sync.Mutex
	files         map[uintptr]*os.File
	fileNames     map[uintptr]string
	statuses      map[uintptr]*dstorageStatus
	queues        map[uintptr]*dsQueueState
	lastChunks    map[uintptr][]dsChunkCache
	readCount     atomic.Uint64
	bytesRead     atomic.Uint64
	pageWrites    atomic.Uint64
	bcpackDecoded atomic.Uint64
	bcpackWarns   atomic.Int32
	zlibWarns     atomic.Int32
	chunkContHits atomic.Uint64
}

type dstorageStatus struct {
	mu      sync.Mutex
	results []uint32
}

func bindDirectStorage(t *LiveTable, vfs *VirtualFS) {
	ds := &dstorageState{
		vfs:        vfs,
		files:      make(map[uintptr]*os.File),
		fileNames:  make(map[uintptr]string),
		statuses:   make(map[uintptr]*dstorageStatus),
		queues:     make(map[uintptr]*dsQueueState),
		lastChunks: make(map[uintptr][]dsChunkCache),
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
	newDSFile := func(f *os.File, name string) uintptr {
		obj, _, _ := k32VirtualAlloc.Call(0, 0x20, 0x3000, 0x04)
		if obj != 0 {
			*(*uintptr)(osPtr(obj)) = dsFileVtable
			ds.mu.Lock()
			ds.files[obj] = f
			ds.fileNames[obj] = name
			ds.mu.Unlock()
		}
		return obj
	}

	dsStatusProto := NewCOMObject("IDStorageStatusArrayX", 16, map[int]uintptr{
		3: syscall.NewCallback(func(this, index uintptr) uintptr { // IsComplete
			idx := int(uint32(index))
			ds.mu.Lock()
			s := ds.statuses[this]
			ds.mu.Unlock()
			if s != nil {
				s.mu.Lock()
				done := idx < 0 || idx >= len(s.results) || s.results[idx] != hresultPending
				s.mu.Unlock()
				if !done {
					return 0
				}
			}
			return 1
		}),
		4: syscall.NewCallback(func(this, index uintptr) uintptr { // GetHResult
			idx := int(uint32(index))
			ds.mu.Lock()
			s := ds.statuses[this]
			ds.mu.Unlock()
			if s != nil {
				s.mu.Lock()
				res := uint32(hresultOK)
				if idx >= 0 && idx < len(s.results) {
					res = s.results[idx]
				}
				s.mu.Unlock()
				return uintptr(res)
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

	recordChunk := func(srcPtr uintptr, opts, srcOff uint64, srcSize uint32) {
		ds.mu.Lock()
		list := ds.lastChunks[srcPtr]
		for i := range list {
			if list[i].srcOff == srcOff && list[i].srcSize == srcSize && list[i].opts == opts {
				ds.mu.Unlock()
				return
			}
		}
		if len(list) >= 16 {
			list = list[1:]
		}
		list = append(list, dsChunkCache{srcPtr: srcPtr, opts: opts, srcOff: srcOff, srcSize: srcSize})
		ds.lastChunks[srcPtr] = list
		ds.mu.Unlock()
	}

	execDSReq := func(r dsPendingCmd) uint32 {
		opts, dstPtr, pageOff, dstSize, srcPtr, srcOff, srcSize, interSize := r.opts, r.dstPtr, r.pageOff, r.dstSize, r.srcPtr, r.srcOff, r.srcSize, r.intermediateSize
		if dstPtr == 0 || dstSize == 0 || srcPtr == 0 {
			return uint32(hresultOK)
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
			if fh != nil && srcSize > 0 && srcSize <= (64<<20) {
				raw = make([]byte, srcSize)
				n, _ := fh.ReadAt(raw, int64(srcOff))
				raw = raw[:n]
			} else if fh == nil && srcSize > 0 && srcSize <= (64<<20) {
				raw = make([]byte, srcSize)
				copy(raw, unsafe.Slice((*byte)(osPtr(srcPtr)), srcSize))
			}
		}
		if len(raw) == 0 {
			slog.Warn("IDStorageQueueX!EnqueueRequest raw vide", "opts", fmt.Sprintf("0x%x", opts), "srcSize", srcSize, "dstSize", dstSize)
			return 0x80070026 // ERROR_HANDLE_EOF
		}
		bcFmt := (opts & optBCPack) >> 1
		isPageDst := (opts & optPageDst) != 0
		var dst []byte
		if isPageDst {
			dst = make([]byte, dstSize)
		} else {
			dst = unsafe.Slice((*byte)(osPtr(dstPtr)), dstSize)
		}
		headHex := func(b []byte) string {
			if len(b) > 8 {
				b = b[:8]
			}
			return hex.EncodeToString(b)
		}
		// Helper de continuation multi-pages (par fichier, jusqu'à 16 chunks en vol par archive) :
		// quand un bloc compressé > 64 KiB est divisé en requêtes de 64 KiB à srcOff + k*65536
		// avec le même srcSize, on décompresse le bloc entier depuis baseOff une seule fois.
		tryChunkContinuation := func() bool {
			ds.mu.Lock()
			list := ds.lastChunks[srcPtr]
			matchIdx := -1
			var lc dsChunkCache
			for i := len(list) - 1; i >= 0; i-- {
				c := list[i]
				if c.opts == opts && c.srcSize == srcSize && srcOff > c.srcOff {
					delta := srcOff - c.srcOff
					if delta%65536 == 0 && delta <= (4<<20) {
						lc = c
						matchIdx = i
						break
					}
				}
			}
			ds.mu.Unlock()
			if matchIdx < 0 {
				return false
			}
			delta := srcOff - lc.srcOff
			needEnd := int(delta) + int(dstSize)
			if len(lc.decoded) < needEnd {
				fullSize := ((int(srcSize) + 65535) &^ 65535)
				if fullSize < needEnd {
					fullSize = needEnd
				}
				if fullSize < 262144 && srcSize > 65536 {
					fullSize = 262144
				}
				ds.mu.Lock()
				fh := ds.files[srcPtr]
				ds.mu.Unlock()
				if fh == nil {
					return false
				}
				baseRaw := make([]byte, srcSize)
				n, _ := fh.ReadAt(baseRaw, int64(lc.srcOff))
				baseRaw = baseRaw[:n]
				if len(baseRaw) == 0 {
					return false
				}
				var fullDec []byte
				if bcFmt != 0 {
					packed := baseRaw
					if opts&optZlib != 0 {
						if zr, err := zlib.NewReader(bytes.NewReader(baseRaw)); err == nil {
							if d, _ := io.ReadAll(zr); len(d) > 0 {
								packed = d
							}
							_ = zr.Close()
						}
					}
					fullDec = make([]byte, fullSize)
					if err := formats.DecodeBCPack(uint8(bcFmt), packed, fullDec, nil); err != nil {
						return false
					}
				} else if opts&optZlib != 0 {
					zr, err := zlib.NewReader(bytes.NewReader(baseRaw))
					if err != nil {
						return false
					}
					fullDec, _ = io.ReadAll(zr)
					_ = zr.Close()
					if len(fullDec) < needEnd {
						return false
					}
				} else {
					return false
				}
				ds.mu.Lock()
				curList := ds.lastChunks[srcPtr]
				for i := range curList {
					if curList[i].srcOff == lc.srcOff && curList[i].srcSize == lc.srcSize && curList[i].opts == lc.opts {
						curList[i].decoded = fullDec
						break
					}
				}
				lc.decoded = fullDec
				ds.mu.Unlock()
			}
			copy(dst, lc.decoded[delta:needEnd])
			if hits := ds.chunkContHits.Add(1); hits == 1 || hits%50 == 0 {
				slog.Info("IDStorageQueueX!continuation bloc multi-pages active", "hits", hits, "bcFmt", bcFmt, "delta", delta, "dstSize", dstSize)
			}
			return true
		}
		fileName := func() string {
			ds.mu.Lock()
			nm := ds.fileNames[srcPtr]
			ds.mu.Unlock()
			return nm
		}
		var written int
		resHR := uint32(hresultOK)
		if bcFmt != 0 {
			// Ordre matériel Xbox Series : ZLIB d'abord (si combiné), puis BCPack (BC1/BC3/BC4/BC5/BC6H/BC7)
			packed := raw
			zlibOK := true
			if opts&optZlib != 0 {
				zr, err := zlib.NewReader(bytes.NewReader(raw))
				if err != nil {
					zlibOK = false
					if ds.bcpackWarns.Add(1) <= 8 {
						slog.Warn("IDStorageQueueX!BCPack+ZLIB en-tête zlib invalide",
							"file", fileName(), "opts", fmt.Sprintf("0x%x", opts), "bcFmt", bcFmt, "srcSize", len(raw),
							"interSize", interSize, "dstSize", dstSize, "srcOff", srcOff, "head", headHex(raw), "err", err)
					}
				} else {
					if interSize > 0 && interSize <= (64<<20) {
						dec := make([]byte, interSize)
						n, _ := io.ReadFull(zr, dec)
						if n > 0 {
							packed = dec[:n]
						} else {
							zlibOK = false
						}
					} else {
						dec, _ := io.ReadAll(zr)
						if len(dec) > 0 {
							packed = dec
						} else {
							zlibOK = false
						}
					}
					_ = zr.Close()
				}
			}
			if zlibOK {
				if err := formats.DecodeBCPack(uint8(bcFmt), packed, dst, nil); err == nil {
					written = int(dstSize)
					ds.bytesRead.Add(uint64(written))
					if srcSize > dstSize {
						recordChunk(srcPtr, opts, srcOff, srcSize)
					}
					if n := ds.bcpackDecoded.Add(1); n == 1 || n%250 == 0 {
						slog.Info("IDStorageQueueX!BCPack décodage matériel pur Go actif", "count", n, "bcFmt", bcFmt, "srcSize", len(packed), "dstSize", dstSize)
					}
				} else if tryChunkContinuation() {
					written = int(dstSize)
					ds.bytesRead.Add(uint64(written))
				} else {
					if ds.bcpackWarns.Add(1) <= 8 {
						slog.Warn("IDStorageQueueX!EnqueueRequest BCPack erreur décodage (repli zéro)",
							"file", fileName(), "opts", fmt.Sprintf("0x%x", opts), "bcFmt", bcFmt, "srcSize", len(packed),
							"interSize", interSize, "dstSize", dstSize, "srcOff", srcOff, "head", headHex(packed), "err", err)
					}
					clear(dst)
					written = int(dstSize)
				}
			} else if tryChunkContinuation() {
				written = int(dstSize)
				ds.bytesRead.Add(uint64(written))
			} else {
				clear(dst)
				written = int(dstSize)
			}
		} else if opts&optZlib != 0 { // ZLIB (RFC 1950)
			if zr, err := zlib.NewReader(bytes.NewReader(raw)); err == nil {
				var rerr error
				written, rerr = io.ReadFull(zr, dst)
				_ = zr.Close()
				ds.bytesRead.Add(uint64(written))
				if srcSize > dstSize {
					recordChunk(srcPtr, opts, srcOff, srcSize)
				}
				if rerr != nil && rerr != io.EOF && rerr != io.ErrUnexpectedEOF {
					if ds.zlibWarns.Add(1) <= 8 {
						slog.Warn("IDStorageQueueX!EnqueueRequest zlib read err",
							"file", fileName(), "opts", fmt.Sprintf("0x%x", opts), "err", rerr, "written", written, "dstSize", dstSize)
					}
				}
			} else if tryChunkContinuation() {
				written = int(dstSize)
				ds.bytesRead.Add(uint64(written))
			} else {
				resHR = 0x8007000D // ERROR_INVALID_DATA
				if ds.zlibWarns.Add(1) <= 8 {
					slog.Warn("IDStorageQueueX!EnqueueRequest zlib err",
						"file", fileName(), "opts", fmt.Sprintf("0x%x", opts), "err", err, "srcSize", srcSize, "dstSize", dstSize,
						"interSize", interSize, "srcOff", srcOff, "head", headHex(raw))
				}
			}
		} else {
			written = copy(dst, raw)
			ds.bytesRead.Add(uint64(written))
		}
		if isPageDst && written > 0 {
			nPages := (pageOff + int(dstSize) + int(xmemPageSize) - 1) / int(xmemPageSize)
			if nPages > 0 && nPages <= 4096 {
				// Le jeu remplit le tableau de pages physiques APRÈS EnqueueRequest (souvent depuis un autre thread) :
				// lire le tableau au moment de l'exécution et attendre jusqu'à 100 ms si une entrée vaut encore 0.
				readPages := func() ([]uintptr, bool) {
					src := unsafe.Slice((*uintptr)(osPtr(dstPtr)), nPages)
					out := make([]uintptr, nPages)
					hasZero := false
					for idx := 0; idx < nPages; idx++ {
						v := atomic.LoadUintptr(&src[idx])
						out[idx] = v
						if v == 0 {
							hasZero = true
						}
					}
					return out, hasZero
				}
				pages, hasZero := readPages()
				if hasZero {
					deadline := time.Now().Add(100 * time.Millisecond)
					for hasZero && time.Now().Before(deadline) {
						time.Sleep(2 * time.Millisecond)
						pages, hasZero = readPages()
					}
				}
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
		return resHR
	}
	runBatch := func(cmds []dsPendingCmd) {
		// Exécute les lots consécutifs de lectures indépendantes en parallèle entre barrières Status/Signal
		workers := runtime.GOMAXPROCS(0)
		if workers > 8 {
			workers = 8
		}
		batchHR := uint32(hresultOK)
		for i := 0; i < len(cmds); {
			if cmds[i].kind == dsCmdRead {
				j := i
				canPar := true
				for j < len(cmds) && cmds[j].kind == dsCmdRead {
					if cmds[j].srcSize > cmds[j].dstSize {
						canPar = false
					}
					j++
				}
				if canPar && j-i >= 4 && workers > 1 {
					batch := cmds[i:j]
					hrs := make([]uint32, len(batch))
					var nextIdx atomic.Int32
					var wg sync.WaitGroup
					nw := workers
					if nw > len(batch) {
						nw = len(batch)
					}
					wg.Add(nw)
					for w := 0; w < nw; w++ {
						go func() {
							defer wg.Done()
							for {
								k := int(nextIdx.Add(1) - 1)
								if k >= len(batch) {
									return
								}
								hrs[k] = execDSReq(batch[k])
							}
						}()
					}
					wg.Wait()
					if batchHR == uint32(hresultOK) {
						for _, hr := range hrs {
							if hr != uint32(hresultOK) {
								batchHR = hr
								break
							}
						}
					}
				} else {
					for k := i; k < j; k++ {
						hr := execDSReq(cmds[k])
						if batchHR == uint32(hresultOK) && hr != uint32(hresultOK) {
							batchHR = hr
						}
					}
				}
				i = j
				continue
			}
			c := cmds[i]
			i++
			switch c.kind {
			case dsCmdStatus:
				ds.mu.Lock()
				s := ds.statuses[c.statusArr]
				ds.mu.Unlock()
				if s != nil {
					s.mu.Lock()
					if c.statusIdx >= 0 && c.statusIdx < len(s.results) {
						// Toujours signaler S_OK : une seule requête hors-limites (ex: srcSize > 64 MiB)
						// ne doit pas figer définitivement le streamer DirectStorage de GTA VI.
						s.results[c.statusIdx] = uint32(hresultOK)
					}
					s.mu.Unlock()
				}
				batchHR = uint32(hresultOK)
			case dsCmdSignal:
				if c.fence >= 0x10000 {
					vtbl := *(*uintptr)(osPtr(c.fence))
					if vtbl >= 0x10000 {
						fn := *(*uintptr)(osPtr(vtbl + 10*8)) // ID3D12Fence::Signal (slot 10)
						if fn >= 0x10000 {
							_, _, _ = syscall.SyscallN(fn, c.fence, c.fenceVal)
						}
					}
				}
			}
		}
	}
	getQueue := func(q uintptr) *dsQueueState {
		ds.mu.Lock()
		qs := ds.queues[q]
		ds.mu.Unlock()
		return qs
	}
	submitQueue := func(q uintptr) {
		qs := getQueue(q)
		if qs == nil {
			return
		}
		qs.mu.Lock()
		batch := qs.unsubmitted
		qs.unsubmitted = nil
		qs.mu.Unlock()
		if len(batch) > 0 {
			runBatch(batch)
		}
	}
	enqueueCmd := func(q uintptr, cmd dsPendingCmd) {
		qs := getQueue(q)
		if qs == nil {
			return
		}
		qs.mu.Lock()
		if !qs.closed {
			qs.unsubmitted = append(qs.unsubmitted, cmd)
		}
		qs.mu.Unlock()
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
			interSize := *(*uint32)(osPtr(reqPtr + 0x34))

			if dstPtr == 0 || dstSize == 0 || srcPtr == 0 {
				slog.Warn("IDStorageQueueX!EnqueueRequest ignoré (null/zero)", "opts", opts, "dst", dstPtr, "dstSize", dstSize, "src", srcPtr, "srcSize", srcSize)
				return 0
			}
			enqueueCmd(this, dsPendingCmd{
				kind: dsCmdRead, opts: opts, dstPtr: dstPtr, pageOff: pageOff, dstSize: dstSize,
				srcPtr: srcPtr, srcOff: srcOff, srcSize: srcSize, intermediateSize: interSize,
			})
			return 0
		}),
		4: syscall.NewCallback(func(this, statusArr, index uintptr) uintptr { // EnqueueStatus (0x20)
			idx := int(uint32(index))
			ds.mu.Lock()
			s := ds.statuses[statusArr]
			ds.mu.Unlock()
			if s != nil {
				s.mu.Lock()
				if idx >= 0 && idx < len(s.results) {
					s.results[idx] = hresultPending
				}
				s.mu.Unlock()
			}
			enqueueCmd(this, dsPendingCmd{
				kind: dsCmdStatus, statusArr: statusArr, statusIdx: idx,
			})
			return hresultOK
		}),
		5: syscall.NewCallback(func(this, fence, val uintptr) uintptr { // EnqueueSignal (0x28)
			enqueueCmd(this, dsPendingCmd{
				kind: dsCmdSignal, fence: fence, fenceVal: val,
			})
			return hresultOK
		}),
		6: syscall.NewCallback(func(this uintptr) uintptr { // Submit (0x30) — asynchrone
			submitQueue(this)
			return hresultOK
		}),
		8: syscall.NewCallback(func(this uintptr) uintptr { // Close (0x40)
			qs := getQueue(this)
			if qs != nil {
				qs.mu.Lock()
				qs.closed = true
				qs.unsubmitted = nil
				qs.cond.Broadcast()
				qs.mu.Unlock()
				<-qs.done
			}
			return hresultOK
		}),
		10: cbOK, // ErrorRecord fallback
	})
	dsQueueVtable := *(*uintptr)(osPtr(dsQueueProto))
	newDSQueue := func(capacity int) uintptr {
		if capacity < 2 {
			capacity = 2048
		}
		if capacity > 8192 {
			capacity = 8192
		}
		obj, _, _ := k32VirtualAlloc.Call(0, 0x20, 0x3000, 0x04)
		if obj != 0 {
			*(*uintptr)(osPtr(obj)) = dsQueueVtable
			qs := &dsQueueState{
				capacity: capacity,
				done:     make(chan struct{}),
			}
			qs.cond = sync.NewCond(&qs.mu)
			ds.mu.Lock()
			ds.queues[obj] = qs
			ds.mu.Unlock()
			go func(q *dsQueueState) {
				defer close(q.done)
				for {
					q.mu.Lock()
					for len(q.submitted) == 0 && !q.closed {
						q.cond.Wait()
					}
					if len(q.submitted) == 0 && q.closed {
						q.mu.Unlock()
						return
					}
					batch := q.submitted[0]
					q.submitted[0] = nil
					q.submitted = q.submitted[1:]
					q.mu.Unlock()
					runBatch(batch)
				}
			}(qs)
		}
		return obj
	}

	cbCreateQueue := syscall.NewCallback(func(this, descPtr, iidPtr, outPtr uintptr) uintptr {
		capVal := 2048
		if descPtr >= 0x10000 {
			c := int(*(*uint16)(osPtr(descPtr + 8)))
			if c >= 2 {
				capVal = c
			}
		}
		slog.Info("IDStorageFactoryX!CreateQueue -> IDStorageQueueX", "capacity", capVal)
		if outPtr != 0 {
			*(*uintptr)(osPtr(outPtr)) = newDSQueue(capVal)
		}
		return hresultOK
	})

	dstorageFactoryObj := NewCOMObject("IDStorageFactoryX", 32, map[int]uintptr{
		3:  cbCreateQueue, // CreateQueue (2020)
		4:  syscall.NewCallback(func(this, pathPtr, iidPtr, outPtr uintptr) uintptr { // OpenFile
			orig := readUTF16Str(pathPtr)
			if strings.Contains(strings.ToLower(orig), "story000.rpf") {
				gpu.ReportWorldLoadingStarted(ds.readCount.Load())
			}
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
				*(*uintptr)(osPtr(outPtr)) = newDSFile(fh, orig)
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
