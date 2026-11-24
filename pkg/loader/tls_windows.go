//go:build windows

package loader

import (
	"encoding/binary"
	"fmt"
	"log/slog"
	"unsafe"

	"github.com/Archivist-Atlas/gta-vi-astral/pkg/pe"
)

const (
	scnMemExecute      uint32 = 0x20000000
	tls64IndexOffset   uintptr = 16
	tlsLoadLen                 = 9
	tlsThunkSize               = 32
	allocGranularity   uintptr = 0x10000
)

// RedirectStaticTLS réécrit les accès `mov r64, gs:[0x58]` d'un exécutable mappé
// en DLL passive lorsque Windows lui a attribué un slot TLS != 0 (portage de victory-loader::tls).
func RedirectStaticTLS(base uintptr, info *pe.ImageInfo) (int, uint32, error) {
	if info.TLS == nil {
		return 0, 0, nil
	}
	indexPtr := *(*uintptr)(ptrFromAddr(base + uintptr(info.TLS.DirectoryRVA) + tls64IndexOffset))
	if indexPtr == 0 {
		return 0, 0, nil
	}
	index := *(*uint32)(ptrFromAddr(indexPtr))
	if index == 0 {
		return 0, 0, nil
	}

	page, err := allocNearImage(base, uintptr(info.SizeOfImage))
	if err != nil {
		return 0, index, err
	}
	if err := writeTLSThunks(page, index); err != nil {
		return 0, index, err
	}

	patched := 0
	for _, sec := range info.Sections {
		if sec.Characteristics&scnMemExecute == 0 || sec.VirtualSize < tlsLoadLen {
			continue
		}
		start := base + uintptr(sec.RVA)
		length := uintptr(sec.VirtualSize)

		oldProt, err := virtualProtect(start, length, pageExecuteReadWrite)
		if err != nil {
			return patched, index, fmt.Errorf("VirtualProtect(%s) : %w", sec.Name, err)
		}
		code := unsafe.Slice((*byte)(ptrFromAddr(start)), length)
		patched += patchTLSLoads(code, start, page)

		_, _ = virtualProtect(start, length, oldProt)
		flushInstructionCache(start, length)
	}

	// Le code CRT qui lit `_tls_index` verra 0 puisque nos thunks ajoutent déjà `index*8`.
	*(*uint32)(ptrFromAddr(indexPtr)) = 0
	slog.Info("TLS statique redirigé", "slot", index, "sites_patches", patched)
	return patched, index, nil
}

// patchTLSLoads remplace chaque `mov r64, gs:[0x58]` (65 48/4C 8B ModRM 25 58 00 00 00)
// par `call thunks[r]; nop4` (E8 rel32 0F 1F 40 00).
func patchTLSLoads(code []byte, at, thunks uintptr) int {
	count := 0
	for i := 0; i+tlsLoadLen <= len(code); {
		c := code[i : i+tlsLoadLen]
		rex := c[1]
		reg := int((c[3] >> 3) & 7)
		if rex&4 != 0 {
			reg |= 8
		}
		if c[0] == 0x65 &&
			(rex == 0x48 || rex == 0x4C) &&
			c[2] == 0x8B &&
			(c[3]&0xC7) == 0x04 &&
			c[4] == 0x25 && c[5] == 0x58 && c[6] == 0 && c[7] == 0 && c[8] == 0 &&
			reg != 4 {
			target := thunks + uintptr(reg*tlsThunkSize)
			rel := int64(target) - int64(at+uintptr(i)+5)
			code[i] = 0xE8
			binary.LittleEndian.PutUint32(code[i+1:i+5], uint32(int32(rel)))
			code[i+5] = 0x0F
			code[i+6] = 0x1F
			code[i+7] = 0x40
			code[i+8] = 0x00
			count++
			i += tlsLoadLen
		} else {
			i++
		}
	}
	return count
}

func writeTLSThunks(page uintptr, index uint32) error {
	var disp [4]byte
	binary.LittleEndian.PutUint32(disp[:], index*8)
	buf := unsafe.Slice((*byte)(ptrFromAddr(page)), 4096)
	for i := range buf {
		buf[i] = 0xCC
	}

	for reg := byte(0); reg < 16; reg++ {
		r := reg >> 3
		low := reg & 7
		code := []byte{
			0x65, 0x48 | (r << 2), 0x8B, (low << 3) | 4, 0x25, 0x58, 0, 0, 0,
			0x48 | (r << 2) | r, 0x8D, 0x80 | (low << 3) | low,
		}
		if low == 4 {
			code = append(code, 0x24) // SIB [r12 + disp32]
		}
		code = append(code, disp[0], disp[1], disp[2], disp[3], 0xC3)
		copy(buf[int(reg)*tlsThunkSize:], code)
	}

	if _, err := virtualProtect(page, 4096, pageExecuteRead); err != nil {
		return fmt.Errorf("VirtualProtect(TLS thunks) : %w", err)
	}
	flushInstructionCache(page, 4096)
	return nil
}

func allocNearImage(base, size uintptr) (uintptr, error) {
	if size >= (1 << 30) {
		return 0, fmt.Errorf("image trop grande pour des thunks TLS à portée rel32")
	}
	above := (base + size + allocGranularity - 1) &^ (allocGranularity - 1)
	for k := uintptr(0); k < (1 << 14); k++ {
		if base > (k+1)*allocGranularity {
			hint := base - (k+1)*allocGranularity
			if p, err := virtualAlloc(hint, 4096, memCommit|memReserve, pageReadWrite); err == nil && p != 0 {
				return p, nil
			}
		}
		hint := above + k*allocGranularity
		if p, err := virtualAlloc(hint, 4096, memCommit|memReserve, pageReadWrite); err == nil && p != 0 {
			return p, nil
		}
	}
	return 0, fmt.Errorf("impossible d'allouer une page libre près de 0x%X pour les thunks TLS", base)
}
