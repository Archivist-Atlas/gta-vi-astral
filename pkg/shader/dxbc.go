// Package shader implémente l'analyse, le dépaquetage natif RDNA2 (DXBC -> XDAT
// -> XBCH -> FSSM), le décodage binaire GFX10 et l'architecture de traduction
// hybride vers DXIL SM 6.6 (approche VIctory) ou SPIR-V Vulkan (approche Leonida).
package shader

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
)

// FourCC identifie un bloc d'un conteneur DXBC.
type FourCC [4]byte

var (
	FourCCDXIL = FourCC{'D', 'X', 'I', 'L'}
	FourCCSTAT = FourCC{'S', 'T', 'A', 'T'}
	FourCCRTS0 = FourCC{'R', 'T', 'S', '0'}
	FourCCPSV0 = FourCC{'P', 'S', 'V', '0'}
	FourCCISG1 = FourCC{'I', 'S', 'G', '1'}
	FourCCOSG1 = FourCC{'O', 'S', 'G', '1'}
	FourCCPSG1 = FourCC{'P', 'S', 'G', '1'}
	FourCCSFI0 = FourCC{'S', 'F', 'I', '0'}
	FourCCHASH = FourCC{'H', 'A', 'S', 'H'}
)

func (f FourCC) String() string {
	var b [4]byte
	for i, c := range f {
		if c >= 0x21 && c <= 0x7E {
			b[i] = c
		} else {
			b[i] = '.'
		}
	}
	return string(b[:])
}

// Part représente une section FourCC d'un conteneur DXBC.
type Part struct {
	FourCC FourCC
	Offset int
	Data   []byte
}

// Container représente un conteneur DXBC complet.
type Container struct {
	Digest       [16]byte
	VersionMajor uint16
	VersionMinor uint16
	Parts        []Part
}

const (
	dxbcHeaderSize = 0x20
	dxbcMaxParts   = 256
)

var (
	ErrNotDXBC   = errors.New("conteneur non DXBC")
	ErrMalformed = errors.New("conteneur shader malformé")
)

// ParseDXBC analyse un conteneur DXBC en mémoire.
func ParseDXBC(data []byte) (*Container, error) {
	if len(data) < dxbcHeaderSize || !bytes.Equal(data[0:4], []byte("DXBC")) {
		return nil, ErrNotDXBC
	}
	total := int(binary.LittleEndian.Uint32(data[0x18:0x1C]))
	if total < dxbcHeaderSize || total > len(data) {
		return nil, fmt.Errorf("%w : taille DXBC incohérente (%d / %d)", ErrMalformed, total, len(data))
	}
	data = data[:total]

	count := int(binary.LittleEndian.Uint32(data[0x1C:0x20]))
	if count > dxbcMaxParts || dxbcHeaderSize+4*count > len(data) {
		return nil, fmt.Errorf("%w : nombre de parts DXBC invalide (%d)", ErrMalformed, count)
	}

	c := &Container{
		VersionMajor: binary.LittleEndian.Uint16(data[0x14:0x16]),
		VersionMinor: binary.LittleEndian.Uint16(data[0x16:0x18]),
		Parts:        make([]Part, 0, count),
	}
	copy(c.Digest[:], data[4:20])

	for i := 0; i < count; i++ {
		off := int(binary.LittleEndian.Uint32(data[dxbcHeaderSize+4*i : dxbcHeaderSize+4*i+4]))
		if off < dxbcHeaderSize || off+8 > len(data) {
			return nil, fmt.Errorf("%w : offset part %d hors limites", ErrMalformed, i)
		}
		var fcc FourCC
		copy(fcc[:], data[off:off+4])
		size := int(binary.LittleEndian.Uint32(data[off+4 : off+8]))
		if off+8+size > len(data) {
			return nil, fmt.Errorf("%w : taille part %s hors limites", ErrMalformed, fcc)
		}
		c.Parts = append(c.Parts, Part{
			FourCC: fcc,
			Offset: off,
			Data:   data[off+8 : off+8+size],
		})
	}
	return c, nil
}

// Part retourne la première section portant le FourCC demandé.
func (c *Container) Part(fcc FourCC) (*Part, bool) {
	for i := range c.Parts {
		if c.Parts[i].FourCC == fcc {
			return &c.Parts[i], true
		}
	}
	return nil, false
}

// BuildDXBC assemble un conteneur DXBC minimal (utile pour les tests et l'émission).
func BuildDXBC(parts []Part) []byte {
	tableSize := dxbcHeaderSize + 4*len(parts)
	total := tableSize
	for _, p := range parts {
		total += 8 + len(p.Data)
	}
	out := make([]byte, 0, total)
	out = append(out, "DXBC"...)
	out = append(out, make([]byte, 16)...) // digest
	out = binary.LittleEndian.AppendUint16(out, 1)
	out = binary.LittleEndian.AppendUint16(out, 0)
	out = binary.LittleEndian.AppendUint32(out, uint32(total))
	out = binary.LittleEndian.AppendUint32(out, uint32(len(parts)))

	off := uint32(tableSize)
	for _, p := range parts {
		out = binary.LittleEndian.AppendUint32(out, off)
		off += uint32(8 + len(p.Data))
	}
	for _, p := range parts {
		out = append(out, p.FourCC[:]...)
		out = binary.LittleEndian.AppendUint32(out, uint32(len(p.Data)))
		out = append(out, p.Data...)
	}
	return out
}
