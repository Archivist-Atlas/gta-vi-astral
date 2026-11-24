package shader

import "fmt"

// SSAValueID identifie une valeur immuable en forme SSA (Static Single Assignment).
type SSAValueID uint32

// SSAOpKind classe la catégorie sémantique d'une opération IR (indépendante de DXIL/SPIR-V).
type SSAOpKind uint8

const (
	OpScalarALU SSAOpKind = iota
	OpVectorALU
	OpScalarMem
	OpBufferMem
	OpImageMem
	OpLocalMem // LDS / DS
	OpInterp   // VINTRP
	OpExport   // EXP (MRT / POS / PARAM)
	OpBranch
	OpEndPgm
)

func (k SSAOpKind) String() string {
	switch k {
	case OpScalarALU:
		return "SALU"
	case OpVectorALU:
		return "VALU"
	case OpScalarMem:
		return "SMEM"
	case OpBufferMem:
		return "BUFFER"
	case OpImageMem:
		return "IMAGE"
	case OpLocalMem:
		return "LDS"
	case OpInterp:
		return "INTERP"
	case OpExport:
		return "EXPORT"
	case OpBranch:
		return "BRANCH"
	case OpEndPgm:
		return "ENDPGM"
	default:
		return "UNKNOWN"
	}
}

// SSAInst représente une instruction IR 1:1 issue d'une instruction machine GFX10
// avec garde de masque EXEC explicite (approche victory-shader::ir + structure).
type SSAInst struct {
	ID         SSAValueID
	PC         uint32
	Kind       SSAOpKind
	Encoding   GFX10Encoding
	ExecGuard  bool // true si l'instruction vectorielle dépend du masque de lane EXEC actif
	RawWord0   uint32
	RawWord1   uint32
}

// SSABlock regroupe les instructions SSA d'un bloc de base du CFG.
type SSABlock struct {
	ID    int
	Insts []SSAInst
}

// SSAModule représente un shader complet levé en IR SSA avant abaissement DXIL ou SPIR-V.
type SSAModule struct {
	Stage      HwStage
	Wave64     bool
	Blocks     []SSABlock
	ValueCount uint32
	HasBarrier bool
	HasExports bool
	HasImages  bool
	HasBuffers bool
}

// LiftToSSA convertit un programme natif RDNA2 (GFX10) en module SSA structuré par blocs.
// Les instructions purement de planification (s_nop / s_waitcnt) sont ignorées comme dans VIctory.
func LiftToSSA(prog NativeProgram) *SSAModule {
	insts := DecodeGFX10(prog.Code)
	cfg := BuildCFG(insts)

	mod := &SSAModule{
		Stage:  prog.Stage,
		Wave64: prog.Wave64,
		Blocks: make([]SSABlock, 0, len(cfg)),
	}

	var nextID SSAValueID = 1
	idx := 0
	for _, b := range cfg {
		sb := SSABlock{ID: b.ID}
		for c := 0; c < b.InstCount && idx < len(insts); c++ {
			gi := insts[idx]
			idx++

			if isSchedulingOnly(gi) {
				continue
			}

			kind, execGuard := classifySSAOp(gi)
			switch kind {
			case OpExport:
				mod.HasExports = true
			case OpImageMem:
				mod.HasImages = true
			case OpBufferMem:
				mod.HasBuffers = true
			}
			if gi.Encoding == EncSOPP && ((gi.Word0>>16)&0x7F) == 0x0A { // s_barrier
				mod.HasBarrier = true
			}

			sb.Insts = append(sb.Insts, SSAInst{
				ID:        nextID,
				PC:        gi.PC,
				Kind:      kind,
				Encoding:  gi.Encoding,
				ExecGuard: execGuard,
				RawWord0:  gi.Word0,
				RawWord1:  gi.Word1,
			})
			nextID++
		}
		mod.Blocks = append(mod.Blocks, sb)
	}
	mod.ValueCount = uint32(nextID - 1)
	return mod
}

// Summary retourne une ligne synthétique de l'IR levée.
func (m *SSAModule) Summary() string {
	return fmt.Sprintf("SSA[%s wave64=%v blocks=%d vals=%d img=%v buf=%v exp=%v barrier=%v]",
		m.Stage, m.Wave64, len(m.Blocks), m.ValueCount, m.HasImages, m.HasBuffers, m.HasExports, m.HasBarrier)
}

func isSchedulingOnly(gi GFX10Inst) bool {
	if gi.Encoding != EncSOPP {
		return false
	}
	op := (gi.Word0 >> 16) & 0x7F
	// s_nop (0x00), s_waitcnt (0x0C), s_waitcnt_vscnt/vmcnt/expcnt/lgkmcnt
	return op == 0x00 || op == 0x0C
}

func classifySSAOp(gi GFX10Inst) (SSAOpKind, bool) {
	if gi.IsEndPgm {
		return OpEndPgm, false
	}
	if gi.IsBranch {
		return OpBranch, false
	}
	switch gi.Encoding {
	case EncSOP1, EncSOP2, EncSOPC, EncSOPK, EncSOPP:
		return OpScalarALU, false
	case EncSMEM:
		return OpScalarMem, false
	case EncVOP1, EncVOP2, EncVOP3, EncVOP3P, EncVOPC:
		return OpVectorALU, true
	case EncVINTRP:
		return OpInterp, true
	case EncMUBUF, EncMTBUF, EncFLAT:
		return OpBufferMem, true
	case EncMIMG:
		return OpImageMem, true
	case EncDS:
		return OpLocalMem, true
	case EncEXP:
		return OpExport, true
	default:
		return OpVectorALU, true
	}
}
