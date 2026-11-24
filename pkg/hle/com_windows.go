//go:build windows

package hle

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"sync"
	"syscall"
	"unsafe"
)

const (
	hresultOK         uintptr = 0x00000000
	hresultNotImpl    uintptr = 0x80004001
	hresultNoIface    uintptr = 0x80004002
	hresultInvalidArg uintptr = 0x80070057

	giReadingNotFound uintptr = 0x838A0003
)

// FormatGUID lit un GUID Windows 16 octets à l'adresse native donnée.
func FormatGUID(addr uintptr) string {
	if addr == 0 {
		return "<nil>"
	}
	b := unsafe.Slice((*byte)(osPtr(addr)), 16)
	d1 := binary.LittleEndian.Uint32(b[0:4])
	d2 := binary.LittleEndian.Uint16(b[4:6])
	d3 := binary.LittleEndian.Uint16(b[6:8])
	return fmt.Sprintf("{%08X-%04X-%04X-%02X%02X-%02X%02X%02X%02X%02X%02X}",
		d1, d2, d3, b[8], b[9], b[10], b[11], b[12], b[13], b[14], b[15])
}

type comSlotMeta struct {
	area string
	slot int
	seen bool
}

var (
	comMu          sync.Mutex
	comSlots       []*comSlotMeta
	comMissingOnce sync.Once
	comMissingCb   uintptr
	comStubPage    uintptr
	comStubUsed    uintptr
	comStubCap     uintptr
	comAddRefCb    uintptr
	comQICb        uintptr
)

func initCOMCallbacks() {
	comMissingOnce.Do(func() {
		comMissingCb = syscall.NewCallback(func(metaID uintptr) uintptr {
			comMu.Lock()
			var m *comSlotMeta
			if int(metaID) < len(comSlots) {
				m = comSlots[metaID]
			}
			first := m != nil && !m.seen
			if m != nil {
				m.seen = true
			}
			comMu.Unlock()
			if first {
				slog.Warn("méthode COM non implémentée (E_NOTIMPL)", "area", m.area, "slot", m.slot, "offset", fmt.Sprintf("0x%X", m.slot*8))
			}
			return hresultNotImpl
		})
		comAddRefCb = syscall.NewCallback(func(this uintptr) uintptr {
			return 1
		})
		comQICb = syscall.NewCallback(func(this, iid, outPtr uintptr) uintptr {
			if outPtr != 0 {
				*(*uintptr)(osPtr(outPtr)) = this
			}
			return hresultOK
		})
	})
}

func emitCOMSlotStub(area string, slot int) uintptr {
	initCOMCallbacks()
	comMu.Lock()
	defer comMu.Unlock()

	const stubSize = 32
	if comStubUsed+stubSize > comStubCap {
		const pageBytes = 64 * 1024
		p, _, _ := k32VirtualAlloc.Call(0, pageBytes, 0x3000 /* COMMIT|RESERVE */, 0x40 /* PAGE_EXECUTE_READWRITE */)
		comStubPage = p
		comStubUsed = 0
		comStubCap = pageBytes
	}

	id := uintptr(len(comSlots))
	comSlots = append(comSlots, &comSlotMeta{area: area, slot: slot})

	at := comStubPage + comStubUsed
	comStubUsed += stubSize
	code := unsafe.Slice((*byte)(osPtr(at)), stubSize)
	for i := range code {
		code[i] = 0xCC
	}
	// mov rcx, id ; mov rax, comMissingCb ; jmp rax
	code[0], code[1] = 0x48, 0xB9
	binary.LittleEndian.PutUint64(code[2:10], uint64(id))
	code[10], code[11] = 0x48, 0xB8
	binary.LittleEndian.PutUint64(code[12:20], uint64(comMissingCb))
	code[20], code[21] = 0xFF, 0xE0
	return at
}

// NewCOMObject alloue un objet COM et sa vtable en mémoire native OS (hors tas Go, GC-safe).
// Les slots 0..2 sont automatiquement initialisés à QueryInterface / AddRef / Release.
func NewCOMObject(area string, numSlots int, overrides map[int]uintptr) uintptr {
	return NewCOMObjectWithHeader(area, numSlots, 8, overrides)
}

// NewCOMObjectWithHeader alloue un objet COM dont l'en-tête objet fait headerBytes (>= 8, multiple de 8),
// suivi immédiatement de sa vtable de numSlots entrées.
func NewCOMObjectWithHeader(area string, numSlots, headerBytes int, overrides map[int]uintptr) uintptr {
	initCOMCallbacks()
	if numSlots < 3 {
		numSlots = 3
	}
	if headerBytes < 8 {
		headerBytes = 8
	}
	headerBytes = (headerBytes + 7) &^ 7
	totalBytes := uintptr(headerBytes + numSlots*8)
	mem, _, _ := k32VirtualAlloc.Call(0, totalBytes, 0x3000, 0x04 /* PAGE_READWRITE */)
	if mem == 0 {
		return 0
	}
	vtableAddr := mem + uintptr(headerBytes)
	*(*uintptr)(osPtr(mem)) = vtableAddr

	vtable := unsafe.Slice((*uintptr)(osPtr(vtableAddr)), numSlots)
	vtable[0] = comQICb
	vtable[1] = comAddRefCb
	vtable[2] = comAddRefCb
	for i := 3; i < numSlots; i++ {
		if fn, ok := overrides[i]; ok && fn != 0 {
			vtable[i] = fn
		} else {
			vtable[i] = emitCOMSlotStub(area, i)
		}
	}
	return mem
}
