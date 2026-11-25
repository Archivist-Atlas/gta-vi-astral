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
