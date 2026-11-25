package shader

import (
	"encoding/binary"
	"strings"
	"testing"
)

func TestEmitHLSLVertexAndPixel(t *testing.T) {
	// Programme VS minimal RDNA2 :
	// 0: v_mov_b32 v1, 1.0 (inline const 242 = 0xF2)
	// 1: exp pos0 v0, v1, v0, v1 (tgt=12, en=0xF)
	// 2: s_endpgm
	code := make([]byte, 0, 16)
	w := func(u uint32) {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], u)
		code = append(code, b[:]...)
	}
	// VOP1: [31]=0, [30:25]=0x3F, [24:17]=vdst(1), [16:9]=op(1=v_mov_b32), [8:0]=src0(242=1.0)
	w((0x3F << 25) | (1 << 17) | (1 << 9) | 242)
	// EXP: [31:26]=0b111110, [9:4]=tgt(12=pos0), [3:0]=en(0xF)
	w((0b111110 << 26) | (12 << 4) | 0xF)
	w((1 << 24) | (0 << 16) | (1 << 8) | 0)
	// SOPP: s_endpgm
	w((0b101111111 << 23) | (0x01 << 16))

	out, err := EmitHLSL(nil, NativeProgram{
		Stage: StageVS,
		Code:  code,
	})
	if err != nil {
		t.Fatalf("EmitHLSL VS échoué : %v", err)
	}
	if out.Target != "vs_5_0" || out.EntryPoint != "VSMain" {
		t.Fatalf("target/entry inattendus : %s/%s", out.Target, out.EntryPoint)
	}
	if out.UnsupportedOps != 0 {
		t.Fatalf("ops non supportés inattendus : %d\n%s", out.UnsupportedOps, out.Source)
	}
	if !strings.Contains(out.Source, "exp_pos.x = asfloat(v[0]);") {
		t.Fatalf("export pos0 manquant dans HLSL généré :\n%s", out.Source)
	}
}

func TestEmitHLSLComputeShader(t *testing.T) {
	code := make([]byte, 0, 12)
	w := func(u uint32) {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], u)
		code = append(code, b[:]...)
	}
	// s_barrier (SOPP op=0x0A)
	w((0b101111111 << 23) | (0x0A << 16))
	// s_endpgm
	w((0b101111111 << 23) | (0x01 << 16))

	out, err := EmitHLSL(nil, NativeProgram{
		Stage:       StageCS,
		ThreadGroup: [3]uint32{8, 8, 1},
		Code:        code,
	})
	if err != nil {
		t.Fatalf("EmitHLSL CS échoué : %v", err)
	}
	if out.Target != "cs_5_0" || out.EntryPoint != "CSMain" {
		t.Fatalf("target/entry CS inattendus : %s/%s", out.Target, out.EntryPoint)
	}
	if !strings.Contains(out.Source, "[numthreads(8, 8, 1)]") || !strings.Contains(out.Source, "GroupMemoryBarrierWithGroupSync();") {
		t.Fatalf("HLSL CS malformé :\n%s", out.Source)
	}
}

func TestEmitHLSLComparisonsAndSelect(t *testing.T) {
	code := make([]byte, 0, 16)
	w := func(u uint32) {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], u)
		code = append(code, b[:]...)
	}
	// SOPC: s_cmp_eq_u32 s0, s1 (op=0x06)
	w((0b101111110 << 23) | (0x06 << 16) | (1 << 8) | 0)
	// SOP2: s_cselect_b32 s2, s0, s1 (op=0x0A)
	w((0b10 << 30) | (0x0A << 23) | (2 << 16) | (1 << 8) | 0)
	// VOPC: v_cmp_lt_f32 v0, v1 (op=0x11)
	w((0x3E << 25) | (0x11 << 17) | (1 << 9) | 256)
	// s_endpgm
	w((0b101111111 << 23) | (0x01 << 16))

	out, err := EmitHLSL(nil, NativeProgram{Stage: StageVS, Code: code})
	if err != nil {
		t.Fatalf("EmitHLSL cmp échoué : %v", err)
	}
	if out.UnsupportedOps != 0 {
		t.Fatalf("ops non supportés : %v", out.MissingOps)
	}
	if !strings.Contains(out.Source, "scc = (s[0] == s[1]);") || !strings.Contains(out.Source, "vcc = (asfloat(v[0]) < asfloat(v[1]));") {
		t.Fatalf("comparaisons absentes :\n%s", out.Source)
	}
}

func TestEmitHLSLStructuredBranch(t *testing.T) {
	code := make([]byte, 0, 16)
	w := func(u uint32) {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], u)
		code = append(code, b[:]...)
	}
	// PC=0: s_cmp_eq_u32 s0, s1
	w((0b101111110 << 23) | (0x06 << 16) | (1 << 8) | 0)
	// PC=4: s_cbranch_scc1 +1 dword (-> PC = 4 + 4 + 4 = 12)
	w((0b101111111 << 23) | (0x05 << 16) | 1)
	// PC=8: v_mov_b32 v1, 1.0
	w((0x3F << 25) | (1 << 17) | (1 << 9) | 242)
	// PC=12: s_endpgm
	w((0b101111111 << 23) | (0x01 << 16))

	out, err := EmitHLSL(nil, NativeProgram{Stage: StageVS, Code: code})
	if err != nil {
		t.Fatalf("EmitHLSL branch échoué : %v", err)
	}
	if out.UnsupportedOps != 0 {
		t.Fatalf("ops non supportés : %v", out.MissingOps)
	}
	if !strings.Contains(out.Source, "[branch] if (_cur == 0u)") || !strings.Contains(out.Source, "_pc = (scc) ? 12u : 8u;") {
		t.Fatalf("structure de branchement absente :\n%s", out.Source)
	}
}
