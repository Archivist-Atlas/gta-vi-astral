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
)

// dstorageState implémente un moteur DirectStorage console (XCRDAPI.dll)
// adossé à VirtualFS avec lecture réelle de fichiers .rpf et décompression zlib.
type dstorageState struct {
	vfs        *VirtualFS
	mu         sync.Mutex
	files      map[uintptr]*os.File
	statuses   map[uintptr]*dstorageStatus
	readCount  atomic.Uint64
	bytesRead  atomic.Uint64
}

type dstorageStatus struct {
	mu      sync.Mutex
	results []uint32
}

func bindDirectStorage(t *LiveTable, vfs *VirtualFS) {
	ds := &dstorageState{
		vfs:      vfs,
		files:    make(map[uintptr]*os.File),
		statuses: make(map[uintptr]*dstorageStatus),
	}

	cbOK := syscall.NewCallback(func() uintptr { return hresultOK })

	// IDStorageFileX (16 slots) : slot 3 = Close, slot 4 = GetFileInformation
	newDSFile := func(f *os.File) uintptr {
		var obj uintptr
		obj = NewCOMObject("IDStorageFileX", 16, map[int]uintptr{
			3: syscall.NewCallback(func(this uintptr) uintptr { // Close
				ds.mu.Lock()
				if fh, ok := ds.files[this]; ok {
					_ = fh.Close()
					delete(ds.files, this)
				}
				ds.mu.Unlock()
				return 0
			}),
			4: syscall.NewCallback(func(this, infoPtr uintptr) uintptr { // GetFileInformation (BY_HANDLE_FILE_INFORMATION = 52 bytes)
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
		ds.mu.Lock()
		ds.files[obj] = f
		ds.mu.Unlock()
		return obj
	}

	// IDStorageStatusArrayX (16 slots) : slot 3 = IsComplete, slot 4 = GetHResult
	newDSStatus := func(capacity int) uintptr {
		if capacity < 1 {
			capacity = 1
		}
		st := &dstorageStatus{results: make([]uint32, capacity)}
		var obj uintptr
		obj = NewCOMObject("IDStorageStatusArrayX", 16, map[int]uintptr{
			3: syscall.NewCallback(func(this, index uintptr) uintptr { return 1 }), // IsComplete -> TRUE
			4: syscall.NewCallback(func(this, index uintptr) uintptr { // GetHResult
				ds.mu.Lock()
				s := ds.statuses[this]
				ds.mu.Unlock()
				if s != nil && int(index) < len(s.results) {
					return uintptr(s.results[index])
				}
				return hresultOK
			}),
		})
		ds.mu.Lock()
		ds.statuses[obj] = st
		ds.mu.Unlock()
		return obj
	}

	// IDStorageQueueX (32 slots) : slot 3 = EnqueueRequest, slot 4 = EnqueueStatus, slot 5 = EnqueueSignal, slot 10 = Submit
	newDSQueue := func() uintptr {
		return NewCOMObject("IDStorageQueueX", 32, map[int]uintptr{
			3: syscall.NewCallback(func(this, reqPtr uintptr) uintptr { // EnqueueRequest
				if reqPtr == 0 {
					return 0
				}
				// DSTORAGE_REQUEST_X (layout simplifié) :
				// +0x00 Options (u64) : bits 0..7 = CompressionFormat (0=None, 1=Zlib), bit 8 = SourceType (0=File, 1=Memory)
				// +0x08 Source.File.File (ptr) / Source.Memory.Source (ptr)
				// +0x10 Source.File.Offset (u64) / Source.Memory.Size (u32)
				// +0x18 Source.File.Size (u32)
				// +0x20 Destination.Memory.Buffer (ptr)
				// +0x28 Destination.Memory.Size (u32) / UncompressedSize (u32)
				opts := *(*uint64)(osPtr(reqPtr))
				compFmt := opts & 0xFF
				srcType := (opts >> 8) & 1
				srcPtr := *(*uintptr)(osPtr(reqPtr + 0x08))
				srcOff := *(*uint64)(osPtr(reqPtr + 0x10))
				srcSize := *(*uint32)(osPtr(reqPtr + 0x18))
				dstPtr := *(*uintptr)(osPtr(reqPtr + 0x20))
				dstSize := *(*uint32)(osPtr(reqPtr + 0x28))

				if dstPtr == 0 || dstSize == 0 {
					return 0
				}
				var raw []byte
				if srcType == 0 && srcPtr != 0 && srcSize > 0 {
					ds.mu.Lock()
					fh := ds.files[srcPtr]
					ds.mu.Unlock()
					if fh != nil {
						raw = make([]byte, srcSize)
						n, _ := fh.ReadAt(raw, int64(srcOff))
						raw = raw[:n]
					}
				} else if srcType == 1 && srcPtr != 0 {
					sz := uint32(srcOff)
					if sz > 0 && sz <= (64<<20) {
						raw = make([]byte, sz)
						copy(raw, unsafe.Slice((*byte)(osPtr(srcPtr)), sz))
					}
				}
				if len(raw) == 0 {
					return 0
				}

				dst := unsafe.Slice((*byte)(osPtr(dstPtr)), dstSize)
				if compFmt == 1 { // ZLIB
					if zr, err := zlib.NewReader(bytes.NewReader(raw)); err == nil {
						n, _ := io.ReadFull(zr, dst)
						_ = zr.Close()
						ds.bytesRead.Add(uint64(n))
					}
				} else {
					n := copy(dst, raw)
					ds.bytesRead.Add(uint64(n))
				}
				ds.readCount.Add(1)
				return 0
			}),
			4:  cbOK, // EnqueueStatus
			5:  cbOK, // EnqueueSignal
			6:  cbOK, // EnqueueSetEvent
			10: cbOK, // Submit
		})
	}

	dstorageFactoryObj := NewCOMObject("IDStorageFactoryX", 32, map[int]uintptr{
		3: syscall.NewCallback(func(this, descPtr, iidPtr, outPtr uintptr) uintptr { // CreateQueue
			slog.Info("IDStorageFactoryX!CreateQueue -> IDStorageQueueX")
			if outPtr != 0 {
				*(*uintptr)(osPtr(outPtr)) = newDSQueue()
			}
			return hresultOK
		}),
		4: syscall.NewCallback(func(this, pathPtr, iidPtr, outPtr uintptr) uintptr { // OpenFile
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
				*(*uintptr)(osPtr(outPtr)) = newDSStatus(int(capacity))
			}
			return hresultOK
		}),
		6: cbOK, // SetDebugFlags
		7: cbOK, // SetStagingBufferSize
		8: cbOK, // SetCpuAffinity / config
		9: cbOK, // GetCompressionCodec
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
