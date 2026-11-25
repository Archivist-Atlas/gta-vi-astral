package shader

import (
	"encoding/binary"
	"fmt"
)

// GFX10Encoding classifie l'encodage binaire d'une instruction AMD RDNA2 (GFX10).
type GFX10Encoding uint8

const (
	EncUnknown GFX10Encoding = iota
	EncVOP2
	EncVOP1
	EncVOPC
	EncSOP2
	EncSOPK
	EncSOP1
	EncSOPC
	EncSOPP
	EncSMEM
	EncVOP3
	EncVOP3P
	EncVINTRP
	EncDS
	EncMUBUF
	EncMTBUF
	EncMIMG
	EncEXP
	EncFLAT
)

func (e GFX10Encoding) String() string {
	switch e {
	case EncVOP2:
		return "VOP2"
	case EncVOP1:
		return "VOP1"
	case EncVOPC:
		return "VOPC"
	case EncSOP2:
		return "SOP2"
	case EncSOPK:
		return "SOPK"
	case EncSOP1:
		return "SOP1"
	case EncSOPC:
		return "SOPC"
	case EncSOPP:
		return "SOPP"
	case EncSMEM:
		return "SMEM"
	case EncVOP3:
		return "VOP3"
	case EncVOP3P:
		return "VOP3P"
	case EncVINTRP:
		return "VINTRP"
	case EncDS:
		return "DS"
	case EncMUBUF:
		return "MUBUF"
	case EncMTBUF:
		return "MTBUF"
	case EncMIMG:
		return "MIMG"
	case EncEXP:
		return "EXP"
	case EncFLAT:
		return "FLAT"
	default:
		return "UNKNOWN"
	}
}

// GFX10Inst représente une instruction RDNA2 décodée depuis le flux machine.
type GFX10Inst struct {
	PC       uint32
	Size     uint8 // 4, 8 ou 12 octets
	Encoding GFX10Encoding
	Word0    uint32
	Word1    uint32
	Literal  uint32
	HasLit   bool
	IsEndPgm bool
	IsBranch bool
}

func (i GFX10Inst) String() string {
	if i.IsEndPgm {
		return fmt.Sprintf("%04x: %-6s s_endpgm", i.PC, i.Encoding)
	}
	if i.Size == 8 {
		return fmt.Sprintf("%04x: %-6s %08x %08x", i.PC, i.Encoding, i.Word0, i.Word1)
	}
	return fmt.Sprintf("%04x: %-6s %08x", i.PC, i.Encoding, i.Word0)
}

// DecodeGFX10 décode un flux binaire RDNA2 (GFX10.1/10.3) jusqu'à s_endpgm ou fin de code.
func DecodeGFX10(code []byte) []GFX10Inst {
	var insts []GFX10Inst
	pc := 0
	for pc+4 <= len(code) {
		w0 := binary.LittleEndian.Uint32(code[pc : pc+4])
		inst := classifyGFX10(uint32(pc), w0)

		if inst.Size == 8 && pc+8 <= len(code) {
			inst.Word1 = binary.LittleEndian.Uint32(code[pc+4 : pc+8])
			// GFX10/RDNA2 : VOP3/VOP3P peuvent porter un littéral 32 bits (instruction 12 octets) si src0/src1/src2 == 0xFF
			if (inst.Encoding == EncVOP3 || inst.Encoding == EncVOP3P) && pc+12 <= len(code) {
				s0 := inst.Word1 & 0x1FF
				s1 := (inst.Word1 >> 9) & 0x1FF
				s2 := (inst.Word1 >> 18) & 0x1FF
				if s0 == 0xFF || s1 == 0xFF || s2 == 0xFF {
					inst.Literal = binary.LittleEndian.Uint32(code[pc+8 : pc+12])
					inst.HasLit = true
					inst.Size = 12
				}
			}
		} else if inst.Size == 8 {
			inst.Size = 4
		}

		// Vérification de constante littérale 32 bits (src0 == 0xFF) pour VOP1/2/C/SOP1/2/C
		if inst.Size == 4 && usesLiteral32(inst.Encoding, w0) && pc+8 <= len(code) {
			inst.Literal = binary.LittleEndian.Uint32(code[pc+4 : pc+8])
			inst.HasLit = true
			inst.Size = 8
		}

		insts = append(insts, inst)
		pc += int(inst.Size)
		if inst.IsEndPgm {
			break
		}
	}
	return insts
}

func classifyGFX10(pc, w0 uint32) GFX10Inst {
	inst := GFX10Inst{PC: pc, Size: 4, Word0: w0}
	switch {
	case (w0 >> 31) == 0:
		// Encodages vectoriels 32 bits : VOP2, VOP1, VOPC
		switch (w0 >> 25) & 0x3F {
		case 0x3F:
			inst.Encoding = EncVOP1
		case 0x3E:
			inst.Encoding = EncVOPC
		default:
			inst.Encoding = EncVOP2
		}
	case (w0 >> 30) == 0b10:
		// Encodages scalaires 32 bits : SOP2, SOPK, SOP1, SOPC, SOPP
		switch {
		case (w0 >> 23) == 0b101111111: // SOPP
			inst.Encoding = EncSOPP
			op := (w0 >> 16) & 0x7F
			if op == 0x01 || op == 0x1B { // s_endpgm / s_endpgm_saved
				inst.IsEndPgm = true
			}
			if op == 0x02 || (op >= 0x04 && op <= 0x09) || op == 0x17 || op == 0x18 {
				inst.IsBranch = true
			}
		case (w0 >> 23) == 0b101111110:
			inst.Encoding = EncSOPC
		case (w0 >> 23) == 0b101111101:
			inst.Encoding = EncSOP1
		case (w0 >> 28) == 0b1011:
			inst.Encoding = EncSOPK
		default:
			inst.Encoding = EncSOP2
		}
	case (w0 >> 26) == 0b110010:
		inst.Encoding = EncVINTRP
	case (w0 >> 26) == 0b111101:
		inst.Encoding = EncSMEM
		inst.Size = 8
	case (w0 >> 26) == 0b110101:
		inst.Encoding = EncVOP3
		inst.Size = 8
	case (w0 >> 26) == 0b110011:
		inst.Encoding = EncVOP3P
		inst.Size = 8
	case (w0 >> 26) == 0b110110:
		inst.Encoding = EncDS
		inst.Size = 8
	case (w0 >> 26) == 0b111000:
		inst.Encoding = EncMUBUF
		inst.Size = 8
	case (w0 >> 26) == 0b111010:
		inst.Encoding = EncMTBUF
		inst.Size = 8
	case (w0 >> 26) == 0b111100:
		inst.Encoding = EncMIMG
		inst.Size = 8
	case (w0 >> 26) == 0b111110:
		inst.Encoding = EncEXP
		inst.Size = 8
	case (w0 >> 26) == 0b110111:
		inst.Encoding = EncFLAT
		inst.Size = 8
	}
	return inst
}

func usesLiteral32(enc GFX10Encoding, w0 uint32) bool {
	switch enc {
	case EncVOP2:
		op := (w0 >> 25) & 0x3F
		if op == 0x23 || op == 0x24 || op == 0x2C || op == 0x2D || op == 0x37 || op == 0x38 {
			// v_madmk_f32 / v_madak_f32 / v_fmamk_f32 / v_fmaak_f32 / v_fmamk_f16 / v_fmaak_f16 portent toujours un littéral 32-bit
			return true
		}
		return (w0 & 0x1FF) == 0xFF
	case EncVOP1, EncVOPC:
		return (w0 & 0x1FF) == 0xFF
	case EncSOP1:
		return (w0 & 0xFF) == 0xFF
	case EncSOP2, EncSOPC:
		return (w0&0xFF) == 0xFF || ((w0>>8)&0xFF) == 0xFF
	default:
		return false
	}
}
