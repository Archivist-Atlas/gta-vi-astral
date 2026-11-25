//go:build windows

package hle

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"
)

var (
	k32CreateEventW = k32.NewProc("CreateEventW")
)

type apuRegion struct {
	va   uintptr
	apu  uint32
	size uint32
}

type apuState struct {
	mu      sync.Mutex
	regions []apuRegion
	nextApu uint32
	shapes  [6]uintptr
}

func (a *apuState) alloc(size, align uint32) (uintptr, uint32) {
	if size == 0 {
		size = 64
	}
	if align < 64 {
		align = 64
	}
	allocSz := (uintptr(size) + uintptr(align) + 4095) &^ 4095
	mem, _, _ := k32VirtualAlloc.Call(0, allocSz, 0x3000 /* COMMIT|RESERVE */, 0x04 /* PAGE_READWRITE */)
	if mem == 0 {
		return 0, 0
	}
	a.mu.Lock()
	if a.nextApu == 0 {
		a.nextApu = 0x10000
	}
	apuAddr := (a.nextApu + align - 1) &^ (align - 1)
	a.nextApu = apuAddr + ((size + 63) &^ 63)
	a.regions = append(a.regions, apuRegion{va: mem, apu: apuAddr, size: size})
	a.mu.Unlock()
	return mem, apuAddr
}

func (a *apuState) mapVA(va uintptr, size uint32) uint32 {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range a.regions {
		if va >= r.va && va < r.va+uintptr(r.size) {
			return r.apu + uint32(va-r.va)
		}
	}
	if a.nextApu == 0 {
		a.nextApu = 0x10000
	}
	apuAddr := (a.nextApu + 63) &^ 63
	sz := size
	if sz == 0 {
		sz = 64
	}
	a.nextApu = apuAddr + ((sz + 63) &^ 63)
	a.regions = append(a.regions, apuRegion{va: va, apu: apuAddr, size: sz})
	return apuAddr
}

func (a *apuState) mapApu(apu uint32) uintptr {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range a.regions {
		if apu >= r.apu && apu < r.apu+r.size {
			return r.va + uintptr(apu-r.apu)
		}
	}
	return 0
}

// bindAcpHal implémente AcpHal.dll (mémoire APU, contextes SHAPE et canal IAcpHal).
func bindAcpHal(t *LiveTable) {
	apu := &apuState{nextApu: 0x10000}
	cbOK := syscall.NewCallback(func() uintptr { return hresultOK })
	cbOne := syscall.NewCallback(func() uintptr { return 1 })

	cbApuCreateHeap := syscall.NewCallback(func(size, flags uintptr) uintptr {
		slog.Info("AcpHal!ApuCreateHeap", "size", uint32(size), "flags", uint32(flags))
		return hresultOK
	})
	cbApuAlloc := syscall.NewCallback(func(vaOut, apuOut, size, align, flags uintptr) uintptr {
		if vaOut == 0 {
			return hresultInvalidArg
		}
		va, apuAddr := apu.alloc(uint32(size), uint32(align))
		if va == 0 {
			return 0x8007000E // E_OUTOFMEMORY
		}
		*(*uintptr)(osPtr(vaOut)) = va
		if apuOut != 0 {
			*(*uint32)(osPtr(apuOut)) = apuAddr
		}
		return hresultOK
	})
	cbApuFree := syscall.NewCallback(func(va uintptr) uintptr {
		return hresultOK
	})
	cbApuMapVA := syscall.NewCallback(func(apuOut, va, size uintptr) uintptr {
		if apuOut == 0 {
			return hresultInvalidArg
		}
		*(*uint32)(osPtr(apuOut)) = apu.mapVA(va, uint32(size))
		return hresultOK
	})
	cbApuMapApu := syscall.NewCallback(func(apuAddr uintptr) uintptr {
		return apu.mapApu(uint32(apuAddr))
	})
	cbApuHeapState := syscall.NewCallback(func(statePtr, flags uintptr) uintptr {
		if statePtr == 0 {
			return hresultInvalidArg
		}
		s := unsafe.Slice((*uint32)(osPtr(statePtr)), 5)
		s[0] = 256 << 20 // free
		s[1] = 0         // used
		s[2] = 0         // lost
		s[3] = 64 << 20  // largest free
		s[4] = 1         // blocks
		return hresultOK
	})

	// AcpHalAllocateShapeContexts(ShapeContexts*) : 0x60 octets
	// struct { uint32 counts[6] (+0); void* arrays[6] (+24); uint32 apu[6] (+72); }
	cbAllocShape := syscall.NewCallback(func(ctxPtr uintptr) uintptr {
		if ctxPtr == 0 {
			return hresultInvalidArg
		}
		counts := unsafe.Slice((*uint32)(osPtr(ctxPtr)), 6)
		ptrs := unsafe.Slice((*uintptr)(osPtr(ctxPtr+24)), 6)
		apus := unsafe.Slice((*uint32)(osPtr(ctxPtr+72)), 6)
		strides := [6]uint32{32, 32, 64, 64, 64, 64}
		for i := 0; i < 6; i++ {
			if counts[i] == 0 {
				ptrs[i] = 0
				apus[i] = 0
				continue
			}
			va, apuAddr := apu.alloc(counts[i]*strides[i], 256)
			if va == 0 {
				return 0x8007000E
			}
			ptrs[i] = va
			apus[i] = apuAddr
			apu.mu.Lock()
			apu.shapes[i] = va
			apu.mu.Unlock()
		}
		slog.Info("AcpHal!AcpHalAllocateShapeContexts", "counts", []uint32{counts[0], counts[1], counts[2], counts[3], counts[4], counts[5]})
		return hresultOK
	})

	// IAcpHal vtable : Connect(0), Disconnect(1), SubmitCommand(2), PopMessage(3), GetNumMessages(4), Release(5)
	// Note : IAcpHal n'hérite pas d'IUnknown, ses méthodes démarrent au slot 0.
	type acpMessage struct {
		kind    uint32
		dropped uint32
		usec    uint32
		data    [16]byte
	}
	var (
		acpMu       sync.Mutex
		acpMask     uint32
		acpCap      = 256
		acpDropped  uint32
		acpMsgs     []acpMessage
		acpSince    = time.Now()
		acpSubOnce  atomic.Bool
	)
	postAcpMsg := func(kind uint32, payload []byte) {
		if acpMask&kind == 0 {
			return
		}
		if len(acpMsgs) >= acpCap {
			acpDropped++
			return
		}
		m := acpMessage{
			kind:    kind,
			dropped: acpDropped,
			usec:    uint32(time.Since(acpSince).Microseconds()),
		}
		acpDropped = 0
		copy(m.data[:], payload)
		acpMsgs = append(acpMsgs, m)
	}
	acpHalObj := NewCOMObject("IAcpHal", 16, map[int]uintptr{
		0: syscall.NewCallback(func(this, cmds, msgs uintptr) uintptr { // Connect
			acpMu.Lock()
			if msgs > 0 && msgs <= 4096 {
				acpCap = int(msgs)
			}
			acpMu.Unlock()
			return hresultOK
		}),
		1: syscall.NewCallback(func(this uintptr) uintptr { // Disconnect
			acpMu.Lock()
			postAcpMsg(0x80 /* DISCONNECTED */, nil)
			acpMu.Unlock()
			return hresultOK
		}),
		2: syscall.NewCallback(func(this, cmdArg, idArg, frameArg, dataPtr, notifApu uintptr) uintptr { // SubmitCommand
			cmd := uint32(cmdArg)
			id := uint64(idArg)
			if acpSubOnce.CompareAndSwap(false, true) {
				slog.Info("IAcpHal!SubmitCommand actif", "cmd", cmd, "id", id, "notif", fmt.Sprintf("0x%X", uint32(notifApu)))
			}
			acpMu.Lock()
			switch cmd {
			case 2: // REGISTER_MESSAGES
				if dataPtr >= 0x10000 {
					acpMask |= *(*uint32)(osPtr(dataPtr))
				}
			case 3: // UNREGISTER_MESSAGES
				if dataPtr >= 0x10000 {
					acpMask &^= *(*uint32)(osPtr(dataPtr))
				}
			case 1: // LOAD_FLOWGRAPH
				var fg [4]byte
				if dataPtr >= 0x10000 {
					binary.LittleEndian.PutUint32(fg[:], *(*uint32)(osPtr(dataPtr + 4)))
				}
				postAcpMsg(0x02 /* FLOWGRAPH_COMPLETED */, fg[:])
			}
			if id != 0 {
				var done [16]byte
				binary.LittleEndian.PutUint32(done[0:4], cmd)
				binary.LittleEndian.PutUint64(done[4:12], id)
				frame := uint32(time.Since(acpSince).Microseconds() * 375 / 1_000_000)
				binary.LittleEndian.PutUint32(done[12:16], frame)
				postAcpMsg(0x10 /* COMMAND_COMPLETED */, done[:])
			}
			acpMu.Unlock()
			if uint32(notifApu) != 0 {
				if va := apu.mapApu(uint32(notifApu)); va >= 0x10000 {
					atomic.StoreUint32((*uint32)(osPtr(va)), 1)
				}
			}
			return hresultOK
		}),
		3: syscall.NewCallback(func(this, msgPtr uintptr) uintptr { // PopMessage
			acpMu.Lock()
			if len(acpMsgs) == 0 {
				acpMu.Unlock()
				return 0
			}
			m := acpMsgs[0]
			acpMsgs = acpMsgs[1:]
			acpMu.Unlock()
			if msgPtr >= 0x10000 {
				*(*acpMessage)(osPtr(msgPtr)) = m
			}
			return 1
		}),
		4: syscall.NewCallback(func(this uintptr) uintptr { // GetNumMessages
			acpMu.Lock()
			n := uintptr(len(acpMsgs))
			acpMu.Unlock()
			return n
		}),
		5: cbOK, // Release
	})
	cbAcpHalCreate := syscall.NewCallback(func(outPtr uintptr) uintptr {
		slog.Info("AcpHal!AcpHalCreate -> IAcpHal actif")
		if outPtr != 0 {
			*(*uintptr)(osPtr(outPtr)) = acpHalObj
		}
		return hresultOK
	})

	for _, dll := range []string{"acphal.dll", "xapu.dll"} {
		t.bind(dll, "ApuCreateHeap", cbApuCreateHeap)
		t.bind(dll, "ApuAlloc", cbApuAlloc)
		t.bind(dll, "ApuFree", cbApuFree)
		t.bind(dll, "ApuMapVirtualAddress", cbApuMapVA)
		t.bind(dll, "ApuMapApuAddress", cbApuMapApu)
		t.bind(dll, "ApuIsVirtualAddressValid", cbOne)
		t.bind(dll, "ApuHeapGetState", cbApuHeapState)
		t.bind(dll, "AcpHalAllocateShapeContexts", cbAllocShape)
		t.bind(dll, "AcpHalReleaseShapeContexts", cbOK)
		t.bind(dll, "AcpHalCreate", cbAcpHalCreate)
	}

	// --- xapu.dll (décodeur audio matériel Xbox Series) ---
	type xapuResult struct {
		id  [24]byte // CommandId : { kind u32, stream u32, sequence u32, options u32, context u64 }
		hr  uint32
		pad uint32
	}
	type xapuConn struct {
		mu      sync.Mutex
		readyEv uintptr
		results []xapuResult
	}
	var (
		nextXApuHandle atomic.Uint64
		xapuMu         sync.Mutex
		xapuConns      = map[uintptr]*xapuConn{}
		xapuEnqLogged  atomic.Bool
		xdspActLogged  atomic.Bool
	)
	nextXApuHandle.Store(0x880000)

	cbXApuConnect := syscall.NewCallback(func(inPtr, outPtr, handleOut uintptr) uintptr {
		if inPtr == 0 || outPtr == 0 || handleOut == 0 {
			return hresultInvalidArg
		}
		// XApuConnectInput : { process u32 (+0), streams u32 (+4), cmds u32 (+8), data_bytes u32 (+12), frames u32 (+16), options u32 (+20) }
		streams := *(*uint32)(osPtr(inPtr + 4))
		dataBytes := *(*uint32)(osPtr(inPtr + 12))
		options := *(*uint32)(osPtr(inPtr + 20))
		var dataMem uintptr
		if dataBytes > 0 {
			allocSz := (uintptr(dataBytes) + 65535) &^ 65535
			dataMem, _, _ = k32VirtualAlloc.Call(0, allocSz, 0x3000, 0x04)
		}
		var readyEv uintptr
		if options&8 != 0 {
			readyEv, _, _ = k32CreateEventW.Call(0, 0, 0, 0)
		}
		*(*uintptr)(osPtr(outPtr + 0)) = dataMem
		*(*uintptr)(osPtr(outPtr + 8)) = readyEv
		h := uintptr(nextXApuHandle.Add(1))
		xapuMu.Lock()
		xapuConns[h] = &xapuConn{readyEv: readyEv}
		xapuMu.Unlock()
		*(*uintptr)(osPtr(handleOut)) = h
		slog.Info("xapu!XApuConnect", "streams", streams, "dataBytes", dataBytes, "handle", h)
		return hresultOK
	})
	cbXApuEnqueue := syscall.NewCallback(func(handle, cmdPtr, cmdSize uintptr) uintptr {
		if xapuEnqLogged.CompareAndSwap(false, true) {
			slog.Info("xapu!XApuEnqueueCommand actif", "handle", handle, "cmdSize", uint32(cmdSize))
		}
		xapuMu.Lock()
		c := xapuConns[handle]
		xapuMu.Unlock()
		if c != nil && cmdPtr >= 0x10000 && uint32(cmdSize) >= 24 {
			var res xapuResult
			copy(res.id[:], unsafe.Slice((*byte)(osPtr(cmdPtr)), 24))
			res.hr = 0 // S_OK
			opts := *(*uint32)(osPtr(cmdPtr + 12))
			c.mu.Lock()
			c.results = append(c.results, res)
			c.mu.Unlock()
			if opts&1 != 0 && c.readyEv != 0 {
				_, _, _ = k32SetEvent.Call(c.readyEv)
			}
		}
		return hresultOK
	})
	cbXApuGetResultCount := syscall.NewCallback(func(handle, totalPtr uintptr) uintptr {
		var n uint32
		xapuMu.Lock()
		c := xapuConns[handle]
		xapuMu.Unlock()
		if c != nil {
			c.mu.Lock()
			n = uint32(len(c.results))
			c.mu.Unlock()
		}
		if totalPtr != 0 {
			*(*uint32)(osPtr(totalPtr)) = n
		}
		return hresultOK
	})
	cbXApuDequeueResult := syscall.NewCallback(func(handle, resPtr uintptr) uintptr {
		xapuMu.Lock()
		c := xapuConns[handle]
		xapuMu.Unlock()
		if c == nil {
			return 0x8AC81001 // XAPU_E_PENDING_RESULTS
		}
		c.mu.Lock()
		if len(c.results) == 0 {
			c.mu.Unlock()
			return 0x8AC81001 // XAPU_E_PENDING_RESULTS
		}
		res := c.results[0]
		c.results = c.results[1:]
		c.mu.Unlock()
		if resPtr >= 0x10000 {
			// XApuResult : CommandId (24B) + HRESULT (4B) + outputFrames/pad
			dst := unsafe.Slice((*byte)(osPtr(resPtr)), 32)
			clear(dst)
			copy(dst[:24], res.id[:])
		}
		return hresultOK
	})
	cbXApuGetUtilization := syscall.NewCallback(func(a, b, outPtr uintptr) uintptr {
		if outPtr != 0 {
			*(*uint32)(osPtr(outPtr)) = 256
		}
		return hresultOK
	})
	t.bind("xapu.dll", "XApuConnect", cbXApuConnect)
	t.bind("xapu.dll", "XApuDisconnect", cbOK)
	t.bind("xapu.dll", "XApuEnqueueCommand", cbXApuEnqueue)
	t.bind("xapu.dll", "XApuDequeueResult", cbXApuDequeueResult)
	t.bind("xapu.dll", "XApuGetResultCount", cbXApuGetResultCount)
	t.bind("xapu.dll", "XApuGetUtilization", cbXApuGetUtilization)

	// --- chad.dll (XDsp processeur audio DSP console) ---
	type xdspStream struct {
		statusPtr uintptr // { submitted u32, completed u32, reserved u32, result i32 }
	}
	var (
		xdspMu      sync.Mutex
		xdspStreams = map[uintptr]*xdspStream{}
	)
	cbXDspConnect := syscall.NewCallback(func(a, b, c, streams, flags, handleOut uintptr) uintptr {
		if handleOut == 0 {
			return hresultInvalidArg
		}
		h := uintptr(nextXApuHandle.Add(1))
		*(*uintptr)(osPtr(handleOut)) = h
		slog.Info("chad!XDspConnectWithMaximumStreamLimit", "streams", uint32(streams), "handle", h)
		return hresultOK
	})
	cbXDspActivate := syscall.NewCallback(func(dsp, descPtr, statusOut, streamOut uintptr) uintptr {
		if xdspActLogged.CompareAndSwap(false, true) {
			slog.Info("chad!XDspActivate actif", "dsp", dsp)
		}
		stMem, _, _ := k32VirtualAlloc.Call(0, 64, 0x3000, 0x04)
		h := uintptr(nextXApuHandle.Add(1))
		xdspMu.Lock()
		xdspStreams[h] = &xdspStream{statusPtr: stMem}
		xdspMu.Unlock()
		if statusOut >= 0x10000 {
			*(*uintptr)(osPtr(statusOut)) = stMem
		}
		if streamOut >= 0x10000 {
			*(*uintptr)(osPtr(streamOut)) = h
		}
		return hresultOK
	})
	cbXDspSubmit := syscall.NewCallback(func(stream, cmdPtr uintptr) uintptr {
		xdspMu.Lock()
		s := xdspStreams[stream]
		xdspMu.Unlock()
		if s != nil && s.statusPtr >= 0x10000 {
			words := unsafe.Slice((*uint32)(osPtr(s.statusPtr)), 4)
			words[0]++
			words[1] = words[0] // completed == submitted
			words[3] = 0        // S_OK
		}
		return hresultOK
	})
	t.bind("chad.dll", "XDspConnectWithMaximumStreamLimit", cbXDspConnect)
	t.bind("chad.dll", "XDspDisconnect", cbOK)
	t.bind("chad.dll", "XDspActivate", cbXDspActivate)
	t.bind("chad.dll", "XDspDeactivate", cbOK)
	t.bind("chad.dll", "XDspSubmitCommand", cbXDspSubmit)
}
