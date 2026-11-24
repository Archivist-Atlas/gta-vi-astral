package shader

import (
	"encoding/binary"
	"testing"
)

func TestEmitSPIRVSkeletonValidHeader(t *testing.T) {
	mod := &SSAModule{
		Stage:  StagePS,
		Wave64: true,
	}
	spv := EmitSPIRVSkeleton(mod)
	if len(spv) < 20 || len(spv)%4 != 0 {
		t.Fatalf("taille SPIR-V invalide : %d octets", len(spv))
	}
	magic := binary.LittleEndian.Uint32(spv[0:4])
	ver := binary.LittleEndian.Uint32(spv[4:8])
	bound := binary.LittleEndian.Uint32(spv[12:16])
	if magic != SPIRVMagic {
		t.Fatalf("magic = 0x%08X, attendu 0x%08X", magic, SPIRVMagic)
	}
	if ver != SPIRVVersion1_2 {
		t.Fatalf("version = 0x%08X, attendu 0x%08X", ver, SPIRVVersion1_2)
	}
	if bound < 5 {
		t.Fatalf("bound trop faible : %d", bound)
	}
}
