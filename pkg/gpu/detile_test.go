package gpu

import "testing"

func TestDetileRDNA2LinearAnd4KBS(t *testing.T) {
	// 1. Linéaire (swizzle=0) : identité
	src := make([]byte, 16*16*4)
	for i := range src {
		src[i] = byte(i)
	}
	out := DetileRDNA2(src, 16, 16, 4, 1, 0)
	if len(out) != len(src) {
		t.Fatalf("taille inattendue : %d != %d", len(out), len(src))
	}
	for i := range src {
		if out[i] != src[i] {
			t.Fatalf("différence linéaire à l'octet %d", i)
		}
	}

	// 2. Tuilé 4 KiB R8_UNORM (256x128, swizzle=0x105)
	tiled := make([]byte, 256*128)
	// Place une sentinelle à (lx=1, ly=0) dans la tuile 0 -> morton2D(1,0) = 1
	tiled[1] = 0xAB
	detiled := DetileRDNA2(tiled, 256, 128, 1, 1, 0x105)
	if len(detiled) != 256*128 {
		t.Fatalf("taille détuilée invalide : %d", len(detiled))
	}
	if detiled[1] != 0xAB {
		t.Fatalf("sentinelle (1,0) attendue 0xAB, reçu 0x%02X", detiled[1])
	}
}

func TestMip0OffsetRDNA2(t *testing.T) {
	// 2048x1024 BC7 (16B par bloc 4x4), 12 mips, SW_4KB_S (0x105) :
	// tail(4K) + mip5(4K) + mip4(8K) + mip3(32K) + mip2(128K) + mip1(512K) = 0xAC000
	// + mip0(2 MiB = 0x200000) = 0x2AC000 exactement (vérifié sur les textures de chargement GTA VI).
	off := Mip0OffsetRDNA2(2048, 1024, 12, 16, 4, 0x105)
	if off != 0xAC000 {
		t.Fatalf("Mip0OffsetRDNA2(2048x1024 BC7 12mips SW_4KB_S) = 0x%X, attendu 0xAC000", off)
	}
	mip0Sz := TiledSizeBytes(2048, 1024, 16, 4, 0x105)
	if off+mip0Sz != 0x2AC000 {
		t.Fatalf("total = 0x%X, attendu 0x2AC000", off+mip0Sz)
	}
	if Mip0OffsetRDNA2(2048, 1024, 1, 16, 4, 0x105) != 0 {
		t.Fatalf("mipLevels=1 doit retourner 0")
	}
	if Mip0OffsetRDNA2(2048, 1024, 12, 16, 4, 0x100) != 0 {
		t.Fatalf("linéaire doit retourner 0")
	}
}
