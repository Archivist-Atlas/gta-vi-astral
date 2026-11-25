package loader

// clwbLen retourne la longueur d'une instruction CLWB (66 [REX] 0F AE /6), ou 0 sinon.
func clwbLen(code []byte) int {
	if len(code) < 4 || code[0] != 0x66 {
		return 0
	}
	at := 1
	if code[1]&0xF0 == 0x40 {
		at = 2
	}
	if len(code) < at+3 || code[at] != 0x0F || code[at+1] != 0xAE {
		return 0
	}
	modrm := code[at+2]
	mode := modrm >> 6
	rm := modrm & 7
	if mode == 3 || (modrm>>3)&7 != 6 {
		return 0
	}
	l := at + 3
	base := rm
	if rm == 4 {
		if len(code) <= l {
			return 0
		}
		base = code[l] & 7
		l++
	}
	switch mode {
	case 0:
		if rm == 5 || (rm == 4 && base == 5) {
			l += 4
		}
	case 1:
		l += 1
	case 2:
		l += 4
	}
	if l > len(code) {
		return 0
	}
	return l
}

// emulateSSE4a émule les instructions AMD SSE4a EXTRQ (66 0F 78/79) et INSERTQ (F2 0F 78/79)
// directement sur les 16 registres XMM sauvegardés (chaque entrée = [Low, High]) et retourne
// la longueur en octets de l'instruction, ou 0 si ce n'est pas une instruction SSE4a.
func emulateSSE4a(code []byte, xmm *[16][2]uint64) int {
	if len(code) < 4 {
		return 0
	}
	prefix := code[0]
	if prefix != 0x66 && prefix != 0xF2 {
		return 0
	}
	var rex byte
	at := 1
	if code[1]&0xF0 == 0x40 {
		rex = code[1]
		at = 2
	}
	if len(code) < at+3 || code[at] != 0x0F || (code[at+1] != 0x78 && code[at+1] != 0x79) {
		return 0
	}
	modrm := code[at+2]
	if modrm>>6 != 3 {
		return 0
	}
	dst := int(((modrm >> 3) & 7) | ((rex & 4) << 1))
	src := int((modrm & 7) | ((rex & 1) << 3))
	immediate := code[at+1] == 0x78
	var fLen, fIdx byte
	var instLen int
	if immediate {
		if len(code) < at+5 {
			return 0
		}
		fLen = code[at+3]
		fIdx = code[at+4]
		instLen = at + 5
	} else {
		var f uint64
		if prefix == 0x66 {
			f = xmm[src][0]
		} else {
			f = xmm[src][1]
		}
		fLen = byte(f)
		fIdx = byte(f >> 8)
		instLen = at + 3
	}
	n := uint32(fLen & 63)
	atBit := uint32(fIdx & 63)
	bits := n
	if bits == 0 {
		bits = 64
	}
	var mask uint64
	if bits == 64 {
		mask = ^uint64(0)
	} else {
		mask = (uint64(1) << bits) - 1
	}
	if prefix == 0x66 { // EXTRQ
		if immediate && (modrm>>3)&7 != 0 {
			return 0
		}
		target := dst
		if immediate {
			target = src
		}
		xmm[target][0] = (xmm[target][0] >> atBit) & mask
	} else { // INSERTQ
		val := (xmm[src][0] & mask) << atBit
		xmm[dst][0] = (xmm[dst][0] & ^(mask << atBit)) | val
	}
	return instLen
}
