package gpu

// DXGIFormatMetrics retourne (octets par élément/bloc, taille de bloc en texels, format D3D12 hôte sûr)
// pour les formats textures Xbox Series / PC courants.
func DXGIFormatMetrics(dxgiFmt uint32) (bpe uint32, block uint32, hostFmt uint32) {
	switch dxgiFmt {
	case 60, 61, 62, 63, 64, 65: // R8_* / A8
		return 1, 1, 61 // DXGI_FORMAT_R8_UNORM
	case 48, 49, 50, 51, 52, 53, 54, 55, 56, 57, 58, 59: // R8G8 / R16
		return 2, 1, 49 // DXGI_FORMAT_R8G8_UNORM
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

// morton2D entrelace les bits bas de (x, y) pour l'adressage micro-tuile Z-order RDNA2.
func morton2D(x, y uint32) uint32 {
	x = (x | (x << 8)) & 0x00FF00FF
	x = (x | (x << 4)) & 0x0F0F0F0F
	x = (x | (x << 2)) & 0x33333333
	x = (x | (x << 1)) & 0x55555555

	y = (y | (y << 8)) & 0x00FF00FF
	y = (y | (y << 4)) & 0x0F0F0F0F
	y = (y | (y << 2)) & 0x33333333
	y = (y | (y << 1)) & 0x55555555

	return x | (y << 1)
}

// DetileRDNA2 convertit une texture 2D Xbox Series (linéaire ou tuilée RDNA2 SW_4KB_S / SW_64KB_*)
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

	swz := swizzle & 0xFF
	// Mode linéaire (0) ou petite texture (< 16 blocs) : copie directe
	if swz == 0 || (bw <= 4 && bh <= 4) {
		copy(dst, src)
		return dst
	}

	// Dimensions d'une tuile en blocs selon le mode RDNA2 (4 KiB vs 64 KiB)
	tileBytes := uint32(4096)
	if swz >= 9 {
		tileBytes = 65536
	}
	elemsPerTile := tileBytes / bpe
	tileW := uint32(1)
	tileH := uint32(1)
	for tileW*tileH < elemsPerTile {
		if tileW <= tileH {
			tileW <<= 1
		} else {
			tileH <<= 1
		}
	}
	tilesX := (bw + tileW - 1) / tileW
	tileMaskX := tileW - 1
	tileMaskY := tileH - 1

	for by := uint32(0); by < bh; by++ {
		ty := by / tileH
		ly := by & tileMaskY
		dstRowOff := uint64(by) * uint64(linearRow)
		for bx := uint32(0); bx < bw; bx++ {
			tx := bx / tileW
			lx := bx & tileMaskX
			tileIdx := uint64(ty)*uint64(tilesX) + uint64(tx)
			localIdx := uint64(morton2D(lx, ly))
			srcOff := (tileIdx*uint64(elemsPerTile) + localIdx) * uint64(bpe)
			dstOff := dstRowOff + uint64(bx)*uint64(bpe)
			if srcOff+uint64(bpe) <= uint64(len(src)) && dstOff+uint64(bpe) <= totalBytes {
				copy(dst[dstOff:dstOff+uint64(bpe)], src[srcOff:srcOff+uint64(bpe)])
			}
		}
	}
	return dst
}
