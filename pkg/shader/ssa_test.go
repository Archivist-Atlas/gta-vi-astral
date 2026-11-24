package shader

import (
	"encoding/binary"
	"testing"
)

func TestLiftToSSADropsNopAndGuardsVectorOps(t *testing.T) {
	// s_nop 0 (ignoré) + v_mov_b32 (VOP1, EXEC-guarded) + s_endpgm
	code := make([]byte, 12)
	binary.LittleEndian.PutUint32(code[0:4], 0xBF800000) // s_nop 0
	binary.LittleEndian.PutUint32(code[4:8], 0x7E000280) // v_mov_b32 v0, 0
	binary.LittleEndian.PutUint32(code[8:12], 0xBF810000) // s_endpgm

	mod := LiftToSSA(NativeProgram{
		Stage:  StagePS,
		Wave64: true,
		Code:   code,
	})
	if mod.ValueCount != 2 {
		t.Fatalf("attendu 2 valeurs SSA (s_nop filtré), obtenu %d", mod.ValueCount)
	}
	if len(mod.Blocks) != 1 || len(mod.Blocks[0].Insts) != 2 {
		t.Fatalf("blocs SSA inattendus : %+v", mod.Blocks)
	}
	if !mod.Blocks[0].Insts[0].ExecGuard || mod.Blocks[0].Insts[0].Kind != OpVectorALU {
		t.Fatalf("v_mov_b32 doit être VALU avec ExecGuard=true : %+v", mod.Blocks[0].Insts[0])
	}
	if mod.Blocks[0].Insts[1].Kind != OpEndPgm {
		t.Fatalf("dernière instruction attendue OpEndPgm, obtenu %s", mod.Blocks[0].Insts[1].Kind)
	}
}
