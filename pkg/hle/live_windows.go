//go:build windows

package hle

import (
	"log/slog"
	"strings"
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
	k32GetModHandleW = k32.NewProc("GetModuleHandleW")
	k32GetProcAddr   = k32.NewProc("GetProcAddress")
	kbaseVirtAlloc2  = kbase.NewProc("VirtualAlloc2")
	d3d12Host        = syscall.NewLazyDLL("d3d12.dll")
	d3d12SerRoot     = d3d12Host.NewProc("D3D12SerializeRootSignature")
	d3d12SerVerRoot  = d3d12Host.NewProc("D3D12SerializeVersionedRootSignature")

	// IID_ID3D12Device : {189819f1-1db6-4b57-be54-1821339b85f7} (import de données)
	iidID3D12Device = [16]byte{
		0xf1, 0x19, 0x98, 0x18, 0xb6, 0x1d, 0x57, 0x4b,
		0xbe, 0x54, 0x18, 0x21, 0x33, 0x9b, 0x85, 0xf7,
	}
)

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
// GDK hors-ligne, réseau hors-ligne, D3D12.X root signature).
func NewLiveTable(profile core.ConsoleProfile) *LiveTable {
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

	// --- 2. xmem.dll (allocation unifiée Xbox Series) ---
	cbXMemAlloc := syscall.NewCallback(func(size, attrs uintptr) uintptr {
		heap, _, _ := k32GetProcHeap.Call()
		if size == 0 {
			size = 16
		}
		// Allocation alignée 64 octets (exigence XMemAlloc / AVX2)
		r, _, _ := k32HeapAlloc.Call(heap, 0x00000008 /* HEAP_ZERO_MEMORY */, size)
		return r
	})
	cbXMemFree := syscall.NewCallback(func(ptr, attrs uintptr) uintptr {
		if ptr == 0 {
			return 0
		}
		heap, _, _ := k32GetProcHeap.Call()
		_, _, _ = k32HeapFree.Call(heap, 0, ptr)
		return 0
	})
	cbXMemVirtualFree := syscall.NewCallback(func(addr, size, freeType uintptr) uintptr {
		r, _, _ := k32VirtualFree.Call(addr, size, freeType)
		return r
	})
	cbXMemVirtualQuery := syscall.NewCallback(func(proc, addr, outBuf uintptr) uintptr {
		r, _, _ := k32VirtualQuery.Call(addr, outBuf, 48)
		return r
	})
	cbXMemWorkingSet := syscall.NewCallback(func(kind, outPtr uintptr) uintptr {
		if outPtr != 0 {
			// Rapporte 12 GiB (Series X) ou 8 GiB (Series S) disponibles
			total := uint64(12) << 30
			if profile == core.ConsoleSeriesS {
				total = uint64(8) << 30
			}
			words := unsafe.Slice((*uint64)(osPtr(outPtr)), 4)
			words[0] = total
			words[1] = total
		}
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
	t.bind("xmem.dll", "XMemAllocDefault", cbXMemAlloc)
	t.bind("xmem.dll", "XMemFree", cbXMemFree)
	t.bind("xmem.dll", "XMemFreeDefault", cbXMemFree)
	t.bind("xmem.dll", "XMemVirtualAlloc", cbXMemVirtualAlloc)
	t.bind("xmem.dll", "XMemVirtualFree", cbXMemVirtualFree)
	t.bind("xmem.dll", "XMemVirtualQuery", cbXMemVirtualQuery)
	t.bind("xmem.dll", "XMemGetWorkingSetStatistics", cbXMemWorkingSet)
	t.bind("xmem.dll", "XMemIsToolsMemAvailable", cbReturnFalse)

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
	cbQueryApiImpl := syscall.NewCallback(func(provider, iid, outPtr uintptr) uintptr {
		slog.Info("xgameruntime!QueryApiImpl", "provider", FormatGUID(provider), "iid", FormatGUID(iid))
		if outPtr != 0 {
			*(*uintptr)(osPtr(outPtr)) = 0
		}
		return 0x80004002 // E_NOINTERFACE
	})
	cbGameInputCreate := syscall.NewCallback(func(outPtr uintptr) uintptr {
		slog.Info("GameInput!GameInputCreate appelé (stub E_NOTIMPL)")
		if outPtr != 0 {
			*(*uintptr)(osPtr(outPtr)) = 0
		}
		return 0x80004001 // E_NOTIMPL
	})
	t.bind("gameinput.dll", "#1", cbGameInputCreate)
	t.bind("gameinput.dll", "GameInputCreate", cbGameInputCreate)
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

	// --- 4. Isolation réseau hors-ligne (ws2_32 & winhttp) ---
	cbNetBlocked := syscall.NewCallback(func() uintptr {
		slog.Debug("appel réseau sortant bloqué (mode hors-ligne)")
		return 0xFFFFFFFF // SOCKET_ERROR / erreur
	})
	t.bind("ws2_32.dll", "connect", cbNetBlocked)
	t.bind("ws2_32.dll", "getaddrinfo", cbNetBlocked)
	t.bind("ws2_32.dll", "GetAddrInfoW", cbNetBlocked)

	// --- 5. d3d12_xs.dll (Root Signature PC + log CreateDevice) ---
	cbSerRootSig := syscall.NewCallback(func(descPtr, version, blobOut, errOut uintptr) uintptr {
		if descPtr != 0 {
			// Offset 32 dans D3D12_ROOT_SIGNATURE_DESC (64-bit) = Flags
			flagsPtr := (*uint32)(osPtr(descPtr + 32))
			*flagsPtr = gpu.SanitizeRootSignatureFlags(*flagsPtr)
		}
		if d3d12SerRoot.Find() == nil {
			r, _, _ := d3d12SerRoot.Call(descPtr, version, blobOut, errOut)
			return r
		}
		return 0x80004001 // E_NOTIMPL
	})
	cbSerVerRootSig := syscall.NewCallback(func(descPtr, blobOut, errOut uintptr) uintptr {
		if d3d12SerVerRoot.Find() == nil {
			r, _, _ := d3d12SerVerRoot.Call(descPtr, blobOut, errOut)
			return r
		}
		return 0x80004001
	})
	cbConfigVirtMem := syscall.NewCallback(func() uintptr {
		slog.Debug("d3d12_xs!D3D12ConfigureVirtualMemoryX -> S_OK")
		return 0 // S_OK
	})
	t.bind("d3d12_xs.dll", "D3D12ConfigureVirtualMemoryX", cbConfigVirtMem)
	t.bind("d3d12_xs.dll", "IID_ID3D12Device", uintptr(unsafe.Pointer(&iidID3D12Device[0])))
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
