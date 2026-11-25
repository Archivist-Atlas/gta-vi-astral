//go:build windows

package hle

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/tphakala/go-opus/opus"
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

func (a *apuState) lookupVA(va uintptr) uint32 {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range a.regions {
		if va >= r.va && va < r.va+uintptr(r.size) {
			return r.apu + uint32(va-r.va)
		}
	}
	return 0
}

func (a *apuState) free(va uintptr) {
	a.mu.Lock()
	for i, r := range a.regions {
		if r.va == va {
			a.regions = append(a.regions[:i], a.regions[i+1:]...)
			a.mu.Unlock()
			_, _, _ = k32VirtualFree.Call(va, 0, 0x8000 /* MEM_RELEASE */)
			return
		}
	}
	a.mu.Unlock()
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
		if va >= 0x10000 {
			apu.free(va)
		}
		return hresultOK
	})
	// ABI AcpHal.dll : uint32 ApuMapVirtualAddress(const void* ptr) -> adresse APU 32 bits (0 si hors tas)
	cbApuMapVA := syscall.NewCallback(func(va uintptr) uintptr {
		return uintptr(apu.lookupVA(va))
	})
	// ABI AcpHal.dll : void* ApuMapApuAddress(uint32 apuAddr)
	cbApuMapApu := syscall.NewCallback(func(apuAddr uintptr) uintptr {
		return apu.mapApu(uint32(apuAddr))
	})
	// ABI AcpHal.dll : bool ApuIsVirtualAddressValid(const void* ptr, uint32 align)
	cbApuIsValid := syscall.NewCallback(func(va, align uintptr) uintptr {
		addr := apu.lookupVA(va)
		if addr == 0 {
			return 0
		}
		al := uint32(align)
		if al != 0 && addr%al != 0 {
			return 0
		}
		return 1
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
		strides := [6]uint32{32, 256, 256, 16, 64, 256}
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
		t.bind(dll, "ApuIsVirtualAddressValid", cbApuIsValid)
		t.bind(dll, "ApuHeapGetState", cbApuHeapState)
		t.bind(dll, "AcpHalAllocateShapeContexts", cbAllocShape)
		t.bind(dll, "AcpHalReleaseShapeContexts", cbOK)
		t.bind(dll, "AcpHalCreate", cbAcpHalCreate)
	}

	// --- xapu.dll (décodeur audio matériel Xbox Series Opus -> PCM float32, ABI 80 octets ApuResult) ---
	const (
		xapuQueueFull      = 0x8AC81000
		xapuPendingResults = 0x8AC81001
		xapuInvalidCommand = 0x8AC81002
		xapuInvalidStream  = 0x8AC81003
		xapuAlreadyActive  = 0x8AC81005
		xapuBadPacket      = 0x8AC81008
		xapuDeinterleaved  = 4
		xapuResultEvent    = 8
		xapuCmdSignal      = 1
		xapuMaxFrames      = 5760 // 120 ms @ 48 kHz
	)
	type xapuResult struct {
		// ABI exact #[repr(C, packed)] 80 octets (0x50) :
		// CommandId(24) + state(4) + stateData(8) + usec(8) + error(4) +
		// frames(4) + outLen(4) + outPtr(8) + pitch(4) + inLen(4) + inPtr(8)
		raw [80]byte
	}
	type xapuStream struct {
		dec      *opus.Decoder
		channels int
		pcmBuf   []int16
	}
	type xapuConn struct {
		mu        sync.Mutex
		dataMem   uintptr
		readyEv   uintptr
		options   uint32
		perStream uint32
		streams   []*xapuStream
		queued    []uint32
		results   []xapuResult
		commands  uint32
		dequeued  uint32
		since     time.Time
	}
	var (
		nextXApuHandle atomic.Uint64
		xapuMu         sync.Mutex
		xapuConns      = map[uintptr]*xapuConn{}
		xapuEnqLogged  atomic.Bool
		xapuDecLogged  atomic.Bool
		xapuDecCount   atomic.Uint64
		xdspActLogged  atomic.Bool
	)
	nextXApuHandle.Store(0x880000)

	cbXApuConnect := syscall.NewCallback(func(inPtr, outPtr, handleOut uintptr) uintptr {
		if inPtr == 0 || outPtr == 0 || handleOut == 0 {
			return hresultInvalidArg
		}
		// XApuConnectInput : { process u32 (+0), streams u32 (+4), cmds u32 (+8), data_bytes u32 (+12), frames u32 (+16), options u32 (+20) }
		process := *(*uint32)(osPtr(inPtr + 0))
		streams := *(*uint32)(osPtr(inPtr + 4))
		perStream := *(*uint32)(osPtr(inPtr + 8))
		dataBytes := *(*uint32)(osPtr(inPtr + 12))
		options := *(*uint32)(osPtr(inPtr + 20))
		if streams == 0 || streams > 500 || perStream == 0 || perStream > 16 || dataBytes == 0 {
			return hresultInvalidArg
		}
		if process != 1 { // DECODE_OPUS = 1
			slog.Warn("xapu!XApuConnect mode non supporté", "process", process)
			return hresultNotImpl
		}
		allocSz := (uintptr(dataBytes) + 65535) &^ 65535
		dataMem, _, _ := k32VirtualAlloc.Call(0, allocSz, 0x3000, 0x04)
		if dataMem == 0 {
			return 0x8007000E // E_OUTOFMEMORY
		}
		var readyEv uintptr
		if options&xapuResultEvent != 0 {
			readyEv, _, _ = k32CreateEventW.Call(0, 0, 0, 0)
		}
		*(*uintptr)(osPtr(outPtr + 0)) = dataMem
		*(*uintptr)(osPtr(outPtr + 8)) = readyEv
		h := uintptr(nextXApuHandle.Add(1))
		xapuMu.Lock()
		xapuConns[h] = &xapuConn{
			dataMem:   dataMem,
			readyEv:   readyEv,
			options:   options,
			perStream: perStream,
			streams:   make([]*xapuStream, streams),
			queued:    make([]uint32, streams),
			since:     time.Now(),
		}
		xapuMu.Unlock()
		*(*uintptr)(osPtr(handleOut)) = h
		slog.Info("xapu!XApuConnect (décodeur Opus 48kHz actif)", "streams", streams, "perStream", perStream, "dataBytes", dataBytes, "options", options, "handle", h)
		return hresultOK
	})
	cbXApuDisconnect := syscall.NewCallback(func(handle uintptr) uintptr {
		xapuMu.Lock()
		c := xapuConns[handle]
		delete(xapuConns, handle)
		xapuMu.Unlock()
		if c == nil {
			return hresultInvalidArg
		}
		if c.readyEv != 0 {
			_, _, _ = k32CloseHandle.Call(c.readyEv)
		}
		if c.dataMem != 0 {
			_, _, _ = k32VirtualFree.Call(c.dataMem, 0, 0x8000)
		}
		return hresultOK
	})
	cbXApuEnqueue := syscall.NewCallback(func(handle, cmdPtr, totalOut uintptr) uintptr {
		if handle == 0 || cmdPtr < 0x10000 {
			return hresultInvalidArg
		}
		xapuMu.Lock()
		c := xapuConns[handle]
		xapuMu.Unlock()
		if c == nil {
			return hresultInvalidArg
		}
		kind := *(*uint32)(osPtr(cmdPtr + 0))
		stream := *(*uint32)(osPtr(cmdPtr + 4))
		opts := *(*uint32)(osPtr(cmdPtr + 12))
		if xapuEnqLogged.CompareAndSwap(false, true) {
			slog.Info("xapu!XApuEnqueueCommand actif", "handle", handle, "kind", kind, "stream", stream)
		}

		c.mu.Lock()
		if int(stream) >= len(c.streams) {
			c.mu.Unlock()
			return xapuInvalidStream
		}
		if c.queued[stream] >= c.perStream {
			c.mu.Unlock()
			return xapuQueueFull
		}

		var res xapuResult
		copy(res.raw[0:24], unsafe.Slice((*byte)(osPtr(cmdPtr)), 24))
		binary.LittleEndian.PutUint64(res.raw[36:44], uint64(time.Since(c.since).Microseconds()))
		binary.LittleEndian.PutUint32(res.raw[64:68], 0x3F800000) // pitch = 1.0f

		switch kind {
		case 1: // ACTIVATE
			ch := int(*(*uint32)(osPtr(cmdPtr + 24)))
			if c.streams[stream] != nil {
				binary.LittleEndian.PutUint32(res.raw[44:48], xapuAlreadyActive)
			} else if ch < 1 || ch > 2 {
				binary.LittleEndian.PutUint32(res.raw[44:48], uint32(hresultInvalidArg))
			} else if dec, err := opus.NewDecoder(48000, ch); err == nil {
				c.streams[stream] = &xapuStream{
					dec:      dec,
					channels: ch,
					pcmBuf:   make([]int16, xapuMaxFrames*ch),
				}
			} else {
				binary.LittleEndian.PutUint32(res.raw[44:48], uint32(hresultInvalidArg))
			}
		case 2: // DEACTIVATE
			c.streams[stream] = nil
		case 3: // PROCESS (décodage Opus -> float32 PCM entrelacé ou désentrelacé)
			firstFrame := int(*(*uint32)(osPtr(cmdPtr + 32)))
			frameCount := int(*(*uint32)(osPtr(cmdPtr + 36)))
			inLen := *(*uint32)(osPtr(cmdPtr + 40))
			inPtr := *(*uint64)(osPtr(cmdPtr + 44))
			outLen := *(*uint32)(osPtr(cmdPtr + 52))
			outPtr := *(*uint64)(osPtr(cmdPtr + 56))
			binary.LittleEndian.PutUint64(res.raw[56:64], outPtr)
			binary.LittleEndian.PutUint32(res.raw[68:72], inLen)
			binary.LittleEndian.PutUint64(res.raw[72:80], inPtr)

			st := c.streams[stream]
			if st == nil || st.dec == nil {
				binary.LittleEndian.PutUint32(res.raw[44:48], xapuInvalidStream)
			} else if inPtr < 0x10000 || outPtr < 0x10000 || inLen == 0 || inLen > (1<<20) || outLen > (4<<20) {
				binary.LittleEndian.PutUint32(res.raw[44:48], uint32(hresultInvalidArg))
			} else {
				pkt := unsafe.Slice((*byte)(osPtr(uintptr(inPtr))), inLen)
				decoded, err := st.dec.Decode(pkt, st.pcmBuf)
				if err != nil {
					binary.LittleEndian.PutUint32(res.raw[44:48], xapuBadPacket)
				} else {
					ch := st.channels
					first := firstFrame
					if first > decoded {
						first = decoded
					}
					frames := decoded - first
					if frameCount < frames {
						frames = frameCount
					}
					maxOutFrames := int(outLen) / (4 * ch)
					if maxOutFrames < frames {
						frames = maxOutFrames
					}
					if frames < 0 {
						frames = 0
					}
					nBytes := uint32(frames * ch * 4)
					if nBytes > 0 {
						outBytes := unsafe.Slice((*byte)(osPtr(uintptr(outPtr))), nBytes)
						kept := st.pcmBuf[first*ch : (first+frames)*ch]
						const inv32768 = float32(1.0 / 32768.0)
						if c.options&xapuDeinterleaved != 0 && ch == 2 {
							rightBase := frames * 4
							for i := 0; i < frames; i++ {
								l := math.Float32bits(float32(kept[i*2]) * inv32768)
								r := math.Float32bits(float32(kept[i*2+1]) * inv32768)
								binary.LittleEndian.PutUint32(outBytes[i*4:], l)
								binary.LittleEndian.PutUint32(outBytes[rightBase+i*4:], r)
							}
						} else {
							for i, sample := range kept {
								binary.LittleEndian.PutUint32(outBytes[i*4:], math.Float32bits(float32(sample)*inv32768))
							}
						}
					}
					binary.LittleEndian.PutUint32(res.raw[48:52], uint32(frames))
					binary.LittleEndian.PutUint32(res.raw[52:56], nBytes)
					if n := xapuDecCount.Add(1); xapuDecLogged.CompareAndSwap(false, true) || n%500 == 0 {
						slog.Info("xapu!Opus décodage PCM actif", "stream", stream, "ch", ch, "inBytes", inLen, "frames", frames, "totalPackets", n)
					}
				}
			}
		case 4: // NOTIFY
		case 5: // RESET
			if st := c.streams[stream]; st != nil && st.dec != nil {
				st.dec.Reset()
			}
		default:
			c.mu.Unlock()
			return xapuInvalidCommand
		}

		c.queued[stream]++
		c.commands++
		totalCmds := c.commands
		c.results = append(c.results, res)
		c.mu.Unlock()

		if totalOut >= 0x10000 {
			*(*uint32)(osPtr(totalOut)) = totalCmds
		}
		if opts&xapuCmdSignal != 0 && c.readyEv != 0 {
			_, _, _ = k32SetEvent.Call(c.readyEv)
		}
		return hresultOK
	})
	// ABI xapu.dll : uint32 XApuGetResultCount(handle, uint32* totalDequeued) -> nombre de résultats en attente
	cbXApuGetResultCount := syscall.NewCallback(func(handle, totalPtr uintptr) uintptr {
		xapuMu.Lock()
		c := xapuConns[handle]
		xapuMu.Unlock()
		if c == nil {
			return 0
		}
		c.mu.Lock()
		pending := uint32(len(c.results))
		dequeued := c.dequeued
		c.mu.Unlock()
		if totalPtr >= 0x10000 {
			*(*uint32)(osPtr(totalPtr)) = dequeued
		}
		return uintptr(pending)
	})
	cbXApuDequeueResult := syscall.NewCallback(func(handle, resPtr uintptr) uintptr {
		if handle == 0 || resPtr < 0x10000 {
			return hresultInvalidArg
		}
		xapuMu.Lock()
		c := xapuConns[handle]
		xapuMu.Unlock()
		if c == nil {
			return xapuPendingResults
		}
		c.mu.Lock()
		if len(c.results) == 0 {
			c.mu.Unlock()
			return xapuPendingResults
		}
		res := c.results[0]
		c.results = c.results[1:]
		stream := binary.LittleEndian.Uint32(res.raw[4:8])
		if int(stream) < len(c.queued) && c.queued[stream] > 0 {
			c.queued[stream]--
		}
		c.dequeued++
		c.mu.Unlock()
		dst := unsafe.Slice((*byte)(osPtr(resPtr)), 80)
		copy(dst, res.raw[:])
		return hresultOK
	})
	cbXApuGetUtilization := syscall.NewCallback(func(a, b, outPtr uintptr) uintptr {
		if outPtr != 0 {
			*(*uint32)(osPtr(outPtr)) = 256
		}
		return hresultOK
	})
	t.bind("xapu.dll", "XApuConnect", cbXApuConnect)
	t.bind("xapu.dll", "XApuDisconnect", cbXApuDisconnect)
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
