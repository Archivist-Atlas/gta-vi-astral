//go:build windows

package loader

import (
	"fmt"
	"syscall"
	"unsafe"
)

const (
	memCommit            uintptr = 0x1000
	memReserve           uintptr = 0x2000
	memRelease           uintptr = 0x8000
	pageReadWrite        uintptr = 0x04
	pageExecuteRead      uintptr = 0x20
	pageExecuteReadWrite uintptr = 0x40

	loadLibrarySearchSystem32  uintptr = 0x00000800
	loadWithAlteredSearchPath  uintptr = 0x00000008
	infiniteTimeout            uintptr = 0xFFFFFFFF
)

var (
	modKernel32               = syscall.NewLazyDLL("kernel32.dll")
	procVirtualAlloc          = modKernel32.NewProc("VirtualAlloc")
	procVirtualProtect        = modKernel32.NewProc("VirtualProtect")
	procFlushInstructionCache = modKernel32.NewProc("FlushInstructionCache")
	procGetCurrentProcess     = modKernel32.NewProc("GetCurrentProcess")
	procLoadLibraryExW        = modKernel32.NewProc("LoadLibraryExW")
	procGetProcAddress        = modKernel32.NewProc("GetProcAddress")
	procAddVectoredExHandler  = modKernel32.NewProc("AddVectoredExceptionHandler")
	procCreateThread          = modKernel32.NewProc("CreateThread")
	procWaitForSingleObject   = modKernel32.NewProc("WaitForSingleObject")
	procGetExitCodeThread     = modKernel32.NewProc("GetExitCodeThread")
	procCloseHandle           = modKernel32.NewProc("CloseHandle")
)

func virtualAlloc(addr, size, allocType, protect uintptr) (uintptr, error) {
	r, _, err := procVirtualAlloc.Call(addr, size, allocType, protect)
	if r == 0 {
		return 0, err
	}
	return r, nil
}

func virtualProtect(addr, size, newProtect uintptr) (uintptr, error) {
	var old uint32
	r, _, err := procVirtualProtect.Call(addr, size, newProtect, uintptr(unsafe.Pointer(&old)))
	if r == 0 {
		return 0, err
	}
	return uintptr(old), nil
}

func flushInstructionCache(addr, size uintptr) {
	hProc, _, _ := procGetCurrentProcess.Call()
	_, _, _ = procFlushInstructionCache.Call(hProc, addr, size)
}

func loadLibraryExW(path string, flags uintptr) (uintptr, error) {
	ptr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	h, _, callErr := procLoadLibraryExW.Call(uintptr(unsafe.Pointer(ptr)), 0, flags)
	if h == 0 {
		return 0, fmt.Errorf("LoadLibraryExW(%q) : %w", path, callErr)
	}
	return h, nil
}

func getProcAddressName(module uintptr, name string) uintptr {
	b := append([]byte(name), 0)
	p, _, _ := procGetProcAddress.Call(module, uintptr(unsafe.Pointer(&b[0])))
	return p
}

func getProcAddressOrdinal(module uintptr, ordinal uint16) uintptr {
	p, _, _ := procGetProcAddress.Call(module, uintptr(ordinal))
	return p
}

// ptrFromAddr convertit une adresse virtuelle OS (hors tas Go, allouée par VirtualAlloc/LoadLibrary)
// en unsafe.Pointer sans déclencher de faux positif `go vet unsafeptr`.
func ptrFromAddr(addr uintptr) unsafe.Pointer {
	return *(*unsafe.Pointer)(unsafe.Pointer(&addr))
}
