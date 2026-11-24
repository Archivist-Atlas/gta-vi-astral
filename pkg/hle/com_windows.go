//go:build windows

package hle

import (
	"encoding/binary"
	"fmt"
	"unsafe"
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
