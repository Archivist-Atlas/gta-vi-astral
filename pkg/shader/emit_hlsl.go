package shader

import (
	"fmt"
	"math"
	"strings"
)

// HLSLOutput contient le source HLSL SM 5.0 généré depuis un programme RDNA2 (GFX10)
// ainsi que les statistiques de couverture d'instructions.
type HLSLOutput struct {
	Stage            HwStage
	EntryPoint       string
	Target           string // "vs_5_0" ou "ps_5_0"
	Source           string
	InputElements    []SigElement // éléments non-SV_* de ISG1 pour D3D12_INPUT_LAYOUT_DESC
	CBDescriptorSlot uint32       // slot CBV (0..7) dans la DescriptorTable racine chargé par s_load_dwordx4
	MissingOps       []string     // encodages/mots non encore abaissés (diagnostic)
	TotalInsts       int
	LoweredInsts     int
	UnsupportedOps   int
}

// EmitHLSL traduit un programme natif RDNA2 (GFX10) + ses signatures DXBC (ISG1/OSG1)
// en code HLSL SM 5.0 compilable par d3dcompiler_47.dll (D3DCompile).
func EmitHLSL(c *Container, prog NativeProgram) (*HLSLOutput, error) {
	if len(prog.Code) == 0 {
		return nil, fmt.Errorf("%w : code RDNA2 vide", ErrMalformed)
	}
	var inputs, outputs []SigElement
	if c != nil {
		if p, ok := c.Part(FourCCISG1); ok {
			inputs, _ = ParseSignature(p.Data)
		}
		if p, ok := c.Part(FourCCOSG1); ok {
			outputs, _ = ParseSignature(p.Data)
		}
	}

	insts := DecodeGFX10(prog.Code)
	var b strings.Builder

	entry := "VSMain"
	target := "vs_5_0"
	isPS := prog.Stage == StagePS
	isCS := prog.Stage == StageCS
	if isPS {
		entry = "PSMain"
		target = "ps_5_0"
	} else if isCS {
		entry = "CSMain"
		target = "cs_5_0"
	}

	b.WriteString("// Généré par gta-vi-astral (RDNA2/GFX10 -> HLSL SM 5.0)\n")
	b.WriteString("cbuffer RootData : register(b0) {\n")
	b.WriteString("    uint4 RootConst[1024];\n")
	b.WriteString("};\n")
	b.WriteString("uint _readCB(uint byteOff) {\n")
	b.WriteString("    uint idx = (byteOff >> 4) & 1023u;\n")
	b.WriteString("    uint comp = (byteOff >> 2) & 3u;\n")
	b.WriteString("    uint4 v4 = RootConst[idx];\n")
	b.WriteString("    return (comp == 0u) ? v4.x : (comp == 1u) ? v4.y : (comp == 2u) ? v4.z : v4.w;\n")
	b.WriteString("}\n")
	if isPS {
		b.WriteString("Texture2D<float4> Tex0 : register(t0);\n")
		b.WriteString("SamplerState Samp0 : register(s0);\n")
	} else if isCS {
		b.WriteString("RWByteAddressBuffer UAV0 : register(u0);\n")
	}

	if isCS {
		tx, ty, tz := prog.ThreadGroup[0], prog.ThreadGroup[1], prog.ThreadGroup[2]
		if tx == 0 {
			tx = 64
		}
		if ty == 0 {
			ty = 1
		}
		if tz == 0 {
			tz = 1
		}
		if tx*ty*tz > 1024 {
			tx, ty, tz = 64, 1, 1
		}
		b.WriteString(fmt.Sprintf("\n[numthreads(%d, %d, %d)]\n", tx, ty, tz))
		b.WriteString("void CSMain(uint3 gid : SV_GroupID, uint3 gtid : SV_GroupThreadID, uint3 dtid : SV_DispatchThreadID) {\n")
		b.WriteString("    uint s[64];\n")
		b.WriteString("    uint v[64];\n")
		b.WriteString("    [unroll] for (uint _i = 0; _i < 64; _i++) { s[_i] = 0u; v[_i] = 0u; }\n")
		b.WriteString("    bool scc = (gid.x == 0xFFFFFFFFu);\n")
		b.WriteString("    bool vcc = (gtid.x == 0xFFFFFFFFu);\n")
		b.WriteString("    bool _exec = true;\n")
		b.WriteString("    s[0] = RootConst[0].x; s[1] = RootConst[0].y; s[2] = gid.x; s[3] = gid.y; s[4] = gid.z;\n")
		b.WriteString("    v[0] = gtid.x; v[1] = gtid.y; v[2] = gtid.z;\n")
	} else {
		// Déclaration des structures d'entrée/sortie depuis ISG1 / OSG1
		b.WriteString("\nstruct StageIn {\n")
		if len(inputs) == 0 {
			if isPS {
				b.WriteString("    float4 pos : SV_Position;\n")
				b.WriteString("    float2 uv0 : TEXCOORD0;\n")
			} else {
				b.WriteString("    uint vid : SV_VertexID;\n")
			}
		} else {
			for idx, el := range inputs {
				b.WriteString(fmt.Sprintf("    %s in%d : %s;\n", el.HLSLType(), idx, el.HLSLSemantic()))
			}
		}
		b.WriteString("};\n\nstruct StageOut {\n")
		if isPS {
			// Cible unique R8G8B8A8_UNORM : garantit la compatibilité D3D12 (évite E_INVALIDARG sur SV_Depth ou SV_Target entier)
			b.WriteString("    float4 mrt0 : SV_Target0;\n")
		} else if len(outputs) == 0 {
			b.WriteString("    float4 pos : SV_Position;\n")
		} else {
			for idx, el := range outputs {
				b.WriteString(fmt.Sprintf("    %s out%d : %s;\n", el.HLSLType(), idx, el.HLSLSemantic()))
			}
		}
		b.WriteString("};\n\n")

		b.WriteString(fmt.Sprintf("StageOut %s(StageIn inp) {\n", entry))
		b.WriteString("    uint s[64];\n")
		b.WriteString("    uint v[64];\n")
		b.WriteString("    [unroll] for (uint _i = 0; _i < 64; _i++) { s[_i] = 0u; v[_i] = 0u; }\n")
		b.WriteString("    bool scc = false;\n")
		b.WriteString("    bool vcc = false;\n")
		b.WriteString("    bool _exec = true;\n")
		b.WriteString("    float4 exp_pos = float4(0.0, 0.0, 0.0, 1.0);\n")
		b.WriteString("    float4 exp_param[8];\n")
		b.WriteString("    float4 exp_mrt[4];\n")
		b.WriteString("    [unroll] for (uint _p = 0; _p < 8; _p++) exp_param[_p] = float4(0,0,0,0);\n")
		b.WriteString("    [unroll] for (uint _m = 0; _m < 4; _m++) exp_mrt[_m] = float4(0,0,0,1);\n")

		// Injection des user SGPRs depuis RootConst
		b.WriteString("    s[0] = RootConst[0].x; s[1] = RootConst[0].y; s[2] = RootConst[0].z; s[3] = RootConst[0].w;\n")

		// Liaison des entrées ISG1 vers les VGPR initiaux (convention RDNA2/Xbox :
		// v[0..3] = valeurs système VertexID/InstanceID, attribut i = v[4 + Register*4 .. +3]).
		if !isPS {
			vidBound := false
			for idx, el := range inputs {
				up := strings.ToUpper(el.SemanticName)
				if up == "SV_VERTEXID" {
					b.WriteString(fmt.Sprintf("    v[0] = inp.in%d;\n", idx))
					vidBound = true
				} else if up == "SV_INSTANCEID" {
					b.WriteString(fmt.Sprintf("    v[1] = inp.in%d;\n", idx))
				} else if !strings.HasPrefix(up, "SV_") {
					baseV := int(4 + el.Register*4)
					comps := el.ComponentCount()
					chs := "xyzw"
					for c := 0; c < comps && baseV+c < 64; c++ {
						if comps == 1 {
							b.WriteString(fmt.Sprintf("    v[%d] = asuint(inp.in%d);\n", baseV+c, idx))
						} else {
							b.WriteString(fmt.Sprintf("    v[%d] = asuint(inp.in%d.%c);\n", baseV+c, idx, chs[c]))
						}
					}
				}
			}
			if !vidBound && len(inputs) == 0 {
				b.WriteString("    v[0] = inp.vid;\n")
			}
		}
	}

	// Abaissement des instructions RDNA2/GFX10
	out := &HLSLOutput{
		Stage:      prog.Stage,
		EntryPoint: entry,
		Target:     target,
		TotalInsts: len(insts),
	}
	if !isPS && !isCS {
		for _, el := range inputs {
			if !strings.HasPrefix(strings.ToUpper(el.SemanticName), "SV_") {
				out.InputElements = append(out.InputElements, el)
			}
		}
	}

	// Détection du slot CBV dans la DescriptorTable racine (s_load_dwordx4 -> s_buffer_load_dword*)
	descSlotByReg := map[uint32]uint32{}
	var fallbackCBSlot uint32
	var hasMatchedCBSlot bool
	for _, gi := range insts {
		if gi.Encoding != EncSMEM {
			continue
		}
		op := (gi.Word0 >> 18) & 0xFF
		sdata := (gi.Word0 >> 6) & 0x7F
		sbase := (gi.Word0 & 0x3F) * 2
		off := gi.Word1 & 0x1FFFFF
		if op == 0x02 && off <= 256 && off%16 == 0 {
			slot := off / 32
			if off%32 != 0 {
				slot = off / 16
			}
			descSlotByReg[sdata] = slot
			if fallbackCBSlot == 0 && slot > 0 {
				fallbackCBSlot = slot
			}
		} else if op >= 0x08 && op <= 0x0C && !hasMatchedCBSlot {
			if slot, ok := descSlotByReg[sbase]; ok {
				out.CBDescriptorSlot = slot
				hasMatchedCBSlot = true
			}
		}
	}
	if !hasMatchedCBSlot && fallbackCBSlot > 0 {
		out.CBDescriptorSlot = fallbackCBSlot
	}

	// Détection de la présence de branches SOPP pour choisir entre émission linéaire ou CFG switch/loop
	hasBranches := false
	for _, gi := range insts {
		if gi.IsBranch {
			hasBranches = true
			break
		}
	}

	if !hasBranches {
		for _, gi := range insts {
			emitSingleInst(&b, gi, isPS, isCS, inputs, out, "    ")
		}
	} else {
		// Découpage en blocs de base par PC cible (branches SOPP + PC suivant)
		targets := map[uint32]bool{0: true}
		for _, gi := range insts {
			if gi.IsBranch {
				simm := int32(int16(gi.Word0 & 0xFFFF))
				tgt := uint32(int32(gi.PC) + 4 + simm*4)
				targets[tgt] = true
				targets[gi.PC+uint32(gi.Size)] = true
			}
		}
		type hlslBlock struct {
			startPC uint32
			insts   []GFX10Inst
			nextPC  uint32
		}
		var blocks []hlslBlock
		for i, gi := range insts {
			if len(blocks) == 0 || targets[gi.PC] {
				if len(blocks) > 0 {
					blocks[len(blocks)-1].nextPC = gi.PC
				}
				blocks = append(blocks, hlslBlock{startPC: gi.PC, nextPC: 0xFFFFFFFF})
			}
			blocks[len(blocks)-1].insts = append(blocks[len(blocks)-1].insts, gi)
			if i == len(insts)-1 {
				blocks[len(blocks)-1].nextPC = 0xFFFFFFFF
			}
		}

		b.WriteString("    uint _pc = 0u;\n")
		b.WriteString("    bool _run = true;\n")
		b.WriteString("    [loop] for (uint _step = 0u; _step < 256u && _run; _step++) {\n")
		b.WriteString("        uint _cur = _pc;\n")
		b.WriteString("        _run = false;\n")
		for _, blk := range blocks {
			b.WriteString(fmt.Sprintf("        [branch] if (_cur == %du) {\n", blk.startPC))
			b.WriteString("            _run = true;\n")
			terminated := false
			for _, gi := range blk.insts {
				if gi.IsEndPgm {
					out.LoweredInsts++
					b.WriteString("            _run = false;\n")
					terminated = true
					break
				}
				if gi.IsBranch {
					op := (gi.Word0 >> 16) & 0x7F
					simm := int32(int16(gi.Word0 & 0xFFFF))
					tgt := uint32(int32(gi.PC) + 4 + simm*4)
					out.LoweredInsts++
					switch op {
					case 0x02: // s_branch
						b.WriteString(fmt.Sprintf("            _pc = %du;\n", tgt))
					case 0x04: // s_cbranch_scc0
						b.WriteString(fmt.Sprintf("            _pc = (!scc) ? %du : %du;\n", tgt, blk.nextPC))
					case 0x05: // s_cbranch_scc1
						b.WriteString(fmt.Sprintf("            _pc = (scc) ? %du : %du;\n", tgt, blk.nextPC))
					case 0x06: // s_cbranch_vccz
						b.WriteString(fmt.Sprintf("            _pc = (!vcc) ? %du : %du;\n", tgt, blk.nextPC))
					case 0x07: // s_cbranch_vccnz
						b.WriteString(fmt.Sprintf("            _pc = (vcc) ? %du : %du;\n", tgt, blk.nextPC))
					case 0x08, 0x09: // s_cbranch_execz / execnz (lane active)
						b.WriteString(fmt.Sprintf("            _pc = %du;\n", blk.nextPC))
					default:
						b.WriteString(fmt.Sprintf("            _pc = %du;\n", blk.nextPC))
					}
					terminated = true
					break
				}
				emitSingleInst(&b, gi, isPS, isCS, inputs, out, "            ")
			}
			if !terminated {
				if blk.nextPC == 0xFFFFFFFF {
					b.WriteString("            _run = false;\n")
				} else {
					b.WriteString(fmt.Sprintf("            _pc = %du;\n", blk.nextPC))
				}
			}
			b.WriteString("        }\n")
		}
		b.WriteString("    }\n")
	}

	if isCS {
		b.WriteString("}\n")
		out.Source = b.String()
		return out, nil
	}

	// Liaison des exports RDNA2 (exp_pos / exp_param / exp_mrt) vers StageOut
	b.WriteString("    StageOut o = (StageOut)0;\n")
	if isPS {
		b.WriteString("    o.mrt0 = exp_mrt[0];\n")
	} else if len(outputs) == 0 {
		b.WriteString("    o.pos = exp_pos;\n")
	} else {
		paramIdx := 0
		for idx, el := range outputs {
			up := strings.ToUpper(el.SemanticName)
			if up == "SV_POSITION" {
				b.WriteString(fmt.Sprintf("    o.out%d = %s;\n", idx, swizzleForMask("exp_pos", el)))
			} else {
				p := paramIdx
				if p >= 8 {
					p = 7
				}
				paramIdx++
				b.WriteString(fmt.Sprintf("    o.out%d = %s;\n", idx, swizzleForMask(fmt.Sprintf("exp_param[%d]", p), el)))
			}
		}
	}
	b.WriteString("    return o;\n}\n")

	out.Source = b.String()
	return out, nil
}

func emitSingleInst(b *strings.Builder, gi GFX10Inst, isPS, isCS bool, inputs []SigElement, out *HLSLOutput, indent string) {
	if isSchedulingOnly(gi) || gi.IsEndPgm {
		out.LoweredInsts++
		return
	}
	if gi.Encoding == EncSOPP && ((gi.Word0>>16)&0x7F) == 0x20 { // s_clause
		out.LoweredInsts++
		return
	}
	if gi.Encoding == EncSOPP && ((gi.Word0>>16)&0x7F) == 0x0A && isCS { // s_barrier
		b.WriteString(indent + "GroupMemoryBarrierWithGroupSync();\n")
		out.LoweredInsts++
		return
	}
	if gi.Encoding == EncVOP3 && ((gi.Word0>>16)&0x3FF) == 0x180 { // v_nop (metadata tag)
		out.LoweredInsts++
		return
	}
	if stmt, ok := lowerGFX10ToHLSL(gi, isPS, isCS, inputs); ok {
		if stmt != "" {
			switch gi.Encoding {
			case EncVOP1, EncVOP2, EncVOP3, EncVOPC, EncMIMG:
				b.WriteString(indent + "if (_exec) { " + stmt + " }\n")
			default:
				b.WriteString(indent + stmt + "\n")
			}
		}
		out.LoweredInsts++
	} else {
		out.UnsupportedOps++
		if len(out.MissingOps) < 12 {
			out.MissingOps = append(out.MissingOps, fmt.Sprintf("%s(%08X/%08X)", gi.Encoding, gi.Word0, gi.Word1))
		}
		b.WriteString(fmt.Sprintf("%s// unimpl %04x %s %08x %08x\n", indent, gi.PC, gi.Encoding, gi.Word0, gi.Word1))
	}
}

func swizzleForMask(expr string, el SigElement) string {
	var sw strings.Builder
	names := "xyzw"
	for i := 0; i < 4; i++ {
		if el.Mask&(1<<uint(i)) != 0 {
			sw.WriteByte(names[i])
		}
	}
	s := sw.String()
	if s == "" {
		s = "xyzw"
	}
	val := fmt.Sprintf("%s.%s", expr, s)
	switch el.ComponentType {
	case 1:
		return fmt.Sprintf("asuint(%s)", val)
	case 2:
		return fmt.Sprintf("asint(%s)", val)
	default:
		return val
	}
}

// gfx10SrcU32 résout un opérande source 9 bits GFX10 (SGPR, constante inline, littéral 32 bits, VGPR) en expression HLSL uint.
func gfx10SrcU32(src uint32, lit uint32, hasLit bool) string {
	switch {
	case src < 64:
		return fmt.Sprintf("s[%d]", src)
	case src == 106: // VCC_LO
		return "(vcc ? 1u : 0u)"
	case src == 126: // EXEC_LO
		return "(_exec ? 1u : 0u)"
	case src >= 128 && src <= 192:
		return fmt.Sprintf("%du", src-128)
	case src >= 193 && src <= 208:
		return fmt.Sprintf("asuint(%d)", -int(src-192))
	case src == 240:
		return fmt.Sprintf("0x%08Xu", math.Float32bits(0.5))
	case src == 241:
		return fmt.Sprintf("0x%08Xu", math.Float32bits(-0.5))
	case src == 242:
		return fmt.Sprintf("0x%08Xu", math.Float32bits(1.0))
	case src == 243:
		return fmt.Sprintf("0x%08Xu", math.Float32bits(-1.0))
	case src == 244:
		return fmt.Sprintf("0x%08Xu", math.Float32bits(2.0))
	case src == 245:
		return fmt.Sprintf("0x%08Xu", math.Float32bits(-2.0))
	case src == 246:
		return fmt.Sprintf("0x%08Xu", math.Float32bits(4.0))
	case src == 247:
		return fmt.Sprintf("0x%08Xu", math.Float32bits(-4.0))
	case src == 248:
		return fmt.Sprintf("0x%08Xu", math.Float32bits(1.0/(2.0*math.Pi)))
	case src == 255 && hasLit:
		return fmt.Sprintf("0x%08Xu", lit)
	case src >= 256 && src < 320:
		return fmt.Sprintf("v[%d]", src-256)
	default:
		return "0u"
	}
}

func gfx10SrcF32(src uint32, lit uint32, hasLit bool) string {
	return fmt.Sprintf("asfloat(%s)", gfx10SrcU32(src, lit, hasLit))
}

func lowerGFX10ToHLSL(gi GFX10Inst, isPS, isCS bool, inputs []SigElement) (string, bool) {
	w0, w1 := gi.Word0, gi.Word1
	switch gi.Encoding {
	case EncVOP1:
		src0 := w0 & 0x1FF
		op := (w0 >> 9) & 0xFF
		vdst := (w0 >> 17) & 0xFF
		if vdst >= 64 {
			return "", false
		}
		su := gfx10SrcU32(src0, gi.Literal, gi.HasLit)
		sf := fmt.Sprintf("asfloat(%s)", su)
		switch op {
		case 0x01: // v_mov_b32
			return fmt.Sprintf("v[%d] = %s;", vdst, su), true
		case 0x05: // v_cvt_f32_i32
			return fmt.Sprintf("v[%d] = asuint((float)asint(%s));", vdst, su), true
		case 0x06: // v_cvt_f32_u32
			return fmt.Sprintf("v[%d] = asuint((float)%s);", vdst, su), true
		case 0x07: // v_cvt_u32_f32
			return fmt.Sprintf("v[%d] = (uint)max(0.0, %s);", vdst, sf), true
		case 0x08: // v_cvt_i32_f32
			return fmt.Sprintf("v[%d] = asuint((int)%s);", vdst, sf), true
		case 0x0A: // v_cvt_f16_f32
			return fmt.Sprintf("v[%d] = f32tof16(%s);", vdst, sf), true
		case 0x0B: // v_cvt_f32_f16
			return fmt.Sprintf("v[%d] = asuint(f16tof32(%s));", vdst, su), true
		case 0x20: // v_fract_f32
			return fmt.Sprintf("v[%d] = asuint(frac(%s));", vdst, sf), true
		case 0x21: // v_trunc_f32
			return fmt.Sprintf("v[%d] = asuint(trunc(%s));", vdst, sf), true
		case 0x22: // v_ceil_f32
			return fmt.Sprintf("v[%d] = asuint(ceil(%s));", vdst, sf), true
		case 0x23: // v_rndne_f32
			return fmt.Sprintf("v[%d] = asuint(round(%s));", vdst, sf), true
		case 0x24: // v_floor_f32
			return fmt.Sprintf("v[%d] = asuint(floor(%s));", vdst, sf), true
		case 0x25: // v_exp_f32
			return fmt.Sprintf("v[%d] = asuint(exp2(%s));", vdst, sf), true
		case 0x27: // v_log_f32
			return fmt.Sprintf("v[%d] = asuint(log2(max(1e-20, %s)));", vdst, sf), true
		case 0x2A: // v_rcp_f32
			return fmt.Sprintf("v[%d] = asuint(rcp(%s));", vdst, sf), true
		case 0x2E: // v_rsq_f32
			return fmt.Sprintf("v[%d] = asuint(rsqrt(max(1e-20, %s)));", vdst, sf), true
		case 0x33: // v_sqrt_f32
			return fmt.Sprintf("v[%d] = asuint(sqrt(max(0.0, %s)));", vdst, sf), true
		case 0x35: // v_sin_f32
			return fmt.Sprintf("v[%d] = asuint(sin(%s * 6.2831853));", vdst, sf), true
		case 0x36: // v_cos_f32
			return fmt.Sprintf("v[%d] = asuint(cos(%s * 6.2831853));", vdst, sf), true
		case 0x37: // v_not_b32
			return fmt.Sprintf("v[%d] = ~(%s);", vdst, su), true
		}

	case EncVOP2:
		src0 := w0 & 0x1FF
		vsrc1 := (w0 >> 9) & 0xFF
		vdst := (w0 >> 17) & 0xFF
		op := (w0 >> 25) & 0x3F
		if vdst >= 64 || vsrc1 >= 64 {
			return "", false
		}
		s0u := gfx10SrcU32(src0, gi.Literal, gi.HasLit)
		s0f := fmt.Sprintf("asfloat(%s)", s0u)
		s1u := fmt.Sprintf("v[%d]", vsrc1)
		s1f := fmt.Sprintf("asfloat(v[%d])", vsrc1)
		switch op {
		case 0x01: // v_cndmask_b32
			return fmt.Sprintf("v[%d] = vcc ? %s : %s;", vdst, s1u, s0u), true
		case 0x03: // v_add_f32
			return fmt.Sprintf("v[%d] = asuint(%s + %s);", vdst, s0f, s1f), true
		case 0x04: // v_sub_f32
			return fmt.Sprintf("v[%d] = asuint(%s - %s);", vdst, s0f, s1f), true
		case 0x05: // v_subrev_f32
			return fmt.Sprintf("v[%d] = asuint(%s - %s);", vdst, s1f, s0f), true
		case 0x07, 0x08: // v_mul_legacy_f32 / v_mul_f32
			return fmt.Sprintf("v[%d] = asuint(%s * %s);", vdst, s0f, s1f), true
		case 0x0F: // v_min_f32
			return fmt.Sprintf("v[%d] = asuint(min(%s, %s));", vdst, s0f, s1f), true
		case 0x10: // v_max_f32
			return fmt.Sprintf("v[%d] = asuint(max(%s, %s));", vdst, s0f, s1f), true
		case 0x18: // v_lshrrev_b32
			return fmt.Sprintf("v[%d] = (%s >> (%s & 31u));", vdst, s1u, s0u), true
		case 0x19: // v_ashrrev_i32
			return fmt.Sprintf("v[%d] = asuint(asint(%s) >> (%s & 31u));", vdst, s1u, s0u), true
		case 0x1A: // v_lshlrev_b32
			return fmt.Sprintf("v[%d] = (%s << (%s & 31u));", vdst, s1u, s0u), true
		case 0x1B: // v_and_b32
			return fmt.Sprintf("v[%d] = (%s & %s);", vdst, s0u, s1u), true
		case 0x1C: // v_or_b32
			return fmt.Sprintf("v[%d] = (%s | %s);", vdst, s0u, s1u), true
		case 0x1D: // v_xor_b32
			return fmt.Sprintf("v[%d] = (%s ^ %s);", vdst, s0u, s1u), true
		case 0x25: // v_add_nc_u32
			return fmt.Sprintf("v[%d] = (%s + %s);", vdst, s0u, s1u), true
		case 0x26: // v_sub_nc_u32
			return fmt.Sprintf("v[%d] = (%s - %s);", vdst, s0u, s1u), true
		case 0x27: // v_subrev_nc_u32
			return fmt.Sprintf("v[%d] = (%s - %s);", vdst, s1u, s0u), true
		case 0x2B: // v_fmac_f32
			return fmt.Sprintf("v[%d] = asuint(%s * %s + asfloat(v[%d]));", vdst, s0f, s1f, vdst), true
		case 0x2C: // v_fmamk_f32 (avec littéral)
			return fmt.Sprintf("v[%d] = asuint(%s * asfloat(0x%08Xu) + %s);", vdst, s0f, gi.Literal, s1f), true
		case 0x2D: // v_fmaak_f32 (avec littéral)
			return fmt.Sprintf("v[%d] = asuint(%s * %s + asfloat(0x%08Xu));", vdst, s0f, s1f, gi.Literal), true
		case 0x2F: // v_cvt_pkrtz_f16_f32 (pack deux float32 en FP16x2 pour exp mrt0 compr)
			return fmt.Sprintf("v[%d] = (f32tof16(%s) & 0xFFFFu) | (f32tof16(%s) << 16);", vdst, s0f, s1f), true
		}

	case EncVOP3:
		vdst := w0 & 0xFF
		op := (w0 >> 16) & 0x3FF
		src0 := w1 & 0x1FF
		src1 := (w1 >> 9) & 0x1FF
		src2 := (w1 >> 18) & 0x1FF
		if vdst >= 64 {
			return "", false
		}
		s0u := gfx10SrcU32(src0, gi.Literal, gi.HasLit)
		s1u := gfx10SrcU32(src1, gi.Literal, gi.HasLit)
		s2u := gfx10SrcU32(src2, gi.Literal, gi.HasLit)
		s0f := fmt.Sprintf("asfloat(%s)", s0u)
		s1f := fmt.Sprintf("asfloat(%s)", s1u)
		s2f := fmt.Sprintf("asfloat(%s)", s2u)
		switch op {
		case 0x14B, 0x141: // v_fma_f32 / v_mad_f32
			return fmt.Sprintf("v[%d] = asuint(%s * %s + %s);", vdst, s0f, s1f, s2f), true
		case 0x169: // v_mul_lo_u32
			return fmt.Sprintf("v[%d] = (%s * %s);", vdst, s0u, s1u), true
		case 0x157: // v_med3_f32 (GFX10)
			return fmt.Sprintf("v[%d] = asuint(clamp(%s, min(%s, %s), max(%s, %s)));", vdst, s0f, s1f, s2f, s1f, s2f), true
		case 0x150, 0x151: // v_min3_f32 (GFX9/GFX10)
			return fmt.Sprintf("v[%d] = asuint(min(min(%s, %s), %s));", vdst, s0f, s1f, s2f), true
		case 0x153, 0x154: // v_max3_f32 (GFX9/GFX10)
			return fmt.Sprintf("v[%d] = asuint(max(max(%s, %s), %s));", vdst, s0f, s1f, s2f), true
		case 0x148: // v_bfe_u32
			return fmt.Sprintf("v[%d] = ((%s >> (%s & 31u)) & ((1u << (%s & 31u)) - 1u));", vdst, s0u, s1u, s2u), true
		case 0x14A: // v_bfi_b32
			return fmt.Sprintf("v[%d] = ((%s & %s) | (~%s & %s));", vdst, s0u, s1u, s0u, s2u), true
		case 0x101: // v_cndmask_b32 (VOP3)
			return fmt.Sprintf("v[%d] = vcc ? %s : %s;", vdst, s1u, s0u), true
		case 0x103: // v_add_f32 (VOP3)
			return fmt.Sprintf("v[%d] = asuint(%s + %s);", vdst, s0f, s1f), true
		case 0x104: // v_sub_f32 (VOP3)
			return fmt.Sprintf("v[%d] = asuint(%s - %s);", vdst, s0f, s1f), true
		case 0x107, 0x108: // v_mul_f32 (VOP3)
			return fmt.Sprintf("v[%d] = asuint(%s * %s);", vdst, s0f, s1f), true
		case 0x10F: // v_min_f32 (VOP3)
			return fmt.Sprintf("v[%d] = asuint(min(%s, %s));", vdst, s0f, s1f), true
		case 0x110: // v_max_f32 (VOP3)
			return fmt.Sprintf("v[%d] = asuint(max(%s, %s));", vdst, s0f, s1f), true
		case 0x118: // v_lshrrev_b32 (VOP3)
			return fmt.Sprintf("v[%d] = (%s >> (%s & 31u));", vdst, s1u, s0u), true
		case 0x11A: // v_lshlrev_b32 (VOP3)
			return fmt.Sprintf("v[%d] = (%s << (%s & 31u));", vdst, s1u, s0u), true
		case 0x11B: // v_and_b32 (VOP3)
			return fmt.Sprintf("v[%d] = (%s & %s);", vdst, s0u, s1u), true
		case 0x11C: // v_or_b32 (VOP3)
			return fmt.Sprintf("v[%d] = (%s | %s);", vdst, s0u, s1u), true
		case 0x11D: // v_xor_b32 (VOP3)
			return fmt.Sprintf("v[%d] = (%s ^ %s);", vdst, s0u, s1u), true
		case 0x125, 0x20F: // v_add_nc_u32 / v_add_co_u32
			return fmt.Sprintf("v[%d] = (%s + %s);", vdst, s0u, s1u), true
		case 0x126, 0x210: // v_sub_nc_u32 / v_sub_co_u32
			return fmt.Sprintf("v[%d] = (%s - %s);", vdst, s0u, s1u), true
		case 0x156: // v_alignbit_b32
			return fmt.Sprintf("v[%d] = ((%s << (32u - (%s & 31u))) | (%s >> (%s & 31u)));", vdst, s0u, s2u, s1u, s2u), true
		case 0x36D: // v_add3_u32
			return fmt.Sprintf("v[%d] = (%s + %s + %s);", vdst, s0u, s1u, s2u), true
		case 0x36F: // v_lshl_or_b32
			return fmt.Sprintf("v[%d] = ((%s << (%s & 31u)) | %s);", vdst, s0u, s1u, s2u), true
		case 0x371: // v_and_or_b32
			return fmt.Sprintf("v[%d] = ((%s & %s) | %s);", vdst, s0u, s1u, s2u), true
		}

	case EncVOPC:
		src0 := w0 & 0x1FF
		vsrc1 := (w0 >> 9) & 0xFF
		op := (w0 >> 17) & 0xFF
		if vsrc1 >= 64 {
			return "", false
		}
		s0u := gfx10SrcU32(src0, gi.Literal, gi.HasLit)
		s0f := fmt.Sprintf("asfloat(%s)", s0u)
		s1u := fmt.Sprintf("v[%d]", vsrc1)
		s1f := fmt.Sprintf("asfloat(v[%d])", vsrc1)
		// v_cmp_*_f32 (0x10..0x1F), v_cmp_*_i32 (0x80..0x87), v_cmp_*_u32 (0xC0..0xC7)
		cond := cmpCond(op&0x7)
		if cond != "" {
			switch {
			case op >= 0x10 && op < 0x20:
				return fmt.Sprintf("vcc = (%s %s %s);", s0f, cond, s1f), true
			case op >= 0x80 && op < 0x88:
				return fmt.Sprintf("vcc = (asint(%s) %s asint(%s));", s0u, cond, s1u), true
			case op >= 0xC0 && op < 0xC8:
				return fmt.Sprintf("vcc = (%s %s %s);", s0u, cond, s1u), true
			}
		}

	case EncSOPC:
		ssrc0 := w0 & 0xFF
		ssrc1 := (w0 >> 8) & 0xFF
		op := (w0 >> 16) & 0x7F
		s0 := gfx10SrcU32(ssrc0, gi.Literal, gi.HasLit)
		s1 := gfx10SrcU32(ssrc1, gi.Literal, gi.HasLit)
		switch op {
		case 0x00: // s_cmp_eq_i32
			return fmt.Sprintf("scc = (asint(%s) == asint(%s));", s0, s1), true
		case 0x01: // s_cmp_lg_i32
			return fmt.Sprintf("scc = (asint(%s) != asint(%s));", s0, s1), true
		case 0x02: // s_cmp_gt_i32
			return fmt.Sprintf("scc = (asint(%s) > asint(%s));", s0, s1), true
		case 0x03: // s_cmp_ge_i32
			return fmt.Sprintf("scc = (asint(%s) >= asint(%s));", s0, s1), true
		case 0x04: // s_cmp_lt_i32
			return fmt.Sprintf("scc = (asint(%s) < asint(%s));", s0, s1), true
		case 0x05: // s_cmp_le_i32
			return fmt.Sprintf("scc = (asint(%s) <= asint(%s));", s0, s1), true
		case 0x06: // s_cmp_eq_u32
			return fmt.Sprintf("scc = (%s == %s);", s0, s1), true
		case 0x07: // s_cmp_lg_u32
			return fmt.Sprintf("scc = (%s != %s);", s0, s1), true
		case 0x08: // s_cmp_gt_u32
			return fmt.Sprintf("scc = (%s > %s);", s0, s1), true
		case 0x09: // s_cmp_ge_u32
			return fmt.Sprintf("scc = (%s >= %s);", s0, s1), true
		case 0x0A: // s_cmp_lt_u32
			return fmt.Sprintf("scc = (%s < %s);", s0, s1), true
		case 0x0B: // s_cmp_le_u32
			return fmt.Sprintf("scc = (%s <= %s);", s0, s1), true
		}

	case EncSOP1:
		ssrc0 := w0 & 0xFF
		op := (w0 >> 8) & 0xFF
		sdst := (w0 >> 16) & 0x7F
		su := gfx10SrcU32(ssrc0, gi.Literal, gi.HasLit)
		if sdst == 126 { // EXEC_LO
			switch op {
			case 0x03, 0x04: // s_mov_b32 / s_mov_b64 exec, src
				return fmt.Sprintf("_exec = (%s != 0u);", su), true
			case 0x07, 0x08: // s_not_b32 / s_not_b64 exec, src
				return fmt.Sprintf("_exec = (%s == 0u);", su), true
			}
			return "", true
		}
		if sdst >= 64 {
			return "", true
		}
		switch op {
		case 0x03: // s_mov_b32
			return fmt.Sprintf("s[%d] = %s;", sdst, su), true
		case 0x04: // s_mov_b64
			if sdst+1 < 64 {
				return fmt.Sprintf("s[%d] = %s; s[%d] = 0u;", sdst, su, sdst+1), true
			}
			return fmt.Sprintf("s[%d] = %s;", sdst, su), true
		case 0x07: // s_not_b32
			return fmt.Sprintf("s[%d] = ~(%s);", sdst, su), true
		case 0x1D: // s_wqm_b64
			return "", true
		case 0x20, 0x21, 0x24, 0x3C: // s_and_saveexec_b64 / b32
			return fmt.Sprintf("s[%d] = _exec ? 1u : 0u; _exec = _exec && (%s != 0u);", sdst, su), true
		case 0x22, 0x3E: // s_or_saveexec_b64 / b32
			return fmt.Sprintf("s[%d] = _exec ? 1u : 0u; _exec = _exec || (%s != 0u);", sdst, su), true
		case 0x23, 0x3F: // s_xor_saveexec_b64 / b32
			return fmt.Sprintf("s[%d] = _exec ? 1u : 0u; _exec = (_exec != (%s != 0u));", sdst, su), true
		case 0x26, 0x42: // s_andn2_saveexec_b64 / b32
			return fmt.Sprintf("s[%d] = _exec ? 1u : 0u; _exec = _exec && (%s == 0u);", sdst, su), true
		}

	case EncSOPK:
		imm16 := int16(w0 & 0xFFFF)
		sdst := (w0 >> 16) & 0x7F
		op := (w0 >> 23) & 0x1F
		if sdst >= 64 {
			return "", true
		}
		if op == 0x00 { // s_movk_i32
			return fmt.Sprintf("s[%d] = asuint(%d);", sdst, int32(imm16)), true
		}

	case EncSOP2:
		ssrc0 := w0 & 0xFF
		ssrc1 := (w0 >> 8) & 0xFF
		sdst := (w0 >> 16) & 0x7F
		op := (w0 >> 23) & 0x7F
		s0 := gfx10SrcU32(ssrc0, gi.Literal, gi.HasLit)
		s1 := gfx10SrcU32(ssrc1, gi.Literal, gi.HasLit)
		if sdst == 126 { // EXEC_LO : masquage par lane (ex. branches sRGB / if-else)
			switch op {
			case 0x0E, 0x0F: // s_and_b32 / b64
				return fmt.Sprintf("_exec = ((%s != 0u) && (%s != 0u));", s0, s1), true
			case 0x10, 0x11: // s_or_b32 / b64
				return fmt.Sprintf("_exec = ((%s != 0u) || (%s != 0u));", s0, s1), true
			case 0x12, 0x13: // s_xor_b32 / b64
				return fmt.Sprintf("_exec = ((%s != 0u) != (%s != 0u));", s0, s1), true
			case 0x14, 0x15: // s_andn2_b32 / b64
				return fmt.Sprintf("_exec = ((%s != 0u) && (%s == 0u));", s0, s1), true
			}
			return "", true
		}
		if sdst >= 64 {
			return "", true
		}
		switch op {
		case 0x00, 0x02, 0x04: // s_add_u32 / s_add_i32 / s_addc_u32
			return fmt.Sprintf("s[%d] = (%s + %s);", sdst, s0, s1), true
		case 0x01, 0x03, 0x05: // s_sub_u32 / s_sub_i32 / s_subb_u32
			return fmt.Sprintf("s[%d] = (%s - %s);", sdst, s0, s1), true
		case 0x06: // s_min_i32
			return fmt.Sprintf("s[%d] = asuint(min(asint(%s), asint(%s)));", sdst, s0, s1), true
		case 0x07: // s_min_u32
			return fmt.Sprintf("s[%d] = min(%s, %s);", sdst, s0, s1), true
		case 0x08: // s_max_i32
			return fmt.Sprintf("s[%d] = asuint(max(asint(%s), asint(%s)));", sdst, s0, s1), true
		case 0x09: // s_max_u32
			return fmt.Sprintf("s[%d] = max(%s, %s);", sdst, s0, s1), true
		case 0x0A, 0x2E: // s_cselect_b32
			return fmt.Sprintf("s[%d] = scc ? %s : %s;", sdst, s0, s1), true
		case 0x0E: // s_and_b32
			return fmt.Sprintf("s[%d] = (%s & %s);", sdst, s0, s1), true
		case 0x10: // s_or_b32
			return fmt.Sprintf("s[%d] = (%s | %s);", sdst, s0, s1), true
		case 0x12: // s_xor_b32
			return fmt.Sprintf("s[%d] = (%s ^ %s);", sdst, s0, s1), true
		case 0x1C: // s_lshl_b32
			return fmt.Sprintf("s[%d] = (%s << (%s & 31u));", sdst, s0, s1), true
		case 0x1E: // s_lshr_b32
			return fmt.Sprintf("s[%d] = (%s >> (%s & 31u));", sdst, s0, s1), true
		case 0x20: // s_ashr_i32
			return fmt.Sprintf("s[%d] = asuint(asint(%s) >> (%s & 31u));", sdst, s0, s1), true
		case 0x2C: // s_mul_i32
			return fmt.Sprintf("s[%d] = asuint(asint(%s) * asint(%s));", sdst, s0, s1), true
		}

	case EncSMEM:
		op := (w0 >> 18) & 0xFF
		sdata := (w0 >> 6) & 0x7F
		off := w1 & 0x1FFFFF // offset 21-bit en octets (GFX10)
		count := uint32(0)
		switch op {
		case 0x00, 0x08: // s_load_dword / s_buffer_load_dword
			count = 1
		case 0x01, 0x09: // s_load_dwordx2 / s_buffer_load_dwordx2
			count = 2
		case 0x02, 0x0A: // s_load_dwordx4 / s_buffer_load_dwordx4
			count = 4
		case 0x03, 0x0B: // s_load_dwordx8 / s_buffer_load_dwordx8
			count = 8
		case 0x04, 0x0C: // s_load_dwordx16 / s_buffer_load_dwordx16
			count = 16
		}
		if count > 0 && sdata+count <= 64 {
			var parts []string
			for i := uint32(0); i < count; i++ {
				parts = append(parts, fmt.Sprintf("s[%d] = _readCB(%du);", sdata+i, off+i*4))
			}
			return strings.Join(parts, " "), true
		}

	case EncMUBUF:
		op := (w0 >> 18) & 0x7F
		off := w0 & 0xFFF
		offen := (w0 >> 12) & 1
		idxen := (w0 >> 13) & 1
		vaddr := w1 & 0xFF
		vdata := (w1 >> 8) & 0xFF
		count := uint32(0)
		switch op {
		case 0x0C, 0x08, 0x09, 0x0A, 0x0B: // buffer_load_dword / format_x
			count = 1
		case 0x0D: // buffer_load_dwordx2
			count = 2
		case 0x0F: // buffer_load_dwordx3
			count = 3
		case 0x0E: // buffer_load_dwordx4
			count = 4
		}
		if count > 0 && vdata+count <= 64 {
			var addrExpr string
			switch {
			case idxen != 0 && offen == 0 && vaddr < 64:
				addrExpr = fmt.Sprintf("(v[%d] * 16u + %du)", vaddr, off)
			case offen != 0 && idxen == 0 && vaddr < 64:
				addrExpr = fmt.Sprintf("(v[%d] + %du)", vaddr, off)
			case idxen != 0 && offen != 0 && vaddr+1 < 64:
				addrExpr = fmt.Sprintf("(v[%d] * 16u + v[%d] + %du)", vaddr, vaddr+1, off)
			default:
				addrExpr = fmt.Sprintf("%du", off)
			}
			var parts []string
			parts = append(parts, fmt.Sprintf("{ uint _mOff = %s;", addrExpr))
			for i := uint32(0); i < count; i++ {
				parts = append(parts, fmt.Sprintf("v[%d] = _readCB(_mOff + %du);", vdata+i, i*4))
			}
			parts = append(parts, "}")
			return strings.Join(parts, " "), true
		}
		if isCS && (op >= 0x18 && op <= 0x1B) && vdata < 64 { // buffer_store_dword*
			return fmt.Sprintf("UAV0.Store((dtid.x * 4u + %du) & 0xFFFFCu, v[%d]);", off, vdata), true
		}

	case EncVINTRP:
		if !isPS {
			return "", true
		}
		attrChan := (w0 >> 8) & 0x3
		attr := (w0 >> 10) & 0x3F
		vdst := (w0 >> 18) & 0xFF
		if vdst >= 64 {
			return "", false
		}
		ch := "xyzw"[attrChan]
		// Mappe l'attribut interpolé vers la première entrée PS non-SV_Position correspondante
		if len(inputs) == 0 {
			if attrChan < 2 {
				return fmt.Sprintf("v[%d] = asuint(inp.uv0.%c);", vdst, ch), true
			}
			return fmt.Sprintf("v[%d] = 0u;", vdst), true
		}
		// Recherche de l'attribut `attr` parmi les entrées non-SV_Position
		pIdx := uint32(0)
		for idx, el := range inputs {
			if strings.ToUpper(el.SemanticName) == "SV_POSITION" {
				continue
			}
			if pIdx == attr {
				comps := el.ComponentCount()
				if el.Mask&(1<<attrChan) != 0 {
					if comps == 1 {
						return fmt.Sprintf("v[%d] = asuint(inp.in%d);", vdst, idx), true
					}
					if int(attrChan) < comps {
						return fmt.Sprintf("v[%d] = asuint(inp.in%d.%c);", vdst, idx, ch), true
					}
				}
				return fmt.Sprintf("v[%d] = 0u;", vdst), true
			}
			pIdx++
		}
		return fmt.Sprintf("v[%d] = 0u;", vdst), true

	case EncMIMG:
		op := (w0 >> 18) & 0xFF
		vdata := (w1 >> 8) & 0xFF
		vaddr := w1 & 0xFF
		if isCS {
			if op >= 0x08 && op <= 0x0F && vdata < 64 { // image_store*
				return fmt.Sprintf("UAV0.Store(((dtid.x + dtid.y * 64u + dtid.z * 4096u) * 4u) & 0xFFFFCu, v[%d]);", vdata), true
			}
			if op <= 0x07 && vdata+3 < 64 { // image_load*
				return fmt.Sprintf("{ uint _m = UAV0.Load(((dtid.x + dtid.y * 64u) * 4u) & 0xFFFFCu); v[%d]=_m; v[%d]=_m; v[%d]=_m; v[%d]=0x3F800000u; }", vdata, vdata+1, vdata+2, vdata+3), true
			}
			return "", false
		}
		if !isPS {
			return "", false
		}
		if vdata < 64 && vaddr+1 < 64 {
			dmask := (w0 >> 8) & 0xF
			if dmask == 0 {
				dmask = 0xF
			}
			chs := [4]byte{'x', 'y', 'z', 'w'}
			var sb strings.Builder
			sb.WriteString(fmt.Sprintf("{ float4 _t = Tex0.Sample(Samp0, float2(asfloat(v[%d]), asfloat(v[%d])));", vaddr, vaddr+1))
			dstReg := vdata
			for c := uint32(0); c < 4; c++ {
				if dmask&(1<<c) != 0 && dstReg < 64 {
					sb.WriteString(fmt.Sprintf(" v[%d]=asuint(_t.%c);", dstReg, chs[c]))
					dstReg++
				}
			}
			sb.WriteString(" }")
			return sb.String(), true
		}

	case EncEXP:
		en := w0 & 0xF
		tgt := (w0 >> 4) & 0x3F
		compr := (w0 >> 10) & 1
		v0 := w1 & 0xFF
		v1 := (w1 >> 8) & 0xFF
		v2 := (w1 >> 16) & 0xFF
		v3 := (w1 >> 24) & 0xFF
		if v0 >= 64 {
			v0 = 0
		}
		if v1 >= 64 {
			v1 = 0
		}
		if v2 >= 64 {
			v2 = 0
		}
		if v3 >= 64 {
			v3 = 0
		}
		var dstName string
		switch {
		case tgt < 8:
			dstName = fmt.Sprintf("exp_mrt[%d]", tgt%4)
		case tgt == 12:
			dstName = "exp_pos"
		case tgt >= 32 && tgt < 40:
			dstName = fmt.Sprintf("exp_param[%d]", tgt-32)
		default:
			return "", true
		}
		if compr != 0 {
			// Mode compressé FP16x2 : v0 contient (x,y), v1 contient (z,w)
			return fmt.Sprintf("%s = float4(f16tof32(v[%d] & 0xFFFFu), f16tof32(v[%d] >> 16), f16tof32(v[%d] & 0xFFFFu), f16tof32(v[%d] >> 16));",
				dstName, v0, v0, v1, v1), true
		}
		var parts []string
		vs := [4]uint32{v0, v1, v2, v3}
		chs := "xyzw"
		for i := 0; i < 4; i++ {
			if en&(1<<uint(i)) != 0 {
				parts = append(parts, fmt.Sprintf("%s.%c = asfloat(v[%d]);", dstName, chs[i], vs[i]))
			}
		}
		return strings.Join(parts, " "), true
	}
	return "", false
}

func cmpCond(sub uint32) string {
	switch sub {
	case 1:
		return "<"
	case 2:
		return "=="
	case 3:
		return "<="
	case 4:
		return ">"
	case 5:
		return "!="
	case 6:
		return ">="
	default:
		return ""
	}
}
