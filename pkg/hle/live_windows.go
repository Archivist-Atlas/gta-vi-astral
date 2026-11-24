//go:build windows

package hle

import (
	"log/slog"
	"strings"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/Archivist-Atlas/gta-vi-astral/pkg/core"
	"github.com/Archivist-Atlas/gta-vi-astral/pkg/gpu"
)

// LiveTable mappe les symboles "dll!func" vers de vrais pointeurs de fonctions natives
// (callbacks Go via syscall.NewCallback) injectés dans l'IAT du jeu.
type LiveTable struct {
	funcs map[string]uintptr
}

var (
	k32              = syscall.NewLazyDLL("kernel32.dll")
	kbase            = syscall.NewLazyDLL("kernelbase.dll")
	k32VirtualAlloc  = k32.NewProc("VirtualAlloc")
	k32VirtualFree   = k32.NewProc("VirtualFree")
	k32VirtualQuery  = k32.NewProc("VirtualQuery")
	k32HeapAlloc     = k32.NewProc("HeapAlloc")
	k32HeapFree      = k32.NewProc("HeapFree")
	k32GetProcHeap   = k32.NewProc("GetProcessHeap")
	k32GetCurrProc   = k32.NewProc("GetCurrentProcess")
	k32LoadLibA      = k32.NewProc("LoadLibraryA")
	k32LoadLibW      = k32.NewProc("LoadLibraryW")
	k32LoadLibExA    = k32.NewProc("LoadLibraryExA")
	k32LoadLibExW    = k32.NewProc("LoadLibraryExW")
	k32GetModHandleA = k32.NewProc("GetModuleHandleA")
	k32GetModHandleW  = k32.NewProc("GetModuleHandleW")
	k32GetProcAddr    = k32.NewProc("GetProcAddress")
	k32CreateFileW          = k32.NewProc("CreateFileW")
	k32CreateFileA          = k32.NewProc("CreateFileA")
	k32CreateFile2          = kbase.NewProc("CreateFile2")
	k32GetFileAttrW         = k32.NewProc("GetFileAttributesW")
	k32GetFileAttrA         = k32.NewProc("GetFileAttributesA")
	k32GetFileAttrExW       = k32.NewProc("GetFileAttributesExW")
	k32FindFirstFileW       = k32.NewProc("FindFirstFileW")
	k32FindFirstFileExW     = k32.NewProc("FindFirstFileExW")
	k32CreateDirW           = k32.NewProc("CreateDirectoryW")
	k32DeleteFileW          = k32.NewProc("DeleteFileW")
	k32GetProcessMemoryInfo = k32.NewProc("K32GetProcessMemoryInfo")
	kbaseVirtAlloc2         = kbase.NewProc("VirtualAlloc2")
	d3d12Host        = syscall.NewLazyDLL("d3d12.dll")
	d3d12SerRoot     = d3d12Host.NewProc("D3D12SerializeRootSignature")
	d3d12SerVerRoot  = d3d12Host.NewProc("D3D12SerializeVersionedRootSignature")

	// IID_ID3D12Device : {189819f1-1db6-4b57-be54-1821339b85f7} (import de données)
	iidID3D12Device = [16]byte{
		0xf1, 0x19, 0x98, 0x18, 0xb6, 0x1d, 0x57, 0x4b,
		0xbe, 0x54, 0x18, 0x21, 0x33, 0x9b, 0x85, 0xf7,
	}
)

var (
	mainImageBase  atomic.Uintptr
	xmemHooksAddr  atomic.Uintptr
	xmemHooksInit  atomic.Bool
	xmemHooksCell  uintptr // cellule OS fixe (non déplacée par le GC) contenant __XMemAllocationHooks__
)

func ensureXMemHooksCell() uintptr {
	if xmemHooksCell == 0 {
		mem, _, _ := k32VirtualAlloc.Call(0, 4096, 0x3000, 0x04)
		xmemHooksCell = mem
	}
	return xmemHooksCell
}

// SetMainImageBase enregistre l'adresse de base de l'image principale mappée
// afin de résoudre les exports de hooks du titre (ex. __XMemAllocationHooks__).
func SetMainImageBase(base uintptr) {
	mainImageBase.Store(base)
	var addr uintptr
	if base != 0 {
		name := append([]byte("__XMemAllocationHooks__"), 0)
		addr, _, _ = k32GetProcAddr.Call(base, uintptr(unsafe.Pointer(&name[0])))
		slog.Info("xmem: __XMemAllocationHooks__", "found", addr != 0)
	}
	xmemHooksAddr.Store(addr)
	xmemHooksInit.Store(true)
	if cell := ensureXMemHooksCell(); cell != 0 {
		*(*uintptr)(osPtr(cell)) = addr
	}
}

// makeXMemTrampoline génère un trampoline x86-64 natif (tail-call pur) pour XMemAlloc / XMemFree.
// Évite toute ré-entrance Go->C->Go sur un thread étranger (qui déclenche exitsyscall).
func makeXMemTrampoline(slotOffset byte, defaultCb uintptr) uintptr {
	cell := ensureXMemHooksCell()
	if cell == 0 {
		return defaultCb
	}
	code, _, _ := k32VirtualAlloc.Call(0, 4096, 0x3000, 0x40 /* PAGE_EXECUTE_READWRITE */)
	if code == 0 {
		return defaultCb
	}
	b := unsafe.Slice((*byte)(osPtr(code)), 64)
	i := 0
	// mov rax, imm64(cell)
	b[i], b[i+1] = 0x48, 0xB8
	*(*uint64)(unsafe.Pointer(&b[i+2])) = uint64(cell)
	i += 10
	// mov rax, [rax]
	b[i], b[i+1], b[i+2] = 0x48, 0x8B, 0x00
	i += 3
	// test rax, rax
	b[i], b[i+1], b[i+2] = 0x48, 0x85, 0xC0
	i += 3
	// jz +9 (saute mov rax,[rax+off] (4) + test (3) + jz (2) + jmp rax (2) = +11)
	b[i], b[i+1] = 0x74, 11
	i += 2
	// mov rax, [rax + slotOffset]
	b[i], b[i+1], b[i+2], b[i+3] = 0x48, 0x8B, 0x40, slotOffset
	i += 4
	// test rax, rax
	b[i], b[i+1], b[i+2] = 0x48, 0x85, 0xC0
	i += 3
	// jz +2 (saute jmp rax)
	b[i], b[i+1] = 0x74, 2
	i += 2
	// jmp rax
	b[i], b[i+1] = 0xFF, 0xE0
	i += 2
	// fallback: mov rax, imm64(defaultCb); jmp rax
	b[i], b[i+1] = 0x48, 0xB8
	*(*uint64)(unsafe.Pointer(&b[i+2])) = uint64(defaultCb)
	i += 10
	b[i], b[i+1] = 0xFF, 0xE0
	return code
}

func resolveXMemHooks() [2]uintptr {
	if !xmemHooksInit.Load() {
		base := mainImageBase.Load()
		if base != 0 {
			name := append([]byte("__XMemAllocationHooks__"), 0)
			addr, _, _ := k32GetProcAddr.Call(base, uintptr(unsafe.Pointer(&name[0])))
			xmemHooksAddr.Store(addr)
			xmemHooksInit.Store(true)
			slog.Info("xmem: __XMemAllocationHooks__", "found", addr != 0)
		}
	}
	addr := xmemHooksAddr.Load()
	if addr == 0 {
		return [2]uintptr{}
	}
	return *(*[2]uintptr)(osPtr(addr))
}

func xmemAllocDefault(size, attrs uintptr) uintptr {
	if size == 0 {
		size = 16
	}
	alignShift := (uint64(attrs) >> 24) & 0x1F
	if alignShift >= 12 || size >= 65536 {
		r, _, _ := k32VirtualAlloc.Call(0, size, 0x3000 /* COMMIT|RESERVE */, 0x04 /* PAGE_READWRITE */)
		return r
	}
	heap, _, _ := k32GetProcHeap.Call()
	r, _, _ := k32HeapAlloc.Call(heap, 0x00000008 /* HEAP_ZERO_MEMORY */, size)
	return r
}

func xmemFreeDefault(ptr, attrs uintptr) {
	if ptr == 0 {
		return
	}
	alignShift := (uint64(attrs) >> 24) & 0x1F
	if alignShift >= 12 {
		_, _, _ = k32VirtualFree.Call(ptr, 0, 0x8000 /* MEM_RELEASE */)
		return
	}
	heap, _, _ := k32GetProcHeap.Call()
	_, _, _ = k32HeapFree.Call(heap, 0, ptr)
}

func xmemAlloc(size, attrs uintptr) uintptr {
	if h := resolveXMemHooks()[0]; h != 0 {
		r, _, _ := syscall.SyscallN(h, size, attrs)
		return r
	}
	return xmemAllocDefault(size, attrs)
}

func xmemFree(ptr, attrs uintptr) {
	if ptr == 0 {
		return
	}
	if h := resolveXMemHooks()[1]; h != 0 {
		_, _, _ = syscall.SyscallN(h, ptr, attrs)
		return
	}
	xmemFreeDefault(ptr, attrs)
}

// xmemAllocCommitted alloue la mémoire d'une ressource D3D12.X "committed" via l'allocateur
// du titre (__XMemAllocationHooks__) avec les attributs graphiques Xbox Series.
func xmemAllocCommitted(size uint64, heapKind uint32) uint64 {
	const graphicsAllocator uint64 = 128 << 16
	pages := uint64(1)
	if size >= 2<<20 {
		pages = 2 // pages 2 MiB (bits 14..15 = 2 -> 0x8000)
	}
	memType := uint64(3) // cacheable (upload/readback)
	if heapKind == 1 {
		memType = 1 // write-combined (default GPU heap)
	}
	const alignLog2 uint64 = 16 // 64 KiB
	attrs := graphicsAllocator | (pages << 14) | (alignLog2 << 24) | (memType << 29)
	ptr := xmemAlloc(uintptr(size), uintptr(attrs))
	if ptr != 0 {
		dst := unsafe.Slice((*byte)(osPtr(ptr)), size)
		clear(dst)
	}
	return uint64(ptr)
}

type memAddressRequirements struct {
	LowestStartingAddress uintptr
	HighestEndingAddress  uintptr
	Alignment             uintptr
}

type memExtendedParameter struct {
	Type    uint64 // MemExtendedParameterAddressRequirements = 1
	Pointer uintptr
}

func xmemAddressRange(flags uint64) (uintptr, uintptr) {
	const address32Bit = uint64(4)
	if flags&address32Bit != 0 {
		return 0, 0xFFFFFFFF
	}
	field := (flags >> 12) & 7
	bits := uint(40)
	if field != 0 {
		bits = uint(47 - field)
	}
	return 1 << 32, (uintptr(1) << bits) - 1
}

func osPtr(addr uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&addr))
}

func readANSIStr(addr uintptr) string {
	if addr == 0 {
		return ""
	}
	var buf []byte
	for i := uintptr(0); i < 512; i++ {
		b := *(*byte)(osPtr(addr + i))
		if b == 0 {
			break
		}
		buf = append(buf, b)
	}
	return string(buf)
}

func readUTF16Str(addr uintptr) string {
	if addr == 0 {
		return ""
	}
	var units []uint16
	for i := uintptr(0); i < 512; i++ {
		u := *(*uint16)(osPtr(addr + i*2))
		if u == 0 {
			break
		}
		units = append(units, u)
	}
	return syscall.UTF16ToString(units)
}

var consolePseudoDLLs = []string{
	"xgameruntime.dll",
	"gameplatformservices.dll",
	"xmem.dll",
	"d3d12_xs.dll",
	"xg_xs.dll",
	"xcrdapi.dll",
	"dstorage_xs.dll",
	"gameinput.dll",
	"acphal.dll",
	"xapu.dll",
	"chad.dll",
}

const pseudoHandleBase uintptr = 0x0000A57A00000000

func pseudoHandleFor(dll string) (uintptr, bool) {
	lower := strings.ToLower(dll)
	if idx := strings.LastIndexAny(lower, `\/`); idx >= 0 {
		lower = lower[idx+1:]
	}
	if !strings.HasSuffix(lower, ".dll") {
		lower += ".dll"
	}
	for i, name := range consolePseudoDLLs {
		if lower == name {
			return pseudoHandleBase + uintptr(i+1), true
		}
	}
	return 0, false
}

func dllNameForPseudo(h uintptr) (string, bool) {
	if h <= pseudoHandleBase {
		return "", false
	}
	idx := int(h - pseudoHandleBase - 1)
	if idx < 0 || idx >= len(consolePseudoDLLs) {
		return "", false
	}
	return consolePseudoDLLs[idx], true
}

// NewLiveTable instancie les shims exécutables de démarrage (XMem, VirtualAlloc,
// VirtualFS, GDK hors-ligne, réseau hors-ligne, D3D12.X).
func NewLiveTable(profile core.ConsoleProfile, vfs *VirtualFS) *LiveTable {
	t := &LiveTable{funcs: make(map[string]uintptr)}

	// --- 1. VirtualAlloc sanitisé (retire MEM_GRAPHICS / MEM_TITLE / PAGE_GRAPHICS_*) ---
	cbVirtualAlloc := syscall.NewCallback(func(addr, size, allocType, protect uintptr) uintptr {
		cleanType := uintptr(SanitizeAllocFlags(uint32(allocType)))
		cleanProt := uintptr(SanitizePageProtect(uint32(protect)))
		r, _, _ := k32VirtualAlloc.Call(addr, size, cleanType, cleanProt)
		return r
	})
	for _, dll := range []string{
		"kernel32.dll",
		"kernelbase.dll",
		"api-ms-win-core-memory-l1-1-0.dll",
		"api-ms-win-core-memory-l1-1-1.dll",
		"api-ms-win-core-memory-l1-1-2.dll",
		"api-ms-win-core-memory-l1-1-3.dll",
		"api-ms-win-core-memory-l1-1-4.dll",
		"api-ms-win-core-memory-l1-1-5.dll",
		"api-ms-win-core-memory-l1-1-6.dll",
	} {
		t.bind(dll, "VirtualAlloc", cbVirtualAlloc)
	}

	// --- 2. xmem.dll (allocation unifiée Xbox Series + trampolines natifs __XMemAllocationHooks__) ---
	cbXMemAllocDefault := syscall.NewCallback(func(size, attrs uintptr) uintptr {
		return xmemAllocDefault(size, attrs)
	})
	cbXMemFreeDefault := syscall.NewCallback(func(ptr, attrs uintptr) uintptr {
		xmemFreeDefault(ptr, attrs)
		return 0
	})
	cbXMemAlloc := makeXMemTrampoline(0, cbXMemAllocDefault)
	cbXMemFree := makeXMemTrampoline(8, cbXMemFreeDefault)
	cbXMemVirtualFree := syscall.NewCallback(func(addr, size, freeType uintptr) uintptr {
		r, _, _ := k32VirtualFree.Call(addr, size, freeType)
		return r
	})
	cbXMemVirtualQuery := syscall.NewCallback(func(proc, addr, outBuf uintptr) uintptr {
		r, _, _ := k32VirtualQuery.Call(addr, outBuf, 48)
		return r
	})
	cbXMemWorkingSet := syscall.NewCallback(func(kind, outPtr uintptr) uintptr {
		if outPtr == 0 {
			return 0
		}
		// PROCESS_MEMORY_COUNTERS (x64 = 72 octets) : PagefileUsage à l'offset 56 (u64 #7)
		var counters [9]uint64
		counters[0] = 72 // cb (u32) + PageFaultCount (u32)
		hProc, _, _ := k32GetCurrProc.Call()
		_, _, _ = k32GetProcessMemoryInfo.Call(hProc, uintptr(unsafe.Pointer(&counters[0])), 72)
		pagefileUsage := counters[7]

		// WorkingSet Xbox Series (0x50 = 80 octets = 10 u64) :
		// +0x00 title_limit, +0x08 title_used, +0x28 gpu_optimal_limit
		gpuOpt := uint64(10) << 30
		standard := (uint64(3) << 30) + (uint64(512) << 20)
		if profile == core.ConsoleSeriesS {
			gpuOpt = uint64(6) << 30
			standard = uint64(2) << 30
		}
		words := unsafe.Slice((*uint64)(osPtr(outPtr)), 10)
		for i := range words {
			words[i] = 0
		}
		words[0] = gpuOpt + standard
		words[1] = pagefileUsage
		words[5] = gpuOpt
		return 1 // TRUE
	})
	cbReturnFalse := syscall.NewCallback(func() uintptr { return 0 })
	cbReturnTrue := syscall.NewCallback(func() uintptr { return 1 })

	cbXMemVirtualAlloc := syscall.NewCallback(func(base, size, kindArg, flagsArg, protectArg uintptr) uintptr {
		const (
			pages2M             uint32 = 0x20000000
			pages64KBit         uint32 = 0x00400000
			pages64K            uint32 = pages2M | pages64KBit
			memCommit           uint32 = 0x00001000
			memReserve          uint32 = 0x00002000
			memReplacePH        uint32 = 0x00004000
			memReservePH        uint32 = 0x00040000
			hostProtMask        uint32 = 0x000001FF
			pageNoAccess        uint32 = 0x00000001
		)
		kind := uint32(kindArg)
		prot := uint32(protectArg) & hostProtMask
		if prot == 0 {
			prot = pageNoAccess
		}
		placeholder := (kind&pages64K) == pages64K && (kind&memCommit) == 0
		if placeholder {
			if kind&memReplacePH != 0 {
				return base
			}
			prot = pageNoAccess
		}
		kind &^= (pages2M | pages64KBit)
		if placeholder {
			kind |= memReserve | memReservePH
		}
		if base == 0 {
			kind |= memReserve
		}

		if kbaseVirtAlloc2.Find() == nil {
			hProc, _, _ := k32GetCurrProc.Call()
			if base == 0 {
				lo, hi := xmemAddressRange(uint64(flagsArg))
				req := memAddressRequirements{
					LowestStartingAddress: lo,
					HighestEndingAddress:  hi,
				}
				if uint32(kindArg)&(pages2M|pages64KBit) == pages2M {
					req.Alignment = 2 << 20
				}
				param := memExtendedParameter{
					Type:    1, // MemExtendedParameterAddressRequirements
					Pointer: uintptr(unsafe.Pointer(&req)),
				}
				r, _, callErr := kbaseVirtAlloc2.Call(
					hProc, base, size, uintptr(kind), uintptr(prot),
					uintptr(unsafe.Pointer(&param)), 1,
				)
				if r == 0 {
					slog.Warn("XMemVirtualAlloc(constrained) échoué", "size", size, "kind", kindArg, "flags", flagsArg, "prot", protectArg, "err", callErr)
				} else {
					slog.Debug("XMemVirtualAlloc", "base", base, "size", size, "res", r)
				}
				return r
			}
			r, _, callErr := kbaseVirtAlloc2.Call(hProc, base, size, uintptr(kind), uintptr(prot), 0, 0)
			if r == 0 {
				slog.Warn("XMemVirtualAlloc(fixed) échoué", "base", base, "size", size, "kind", kindArg, "prot", protectArg, "err", callErr)
			}
			return r
		}
		r, _, _ := k32VirtualAlloc.Call(base, size, uintptr(kind), uintptr(prot))
		return r
	})

	t.bind("xmem.dll", "XMemAlloc", cbXMemAlloc)
	t.bind("xmem.dll", "XMemAllocDefault", cbXMemAllocDefault)
	t.bind("xmem.dll", "XMemFree", cbXMemFree)
	t.bind("xmem.dll", "XMemFreeDefault", cbXMemFreeDefault)
	t.bind("xmem.dll", "XMemVirtualAlloc", cbXMemVirtualAlloc)
	t.bind("xmem.dll", "XMemVirtualFree", cbXMemVirtualFree)
	t.bind("xmem.dll", "XMemVirtualQuery", cbXMemVirtualQuery)
	t.bind("xmem.dll", "XMemGetWorkingSetStatistics", cbXMemWorkingSet)
	t.bind("xmem.dll", "XMemIsToolsMemAvailable", cbReturnFalse)
	bindXMemPhysical(t)

	// --- 3. xgameruntime.dll (GDK hors-ligne minimal pour boot) ---
	devType := uintptr(0x00030001) // Xbox Series X
	if profile == core.ConsoleSeriesS {
		devType = 0x00030002 // Xbox Series S
	}
	cbGetDeviceType := syscall.NewCallback(func() uintptr { return devType })
	cbInitApiImpl := syscall.NewCallback(func() uintptr {
		slog.Debug("xgameruntime!InitializeApiImpl -> S_OK")
		return 0 // S_OK
	})
	cbGetApiVerImpl := syscall.NewCallback(func() uintptr {
		return 1
	})
	// --- 3a. Objets COM GDK (QueryApiImpl) & IGameInput ---
	// IGameInput (22 slots) : renvoie READING_NOT_FOUND sur GetCurrentReading et S_OK sur les callbacks
	cbGIReadingNotFound := syscall.NewCallback(func() uintptr { return giReadingNotFound })
	cbGISuccess := syscall.NewCallback(func() uintptr { return hresultOK })
	cbGITimestamp := syscall.NewCallback(func() uintptr { return 1000 })
	gameInputObj := NewCOMObject("IGameInput", 22, map[int]uintptr{
		3:  cbGITimestamp,       // GetCurrentTimestamp
		4:  cbGIReadingNotFound, // GetCurrentReading
		5:  cbGIReadingNotFound, // GetNextReading
		6:  cbGIReadingNotFound, // GetPreviousReading
		7:  cbGIReadingNotFound, // GetTemporalReading
		8:  cbGISuccess,         // RegisterReadingCallback
		9:  cbGISuccess,         // RegisterDeviceCallback
		10: cbGISuccess,         // RegisterSystemButtonCallback
		11: cbGISuccess,         // RegisterGuideButtonCallback
		12: cbGISuccess,         // RegisterKeyboardLayoutCallback
		13: cbGISuccess,         // StopCallback
		14: cbReturnTrue,        // UnregisterCallback
		16: cbGISuccess,         // FindDeviceFromId
		17: cbGISuccess,         // FindDeviceFromObject
		18: cbGISuccess,         // FindDeviceFromPlatformHandle
		19: cbGISuccess,         // FindDeviceFromPlatformString
		20: cbGISuccess,         // EnableOemDeviceSupport
		21: cbGISuccess,         // SetFocusPolicy
	})

	var nextHandle atomic.Uint64
	nextHandle.Store(0x1000)

	putStr := func(s string, size, bufPtr, usedPtr uintptr) uintptr {
		need := uintptr(len(s) + 1)
		if bufPtr == 0 || size < need {
			return 0x8007007A // HRESULT_FROM_WIN32(ERROR_INSUFFICIENT_BUFFER)
		}
		dst := unsafe.Slice((*byte)(osPtr(bufPtr)), need)
		copy(dst, s)
		dst[len(s)] = 0
		if usedPtr != 0 {
			*(*uintptr)(osPtr(usedPtr)) = need
		}
		return hresultOK
	}

	cbTaskQueueCreate := syscall.NewCallback(func(this, workMode, compMode, outPtr uintptr) uintptr {
		if outPtr != 0 {
			*(*uintptr)(osPtr(outPtr)) = uintptr(nextHandle.Add(1))
		}
		return hresultOK
	})
	cbRegisterToken5 := syscall.NewCallback(func(this, a1, a2, a3, outToken uintptr) uintptr {
		if outToken != 0 {
			*(*uint64)(osPtr(outToken)) = nextHandle.Add(1)
		}
		return hresultOK
	})
	cbNetConnectivityHint := syscall.NewCallback(func(this, outPtr uintptr) uintptr {
		if outPtr != 0 {
			// ConnectivityHint: level=1 (None), cost=0, iface=0, initialized=true
			b := unsafe.Slice((*byte)(osPtr(outPtr)), 16)
			for i := range b {
				b[i] = 0
			}
			b[0] = 1  // level = 1
			b[12] = 1 // initialized = true
		}
		return hresultOK
	})
	cbGameGetTitleID := syscall.NewCallback(func(this, outPtr uintptr) uintptr {
		if outPtr != 0 {
			*(*uint32)(osPtr(outPtr)) = 0x7952987A // GTA VI Xbox Series TitleId
		}
		return hresultOK
	})
	cbSysConsoleID := syscall.NewCallback(func(this, size, bufPtr, usedPtr uintptr) uintptr {
		return putStr("00000000000000000000000000000000000001", size, bufPtr, usedPtr)
	})
	cbSysSandboxID := syscall.NewCallback(func(this, size, bufPtr, usedPtr uintptr) uintptr {
		return putStr("RETAIL", size, bufPtr, usedPtr)
	})
	cbSysDeviceID := syscall.NewCallback(func(this, size, bufPtr, usedPtr uintptr) uintptr {
		return putStr("QVNUUkFMIG9mZmxpbmUgZGV2aWNlIGlkZW50aXR5ISE=", size, bufPtr, usedPtr)
	})

	gdkProviders := map[string]uintptr{
		"{073B7DCB-1FCF-4030-94BE-E3C9EB623428}": NewCOMObject("xasync", 64, map[int]uintptr{
			12: cbTaskQueueCreate, // 0x60 XTaskQueueCreate
			13: cbTaskQueueCreate, // 0x68 XTaskQueueCreateComposite
			29: cbGISuccess,       // 0xE8 CheckThread
		}),
		"{37E56907-2F10-41E8-B72F-36EDB185331A}": NewCOMObject("xnetworking", 64, map[int]uintptr{
			15: cbNetConnectivityHint, // 0x78 GetConnectivityHint
			16: cbRegisterToken5,      // 0x80 RegisterConnectivityHintChanged
			17: cbReturnTrue,          // 0x88 UnregisterConnectivityHintChanged
		}),
		"{01ACD177-91F9-4763-A38E-CCBB55CE32E0}": NewCOMObject("xuser", 64, map[int]uintptr{
			33: cbRegisterToken5, // 0x108 XUserRegisterForChangeEvent
			34: cbReturnTrue,     // 0x110 XUserUnregisterForChangeEvent
		}),
		"{973A344E-24BF-4D0F-8457-56C534892B29}": NewCOMObject("xgame", 64, map[int]uintptr{
			3: cbGameGetTitleID, // 0x18 XGameGetXboxTitleId
		}),
		"{E349BD1A-FC20-4E40-B99C-4178CC6B409F}": NewCOMObject("xsystem", 64, map[int]uintptr{
			3: cbSysConsoleID, // 0x18 ConsoleId
			4: cbSysSandboxID, // 0x20 SandboxId ("RETAIL")
			5: cbSysDeviceID,  // 0x28 DeviceId
		}),
		"{AF406016-E850-4AA8-A88D-2F3DCB9DAC7E}": NewCOMObject("xpackage", 64, map[int]uintptr{
			3: syscall.NewCallback(func(this, size, bufPtr uintptr) uintptr { // XPackageGetCurrentProcessPackageIdentifier
				return putStr("7952987A", size, bufPtr, 0)
			}),
			4: cbReturnTrue, // XPackageIsPackaged
			5: syscall.NewCallback(func(this, id, selCount, sels, interval, queue, outPtr uintptr) uintptr { // CreateInstallationMonitor
				if outPtr != 0 {
					*(*uintptr)(osPtr(outPtr)) = uintptr(nextHandle.Add(1))
				}
				return hresultOK
			}),
			6: cbGISuccess, // CloseInstallationMonitorHandle
			7: syscall.NewCallback(func(this, mon, outPtr uintptr) uintptr { // GetInstallationProgress
				if outPtr != 0 {
					// { total_bytes, installed_bytes, launch_bytes, launchable=true, completed=true }
					const pkgBytes = uint64(100) << 30
					*(*uint64)(osPtr(outPtr + 0)) = pkgBytes
					*(*uint64)(osPtr(outPtr + 8)) = pkgBytes
					*(*uint64)(osPtr(outPtr + 16)) = pkgBytes
					*(*uint8)(osPtr(outPtr + 24)) = 1
					*(*uint8)(osPtr(outPtr + 25)) = 1
				}
				return 0
			}),
			9: syscall.NewCallback(func(this, id, selCount, sels, outAvail uintptr) uintptr { // XPackageFindChunkAvailability
				if outAvail != 0 {
					*(*uint32)(osPtr(outAvail)) = 0 // XPackageChunkAvailability::Ready
				}
				return hresultOK
			}),
			39: cbGISuccess, // XPackageEnumeratePackages (aucun DLC externe)
		}),
		"{F4FAF4D4-2D04-4FCE-B3E0-474A713A3E84}": NewCOMObject("storage", 64, map[int]uintptr{
			3: syscall.NewCallback(func(this, outSize uintptr) uintptr { // XPersistentLocalStorageGetPathSize
				if outSize != 0 {
					*(*uintptr)(osPtr(outSize)) = 4 // "T:\\\0"
				}
				return hresultOK
			}),
			4: syscall.NewCallback(func(this, size, bufPtr, usedPtr uintptr) uintptr { // XPersistentLocalStorageGetPath
				if size <= 3 {
					return 0x80004005 // E_FAIL (ABI Xbox)
				}
				return putStr(`T:\`, size, bufPtr, usedPtr)
			}),
			5: syscall.NewCallback(func(this, outSpace uintptr) uintptr { // XPersistentLocalStorageGetSpaceInfo
				if outSpace != 0 {
					// { available_free, total_free, used, total } = 4 GiB libre
					w := unsafe.Slice((*uint64)(osPtr(outSpace)), 4)
					quota := uint64(4) << 30
					w[0], w[1], w[2], w[3] = quota, quota, 0, quota
				}
				return hresultOK
			}),
		}),
		"{03F0FE74-FDD9-4E5C-B630-F9339C47ACC5}": NewCOMObject("display", 64, map[int]uintptr{
			4: syscall.NewCallback(func(this, pref, info uintptr) uintptr { // XDisplayTryEnableHdrMode -> 2 (Disabled / SDR)
				return 2
			}),
		}),
	}

	// --- 3d. Réécriture des chemins console (VirtualFS : G:\ / GAME:\ / T:\) ---
	if vfs != nil {
		rewriteW := func(pathPtr uintptr, isWrite bool) (*uint16, bool) {
			if pathPtr == 0 {
				return nil, false
			}
			orig := readUTF16Str(pathPtr)
			var mapped string
			var changed bool
			if isWrite {
				if m, err := vfs.ResolveWrite(orig); err == nil && m != orig {
					mapped, changed = m, true
				}
			} else {
				mapped, changed = vfs.ResolveRead(orig)
			}
			if !changed {
				return nil, false
			}
			slog.Debug("VirtualFS rewrite", "from", orig, "to", mapped)
			p, err := syscall.UTF16PtrFromString(mapped)
			if err != nil {
				return nil, false
			}
			return p, true
		}

		rewriteA := func(pathPtr uintptr, isWrite bool) ([]byte, bool) {
			if pathPtr == 0 {
				return nil, false
			}
			orig := readANSIStr(pathPtr)
			var mapped string
			var changed bool
			if isWrite {
				if m, err := vfs.ResolveWrite(orig); err == nil && m != orig {
					mapped, changed = m, true
				}
			} else {
				mapped, changed = vfs.ResolveRead(orig)
			}
			if !changed {
				return nil, false
			}
			slog.Debug("VirtualFS rewriteA", "from", orig, "to", mapped)
			return append([]byte(mapped), 0), true
		}

		cbCreateFileW := syscall.NewCallback(func(namePtr, access, share, sec, disp, flags, tmpl uintptr) uintptr {
			isWrite := (access & 0x40000000) != 0 // GENERIC_WRITE
			var r uintptr
			if mapped, ok := rewriteW(namePtr, isWrite); ok {
				r, _, _ = k32CreateFileW.Call(uintptr(unsafe.Pointer(mapped)), access, share, sec, disp, flags, tmpl)
			} else {
				r, _, _ = k32CreateFileW.Call(namePtr, access, share, sec, disp, flags, tmpl)
			}
			if r == ^uintptr(0) && namePtr != 0 {
				slog.Debug("CreateFileW échoué", "path", readUTF16Str(namePtr))
			}
			return r
		})
		cbCreateFileA := syscall.NewCallback(func(namePtr, access, share, sec, disp, flags, tmpl uintptr) uintptr {
			isWrite := (access & 0x40000000) != 0
			var r uintptr
			if mapped, ok := rewriteA(namePtr, isWrite); ok {
				r, _, _ = k32CreateFileA.Call(uintptr(unsafe.Pointer(&mapped[0])), access, share, sec, disp, flags, tmpl)
			} else {
				r, _, _ = k32CreateFileA.Call(namePtr, access, share, sec, disp, flags, tmpl)
			}
			if r == ^uintptr(0) && namePtr != 0 {
				slog.Debug("CreateFileA échoué", "path", readANSIStr(namePtr))
			}
			return r
		})
		cbCreateFile2 := syscall.NewCallback(func(namePtr, access, share, disp, extParams uintptr) uintptr {
			isWrite := (access & 0x40000000) != 0
			var r uintptr
			if mapped, ok := rewriteW(namePtr, isWrite); ok {
				r, _, _ = k32CreateFile2.Call(uintptr(unsafe.Pointer(mapped)), access, share, disp, extParams)
			} else {
				r, _, _ = k32CreateFile2.Call(namePtr, access, share, disp, extParams)
			}
			if r == ^uintptr(0) && namePtr != 0 {
				slog.Debug("CreateFile2 échoué", "path", readUTF16Str(namePtr))
			}
			return r
		})
		cbGetFileAttrW := syscall.NewCallback(func(namePtr uintptr) uintptr {
			if mapped, ok := rewriteW(namePtr, false); ok {
				r, _, _ := k32GetFileAttrW.Call(uintptr(unsafe.Pointer(mapped)))
				return r
			}
			r, _, _ := k32GetFileAttrW.Call(namePtr)
			return r
		})
		cbGetFileAttrA := syscall.NewCallback(func(namePtr uintptr) uintptr {
			if mapped, ok := rewriteA(namePtr, false); ok {
				r, _, _ := k32GetFileAttrA.Call(uintptr(unsafe.Pointer(&mapped[0])))
				return r
			}
			r, _, _ := k32GetFileAttrA.Call(namePtr)
			return r
		})
		cbGetFileAttrExW := syscall.NewCallback(func(namePtr, level, infoPtr uintptr) uintptr {
			if mapped, ok := rewriteW(namePtr, false); ok {
				r, _, _ := k32GetFileAttrExW.Call(uintptr(unsafe.Pointer(mapped)), level, infoPtr)
				return r
			}
			r, _, _ := k32GetFileAttrExW.Call(namePtr, level, infoPtr)
			return r
		})
		cbFindFirstFileW := syscall.NewCallback(func(namePtr, dataPtr uintptr) uintptr {
			if mapped, ok := rewriteW(namePtr, false); ok {
				r, _, _ := k32FindFirstFileW.Call(uintptr(unsafe.Pointer(mapped)), dataPtr)
				return r
			}
			r, _, _ := k32FindFirstFileW.Call(namePtr, dataPtr)
			return r
		})
		cbFindFirstFileExW := syscall.NewCallback(func(namePtr, infoLevel, dataPtr, searchOp, filter, flags uintptr) uintptr {
			if mapped, ok := rewriteW(namePtr, false); ok {
				r, _, _ := k32FindFirstFileExW.Call(uintptr(unsafe.Pointer(mapped)), infoLevel, dataPtr, searchOp, filter, flags)
				return r
			}
			r, _, _ := k32FindFirstFileExW.Call(namePtr, infoLevel, dataPtr, searchOp, filter, flags)
			return r
		})
		cbCreateDirW := syscall.NewCallback(func(namePtr, sec uintptr) uintptr {
			if mapped, ok := rewriteW(namePtr, true); ok {
				r, _, _ := k32CreateDirW.Call(uintptr(unsafe.Pointer(mapped)), sec)
				return r
			}
			r, _, _ := k32CreateDirW.Call(namePtr, sec)
			return r
		})
		cbDeleteFileW := syscall.NewCallback(func(namePtr uintptr) uintptr {
			if mapped, ok := rewriteW(namePtr, true); ok {
				r, _, _ := k32DeleteFileW.Call(uintptr(unsafe.Pointer(mapped)))
				return r
			}
			r, _, _ := k32DeleteFileW.Call(namePtr)
			return r
		})

		for _, dll := range []string{
			"kernel32.dll",
			"kernelbase.dll",
			"api-ms-win-core-file-l1-1-0.dll",
			"api-ms-win-core-file-l1-2-0.dll",
			"api-ms-win-core-file-l1-2-1.dll",
			"api-ms-win-core-file-l1-2-2.dll",
		} {
			t.bind(dll, "CreateFileW", cbCreateFileW)
			t.bind(dll, "CreateFileA", cbCreateFileA)
			t.bind(dll, "CreateFile2", cbCreateFile2)
			t.bind(dll, "GetFileAttributesW", cbGetFileAttrW)
			t.bind(dll, "GetFileAttributesA", cbGetFileAttrA)
			t.bind(dll, "GetFileAttributesExW", cbGetFileAttrExW)
			t.bind(dll, "FindFirstFileW", cbFindFirstFileW)
			t.bind(dll, "FindFirstFileExW", cbFindFirstFileExW)
			t.bind(dll, "CreateDirectoryW", cbCreateDirW)
			t.bind(dll, "DeleteFileW", cbDeleteFileW)
		}
	}

	cbQueryApiImpl := syscall.NewCallback(func(provider, iid, outPtr uintptr) uintptr {
		provGUID := FormatGUID(provider)
		iidGUID := FormatGUID(iid)
		if obj, ok := gdkProviders[provGUID]; ok && obj != 0 {
			slog.Debug("xgameruntime!QueryApiImpl résolu", "provider", provGUID, "iid", iidGUID)
			if outPtr != 0 {
				*(*uintptr)(osPtr(outPtr)) = obj
			}
			return hresultOK
		}
		slog.Info("xgameruntime!QueryApiImpl inconnu", "provider", provGUID, "iid", iidGUID)
		if outPtr != 0 {
			*(*uintptr)(osPtr(outPtr)) = 0
		}
		return hresultNoIface
	})
	cbGameInputCreate := syscall.NewCallback(func(outPtr uintptr) uintptr {
		slog.Info("GameInput!GameInputCreate -> IGameInput COM actif")
		if outPtr != 0 {
			*(*uintptr)(osPtr(outPtr)) = gameInputObj
		}
		return hresultOK
	})
	t.bind("gameinput.dll", "#1", cbGameInputCreate)
	t.bind("gameinput.dll", "GameInputCreate", cbGameInputCreate)

	// --- 3c. XCRDAPI.dll!DStorageGetFactory (DirectStorage réel via pkg/hle/dstorage_windows.go) ---
	bindDirectStorage(t, vfs)
	t.bind("xgameruntime.dll", "XSystemGetDeviceType", cbGetDeviceType)
	t.bind("xgameruntime.dll", "XSystemIsRemoteConnectEnabled", cbReturnFalse)
	t.bind("xgameruntime.dll", "XErrorReport", cbReturnTrue)
	t.bind("xgameruntime.dll", "InitializeApiImpl", cbInitApiImpl)
	t.bind("xgameruntime.dll", "InitializeApiImplEx", cbInitApiImpl)
	t.bind("xgameruntime.dll", "InitializeApiImplEx2", cbInitApiImpl)
	t.bind("xgameruntime.dll", "InitializeApiImplWithVersion", cbInitApiImpl)
	t.bind("xgameruntime.dll", "UninitializeApiImpl", cbInitApiImpl)
	t.bind("xgameruntime.dll", "GetApiVersionImpl", cbGetApiVerImpl)
	t.bind("xgameruntime.dll", "QueryApiImpl", cbQueryApiImpl)

	// --- 3b. Routage dynamique LoadLibrary* / GetModuleHandle* / GetProcAddress ---
	cbLoadLibA := syscall.NewCallback(func(namePtr uintptr) uintptr {
		name := readANSIStr(namePtr)
		if h, ok := pseudoHandleFor(name); ok {
			slog.Debug("LoadLibraryA -> pseudo-handle console", "dll", name)
			return h
		}
		r, _, _ := k32LoadLibA.Call(namePtr)
		return r
	})
	cbLoadLibW := syscall.NewCallback(func(namePtr uintptr) uintptr {
		name := readUTF16Str(namePtr)
		if h, ok := pseudoHandleFor(name); ok {
			slog.Debug("LoadLibraryW -> pseudo-handle console", "dll", name)
			return h
		}
		r, _, _ := k32LoadLibW.Call(namePtr)
		return r
	})
	cbLoadLibExA := syscall.NewCallback(func(namePtr, hFile, flags uintptr) uintptr {
		name := readANSIStr(namePtr)
		if h, ok := pseudoHandleFor(name); ok {
			slog.Debug("LoadLibraryExA -> pseudo-handle console", "dll", name)
			return h
		}
		r, _, _ := k32LoadLibExA.Call(namePtr, hFile, flags)
		return r
	})
	cbLoadLibExW := syscall.NewCallback(func(namePtr, hFile, flags uintptr) uintptr {
		name := readUTF16Str(namePtr)
		if h, ok := pseudoHandleFor(name); ok {
			slog.Debug("LoadLibraryExW -> pseudo-handle console", "dll", name)
			return h
		}
		r, _, _ := k32LoadLibExW.Call(namePtr, hFile, flags)
		return r
	})
	cbGetModA := syscall.NewCallback(func(namePtr uintptr) uintptr {
		if namePtr != 0 {
			name := readANSIStr(namePtr)
			if h, ok := pseudoHandleFor(name); ok {
				return h
			}
		}
		r, _, _ := k32GetModHandleA.Call(namePtr)
		return r
	})
	cbGetModW := syscall.NewCallback(func(namePtr uintptr) uintptr {
		if namePtr != 0 {
			name := readUTF16Str(namePtr)
			if h, ok := pseudoHandleFor(name); ok {
				return h
			}
		}
		r, _, _ := k32GetModHandleW.Call(namePtr)
		return r
	})
	cbGetProcAddr := syscall.NewCallback(func(hMod, namePtr uintptr) uintptr {
		if dll, ok := dllNameForPseudo(hMod); ok {
			sym := ""
			if namePtr > 0xFFFF {
				sym = readANSIStr(namePtr)
			}
			if fn, found := t.Lookup(dll, sym); found {
				slog.Debug("GetProcAddress(console) résolu", "dll", dll, "symbol", sym)
				return fn
			}
			slog.Warn("GetProcAddress(console) non implémenté", "dll", dll, "symbol", sym)
			return 0
		}
		r, _, _ := k32GetProcAddr.Call(hMod, namePtr)
		return r
	})
	for _, dll := range []string{
		"kernel32.dll",
		"kernelbase.dll",
		"api-ms-win-core-libraryloader-l1-2-0.dll",
		"api-ms-win-core-libraryloader-l1-2-1.dll",
	} {
		t.bind(dll, "LoadLibraryA", cbLoadLibA)
		t.bind(dll, "LoadLibraryW", cbLoadLibW)
		t.bind(dll, "LoadLibraryExA", cbLoadLibExA)
		t.bind(dll, "LoadLibraryExW", cbLoadLibExW)
		t.bind(dll, "GetModuleHandleA", cbGetModA)
		t.bind(dll, "GetModuleHandleW", cbGetModW)
		t.bind(dll, "GetProcAddress", cbGetProcAddr)
	}

	// --- 3e. OutputDebugStringA/W (capture les diagnostics / assertions du titre) ---
	cbOutDbgA := syscall.NewCallback(func(ptr uintptr) uintptr {
		if ptr != 0 {
			s := strings.TrimSpace(readANSIStr(ptr))
			if s != "" {
				slog.Info("TITLE OutputDebugStringA", "msg", s)
			}
		}
		return 0
	})
	cbOutDbgW := syscall.NewCallback(func(ptr uintptr) uintptr {
		if ptr != 0 {
			s := strings.TrimSpace(readUTF16Str(ptr))
			if s != "" {
				slog.Info("TITLE OutputDebugStringW", "msg", s)
			}
		}
		return 0
	})
	for _, dll := range []string{
		"kernel32.dll",
		"kernelbase.dll",
		"api-ms-win-core-debug-l1-1-0.dll",
		"api-ms-win-core-debug-l1-1-1.dll",
	} {
		t.bind(dll, "OutputDebugStringA", cbOutDbgA)
		t.bind(dll, "OutputDebugStringW", cbOutDbgW)
	}

	// --- 4. Isolation réseau hors-ligne (ws2_32 & winhttp) ---
	cbNetBlocked := syscall.NewCallback(func() uintptr {
		slog.Debug("appel réseau sortant bloqué (mode hors-ligne)")
		return 0xFFFFFFFF // SOCKET_ERROR / erreur
	})
	t.bind("ws2_32.dll", "connect", cbNetBlocked)
	t.bind("ws2_32.dll", "getaddrinfo", cbNetBlocked)
	t.bind("ws2_32.dll", "GetAddrInfoW", cbNetBlocked)

	// --- 5. d3d12_xs.dll (Root Signature PC + log CreateDevice) ---
	type rootSigDesc struct {
		NumParameters     uint32
		_pad0             uint32
		Parameters        uintptr
		NumStaticSamplers uint32
		_pad1             uint32
		StaticSamplers    uintptr
		Flags             uint32
		_pad2             uint32
	}
	type versionedRootSigDesc struct {
		Version uint32
		_pad    uint32
		Desc    rootSigDesc
	}
	cbSerRootSig := syscall.NewCallback(func(descPtr, version, blobOut, errOut uintptr) uintptr {
		if d3d12SerRoot.Find() != nil {
			return 0x80004001 // E_NOTIMPL
		}
		if descPtr != 0 {
			cp := *(*rootSigDesc)(osPtr(descPtr))
			cp.Flags = gpu.SanitizeRootSignatureFlags(cp.Flags) &^ 0xFF000000
			r, _, _ := d3d12SerRoot.Call(uintptr(unsafe.Pointer(&cp)), version, blobOut, errOut)
			return r
		}
		r, _, _ := d3d12SerRoot.Call(descPtr, version, blobOut, errOut)
		return r
	})
	cbSerVerRootSig := syscall.NewCallback(func(descPtr, blobOut, errOut uintptr) uintptr {
		if d3d12SerVerRoot.Find() != nil {
			return 0x80004001
		}
		if descPtr != 0 {
			cp := *(*versionedRootSigDesc)(osPtr(descPtr))
			cp.Desc.Flags = gpu.SanitizeRootSignatureFlags(cp.Desc.Flags) &^ 0xFF000000
			r, _, _ := d3d12SerVerRoot.Call(uintptr(unsafe.Pointer(&cp)), blobOut, errOut)
			return r
		}
		r, _, _ := d3d12SerVerRoot.Call(descPtr, blobOut, errOut)
		return r
	})
	cbConfigVirtMem := syscall.NewCallback(func(cfgPtr uintptr) uintptr {
		if cfgPtr != 0 {
			// VirtualMemory (0x20 octets) : +0x10 lowest, +0x18 highest
			*(*uint64)(osPtr(cfgPtr + 0x10)) = 0x0000000000010000
			*(*uint64)(osPtr(cfgPtr + 0x18)) = 0x00007FFFFFFEFFFF
		}
		slog.Debug("d3d12_xs!D3D12ConfigureVirtualMemoryX -> S_OK")
		return 0 // S_OK
	})
	// ID3D12DeviceX complet (pkg/hle/d3d12x_windows.go : 160 slots, en-tête 0x58, out_of_line=1)
	d3d12DeviceObj := newD3D12XDevice()
	cbCreateDeviceX := syscall.NewCallback(func(adapter, params, iidPtr, outPtr uintptr) uintptr {
		slog.Info("d3d12_xs!D3D12XboxCreateDevice -> ID3D12DeviceX COM actif", "iid", FormatGUID(iidPtr))
		hwnd := gpu.EnsureWindow("gta-vi-astral — GTA VI (Xbox Series)", 1280, 720)
		if _, err := gpu.InitHostD3D12(hwnd, 1280, 720); err != nil {
			slog.Warn("Host D3D12 non initialisé (mode logiciel/stub)", "err", err)
		}
		if outPtr != 0 {
			*(*uintptr)(osPtr(outPtr)) = d3d12DeviceObj
		}
		return hresultOK
	})
	t.bind("d3d12_xs.dll", "D3D12ConfigureVirtualMemoryX", cbConfigVirtMem)
	t.bind("d3d12_xs.dll", "D3D12XboxCreateDevice", cbCreateDeviceX)
	t.bind("d3d12_xs.dll", "IID_ID3D12Device", uintptr(unsafe.Pointer(&iidID3D12Device[0])))

	// --- 6. xg_xs.dll (layout & swizzle textures Xbox Series) ---
	bindXG(t)
	t.bind("d3d12_xs.dll", "D3D12SerializeRootSignature", cbSerRootSig)
	t.bind("d3d12_xs.dll", "D3D12SerializeVersionedRootSignature", cbSerVerRootSig)

	return t
}

func (t *LiveTable) bind(dll, symbol string, fn uintptr) {
	t.funcs[strings.ToLower(dll)+"!"+strings.ToLower(symbol)] = fn
}

// Lookup retourne l'adresse native du shim si implémenté en live.
func (t *LiveTable) Lookup(dll, symbol string) (uintptr, bool) {
	fn, ok := t.funcs[strings.ToLower(dll)+"!"+strings.ToLower(symbol)]
	return fn, ok
}
