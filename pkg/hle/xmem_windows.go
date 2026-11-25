//go:build windows

package hle

import (
	"log/slog"
	"sync"
	"syscall"
	"unsafe"
)

const (
	xmemPageSize   uintptr = 64 << 10 // 64 KiB par page physique console
	xmemPageFrames uintptr = 16       // PFN en unités de 4 KiB (64 KiB / 4 KiB = 16)

	secReserve            uintptr = 0x04000000
	memCommitFlag         uintptr = 0x00001000
	memReleaseFlag        uintptr = 0x00008000
	memPreservePlacehold  uintptr = 0x00000002
	memReplacePlacehold   uintptr = 0x00004000
	pageRW                uintptr = 0x04
	invalidHandleVal      uintptr = ^uintptr(0)
)

var (
	k32CreateFileMappingW = k32.NewProc("CreateFileMappingW")
	k32CloseHandle        = k32.NewProc("CloseHandle")
	kbaseMapViewOfFile3   = kbase.NewProc("MapViewOfFile3")
	kbaseUnmapViewOfFile2 = kbase.NewProc("UnmapViewOfFile2")
)

type physSlice struct {
	section uintptr
	index   uintptr
}

type physView struct {
	base  uintptr
	size  uintptr
	slice physSlice
}

type physAllocator struct {
	mu       sync.Mutex
	nextPage uintptr
	pages    map[uintptr]physSlice
	views    map[uintptr]physView
}

func newPhysAllocator() *physAllocator {
	return &physAllocator{
		nextPage: 0x100000, // 0 reste inutilisé, aligné sur 16 (PFN 4 KiB)
		pages:    make(map[uintptr]physSlice),
		views:    make(map[uintptr]physView),
	}
}

func (p *physAllocator) allocate(flags, countPtr, pagesPtr uintptr) uintptr {
	if countPtr == 0 || pagesPtr == 0 {
		return 0
	}
	n := *(*uintptr)(osPtr(countPtr))
	if n == 0 {
		return 0
	}
	totalBytes := uint64(n) * uint64(xmemPageSize)
	hSection, _, callErr := k32CreateFileMappingW.Call(
		invalidHandleVal,
		0,
		pageRW|secReserve,
		uintptr(totalBytes>>32),
		uintptr(totalBytes&0xFFFFFFFF),
		0,
	)
	if hSection == 0 {
		slog.Error("XMemAllocatePhysicalPages: CreateFileMappingW échoué", "pages", n, "bytes", totalBytes, "err", callErr)
		return 0
	}

	p.mu.Lock()
	first := p.nextPage
	p.nextPage += n * xmemPageFrames
	out := unsafe.Slice((*uintptr)(osPtr(pagesPtr)), n)
	for i := uintptr(0); i < n; i++ {
		id := first + i*xmemPageFrames
		p.pages[id] = physSlice{section: hSection, index: i}
		out[i] = id
	}
	p.mu.Unlock()

	slog.Info("XMemAllocatePhysicalPages", "pages", n, "MiB", totalBytes/(1024*1024))
	return 1 // TRUE
}

func (p *physAllocator) free(n, pagesPtr uintptr) uintptr {
	if pagesPtr == 0 || n == 0 {
		return 1
	}
	in := unsafe.Slice((*uintptr)(osPtr(pagesPtr)), n)
	p.mu.Lock()
	for _, id := range in {
		delete(p.pages, id)
	}
	p.mu.Unlock()
	return 1
}

func (p *physAllocator) unmapLocked(base, length uintptr) {
	if kbaseUnmapViewOfFile2.Find() != nil {
		return
	}
	hProc, _, _ := k32GetCurrProc.Call()
	end := base + length
	for vBase, v := range p.views {
		if vBase < end && vBase+v.size > base {
			_, _, _ = kbaseUnmapViewOfFile2.Call(hProc, vBase, memPreservePlacehold)
			delete(p.views, vBase)
		}
	}
}

func (p *physAllocator) mapRunLocked(base, length uintptr, first physSlice) bool {
	if kbaseMapViewOfFile3.Find() != nil {
		return false
	}
	hProc, _, _ := k32GetCurrProc.Call()
	// Scinde le placeholder autour de [base, base+length) (échoue sans gravité si déjà exact)
	_, _, _ = k32VirtualFree.Call(base, length, memReleaseFlag|memPreservePlacehold)

	offset := uint64(first.index) * uint64(xmemPageSize)
	view, _, callErr := kbaseMapViewOfFile3.Call(
		first.section,
		hProc,
		base,
		uintptr(offset),
		length,
		memReplacePlacehold,
		pageRW,
		0,
		0,
	)
	if view != base {
		slog.Warn("XMemMapPhysicalPages: MapViewOfFile3 échoué", "base", base, "len", length, "err", callErr)
		return false
	}
	committed, _, commitErr := k32VirtualAlloc.Call(base, length, memCommitFlag, pageRW)
	if committed == 0 {
		slog.Warn("XMemMapPhysicalPages: VirtualAlloc(COMMIT) échoué", "base", base, "len", length, "err", commitErr)
		_, _, _ = kbaseUnmapViewOfFile2.Call(hProc, view, memPreservePlacehold)
		return false
	}
	p.views[base] = physView{base: base, size: length, slice: first}
	return true
}

func (p *physAllocator) mapPages(address, n, pagesPtr uintptr) uintptr {
	if address == 0 || n == 0 {
		return address
	}
	p.mu.Lock()
	defer p.mu.Unlock()

	p.unmapLocked(address, n*xmemPageSize)
	if pagesPtr == 0 {
		return address
	}
	ids := unsafe.Slice((*uintptr)(osPtr(pagesPtr)), n)
	for i := uintptr(0); i < n; {
		first, ok := p.pages[ids[i]]
		if !ok {
			slog.Warn("XMemMapPhysicalPages: page inconnue", "page", ids[i])
			return 0
		}
		run := uintptr(1)
		for i+run < n {
			next, okN := p.pages[ids[i+run]]
			if !okN || next.section != first.section || next.index != first.index+run {
				break
			}
			run++
		}
		if !p.mapRunLocked(address+i*xmemPageSize, run*xmemPageSize, first) {
			return 0
		}
		i += run
	}
	return address
}

func (p *physAllocator) mapScatter(addressesPtr, n, pagesPtr uintptr) uintptr {
	if addressesPtr == 0 {
		return 0
	}
	addrs := unsafe.Slice((*uintptr)(osPtr(addressesPtr)), n)
	var ids []uintptr
	if pagesPtr != 0 {
		ids = unsafe.Slice((*uintptr)(osPtr(pagesPtr)), n)
	}
	for i := uintptr(0); i < n; i++ {
		var pArg uintptr
		if ids != nil && ids[i] != 0 {
			pArg = pagesPtr + i*unsafe.Sizeof(uintptr(0))
		}
		if p.mapPages(addrs[i], 1, pArg) == 0 {
			return 0
		}
	}
	return 1
}

var globalPhysAlloc *physAllocator

// xmemMapAlias aliase les pages physiques déjà mappées à [from, from+length) vers [to, to+length)
// (utilisé par ID3D12CommandQueueX::CopyPageMappingsBatchX pour le streaming de textures GTA VI).
// Si from == 0, la plage [to, to+length) est simplement démappée en préservant le placeholder.
func xmemMapAlias(to, from, length uint64) bool {
	if globalPhysAlloc == nil || to == 0 || length == 0 {
		return true
	}
	return globalPhysAlloc.mapAlias(uintptr(to), uintptr(from), uintptr(length))
}

func (p *physAllocator) findSliceLocked(addr uintptr) (physSlice, bool) {
	for vBase, v := range p.views {
		if addr >= vBase && addr < vBase+v.size {
			pageOff := (addr - vBase) / xmemPageSize
			return physSlice{section: v.slice.section, index: v.slice.index + pageOff}, true
		}
	}
	return physSlice{}, false
}

func (p *physAllocator) mapAlias(to, from, length uintptr) bool {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.unmapLocked(to, length)
	if from == 0 {
		return true
	}
	nPages := length / xmemPageSize
	for i := uintptr(0); i < nPages; {
		first, ok := p.findSliceLocked(from + i*xmemPageSize)
		if !ok {
			return false
		}
		run := uintptr(1)
		for i+run < nPages {
			next, okN := p.findSliceLocked(from + (i+run)*xmemPageSize)
			if !okN || next.section != first.section || next.index != first.index+run {
				break
			}
			run++
		}
		if !p.mapRunLocked(to+i*xmemPageSize, run*xmemPageSize, first) {
			return false
		}
		i += run
	}
	return true
}

// bindXMemPhysical enregistre les fonctions de pages physiques XMem dans LiveTable.
func bindXMemPhysical(t *LiveTable) {
	phys := newPhysAllocator()
	globalPhysAlloc = phys
	t.bind("xmem.dll", "XMemAllocatePhysicalPages", syscall.NewCallback(phys.allocate))
	t.bind("xmem.dll", "XMemFreePhysicalPages", syscall.NewCallback(phys.free))
	t.bind("xmem.dll", "XMemMapPhysicalPages", syscall.NewCallback(phys.mapPages))
	t.bind("xmem.dll", "XMemMapPhysicalPagesScatter", syscall.NewCallback(phys.mapScatter))
	t.bind("xmem.dll", "XMemTransferMemory", syscall.NewCallback(func(kind, size uintptr) uintptr { return 1 }))
}
