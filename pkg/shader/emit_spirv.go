package shader

import (
	"encoding/binary"
)

// Constantes binaires SPIR-V 1.2 (spécification Khronos).
const (
	SPIRVMagic      uint32 = 0x07230203
	SPIRVVersion1_2 uint32 = 0x00010200

	spvOpCapability    uint16 = 17
	spvOpMemoryModel   uint16 = 14
	spvOpEntryPoint    uint16 = 15
	spvOpExecutionMode uint16 = 16
	spvOpTypeVoid      uint16 = 19
	spvOpTypeFunction  uint16 = 33
	spvOpFunction      uint16 = 54
	spvOpFunctionEnd   uint16 = 56
	spvOpLabel         uint16 = 248
	spvOpReturn        uint16 = 253

	spvCapShader   uint32 = 1
	spvCapMeshEXT  uint32 = 5283
	spvAddrLogical uint32 = 0
	spvMemGLSL450  uint32 = 1

	spvExecVertex     uint32 = 0
	spvExecGeometry   uint32 = 3
	spvExecFragment   uint32 = 4
	spvExecGLCompute  uint32 = 5
	spvExecMeshEXT    uint32 = 5365
	spvModeOriginUpL  uint32 = 7 // OriginUpperLeft
	spvModeLocalSize  uint32 = 17
)

type spvBuilder struct {
	words  []uint32
	nextID uint32
}

func newSPVBuilder() *spvBuilder {
	return &spvBuilder{
		words:  make([]uint32, 5, 64), // réserve les 5 mots d'en-tête SPIR-V
		nextID: 1,
	}
}

func (b *spvBuilder) allocID() uint32 {
	id := b.nextID
	b.nextID++
	return id
}

func (b *spvBuilder) emit(op uint16, operands ...uint32) {
	wordCount := uint32(1 + len(operands))
	b.words = append(b.words, (wordCount<<16)|uint32(op))
	b.words = append(b.words, operands...)
}

func encodeSPVString(s string) []uint32 {
	raw := append([]byte(s), 0)
	for len(raw)%4 != 0 {
		raw = append(raw, 0)
	}
	out := make([]uint32, len(raw)/4)
	for i := range out {
		out[i] = binary.LittleEndian.Uint32(raw[i*4 : (i+1)*4])
	}
	return out
}

func (b *spvBuilder) bytes() []byte {
	b.words[0] = SPIRVMagic
	b.words[1] = SPIRVVersion1_2
	b.words[2] = 0x00415354 // générateur ASTRAL ("AST")
	b.words[3] = b.nextID   // Bound
	b.words[4] = 0          // Schema

	out := make([]byte, len(b.words)*4)
	for i, w := range b.words {
		binary.LittleEndian.PutUint32(out[i*4:(i+1)*4], w)
	}
	return out
}

// EmitSPIRVSkeleton génère un module binaire SPIR-V 1.2 structurellement valide
// à partir d'un SSAModule (en-tête, capabilities, point d'entrée selon l'étage RDNA2,
// mode d'exécution et squelette de fonction main).
func EmitSPIRVSkeleton(mod *SSAModule) []byte {
	b := newSPVBuilder()

	b.emit(spvOpCapability, spvCapShader)
	execModel := spvExecVertex
	switch mod.Stage {
	case StagePS:
		execModel = spvExecFragment
	case StageCS:
		execModel = spvExecGLCompute
	case StageGS:
		execModel = spvExecGeometry
	case StageMS:
		execModel = spvExecMeshEXT
		b.emit(spvOpCapability, spvCapMeshEXT)
	}

	b.emit(spvOpMemoryModel, spvAddrLogical, spvMemGLSL450)

	voidTy := b.allocID()
	fnTy := b.allocID()
	mainFn := b.allocID()
	entryLabel := b.allocID()

	epArgs := append([]uint32{execModel, mainFn}, encodeSPVString("main")...)
	b.emit(spvOpEntryPoint, epArgs...)

	switch mod.Stage {
	case StagePS:
		b.emit(spvOpExecutionMode, mainFn, spvModeOriginUpL)
	case StageCS:
		b.emit(spvOpExecutionMode, mainFn, spvModeLocalSize, 64, 1, 1)
	}

	b.emit(spvOpTypeVoid, voidTy)
	b.emit(spvOpTypeFunction, fnTy, voidTy)

	b.emit(spvOpFunction, voidTy, mainFn, 0, fnTy)
	b.emit(spvOpLabel, entryLabel)
	b.emit(spvOpReturn)
	b.emit(spvOpFunctionEnd)

	return b.bytes()
}
