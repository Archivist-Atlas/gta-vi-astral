//go:build windows

package hle

import (
	"log/slog"
	"sync"
	"syscall"
	"unsafe"
)

// xgResourceDesc correspond à D3D12_RESOURCE_DESC (0x38 octets).
type xgResourceDesc struct {
	Dimension    uint32
	_pad0        uint32
	Alignment    uint64
	Width        uint64
	Height       uint32
	DepthOrArray uint16
	Mips         uint16
	Format       uint32
	Samples      uint32
	Quality      uint32
	Layout       uint32
	Flags        uint32
}

type xgMipInfo struct {
	Offset     uint64
	Size       uint64
	SliceSize  uint64
	PitchTexel uint32
	PitchBytes uint32
	Width      uint32
	Height     uint32
	Depth      uint32
}

type xgComputerState struct {
	desc      xgResourceDesc
	totalSize uint64
	alignment uint64
	bpe       uint32
	blockSize uint32
	mips      []xgMipInfo
	swizzle   uint32
}

func xgFormatMetrics(dxgiFmt uint32) (bpe uint32, block uint32) {
	switch dxgiFmt {
	case 70, 71, 72, 79, 80, 81: // BC1, BC4
		return 8, 4
	case 73, 74, 75, 76, 77, 78, 82, 83, 84, 94, 95, 96, 97, 98, 99: // BC2, BC3, BC5, BC6H, BC7
		return 16, 4
	case 1, 2, 3, 4: // R32G32B32A32
		return 16, 1
	case 5, 6, 7, 8: // R32G32B32
		return 12, 1
	case 9, 10, 11, 12, 13, 14, 15, 16, 17, 18, 19, 20, 44, 45: // R16G16B16A16, R32G32, D32_S8
		return 8, 1
	case 48, 49, 50, 51, 52, 53, 54, 55, 56, 57, 58, 59: // R8G8, R16, D16
		return 2, 1
	case 60, 61, 62, 63, 64, 65: // R8, A8
		return 1, 1
	default: // R8G8B8A8, B8G8R8A8, R10G10B10A2, R11G11B10, R32, D32, D24S8
		return 4, 1
	}
}

func xgSuggestSwizzle(desc xgResourceDesc) uint32 {
	const (
		tex1D     = 2
		allowRT   = 1
		allowDS   = 2
		swLinear  = 0
		sw4KBS    = 5
		sw64KBZX  = 24
		sw64KBRX  = 27
	)
	if desc.Dimension == tex1D {
		return swLinear
	}
	if desc.Flags&allowDS != 0 {
		return sw64KBZX
	}
	if desc.Flags&allowRT != 0 {
		return sw64KBRX
	}
	return sw4KBS
}

func xgBuildComputerState(desc xgResourceDesc) *xgComputerState {
	bpe, block := xgFormatMetrics(desc.Format)
	w := uint32(desc.Width)
	if w == 0 {
		w = 1
	}
	h := desc.Height
	if h == 0 {
		h = 1
	}
	depthOrArr := uint32(desc.DepthOrArray)
	if depthOrArr == 0 {
		depthOrArr = 1
	}
	mipCount := uint32(desc.Mips)
	if mipCount == 0 {
		m := w
		if h > m {
			m = h
		}
		mipCount = 1
		for m > 1 && mipCount < 15 {
			m >>= 1
			mipCount++
		}
	}
	if mipCount > 15 {
		mipCount = 15
	}
	swz := xgSuggestSwizzle(desc)
	if desc.Layout == 1 {
		swz = 0
	} else if desc.Layout&^uint32(0xFF) == 0x100 {
		swz = desc.Layout & 0xFF
	}

	align := uint64(4096)
	if swz >= 9 {
		align = 65536
	}
	if desc.Alignment >= 65536 {
		align = desc.Alignment
	}

	mips := make([]xgMipInfo, mipCount)
	var offset uint64
	is3D := desc.Dimension == 4
	for i := uint32(0); i < mipCount; i++ {
		mw := w >> i
		if mw == 0 {
			mw = 1
		}
		mh := h >> i
		if mh == 0 {
			mh = 1
		}
		md := uint32(1)
		if is3D {
			md = depthOrArr >> i
			if md == 0 {
				md = 1
			}
		}
		bw := (mw + block - 1) / block
		bh := (mh + block - 1) / block
		pitchBytes := (bw*bpe + 255) &^ 255
		if pitchBytes < 256 {
			pitchBytes = 256
		}
		pitchTexels := (pitchBytes / bpe) * block
		sliceSize := uint64(pitchBytes) * uint64(bh)
		mipSize := sliceSize * uint64(md)
		if !is3D {
			mipSize = sliceSize * uint64(depthOrArr)
		}
		mipSize = (mipSize + 255) &^ 255
		mips[i] = xgMipInfo{
			Offset:     offset,
			Size:       mipSize,
			SliceSize:  sliceSize,
			PitchTexel: pitchTexels,
			PitchBytes: pitchBytes,
			Width:      mw,
			Height:     mh,
			Depth:      md,
		}
		offset += mipSize
	}
	total := (offset + align - 1) &^ (align - 1)
	if total < align {
		total = align
	}
	return &xgComputerState{
		desc:      desc,
		totalSize: total,
		alignment: align,
		bpe:       bpe,
		blockSize: block,
		mips:      mips,
		swizzle:   swz,
	}
}

func bindXG(t *LiveTable) {
	var (
		mu        sync.Mutex
		computers = make(map[uintptr]*xgComputerState)
	)

	cbAddRef := syscall.NewCallback(func(this uintptr) uintptr { return 2 })
	cbRelease := syscall.NewCallback(func(this uintptr) uintptr { return 1 })

	// Slot 2 (0x10): GetResourceLayout(this, outLayout 0x1B40 bytes)
	// Offsets vérifiés dans VIctory xg.rs :
	// LayoutOut: +0x00 size(u64), +0x08 align(u64), +0x10 mips(u32), +0x14 planes(u32), +0x18 plane[0] (0x6C8)
	// PlaneOut:  +0x00 usage(u32), +0x08 size(u64), +0x10 slice_size(u64), +0x18 offset(u64), +0x20 align(u64),
	//            +0x28 element_bytes(u32), +0x2C standard_mips(u32), +0x30 tail_mips(u32), +0x38 mips[15] (0x70 each)
	// MipOut:    +0x00 size(u64), +0x08 offset(u64), +0x10 slice_size(u64), +0x18 pitch_texels(u32), +0x1C pitch_bytes(u32), +0x20 align(u32)
	cbGetLayout := syscall.NewCallback(func(this, outPtr uintptr) uintptr {
		if outPtr == 0 {
			return hresultInvalidArg
		}
		mu.Lock()
		st := computers[this]
		mu.Unlock()
		if st == nil {
			return hresultInvalidArg
		}
		buf := unsafe.Slice((*byte)(osPtr(outPtr)), 0x1B40)
		clear(buf)

		*(*uint64)(osPtr(outPtr + 0x00)) = st.totalSize
		*(*uint64)(osPtr(outPtr + 0x08)) = st.alignment
		*(*uint32)(osPtr(outPtr + 0x10)) = uint32(len(st.mips))
		*(*uint32)(osPtr(outPtr + 0x14)) = 1 // 1 plane

		p0 := outPtr + 0x18
		*(*uint32)(osPtr(p0 + 0x00)) = 1 // usage = data
		*(*uint64)(osPtr(p0 + 0x08)) = st.totalSize
		*(*uint64)(osPtr(p0 + 0x10)) = st.totalSize
		*(*uint64)(osPtr(p0 + 0x18)) = 0
		*(*uint64)(osPtr(p0 + 0x20)) = st.alignment
		*(*uint32)(osPtr(p0 + 0x28)) = st.bpe
		*(*uint32)(osPtr(p0 + 0x2C)) = uint32(len(st.mips))
		*(*uint32)(osPtr(p0 + 0x30)) = 0

		for i, m := range st.mips {
			mp := p0 + 0x38 + uintptr(i)*0x70
			*(*uint64)(osPtr(mp + 0x00)) = m.Size
			*(*uint64)(osPtr(mp + 0x08)) = m.Offset
			*(*uint64)(osPtr(mp + 0x10)) = m.SliceSize
			*(*uint32)(osPtr(mp + 0x18)) = m.PitchTexel
			*(*uint32)(osPtr(mp + 0x1C)) = m.PitchBytes
			*(*uint32)(osPtr(mp + 0x20)) = uint32(st.alignment)
			*(*uint32)(osPtr(mp + 0x24)) = m.Width
			*(*uint32)(osPtr(mp + 0x28)) = m.Height
			*(*uint32)(osPtr(mp + 0x2C)) = m.Depth
			*(*uint32)(osPtr(mp + 0x38)) = m.Width
			*(*uint32)(osPtr(mp + 0x3C)) = m.Height
			*(*uint32)(osPtr(mp + 0x40)) = m.Depth
			*(*uint32)(osPtr(mp + 0x44)) = 1
			*(*uint32)(osPtr(mp + 0x48)) = st.swizzle
		}
		*(*uint32)(osPtr(outPtr + 0x1B38)) = st.desc.Dimension
		return hresultOK
	})

	// Slot 8 (0x40): CopyIntoSubresource(this, tiled, plane, subres, linear, rowPitch, depthPitch)
	cbCopyInto := syscall.NewCallback(func(this, tiledPtr, plane, subres, linearPtr, rowPitch, depthPitch uintptr) uintptr {
		if tiledPtr == 0 || linearPtr == 0 {
			return hresultInvalidArg
		}
		mu.Lock()
		st := computers[this]
		mu.Unlock()
		if st == nil || len(st.mips) == 0 {
			return hresultOK
		}
		mipIdx := int(uint32(subres)) % len(st.mips)
		m := st.mips[mipIdx]
		rp := uint64(uint32(rowPitch))
		rows := uint64((m.Height + st.blockSize - 1) / st.blockSize)
		rowBytes := uint64((m.Width+st.blockSize-1)/st.blockSize) * uint64(st.bpe)
		if rp == 0 {
			rp = rowBytes
		}
		var copyBytes uint64
		if rows <= 1 {
			copyBytes = rowBytes
		} else {
			copyBytes = (rows-1)*rp + rowBytes
		}
		if copyBytes > m.Size {
			copyBytes = m.Size
		}
		if copyBytes > 0 && m.Offset+copyBytes <= st.totalSize {
			dst := unsafe.Slice((*byte)(osPtr(tiledPtr+uintptr(m.Offset))), copyBytes)
			src := unsafe.Slice((*byte)(osPtr(linearPtr)), copyBytes)
			copy(dst, src)
		}
		return hresultOK
	})

	// Slot 9 (0x48): CopyFromSubresource(this, linear, plane, subres, sample, tiled, rowPitch, depthPitch)
	cbCopyFrom := syscall.NewCallback(func(this, linearPtr, plane, subres, sample, tiledPtr, rowPitch, depthPitch uintptr) uintptr {
		if tiledPtr == 0 || linearPtr == 0 {
			return hresultInvalidArg
		}
		mu.Lock()
		st := computers[this]
		mu.Unlock()
		if st == nil || len(st.mips) == 0 {
			return hresultOK
		}
		mipIdx := int(uint32(subres)) % len(st.mips)
		m := st.mips[mipIdx]
		rp := uint64(uint32(rowPitch))
		rows := uint64((m.Height + st.blockSize - 1) / st.blockSize)
		rowBytes := uint64((m.Width+st.blockSize-1)/st.blockSize) * uint64(st.bpe)
		if rp == 0 {
			rp = rowBytes
		}
		var copyBytes uint64
		if rows <= 1 {
			copyBytes = rowBytes
		} else {
			copyBytes = (rows-1)*rp + rowBytes
		}
		if copyBytes > m.Size {
			copyBytes = m.Size
		}
		if copyBytes > 0 && m.Offset+copyBytes <= st.totalSize {
			dst := unsafe.Slice((*byte)(osPtr(linearPtr)), copyBytes)
			src := unsafe.Slice((*byte)(osPtr(tiledPtr+uintptr(m.Offset))), copyBytes)
			copy(dst, src)
		}
		return hresultOK
	})

	// Slot 10 (0x50): GetResourceTiling(this, tiles, packed, shape, count, first, outTilings)
	cbTiling := syscall.NewCallback(func(this, tilesPtr, packedPtr, shapePtr, countPtr, first, outTilings uintptr) uintptr {
		mu.Lock()
		st := computers[this]
		mu.Unlock()
		if st == nil {
			return hresultInvalidArg
		}
		// Dimensions standard d'une tuile 64 KiB D3D12 / RDNA2 selon (bpe, blockSize).
		var tileW, tileH, tileD uint32 = 128, 128, 1
		if st.blockSize == 4 {
			if st.bpe <= 8 { // BC1 / BC4 (8 B / bloc 4x4) -> 128x64 blocs = 512x256 texels
				tileW, tileH = 512, 256
			} else { // BC2 / BC3 / BC5 / BC6H / BC7 (16 B / bloc 4x4) -> 64x64 blocs = 256x256 texels
				tileW, tileH = 256, 256
			}
		} else {
			switch st.bpe {
			case 1:
				tileW, tileH = 256, 256
			case 2:
				tileW, tileH = 256, 128
			case 4:
				tileW, tileH = 128, 128
			case 8:
				tileW, tileH = 128, 64
			default: // 12 / 16 B
				tileW, tileH = 64, 64
			}
		}
		if st.desc.Dimension == 4 { // Texture3D : tuile 3D 64 KiB
			tileW, tileH, tileD = 32, 32, 16
		}

		// Séparation mips standards (>= 1 tuile) vs mips packés (< 1 tuile).
		numMips := len(st.mips)
		numStandard := 0
		for i := 0; i < numMips; i++ {
			m := st.mips[i]
			if m.Width < tileW || m.Height < tileH || m.Depth < tileD {
				break
			}
			numStandard++
		}
		numPacked := numMips - numStandard

		type subTiling struct {
			w, h, d, start uint32
		}
		subTilings := make([]subTiling, numMips)
		var tileIdx uint32
		for i := 0; i < numStandard; i++ {
			m := st.mips[i]
			wt := (m.Width + tileW - 1) / tileW
			ht := (m.Height + tileH - 1) / tileH
			dt := (m.Depth + tileD - 1) / tileD
			if wt == 0 {
				wt = 1
			}
			if ht == 0 {
				ht = 1
			}
			if dt == 0 {
				dt = 1
			}
			subTilings[i] = subTiling{w: wt, h: ht, d: dt, start: tileIdx}
			tileIdx += wt * ht * dt
		}
		var tilesForPacked, startPacked uint32
		if numPacked > 0 {
			tilesForPacked = 1
			startPacked = tileIdx
			tileIdx += tilesForPacked
			for i := numStandard; i < numMips; i++ {
				subTilings[i] = subTiling{w: 0, h: 0, d: 0, start: 0xFFFFFFFF} // D3D12_PACKED_TILE
			}
		}
		arraySlices := uint32(st.desc.DepthOrArray)
		if st.desc.Dimension == 4 || arraySlices == 0 {
			arraySlices = 1
		}
		totalTiles := tileIdx * arraySlices
		if totalTiles == 0 {
			totalTiles = 1
		}

		if tilesPtr != 0 {
			*(*uint32)(osPtr(tilesPtr)) = totalTiles
		}
		if packedPtr != 0 {
			*(*uint8)(osPtr(packedPtr + 0)) = uint8(numStandard)
			*(*uint8)(osPtr(packedPtr + 1)) = uint8(numPacked)
			*(*uint32)(osPtr(packedPtr + 4)) = tilesForPacked
			*(*uint32)(osPtr(packedPtr + 8)) = startPacked
		}
		if shapePtr != 0 {
			*(*uint32)(osPtr(shapePtr + 0)) = tileW
			*(*uint32)(osPtr(shapePtr + 4)) = tileH
			*(*uint32)(osPtr(shapePtr + 8)) = tileD
		}
		if countPtr != 0 {
			avail := *(*uint32)(osPtr(countPtr))
			firstIdx := uint32(first)
			totalSubs := uint32(numMips) * arraySlices
			var n uint32
			if firstIdx < totalSubs {
				n = totalSubs - firstIdx
				if avail < n {
					n = avail
				}
			}
			*(*uint32)(osPtr(countPtr)) = n
			if outTilings != 0 && numMips > 0 {
				for i := uint32(0); i < n; i++ {
					subIdx := firstIdx + i
					sliceIdx := subIdx / uint32(numMips)
					mipIdx := subIdx % uint32(numMips)
					stSub := subTilings[mipIdx]
					start := stSub.start
					if start != 0xFFFFFFFF {
						start += sliceIdx * tileIdx
					}
					p := outTilings + uintptr(i)*12
					*(*uint32)(osPtr(p + 0)) = stSub.w
					*(*uint16)(osPtr(p + 4)) = uint16(stSub.h)
					*(*uint16)(osPtr(p + 6)) = uint16(stSub.d)
					*(*uint32)(osPtr(p + 8)) = start
				}
			}
		}
		return hresultOK
	})

	// Vtable partagée pour tous les IXGTextureAddressComputer
	proto := NewCOMObject("IXGTextureAddressComputer", 16, map[int]uintptr{
		0:  cbAddRef,
		1:  cbRelease,
		2:  cbGetLayout,
		8:  cbCopyInto,
		9:  cbCopyFrom,
		10: cbTiling,
	})
	sharedVtable := *(*uintptr)(osPtr(proto))

	cbSuggestSwizzle := syscall.NewCallback(func(descPtr, flags, outPtr uintptr) uintptr {
		if descPtr == 0 || outPtr == 0 {
			return hresultInvalidArg
		}
		desc := *(*xgResourceDesc)(osPtr(descPtr))
		*(*uint32)(osPtr(outPtr)) = 0x100 | xgSuggestSwizzle(desc)
		return hresultOK
	})

	cbCreateComputer := syscall.NewCallback(func(descPtr, outPtr uintptr) uintptr {
		if descPtr == 0 || outPtr == 0 {
			return hresultInvalidArg
		}
		desc := *(*xgResourceDesc)(osPtr(descPtr))
		st := xgBuildComputerState(desc)
		mem, _, _ := k32VirtualAlloc.Call(0, 64, 0x3000, 0x04)
		if mem == 0 {
			return 0x8007000E // E_OUTOFMEMORY
		}
		*(*uintptr)(osPtr(mem)) = sharedVtable
		mu.Lock()
		computers[mem] = st
		mu.Unlock()
		*(*uintptr)(osPtr(outPtr)) = mem
		slog.Debug("xg_xs!XGCreateTextureComputer", "w", desc.Width, "h", desc.Height, "fmt", desc.Format, "size", st.totalSize)
		return hresultOK
	})

	t.bind("xg_xs.dll", "XGSuggestSwizzleMode", cbSuggestSwizzle)
	t.bind("xg_xs.dll", "XGCreateTextureComputer", cbCreateComputer)
}
