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

func buildSigPart(elems []SigElement) []byte {
	hdrSize := 8 + len(elems)*32
	buf := make([]byte, hdrSize)
	binary.LittleEndian.PutUint32(buf[0:4], uint32(len(elems)))
	binary.LittleEndian.PutUint32(buf[4:8], 8)
	for i, el := range elems {
		nameOff := uint32(len(buf))
		buf = append(buf, []byte(el.SemanticName)...)
		buf = append(buf, 0)
		off := 8 + i*32
		binary.LittleEndian.PutUint32(buf[off+0:off+4], el.Stream)
		binary.LittleEndian.PutUint32(buf[off+4:off+8], nameOff)
		binary.LittleEndian.PutUint32(buf[off+8:off+12], el.SemanticIndex)
		binary.LittleEndian.PutUint32(buf[off+12:off+16], el.SystemValue)
		binary.LittleEndian.PutUint32(buf[off+16:off+20], el.ComponentType)
		binary.LittleEndian.PutUint32(buf[off+20:off+24], el.Register)
		buf[off+24] = el.Mask
		buf[off+25] = el.ReadWriteMask
	}
	return buf
}

func TestEmitHLSLPackedStageAndMIMGNSA(t *testing.T) {
	// Signature DXBC packée : TEXCOORD0 (.xy) + TEXCOORD1 (.zw) dans Register 0, COLOR0 (.xyzw) dans Register 1
	packedElems := []SigElement{
		{SemanticName: "TEXCOORD", SemanticIndex: 0, ComponentType: 3, Register: 0, Mask: 0x3},
		{SemanticName: "TEXCOORD", SemanticIndex: 1, ComponentType: 3, Register: 0, Mask: 0xC},
		{SemanticName: "COLOR", SemanticIndex: 0, ComponentType: 3, Register: 1, Mask: 0xF},
	}

	// 1) VS StageOut avec OSG1 packé + SV_Position
	vsElems := append(append([]SigElement{}, packedElems...), SigElement{
		SemanticName: "SV_Position", SemanticIndex: 0, ComponentType: 3, Register: 2, Mask: 0xF,
	})
	vsCont := &Container{Parts: []Part{{FourCC: FourCCOSG1, Data: buildSigPart(vsElems)}}}
	vsCode := make([]byte, 0, 8)
	var b4 [4]byte
	binary.LittleEndian.PutUint32(b4[:], (0b101111111<<23)|(0x01<<16)) // s_endpgm
	vsCode = append(vsCode, b4[:]...)
	vsOut, err := EmitHLSL(vsCont, NativeProgram{Stage: StageVS, Code: vsCode})
	if err != nil {
		t.Fatalf("EmitHLSL VS packé échoué : %v", err)
	}
	for _, want := range []string{
		"o.out0 = exp_param[0].xy;",
		"o.out1 = exp_param[0].zw;",
		"o.out2 = exp_param[1].xyzw;",
		"o.out3 = exp_pos.xyzw;",
	} {
		if !strings.Contains(vsOut.Source, want) {
			t.Fatalf("sortie VS packée attendue %q absente :\n%s", want, vsOut.Source)
		}
	}

	// 2) PS EncVINTRP packé + MIMG NSA (dwords=1 -> vaddrU=8, vaddrV=15)
	psCont := &Container{Parts: []Part{{FourCC: FourCCISG1, Data: buildSigPart(packedElems)}}}
	psCode := make([]byte, 0, 40)
	w := func(u uint32) {
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], u)
		psCode = append(psCode, b[:]...)
	}
	vintrp := func(vdst, attr, chanIdx uint32) uint32 {
		return (0b110010 << 26) | (vdst << 18) | (attr << 10) | (chanIdx << 8)
	}
	w(vintrp(8, 0, 0))  // TEXCOORD0.x -> inp.in0.x
	w(vintrp(15, 0, 1)) // TEXCOORD0.y -> inp.in0.y
	w(vintrp(6, 0, 2))  // TEXCOORD1.x -> inp.in1.x
	w(vintrp(7, 0, 3))  // TEXCOORD1.y -> inp.in1.y
	w(vintrp(11, 1, 3)) // COLOR0.w    -> inp.in2.w
	// MIMG: op=0x20 (image_sample), dmask=0x8 (w), dwords=1 (NSA -> bits[2:1]=1), vdata=0, vaddr0=8, NSA[0]=15
	w((0b111100 << 26) | (0x20 << 18) | (0x8 << 8) | (1 << 1))
	w((0 << 8) | 8)
	w(15)
	// s_endpgm
	w((0b101111111 << 23) | (0x01 << 16))

	psOut, err := EmitHLSL(psCont, NativeProgram{Stage: StagePS, Code: psCode})
	if err != nil {
		t.Fatalf("EmitHLSL PS packé/NSA échoué : %v", err)
	}
	if psOut.UnsupportedOps != 0 {
		t.Fatalf("ops non supportés inattendus dans PS NSA : %v\n%s", psOut.MissingOps, psOut.Source)
	}
	for _, want := range []string{
		"v[8] = asuint(inp.in0.x);",
		"v[15] = asuint(inp.in0.y);",
		"v[6] = asuint(inp.in1.x);",
		"v[7] = asuint(inp.in1.y);",
		"v[11] = asuint(inp.in2.w);",
		"Tex0.Sample(Samp0, float2(asfloat(v[8]), asfloat(v[15])))",
	} {
		if !strings.Contains(psOut.Source, want) {
			t.Fatalf("ligne PS packée/NSA attendue %q absente :\n%s", want, psOut.Source)
		}
	}
}
