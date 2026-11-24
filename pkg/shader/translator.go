package shader

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// TargetIR sélectionne le format de sortie du traducteur hybride.
type TargetIR string

const (
	TargetDXIL  TargetIR = "dxil"  // Cible D3D12 SM 6.6 (approche VIctory)
	TargetSPIRV TargetIR = "spirv" // Cible Vulkan 1.2 SPIR-V (approche Leonida)
)

// TranslateOptions configure la traduction RDNA2 -> IR cible.
type TranslateOptions struct {
	Target              TargetIR
	Wave64Emulate       bool // Émulation wave64 sur GPU 32-wide (NVIDIA/Intel, Leonida)
	BaryFallback        bool // Fallback sans VK_KHR_fragment_shader_barycentric
	TexelOffsetFallback bool // Fallback sans VK_KHR_maintenance8
	MeshAsCompute       bool // Émulation mesh shaders en compute
	LoopLimit           uint32 // Garde-fou anti-TDR sur boucles (approche VIctory)
}

// IRBlock représente un bloc de base dans la représentation intermédiaire SSA.
type IRBlock struct {
	ID           int         `json:"id"`
	StartPC      uint32      `json:"start_pc"`
	EndPC        uint32      `json:"end_pc"`
	InstCount    int         `json:"inst_count"`
	EndsInBranch bool        `json:"ends_in_branch"`
	EndsInEndPgm bool        `json:"ends_in_endpgm"`
}

// TranslatedMetadata rassemble l'index de métadonnées nécessaire à la création de PSO
// (combiné de VIctory HostShader et Leonida index.json).
type TranslatedMetadata struct {
	Hash          string    `json:"hash"`
	Stage         string    `json:"stage"`
	Target        TargetIR  `json:"target"`
	Wave64        bool      `json:"wave64"`
	Wave64Emulate bool      `json:"wave64_emulate"`
	VGPRs         uint16    `json:"vgprs"`
	SGPRs         uint16    `json:"sgprs"`
	LDSBytes      uint32    `json:"lds_bytes"`
	InstCount     int       `json:"inst_count"`
	BlockCount    int       `json:"block_count"`
	RootParams    int       `json:"root_params"`
	UsedResources int       `json:"used_resources"`
	Blocks        []IRBlock `json:"blocks"`
}

// BuildCFG découpe une séquence d'instructions GFX10 en blocs de base pour la passe SSA/structuration.
func BuildCFG(insts []GFX10Inst) []IRBlock {
	if len(insts) == 0 {
		return nil
	}
	var blocks []IRBlock
	cur := IRBlock{ID: 0, StartPC: insts[0].PC}
	for idx, inst := range insts {
		cur.InstCount++
		cur.EndPC = inst.PC + uint32(inst.Size)
		if inst.IsBranch || inst.IsEndPgm || idx == len(insts)-1 {
			cur.EndsInBranch = inst.IsBranch
			cur.EndsInEndPgm = inst.IsEndPgm
			blocks = append(blocks, cur)
			if idx+1 < len(insts) {
				cur = IRBlock{ID: len(blocks), StartPC: insts[idx+1].PC}
			}
		}
	}
	return blocks
}

// AnalyzeAndPlan exécute les phases front-end du compilateur (décodage GFX10 -> CFG -> plan de traduction)
// et produit un descripteur JSON prêt à être mis en cache.
func AnalyzeAndPlan(prog NativeProgram, opts TranslateOptions) (TranslatedMetadata, []byte, error) {
	if opts.Target == "" {
		opts.Target = TargetDXIL
	}
	insts := DecodeGFX10(prog.Code)
	blocks := BuildCFG(insts)

	sum := sha256.Sum256(prog.Code)
	meta := TranslatedMetadata{
		Hash:          hex.EncodeToString(sum[:8]),
		Stage:         prog.Stage.String(),
		Target:        opts.Target,
		Wave64:        prog.Wave64,
		Wave64Emulate: prog.Wave64 && opts.Wave64Emulate,
		VGPRs:         prog.VGPRs,
		SGPRs:         prog.SGPRs,
		LDSBytes:      prog.LDSBytes,
		InstCount:     len(insts),
		BlockCount:    len(blocks),
		RootParams:    len(prog.Layout),
		UsedResources: len(prog.UsedResources),
		Blocks:        blocks,
	}
	raw, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return meta, nil, fmt.Errorf("sérialisation métadonnées shader : %w", err)
	}
	return meta, raw, nil
}
