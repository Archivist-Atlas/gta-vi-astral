package shader

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// SigElement décrit un élément d'une signature d'entrée/sortie DXBC (ISG1 / OSG1 / PSG1).
type SigElement struct {
	Stream        uint32
	SemanticName  string
	SemanticIndex uint32
	SystemValue   uint32
	ComponentType uint32 // 1=UINT, 2=SINT, 3=FLOAT
	Register      uint32
	Mask          uint8
	ReadWriteMask uint8
}

// HLSLType retourne le type HLSL correspondant à ComponentType et Mask (ex. float4, uint).
func (e SigElement) HLSLType() string {
	base := "float"
	switch e.ComponentType {
	case 1:
		base = "uint"
	case 2:
		base = "int"
	}
	comps := 0
	for b := uint8(0); b < 4; b++ {
		if e.Mask&(1<<b) != 0 {
			comps++
		}
	}
	if comps <= 1 {
		return base
	}
	return fmt.Sprintf("%s%d", base, comps)
}

// HLSLSemantic retourne la sémantique HLSL complète (ex. SV_Position, TEXCOORD0, SV_Target0).
func (e SigElement) HLSLSemantic() string {
	upper := strings.ToUpper(e.SemanticName)
	if upper == "SV_POSITION" || upper == "SV_VERTEXID" || upper == "SV_INSTANCEID" || upper == "SV_ISFRONTFACE" {
		return e.SemanticName
	}
	return fmt.Sprintf("%s%d", e.SemanticName, e.SemanticIndex)
}

// ComponentCount retourne le nombre de composantes (1..4) actives dans Mask.
func (e SigElement) ComponentCount() int {
	n := 0
	for b := uint8(0); b < 4; b++ {
		if e.Mask&(1<<b) != 0 {
			n++
		}
	}
	if n == 0 {
		return 1
	}
	return n
}

// DXGIFormat retourne le DXGI_FORMAT correspondant à ComponentType et Mask.
func (e SigElement) DXGIFormat() uint32 {
	n := e.ComponentCount()
	switch e.ComponentType {
	case 1: // UINT
		switch n {
		case 1:
			return 42 // R32_UINT
		case 2:
			return 17 // R32G32_UINT
		case 3:
			return 7 // R32G32B32_UINT
		default:
			return 3 // R32G32B32A32_UINT
		}
	case 2: // SINT
		switch n {
		case 1:
			return 43 // R32_SINT
		case 2:
			return 18 // R32G32_SINT
		case 3:
			return 8 // R32G32B32_SINT
		default:
			return 4 // R32G32B32A32_SINT
		}
	default: // FLOAT
		switch n {
		case 1:
			return 41 // R32_FLOAT
		case 2:
			return 16 // R32G32_FLOAT
		case 3:
			return 6 // R32G32B32_FLOAT
		default:
			return 2 // R32G32B32A32_FLOAT
		}
	}
}

// ParseSignature décode un bloc DXBC ISG1, OSG1 ou PSG1 (entrées de 32 octets).
func ParseSignature(data []byte) ([]SigElement, error) {
	if len(data) < 8 {
		return nil, fmt.Errorf("%w : bloc signature trop court (%d octets)", ErrMalformed, len(data))
	}
	count := int(binary.LittleEndian.Uint32(data[0:4]))
	if count == 0 {
		return nil, nil
	}
	const elemSize = 32
	if count > 64 || 8+count*elemSize > len(data) {
		return nil, fmt.Errorf("%w : nombre d'éléments signature invalide (%d)", ErrMalformed, count)
	}
	out := make([]SigElement, 0, count)
	for i := 0; i < count; i++ {
		off := 8 + i*elemSize
		nameOff := int(binary.LittleEndian.Uint32(data[off+4 : off+8]))
		name := readCString(data, nameOff)
		if name == "" {
			name = "TEXCOORD"
		}
		out = append(out, SigElement{
			Stream:        binary.LittleEndian.Uint32(data[off+0 : off+4]),
			SemanticName:  name,
			SemanticIndex: binary.LittleEndian.Uint32(data[off+8 : off+12]),
			SystemValue:   binary.LittleEndian.Uint32(data[off+12 : off+16]),
			ComponentType: binary.LittleEndian.Uint32(data[off+16 : off+20]),
			Register:      binary.LittleEndian.Uint32(data[off+20 : off+24]),
			Mask:          data[off+24],
			ReadWriteMask: data[off+25],
		})
	}
	return out, nil
}

func readCString(data []byte, off int) string {
	if off < 0 || off >= len(data) {
		return ""
	}
	end := off
	for end < len(data) && data[end] != 0 && end-off < 64 {
		end++
	}
	return string(data[off:end])
}
