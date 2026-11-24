//go:build windows

package gpu

import (
	"log/slog"
	"runtime"
	"sync"
	"syscall"
	"unsafe"
)

var (
	modUser32           = syscall.NewLazyDLL("user32.dll")
	modKernel32Win      = syscall.NewLazyDLL("kernel32.dll")
	procRegisterClassEx = modUser32.NewProc("RegisterClassExW")
	procCreateWindowEx  = modUser32.NewProc("CreateWindowExW")
	procShowWindow      = modUser32.NewProc("ShowWindow")
	procUpdateWindow    = modUser32.NewProc("UpdateWindow")
	procDefWindowProc   = modUser32.NewProc("DefWindowProcW")
	procGetMessage      = modUser32.NewProc("GetMessageW")
	procTranslateMsg    = modUser32.NewProc("TranslateMessage")
	procDispatchMsg     = modUser32.NewProc("DispatchMessageW")
	procPostQuitMsg     = modUser32.NewProc("PostQuitMessage")
	procGetModuleHandle = modKernel32Win.NewProc("GetModuleHandleW")
)

type wndClassExW struct {
	Size       uint32
	Style      uint32
	WndProc    uintptr
	ClsExtra   int32
	WndExtra   int32
	Instance   uintptr
	Icon       uintptr
	Cursor     uintptr
	Background uintptr
	MenuName   *uint16
	ClassName  *uint16
	IconSm     uintptr
}

type winMsg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	PtX     int32
	PtY     int32
}

var (
	winOnce sync.Once
	winHWND uintptr
)

// EnsureWindow crée (une seule fois) la fenêtre Win32 native d'affichage pour le backend GPU
// sur un thread OS dédié qui pompe les messages Windows.
func EnsureWindow(title string, width, height int) uintptr {
	winOnce.Do(func() {
		ready := make(chan uintptr, 1)
		go func() {
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()

			hInst, _, _ := procGetModuleHandle.Call(0)
			className, _ := syscall.UTF16PtrFromString("AstralGameWindowClass")
			titlePtr, _ := syscall.UTF16PtrFromString(title)

			wndProc := syscall.NewCallback(func(hwnd, msg, wParam, lParam uintptr) uintptr {
				if msg == 0x0002 { // WM_DESTROY
					_, _, _ = procPostQuitMsg.Call(0)
					return 0
				}
				r, _, _ := procDefWindowProc.Call(hwnd, msg, wParam, lParam)
				return r
			})

			wc := wndClassExW{
				Size:       uint32(unsafe.Sizeof(wndClassExW{})),
				Style:      0x0003, // CS_HREDRAW | CS_VREDRAW
				WndProc:    wndProc,
				Instance:   hInst,
				Background: 5, // COLOR_WINDOW
				ClassName:  className,
			}
			_, _, _ = procRegisterClassEx.Call(uintptr(unsafe.Pointer(&wc)))

			const wsOverlappedWindow = 0x00CF0000
			const cwUseDefault = 0x80000000
			hwnd, _, _ := procCreateWindowEx.Call(
				0,
				uintptr(unsafe.Pointer(className)),
				uintptr(unsafe.Pointer(titlePtr)),
				wsOverlappedWindow,
				cwUseDefault, cwUseDefault,
				uintptr(width), uintptr(height),
				0, 0, hInst, 0,
			)
			if hwnd != 0 {
				_, _, _ = procShowWindow.Call(hwnd, 5 /* SW_SHOW */)
				_, _, _ = procUpdateWindow.Call(hwnd)
				slog.Info("fenêtre Win32 ASTRAL ouverte", "hwnd", hwnd, "width", width, "height", height)
			}
			ready <- hwnd

			var m winMsg
			for {
				ret, _, _ := procGetMessage.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
				if int32(ret) <= 0 {
					break
				}
				_, _, _ = procTranslateMsg.Call(uintptr(unsafe.Pointer(&m)))
				_, _, _ = procDispatchMsg.Call(uintptr(unsafe.Pointer(&m)))
			}
		}()
		winHWND = <-ready
	})
	return winHWND
}
