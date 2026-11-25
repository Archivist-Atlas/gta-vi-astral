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
