//go:build windows

package gpu

import (
	_ "embed"
	"log/slog"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
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
	procPostMessage         = modUser32.NewProc("PostMessageW")
	procGetClientRect       = modUser32.NewProc("GetClientRect")
	procLoadCursorWin       = modUser32.NewProc("LoadCursorW")
	procSetCursorWin        = modUser32.NewProc("SetCursor")
	procGetWindowLongPtr    = modUser32.NewProc("GetWindowLongPtrW")
	procSetWindowLongPtr    = modUser32.NewProc("SetWindowLongPtrW")
	procGetWindowPlacement  = modUser32.NewProc("GetWindowPlacement")
	procSetWindowPlacement  = modUser32.NewProc("SetWindowPlacement")
	procMonitorFromWindow   = modUser32.NewProc("MonitorFromWindow")
	procGetMonitorInfo      = modUser32.NewProc("GetMonitorInfoW")
	procSetWindowPos        = modUser32.NewProc("SetWindowPos")
	procGetModuleHandle     = modKernel32Win.NewProc("GetModuleHandleW")
	procGetCurrentProcessId = modKernel32Win.NewProc("GetCurrentProcessId")
	procGetCurrentProcess   = modKernel32Win.NewProc("GetCurrentProcess")
	procTerminateProcess    = modKernel32Win.NewProc("TerminateProcess")
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

type winRect struct {
	Left, Top, Right, Bottom int32
}

type winPoint struct {
	X, Y int32
}

type winPlacement struct {
	Length         uint32
	Flags          uint32
	ShowCmd        uint32
	MinPosition    winPoint
	MaxPosition    winPoint
	NormalPosition winRect
}

type winMonitorInfo struct {
	Size    uint32
	Monitor winRect
	Work    winRect
	Flags   uint32
}

const (
	wmAppToggleFullscreen = 0x8001 // WM_APP + 1
	vkF11                 = 0x7A
	vkReturn              = 0x0D
)

var (
	winOnce           sync.Once
	winHWND           uintptr
	winHWNDAtomic     atomic.Uintptr
	winFullscreen     atomic.Bool
	winStartFull      atomic.Bool
	winSavedStyle     uintptr
	winSavedPlacement winPlacement
	winFSMu           sync.Mutex
	winFSOnChange     func(bool)
)

// ConfigureWindowFullscreen définit l'état plein écran initial (lu depuis astral.toml)
// et le callback appelé à chaque bascule utilisateur pour le mémoriser.
func ConfigureWindowFullscreen(initial bool, onChange func(bool)) {
	winStartFull.Store(initial)
	winFSMu.Lock()
	winFSOnChange = onChange
	winFSMu.Unlock()
}

// IsWindowFullscreen indique si la fenêtre ASTRAL est actuellement en plein écran sans bordure.
func IsWindowFullscreen() bool {
	return winFullscreen.Load()
}

// ToggleWindowFullscreen bascule la fenêtre principale ASTRAL entre fenêtré et plein écran sans bordure.
func ToggleWindowFullscreen() {
	if hwnd := winHWNDAtomic.Load(); hwnd != 0 {
		_, _, _ = procPostMessage.Call(hwnd, wmAppToggleFullscreen, 0, 0)
	}
}

func toggleFullscreenOnWindow(hwnd uintptr) {
	applyFullscreenOnWindow(hwnd, true)
}

func applyFullscreenOnWindow(hwnd uintptr, persist bool) {
	if hwnd == 0 {
		return
	}
	const (
		gwlStyle           = ^uintptr(15) // -16 (GWL_STYLE)
		wsOverlappedWindow = uintptr(0x00CF0000)
		monitorNearest     = uintptr(2)
		swpNoSize          = uintptr(0x0001)
		swpNoMove          = uintptr(0x0002)
		swpNoZOrder        = uintptr(0x0004)
		swpFrameChanged    = uintptr(0x0020)
		swpShowWindow      = uintptr(0x0040)
		swpNoOwnerZOrder   = uintptr(0x0200)
	)
	newState := false
	if !winFullscreen.Load() {
		style, _, _ := procGetWindowLongPtr.Call(hwnd, gwlStyle)
		winSavedStyle = style
		winSavedPlacement.Length = uint32(unsafe.Sizeof(winSavedPlacement))
		_, _, _ = procGetWindowPlacement.Call(hwnd, uintptr(unsafe.Pointer(&winSavedPlacement)))

		hMon, _, _ := procMonitorFromWindow.Call(hwnd, monitorNearest)
		var mi winMonitorInfo
		mi.Size = uint32(unsafe.Sizeof(mi))
		if ok, _, _ := procGetMonitorInfo.Call(hMon, uintptr(unsafe.Pointer(&mi))); ok != 0 {
			_, _, _ = procSetWindowLongPtr.Call(hwnd, gwlStyle, style&^wsOverlappedWindow)
			monW := uintptr(mi.Monitor.Right - mi.Monitor.Left)
			monH := uintptr(mi.Monitor.Bottom - mi.Monitor.Top)
			_, _, _ = procSetWindowPos.Call(
				hwnd, 0,
				uintptr(mi.Monitor.Left), uintptr(mi.Monitor.Top),
				monW, monH,
				swpNoOwnerZOrder|swpFrameChanged|swpShowWindow,
			)
			winFullscreen.Store(true)
			newState = true
			slog.Info("Fenêtre ASTRAL basculée en plein écran sans bordure", "w", monW, "h", monH)
		} else {
			return
		}
	} else {
		style := winSavedStyle
		if style == 0 {
			style = 0x10CF0000 // WS_OVERLAPPEDWINDOW | WS_VISIBLE
		}
		_, _, _ = procSetWindowLongPtr.Call(hwnd, gwlStyle, style)
		if winSavedPlacement.Length != 0 {
			_, _, _ = procSetWindowPlacement.Call(hwnd, uintptr(unsafe.Pointer(&winSavedPlacement)))
		}
		_, _, _ = procSetWindowPos.Call(
			hwnd, 0, 0, 0, 0, 0,
			swpNoMove|swpNoSize|swpNoZOrder|swpNoOwnerZOrder|swpFrameChanged,
		)
		winFullscreen.Store(false)
		newState = false
		slog.Info("Fenêtre ASTRAL revenue en mode fenêtré")
	}
	if persist {
		winFSMu.Lock()
		cb := winFSOnChange
		winFSMu.Unlock()
		if cb != nil {
			go cb(newState)
		}
	}
}

// MainWindowHWND retourne le HWND de la fenêtre principale ASTRAL si elle est déjà créée.
func MainWindowHWND() uintptr {
	return winHWNDAtomic.Load()
}

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

			hCurArrow, _, _ := procLoadCursorWin.Call(0, 32512 /* IDC_ARROW */)
			hCurHand, _, _ := procLoadCursorWin.Call(0, 32649 /* IDC_HAND */)
			clientToBackbuf := func(hwnd uintptr, lParam uintptr) (int, int) {
				cx := int(int16(lParam & 0xFFFF))
				cy := int(int16((lParam >> 16) & 0xFFFF))
				var rc [4]int32
				_, _, _ = procGetClientRect.Call(hwnd, uintptr(unsafe.Pointer(&rc[0])))
				cw := int(rc[2] - rc[0])
				ch := int(rc[3] - rc[1])
				if cw <= 0 || ch <= 0 {
					return cx, cy
				}
				return cx * width / cw, cy * height / ch
			}

			wndProc := syscall.NewCallback(func(hwnd, msg, wParam, lParam uintptr) uintptr {
				switch msg {
				case 0x0202: // WM_LBUTTONUP
					if !IsIntroReached() {
						bx, by := clientToBackbuf(hwnd, lParam)
						if IsPointInAudioButton(bx, by, width, height) {
							ToggleLoadingAudio()
							return 0
						}
						if IsPointInFullscreenButton(bx, by, width, height) {
							toggleFullscreenOnWindow(hwnd)
							return 0
						}
					}
				case 0x0200: // WM_MOUSEMOVE
					if !IsIntroReached() {
						bx, by := clientToBackbuf(hwnd, lParam)
						if (IsPointInAudioButton(bx, by, width, height) || IsPointInFullscreenButton(bx, by, width, height)) && hCurHand != 0 {
							_, _, _ = procSetCursorWin.Call(hCurHand)
						} else if hCurArrow != 0 {
							_, _, _ = procSetCursorWin.Call(hCurArrow)
						}
					}
				case 0x0100: // WM_KEYDOWN
					isRepeat := (lParam & (1 << 30)) != 0
					if wParam == vkF11 && !isRepeat {
						toggleFullscreenOnWindow(hwnd)
						return 0
					}
					if !IsIntroReached() && wParam == 'M' && !isRepeat {
						ToggleLoadingAudio()
						return 0
					}
				case 0x0104: // WM_SYSKEYDOWN (Alt+Entrée)
					isRepeat := (lParam & (1 << 30)) != 0
					altDown := (lParam & (1 << 29)) != 0
					if wParam == vkReturn && altDown && !isRepeat {
						toggleFullscreenOnWindow(hwnd)
						return 0
					}
				case wmAppToggleFullscreen:
					toggleFullscreenOnWindow(hwnd)
					return 0
				case 0x0002: // WM_DESTROY
					StopLoadingAudio()
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
				winHWNDAtomic.Store(hwnd)
				const wmSetIcon = 0x0080
				if hIconBig != 0 {
					_, _, _ = procSendMessage.Call(hwnd, wmSetIcon, 1 /* ICON_BIG */, hIconBig)
				}
				if hIconSmall != 0 {
					_, _, _ = procSendMessage.Call(hwnd, wmSetIcon, 0 /* ICON_SMALL */, hIconSmall)
				}
				_, _, _ = procShowWindow.Call(hwnd, 5 /* SW_SHOW */)
				if winStartFull.Load() {
					applyFullscreenOnWindow(hwnd, false)
				}
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
			// La fenêtre principale ASTRAL a été fermée par l'utilisateur : terminer
			// proprement et immédiatement le processus (avec sécurité TerminateProcess
			// si un thread invité bloque ExitProcess) afin qu'astral.exe ne reste jamais
			// actif en arrière-plan.
			slog.Info("fenêtre Win32 ASTRAL fermée — arrêt du processus")
			go func() {
				time.Sleep(250 * time.Millisecond)
				curProc, _, _ := procGetCurrentProcess.Call()
				_, _, _ = procTerminateProcess.Call(curProc, 0)
			}()
			os.Exit(0)
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
