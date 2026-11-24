package formats

import (
	"errors"
	"fmt"
)

// Swizzle identifie un mode de tuilage RDNA2 / GFX10.3 (ordre AMD addrlib ADDR_SW_*).
// Basé sur victory-formats::texture (AMD addrlib MIT) et les relevés Leonida.
type Swizzle uint32

const (
	SwizzleLinear    Swizzle = 0
	Swizzle256B_S    Swizzle = 1
	Swizzle256B_D    Swizzle = 2
	Swizzle4KB_S     Swizzle = 5
	Swizzle4KB_D     Swizzle = 6
	Swizzle64KB_S    Swizzle = 9
	Swizzle64KB_D    Swizzle = 10
	Swizzle64KB_S_T  Swizzle = 17
	Swizzle64KB_D_T  Swizzle = 18
	Swizzle4KB_S_X   Swizzle = 21
	Swizzle4KB_D_X   Swizzle = 22
	Swizzle4KB_R_X   Swizzle = 23
	Swizzle64KB_Z_X  Swizzle = 24
	Swizzle64KB_S_X  Swizzle = 25
	Swizzle64KB_D_X  Swizzle = 26
	Swizzle64KB_R_X  Swizzle = 27
)

// BlockLog2 retourne log2 de la taille de bloc en octets (0 pour linéaire).
func (s Swizzle) BlockLog2() (uint32, bool) {
	switch s {
	case SwizzleLinear:
		return 0, true
	case Swizzle256B_S, Swizzle256B_D:
		return 8, true
	case Swizzle4KB_S, Swizzle4KB_D, Swizzle4KB_S_X, Swizzle4KB_D_X, Swizzle4KB_R_X:
		return 12, true
	case Swizzle64KB_S, Swizzle64KB_D, Swizzle64KB_S_T, Swizzle64KB_D_T,
		Swizzle64KB_Z_X, Swizzle64KB_S_X, Swizzle64KB_D_X, Swizzle64KB_R_X:
		return 16, true
	default:
		return 0, false
	}
}

// IsXOR indique si le mode utilise un XOR de pipe/bank (_T ou _X).
func (s Swizzle) IsXOR() bool { return s >= 16 }

func (s Swizzle) String() string {
	switch s {
	case SwizzleLinear:
		return "LINEAR"
	case Swizzle256B_S:
		return "SW_256B_S"
	case Swizzle256B_D:
		return "SW_256B_D"
	case Swizzle4KB_S:
		return "SW_4KB_S"
	case Swizzle4KB_D:
		return "SW_4KB_D"
	case Swizzle64KB_S:
		return "SW_64KB_S"
	case Swizzle64KB_D:
		return "SW_64KB_D"
	case Swizzle64KB_S_X:
		return "SW_64KB_S_X"
	case Swizzle64KB_D_X:
		return "SW_64KB_D_X"
	default:
		return fmt.Sprintf("SW_%d", uint32(s))
	}
}

// TextureDim représente la dimension d'une texture GPU.
type TextureDim uint8

const (
	Tex1D TextureDim = 1
	Tex2D TextureDim = 2
	Tex3D TextureDim = 3
)

// TextureDesc décrit un plan de texture console (unités en texels + taille de bloc BC).
type TextureDesc struct {
	Dimension    TextureDim
	Width        uint32
	Height       uint32
	Depth        uint32 // Profondeur (3D) ou taille de tableau (1D/2D)
	Mips         uint32
	ElementBytes uint32 // 1, 2, 4, 8 ou 16 octets par texel ou bloc 4x4
	Block        uint32 // 1 (non compressé) ou 4 (BC1..BC7)
	Samples      uint32 // 1, 2, 4, 8
	Swizzle      Swizzle
}

// SurfaceLayout décrit le résultat du calcul de taille et d'alignement GPU.
type SurfaceLayout struct {
	TotalBytes    uint64
	Alignment     uint32
	BlockWidth    uint32
	BlockHeight   uint32
	PitchElements uint32
}

var ErrInvalidTexture = errors.New("description de texture invalide")

// ComputeTextureLayout calcule l'alignement, les dimensions de bloc et la taille
// approximative de surface pour un mode de tuilage RDNA2 donné.
func ComputeTextureLayout(d TextureDesc) (SurfaceLayout, error) {
	if d.Width == 0 || d.Height == 0 || d.Depth == 0 || d.Mips == 0 {
		return SurfaceLayout{}, ErrInvalidTexture
	}
	if d.ElementBytes != 1 && d.ElementBytes != 2 && d.ElementBytes != 4 && d.ElementBytes != 8 && d.ElementBytes != 16 {
		return SurfaceLayout{}, ErrInvalidTexture
	}
	if d.Block != 1 && d.Block != 4 {
		return SurfaceLayout{}, ErrInvalidTexture
	}
	blkLog2, ok := d.Swizzle.BlockLog2()
	if !ok {
		return SurfaceLayout{}, ErrInvalidTexture
	}

	elemW := (d.Width + d.Block - 1) / d.Block
	elemH := (d.Height + d.Block - 1) / d.Block

	if d.Swizzle == SwizzleLinear {
		// Alignement ligne 256 octets sur D3D12/RDNA2
		rowBytes := alignUp(uint64(elemW)*uint64(d.ElementBytes), 256)
		total := rowBytes * uint64(elemH) * uint64(d.Depth)
		return SurfaceLayout{
			TotalBytes:    total,
			Alignment:     256,
			BlockWidth:    1,
			BlockHeight:   1,
			PitchElements: uint32(rowBytes / uint64(d.ElementBytes)),
		}, nil
	}

	blockBytes := uint32(1) << blkLog2
	elemsPerBlock := blockBytes / d.ElementBytes
	bw, bh := squareBlockDims(elemsPerBlock)

	var sliceBytes uint64
	mw, mh := elemW, elemH
	for m := uint32(0); m < d.Mips; m++ {
		wAligned := alignUp(uint64(mw), uint64(bw))
		hAligned := alignUp(uint64(mh), uint64(bh))
		sliceBytes += wAligned * hAligned * uint64(d.ElementBytes)
		if mw > 1 {
			mw >>= 1
		}
		if mh > 1 {
			mh >>= 1
		}
	}
	sliceBytes = alignUp(sliceBytes, uint64(blockBytes))
	return SurfaceLayout{
		TotalBytes:    sliceBytes * uint64(d.Depth),
		Alignment:     blockBytes,
		BlockWidth:    bw,
		BlockHeight:   bh,
		PitchElements: uint32(alignUp(uint64(elemW), uint64(bw))),
	}, nil
}

func squareBlockDims(elems uint32) (uint32, uint32) {
	var log2 uint32
	for (uint32(1) << log2) < elems {
		log2++
	}
	wLog2 := (log2 + 1) / 2
	hLog2 := log2 - wLog2
	return 1 << wLog2, 1 << hLog2
}

func alignUp(v, align uint64) uint64 {
	if align == 0 {
		return v
	}
	return (v + align - 1) &^ (align - 1)
}
