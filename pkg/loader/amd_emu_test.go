package loader

import "testing"

func TestEmulateSSE4aInsertqExtrq(t *testing.T) {
	var xmm [16][2]uint64
	xmm[0][0] = 0x1111_2222_3333_4444
	xmm[1][0] = 0x0000_0000_0000_ABCD

	// insertq xmm0, xmm1, len=16, idx=16 -> F2 0F 78 C1 10 10
	n := emulateSSE4a([]byte{0xF2, 0x0F, 0x78, 0xC1, 0x10, 0x10}, &xmm)
	if n != 6 {
		t.Fatalf("insertq len=%d, want 6", n)
	}
	if xmm[0][0] != 0x1111_2222_ABCD_4444 {
		t.Fatalf("insertq xmm0=0x%X, want 0x11112222ABCD4444", xmm[0][0])
	}

	// extrq xmm0, len=8, idx=20 -> 66 0F 78 C0 08 14
	n = emulateSSE4a([]byte{0x66, 0x0F, 0x78, 0xC0, 0x08, 0x14}, &xmm)
	if n != 6 {
		t.Fatalf("extrq len=%d, want 6", n)
	}
	if xmm[0][0] != 0xBC {
		t.Fatalf("extrq xmm0=0x%X, want 0xBC", xmm[0][0])
	}

	// Exact crashing instruction from game_scarlett_master.exe+0x455BAC7:
	// F2 0F 78 C9 08 10 -> insertq xmm1, xmm1, len=8, idx=16
	xmm[1][0] = 0x0000_0000_0000_005A
	n = emulateSSE4a([]byte{0xF2, 0x0F, 0x78, 0xC9, 0x08, 0x10}, &xmm)
	if n != 6 || xmm[1][0] != 0x0000_0000_005A_005A {
		t.Fatalf("real insertq len=%d xmm1=0x%X, want len=6 xmm1=0x5A005A", n, xmm[1][0])
	}
}
