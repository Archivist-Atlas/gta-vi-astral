//go:build windows

package gpu

import (
	_ "embed"
	"log/slog"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

//go:embed astral_icon_256.png
var astralIcon256 []byte

//go:embed astral_icon_32.png
var astralIcon32 []byte

var (
	modUser32               = syscall.NewLazyDLL("user32.dll")
	modKernel32Win          = syscall.NewLazyDLL("kernel32.dll")
	procRegisterClassEx     = modUser32.NewProc("RegisterClassExW")
	procCreateWindowEx      = modUser32.NewProc("CreateWindowExW")
	procShowWindow          = modUser32.NewProc("ShowWindow")
	procUpdateWindow        = modUser32.NewProc("UpdateWindow")
	procDefWindowProc       = modUser32.NewProc("DefWindowProcW")
	procGetMessage          = modUser32.NewProc("GetMessageW")
	procTranslateMsg        = modUser32.NewProc("TranslateMessage")
	procDispatchMsg         = modUser32.NewProc("DispatchMessageW")
	procPostQuitMsg         = modUser32.NewProc("PostQuitMessage")
	procEnumWindows         = modUser32.NewProc("EnumWindows")
	procGetWindowThreadPID  = modUser32.NewProc("GetWindowThreadProcessId")
	procIsWindowVisible     = modUser32.NewProc("IsWindowVisible")
	procCreateIconFromResEx = modUser32.NewProc("CreateIconFromResourceEx")
	procSendMessage         = modUser32.NewProc("SendMessageW")
	procGetModuleHandle     = modKernel32Win.NewProc("GetModuleHandleW")
	procGetCurrentProcessId = modKernel32Win.NewProc("GetCurrentProcessId")
)

func createIconFromPNG(pngData []byte) uintptr {
	if len(pngData) == 0 {
		return 0
	}
	hIcon, _, _ := procCreateIconFromResEx.Call(
		uintptr(unsafe.Pointer(&pngData[0])),
		uintptr(len(pngData)),
		1,          // fIcon = TRUE
		0x00030000, // dwVer = 3.0 (PNG support Vista+)
		0, 0,       // taille par défaut adaptée au DPI système
		0,          // LR_DEFAULTCOLOR
	)
	return hIcon
}

// WindowIcons retourne les handles HICON (grand pour la barre des tâches, petit pour la barre de titre)
// générés depuis l'icône GTA VI Astral embarquée.
func WindowIcons() (big, small uintptr) {
	big = createIconFromPNG(astralIcon256)
	small = createIconFromPNG(astralIcon32)
	if small == 0 {
		small = big
	}
	return big, small
}

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

			hIconBig := createIconFromPNG(astralIcon256)
			hIconSmall := createIconFromPNG(astralIcon32)
			if hIconSmall == 0 {
				hIconSmall = hIconBig
			}

			wc := wndClassExW{
				Size:       uint32(unsafe.Sizeof(wndClassExW{})),
				Style:      0x0003, // CS_HREDRAW | CS_VREDRAW
				WndProc:    wndProc,
				Instance:   hInst,
				Icon:       hIconBig,
				IconSm:     hIconSmall,
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
				const wmSetIcon = 0x0080
				if hIconBig != 0 {
					_, _, _ = procSendMessage.Call(hwnd, wmSetIcon, 1 /* ICON_BIG */, hIconBig)
				}
				if hIconSmall != 0 {
					_, _, _ = procSendMessage.Call(hwnd, wmSetIcon, 0 /* ICON_SMALL */, hIconSmall)
				}
				_, _, _ = procShowWindow.Call(hwnd, 5 /* SW_SHOW */)
				_, _, _ = procUpdateWindow.Call(hwnd)
				slog.Info("fenêtre Win32 ASTRAL ouverte", "hwnd", hwnd, "width", width, "height", height, "icon", hIconBig != 0)
			}
			ready <- hwnd
			// Masque discrètement toute fenêtre Win32 en double créée par l'exécutable invité
			// dans le même processus (sans la détruire, afin que son HWND reste valide pour le moteur).
			go hideDuplicateProcessWindows(hwnd)

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

func hideDuplicateProcessWindows(keepHWND uintptr) {
	if keepHWND == 0 {
		return
	}
	myPID, _, _ := procGetCurrentProcessId.Call()
	cb := syscall.NewCallback(func(hwnd, lParam uintptr) uintptr {
		if hwnd == 0 || hwnd == keepHWND {
			return 1
		}
		var pid uint32
		_, _, _ = procGetWindowThreadPID.Call(hwnd, uintptr(unsafe.Pointer(&pid)))
		if uintptr(pid) == myPID {
			vis, _, _ := procIsWindowVisible.Call(hwnd)
			if vis != 0 {
				_, _, _ = procShowWindow.Call(hwnd, 0 /* SW_HIDE */)
			}
		}
		return 1
	})
	for i := 0; i < 20; i++ {
		time.Sleep(500 * time.Millisecond)
		_, _, _ = procEnumWindows.Call(cb, 0)
	}
}
