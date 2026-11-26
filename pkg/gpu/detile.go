package gpu

// DXGIFormatMetrics retourne (octets par élément/bloc, taille de bloc en texels, format D3D12 hôte sûr)
// pour les formats textures Xbox Series / PC courants.
func DXGIFormatMetrics(dxgiFmt uint32) (bpe uint32, block uint32, hostFmt uint32) {
	switch dxgiFmt {
	case 1, 2, 3, 4: // R32G32B32A32_* (dégradés UI Scaleform 256x1 RGBA32F)
		return 16, 1, 2 // DXGI_FORMAT_R32G32B32A32_FLOAT
	case 15, 16, 17, 18: // R32G32_*
		return 8, 1, 16 // DXGI_FORMAT_R32G32_FLOAT
	case 33, 34, 35, 36, 37, 38: // R16G16_*
		return 4, 1, 34 // DXGI_FORMAT_R16G16_FLOAT
	case 39, 40, 41, 42, 43: // R32_* / D32_FLOAT
		return 4, 1, 41 // DXGI_FORMAT_R32_FLOAT
	case 60, 61, 62, 63, 64, 65: // R8_* / A8
		return 1, 1, 61 // DXGI_FORMAT_R8_UNORM
	case 48, 49, 50, 51, 52: // R8G8_*
		return 2, 1, 49 // DXGI_FORMAT_R8G8_UNORM
	case 53, 54: // R16_TYPELESS / R16_FLOAT
		return 2, 1, 54 // DXGI_FORMAT_R16_FLOAT
	case 55, 56: // D16_UNORM / R16_UNORM
		return 2, 1, 56 // DXGI_FORMAT_R16_UNORM
	case 57: // R16_UINT
		return 2, 1, 57
	case 58: // R16_SNORM
		return 2, 1, 58
	case 59: // R16_SINT
		return 2, 1, 59
	case 27, 28, 29, 30, 31, 32: // R8G8B8A8_*
		if dxgiFmt == 29 {
			return 4, 1, 29
		}
		return 4, 1, 28 // DXGI_FORMAT_R8G8B8A8_UNORM
	case 87, 90, 91: // B8G8R8A8_*
		if dxgiFmt == 91 {
			return 4, 1, 91
		}
		return 4, 1, 87 // DXGI_FORMAT_B8G8R8A8_UNORM
	case 9, 10, 11, 12, 13, 14: // R16G16B16A16_*
		return 8, 1, 10 // DXGI_FORMAT_R16G16B16A16_FLOAT
	case 70, 71, 72: // BC1
		return 8, 4, 71
	case 73, 74, 75: // BC2
		return 16, 4, 74
	case 76, 77, 78: // BC3
		return 16, 4, 77
	case 79, 80, 81: // BC4
		return 8, 4, 80
	case 82, 83, 84: // BC5
		return 16, 4, 83
	case 94, 95, 96: // BC6H
		return 16, 4, 95
	case 97, 98, 99: // BC7
		return 16, 4, 98
	default:
		return 4, 1, 28
	}
}

// microTilePatterns reprend les équations micro-tuile 256 octets GFX10/RDNA2 (addrlib RBPLUS)
// indexées par [kind: 0=SW_S, 1=SW_D][log2(bpe): 0..4 (1B..16B)].
// Chaque paire de caractères donne l'axe et le bit de coordonnée pour les bits d'adresse 0..7.
var microTilePatterns = [2][5]string{
	{ // Thin Standard Swizzle (SW_*_S : 1=256B_S, 5=4KB_S, 9=64KB_S, 21=4KB_S_X, 25=64KB_S_X)
		"x0x1x2x3y0y1y2y3", // 1 BPP
		"--x0x1x2y0y1y2x3", // 2 BPP
		"----x0x1y0y1y2x2", // 4 BPP (RGBA8)
		"------x0y0y1x1x2", // 8 BPP (BC1/BC4)
		"--------y0y1x0x1", // 16 BPP (BC2/BC3/BC5/BC6H/BC7)
	},
	{ // Thin Display Swizzle (SW_*_D : 2=256B_D, 6=4KB_D, 10=64KB_D, 22=4KB_D_X, 26=64KB_D_X)
		"x0x1x2y1y0y2x3y3", // 1 BPP
		"--x0x1x2y0y1y2x3", // 2 BPP
		"----x0x1y0y1x2y2", // 4 BPP
		"------x0y0x1x2y1", // 8 BPP
		"--------x0y0x1y1", // 16 BPP
	},
}

func rdna2SwizzleInfo(swz uint32) (kind int, blockLog2 uint32, tiled bool) {
	switch swz & 0xFF {
	case 0:
		return 0, 0, false // LINEAR
	case 1:
		return 0, 8, true // SW_256B_S
	case 2, 3:
		return 1, 8, true // SW_256B_D / R
	case 5, 21:
		return 0, 12, true // SW_4KB_S / SW_4KB_S_X
	case 6, 7, 22, 23:
		return 1, 12, true // SW_4KB_D / R
	case 9, 25:
		return 0, 16, true // SW_64KB_S / SW_64KB_S_X
	case 10, 11, 26, 27:
		return 1, 16, true // SW_64KB_D / R
	default:
		if swz&0xFF >= 9 {
			return 0, 16, true
		}
		return 0, 12, true
	}
}

func buildAxisLUT(kind int, bpeLog2, blockLog2 uint32) (xLUT, yLUT []uint32, tileW, tileH uint32) {
	pat := microTilePatterns[kind][bpeLog2]
	var xBits, yBits []uint32
	for bit := uint32(0); bit < 8; bit++ {
		ax := pat[2*bit]
		if ax != 'x' && ax != 'y' {
			continue
		}
		idx := int(pat[2*bit+1] - '0')
		if ax == 'x' {
			for len(xBits) <= idx {
				xBits = append(xBits, 0)
			}
			xBits[idx] = bit
		} else {
			for len(yBits) <= idx {
				yBits = append(yBits, 0)
			}
			yBits[idx] = bit
		}
	}
	// Au-delà des 256 octets de micro-tuile, GFX10 alterne Y puis X jusqu'à blockLog2
	for bit := uint32(8); bit < blockLog2; bit++ {
		if (bit-8)%2 == 0 {
			yBits = append(yBits, bit)
		} else {
			xBits = append(xBits, bit)
		}
	}
	tileW = 1 << len(xBits)
	tileH = 1 << len(yBits)
	xLUT = make([]uint32, tileW)
	for c := uint32(0); c < tileW; c++ {
		var off uint32
		for i, to := range xBits {
			if (c>>uint(i))&1 != 0 {
				off |= 1 << to
			}
		}
		xLUT[c] = off
	}
	yLUT = make([]uint32, tileH)
	for c := uint32(0); c < tileH; c++ {
		var off uint32
		for i, to := range yBits {
			if (c>>uint(i))&1 != 0 {
				off |= 1 << to
			}
		}
		yLUT[c] = off
	}
	return xLUT, yLUT, tileW, tileH
}

// TiledSizeBytes retourne la taille minimale en octets de la surface en mémoire invité
// en tenant compte de l'alignement de tuile RDNA2 (ou de l'alignement 256 o en linéaire).
func TiledSizeBytes(width, height, bpe, block, swizzle uint32) uint64 {
	if width == 0 || height == 0 || bpe == 0 || block == 0 {
		return 0
	}
	bw := (width + block - 1) / block
	bh := (height + block - 1) / block
	linearRow := uint64(bw) * uint64(bpe)
	kind, blockLog2, tiled := rdna2SwizzleInfo(swizzle)
	if !tiled {
		if bh > 1 {
			linearRow = (linearRow + 255) &^ 255
		}
		return linearRow * uint64(bh)
	}
	var bpeLog2 uint32
	for (uint32(1) << bpeLog2) < bpe && bpeLog2 < 4 {
		bpeLog2++
	}
	_, _, tileW, tileH := buildAxisLUT(kind, bpeLog2, blockLog2)
	tilesX := uint64((bw + tileW - 1) / tileW)
	tilesY := uint64((bh + tileH - 1) / tileH)
	return tilesX * tilesY * (uint64(1) << blockLog2)
}

// DetileRDNA2 convertit une texture 2D Xbox Series (linéaire ou tuilée RDNA2 SW_256B/4KB/64KB S/D)
// en disposition linéaire rangée par rangée prête pour CopyTextureRegion sur le GPU hôte.
func DetileRDNA2(src []byte, width, height, bpe, block, swizzle uint32) []byte {
	if width == 0 || height == 0 || bpe == 0 || block == 0 {
		return nil
	}
	bw := (width + block - 1) / block
	bh := (height + block - 1) / block
	linearRow := bw * bpe
	totalBytes := uint64(linearRow) * uint64(bh)
	if totalBytes == 0 || totalBytes > (64<<20) {
		return nil
	}
	dst := make([]byte, totalBytes)

	kind, blockLog2, tiled := rdna2SwizzleInfo(swizzle)
	if !tiled {
		alignedRow := (linearRow + 255) &^ 255
		if bh > 1 && alignedRow > linearRow && uint64(len(src)) >= uint64(alignedRow)*uint64(bh) {
			for y := uint32(0); y < bh; y++ {
				sOff := uint64(y) * uint64(alignedRow)
				dOff := uint64(y) * uint64(linearRow)
				copy(dst[dOff:dOff+uint64(linearRow)], src[sOff:sOff+uint64(linearRow)])
			}
			return dst
		}
		copy(dst, src)
		return dst
	}
	var bpeLog2 uint32
	for (uint32(1) << bpeLog2) < bpe && bpeLog2 < 4 {
		bpeLog2++
	}
	xLUT, yLUT, tileW, tileH := buildAxisLUT(kind, bpeLog2, blockLog2)
	tileBytes := uint64(1) << blockLog2
	tilesX := (bw + tileW - 1) / tileW
	tileMaskX := tileW - 1
	tileMaskY := tileH - 1

	for by := uint32(0); by < bh; by++ {
		ty := by / tileH
		yOff := uint64(yLUT[by&tileMaskY])
		dstRowOff := uint64(by) * uint64(linearRow)
		for bx := uint32(0); bx < bw; bx++ {
			tx := bx / tileW
			tileIdx := uint64(ty)*uint64(tilesX) + uint64(tx)
			srcOff := tileIdx*tileBytes + yOff + uint64(xLUT[bx&tileMaskX])
			dstOff := dstRowOff + uint64(bx)*uint64(bpe)
			if srcOff+uint64(bpe) <= uint64(len(src)) && dstOff+uint64(bpe) <= totalBytes {
				copy(dst[dstOff:dstOff+uint64(bpe)], src[srcOff:srcOff+uint64(bpe)])
			}
		}
	}
	return dst
}
