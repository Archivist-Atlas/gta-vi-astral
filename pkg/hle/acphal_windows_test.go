//go:build windows

package hle

import (
	"encoding/binary"
	"math"
	"syscall"
	"testing"
	"unsafe"

	"github.com/tphakala/go-opus/opus"
)

func TestAcpHalAndXApuOpusDecode(t *testing.T) {
	lt := &LiveTable{funcs: make(map[string]uintptr)}
	bindAcpHal(lt)

	// 1. Vérification ABI AcpHal : ApuAlloc -> ApuMapVirtualAddress(ptr) -> ApuMapApuAddress(apu) -> ApuIsVirtualAddressValid -> ApuFree
	fnAlloc := lt.funcs["acphal.dll!apualloc"]
	fnMapVA := lt.funcs["acphal.dll!apumapvirtualaddress"]
	fnMapApu := lt.funcs["acphal.dll!apumapapuaddress"]
	fnValid := lt.funcs["acphal.dll!apuisvirtualaddressvalid"]
	fnFree := lt.funcs["acphal.dll!apufree"]
	if fnAlloc == 0 || fnMapVA == 0 || fnMapApu == 0 || fnValid == 0 || fnFree == 0 {
		t.Fatal("exports AcpHal manquants")
	}

	var va uintptr
	var apuAddr uint32
	hr, _, _ := syscall.SyscallN(fnAlloc, uintptr(unsafe.Pointer(&va)), uintptr(unsafe.Pointer(&apuAddr)), 4096, 256, 0)
	if hr != 0 || va == 0 || apuAddr == 0 {
		t.Fatalf("ApuAlloc échoué: hr=0x%X va=0x%X apu=0x%X", hr, va, apuAddr)
	}
	gotApu, _, _ := syscall.SyscallN(fnMapVA, va+128)
	if uint32(gotApu) != apuAddr+128 {
		t.Fatalf("ApuMapVirtualAddress attendu 0x%X, obtenu 0x%X", apuAddr+128, uint32(gotApu))
	}
	gotVA, _, _ := syscall.SyscallN(fnMapApu, uintptr(apuAddr+128))
	if gotVA != va+128 {
		t.Fatalf("ApuMapApuAddress attendu 0x%X, obtenu 0x%X", va+128, gotVA)
	}
	valid, _, _ := syscall.SyscallN(fnValid, va, 256)
	if valid != 1 {
		t.Fatalf("ApuIsVirtualAddressValid attendu 1, obtenu %d", valid)
	}
	_, _, _ = syscall.SyscallN(fnFree, va)
	if v2, _, _ := syscall.SyscallN(fnValid, va, 0); v2 != 0 {
		t.Fatalf("ApuIsVirtualAddressValid après ApuFree attendu 0, obtenu %d", v2)
	}

	// 2. Vérification xapu.dll : XApuConnect -> ACTIVATE -> PROCESS (Opus 48kHz stéréo) -> XApuGetResultCount -> XApuDequeueResult (80 octets)
	fnConnect := lt.funcs["xapu.dll!xapuconnect"]
	fnEnq := lt.funcs["xapu.dll!xapuenqueuecommand"]
	fnCount := lt.funcs["xapu.dll!xapugetresultcount"]
	fnDeq := lt.funcs["xapu.dll!xapudequeueresult"]
	fnDisc := lt.funcs["xapu.dll!xapudisconnect"]

	var inBuf [24]byte
	binary.LittleEndian.PutUint32(inBuf[0:4], 1)       // process = DECODE_OPUS
	binary.LittleEndian.PutUint32(inBuf[4:8], 16)      // streams
	binary.LittleEndian.PutUint32(inBuf[8:12], 4)      // cmds per stream
	binary.LittleEndian.PutUint32(inBuf[12:16], 65536) // memory
	binary.LittleEndian.PutUint32(inBuf[20:24], 4)     // DEINTERLEAVED_OUTPUT

	var outBuf [16]byte
	var handle uintptr
	hr, _, _ = syscall.SyscallN(fnConnect, uintptr(unsafe.Pointer(&inBuf[0])), uintptr(unsafe.Pointer(&outBuf[0])), uintptr(unsafe.Pointer(&handle)))
	if hr != 0 || handle == 0 {
		t.Fatalf("XApuConnect échoué: hr=0x%X handle=%d", hr, handle)
	}
	defer syscall.SyscallN(fnDisc, handle)

	// ACTIVATE stream 0 en stéréo (2 canaux)
	var actCmd [56]byte
	binary.LittleEndian.PutUint32(actCmd[0:4], 1)   // ACTIVATE
	binary.LittleEndian.PutUint32(actCmd[4:8], 0)   // stream 0
	binary.LittleEndian.PutUint32(actCmd[8:12], 1)  // seq
	binary.LittleEndian.PutUint32(actCmd[24:28], 2) // channels = 2
	var totalCmds uint32
	hr, _, _ = syscall.SyscallN(fnEnq, handle, uintptr(unsafe.Pointer(&actCmd[0])), uintptr(unsafe.Pointer(&totalCmds)))
	if hr != 0 || totalCmds != 1 {
		t.Fatalf("ACTIVATE échoué: hr=0x%X total=%d", hr, totalCmds)
	}
	pend, _, _ := syscall.SyscallN(fnCount, handle, 0)
	if pend != 1 {
		t.Fatalf("XApuGetResultCount attendu 1, obtenu %d", pend)
	}
	var resBuf [80]byte
	hr, _, _ = syscall.SyscallN(fnDeq, handle, uintptr(unsafe.Pointer(&resBuf[0])))
	if hr != 0 || binary.LittleEndian.Uint32(resBuf[44:48]) != 0 {
		t.Fatalf("Dequeue ACTIVATE échoué: hr=0x%X err=0x%X", hr, binary.LittleEndian.Uint32(resBuf[44:48]))
	}

	// Encodage d'un paquet Opus 48 kHz stéréo 20 ms (960 frames)
	enc, err := opus.NewEncoder(opus.EncoderConfig{SampleRate: 48000, Channels: 2})
	if err != nil {
		t.Fatalf("opus.NewEncoder: %v", err)
	}
	const frameSize = 960
	srcPCM := make([]int16, frameSize*2)
	for i := 0; i < frameSize; i++ {
		s := int16(12000.0 * math.Sin(2*math.Pi*440.0*float64(i)/48000.0))
		srcPCM[i*2] = s
		srcPCM[i*2+1] = -s
	}
	pkt := make([]byte, 4000)
	pktLen, err := enc.Encode(srcPCM, pkt)
	if err != nil || pktLen == 0 {
		t.Fatalf("opus.Encode: %v len=%d", err, pktLen)
	}
	pkt = pkt[:pktLen]

	outPCM := make([]byte, frameSize*2*4)
	var procCmd [64]byte
	binary.LittleEndian.PutUint32(procCmd[0:4], 3) // PROCESS
	binary.LittleEndian.PutUint32(procCmd[4:8], 0) // stream 0
	binary.LittleEndian.PutUint32(procCmd[8:12], 2)
	binary.LittleEndian.PutUint32(procCmd[32:36], 0)         // first_frame
	binary.LittleEndian.PutUint32(procCmd[36:40], frameSize) // frame_count
	binary.LittleEndian.PutUint32(procCmd[40:44], uint32(len(pkt)))
	binary.LittleEndian.PutUint64(procCmd[44:52], uint64(uintptr(unsafe.Pointer(&pkt[0]))))
	binary.LittleEndian.PutUint32(procCmd[52:56], uint32(len(outPCM)))
	binary.LittleEndian.PutUint64(procCmd[56:64], uint64(uintptr(unsafe.Pointer(&outPCM[0]))))

	hr, _, _ = syscall.SyscallN(fnEnq, handle, uintptr(unsafe.Pointer(&procCmd[0])), uintptr(unsafe.Pointer(&totalCmds)))
	if hr != 0 || totalCmds != 2 {
		t.Fatalf("PROCESS échoué: hr=0x%X total=%d", hr, totalCmds)
	}
	var dequeued uint32
	pend, _, _ = syscall.SyscallN(fnCount, handle, uintptr(unsafe.Pointer(&dequeued)))
	if pend != 1 || dequeued != 1 {
		t.Fatalf("XApuGetResultCount attendu pend=1 deq=1, obtenu pend=%d deq=%d", pend, dequeued)
	}
	hr, _, _ = syscall.SyscallN(fnDeq, handle, uintptr(unsafe.Pointer(&resBuf[0])))
	if hr != 0 {
		t.Fatalf("Dequeue PROCESS échoué: hr=0x%X", hr)
	}
	if errCode := binary.LittleEndian.Uint32(resBuf[44:48]); errCode != 0 {
		t.Fatalf("PROCESS ApuResult.error=0x%X", errCode)
	}
	gotFrames := binary.LittleEndian.Uint32(resBuf[48:52])
	gotOutLen := binary.LittleEndian.Uint32(resBuf[52:56])
	gotInPtr := binary.LittleEndian.Uint64(resBuf[72:80])
	if gotFrames != frameSize || gotOutLen != frameSize*2*4 || gotInPtr != uint64(uintptr(unsafe.Pointer(&pkt[0]))) {
		t.Fatalf("ApuResult inattendu: frames=%d outLen=%d inPtr=0x%X", gotFrames, gotOutLen, gotInPtr)
	}
	nonZero := 0
	for i := 0; i < len(outPCM); i += 4 {
		v := math.Float32frombits(binary.LittleEndian.Uint32(outPCM[i:]))
		if v > 0.01 || v < -0.01 {
			nonZero++
		}
	}
	if nonZero < 100 {
		t.Fatalf("PCM float32 décodé quasi-silencieux (%d échantillons non nuls)", nonZero)
	}
}
