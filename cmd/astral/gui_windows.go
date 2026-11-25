//go:build windows

package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/Archivist-Atlas/gta-vi-astral/pkg/core"
	"github.com/Archivist-Atlas/gta-vi-astral/pkg/game"
	"github.com/Archivist-Atlas/gta-vi-astral/pkg/gpu"
)

var (
	procAttachConsole = guiKernel32.NewProc("AttachConsole")
	procGetStdHandle  = guiKernel32.NewProc("GetStdHandle")
	procGetFileType   = guiKernel32.NewProc("GetFileType")
	guiUser32         = syscall.NewLazyDLL("user32.dll")
	guiGdi32    = syscall.NewLazyDLL("gdi32.dll")
	guiKernel32 = syscall.NewLazyDLL("kernel32.dll")
	guiShell32  = syscall.NewLazyDLL("shell32.dll")
	guiOle32    = syscall.NewLazyDLL("ole32.dll")
	guiDwmApi   = syscall.NewLazyDLL("dwmapi.dll")

	procRegClassEx       = guiUser32.NewProc("RegisterClassExW")
	procCreateWinEx      = guiUser32.NewProc("CreateWindowExW")
	procShowWin          = guiUser32.NewProc("ShowWindow")
	procUpdateWin        = guiUser32.NewProc("UpdateWindow")
	procDefWinProc       = guiUser32.NewProc("DefWindowProcW")
	procGetMsg           = guiUser32.NewProc("GetMessageW")
	procTransMsg         = guiUser32.NewProc("TranslateMessage")
	procDispMsg          = guiUser32.NewProc("DispatchMessageW")
	procPostQuit         = guiUser32.NewProc("PostQuitMessage")
	procSendMsg          = guiUser32.NewProc("SendMessageW")
	procPostMsg          = guiUser32.NewProc("PostMessageW")
	procSetWinText       = guiUser32.NewProc("SetWindowTextW")
	procEnableWin        = guiUser32.NewProc("EnableWindow")
	procGetSysMetrics    = guiUser32.NewProc("GetSystemMetrics")
	procLoadCursor       = guiUser32.NewProc("LoadCursorW")
	procInvalidateRect   = guiUser32.NewProc("InvalidateRect")
	procMessageBox       = guiUser32.NewProc("MessageBoxW")
	procCreateSolidBrush = guiGdi32.NewProc("CreateSolidBrush")
	procCreateFont       = guiGdi32.NewProc("CreateFontW")
	procSetTextColor     = guiGdi32.NewProc("SetTextColor")
	procSetBkColor       = guiGdi32.NewProc("SetBkColor")
	procSetBkMode        = guiGdi32.NewProc("SetBkMode")
	procGetModHandle     = guiKernel32.NewProc("GetModuleHandleW")
	procGetConsoleWin    = guiKernel32.NewProc("GetConsoleWindow")
	procGetConsoleProcs  = guiKernel32.NewProc("GetConsoleProcessList")
	procSHBrowseFolder   = guiShell32.NewProc("SHBrowseForFolderW")
	procSHGetPathFromID  = guiShell32.NewProc("SHGetPathFromIDListW")
	procCoInitEx         = guiOle32.NewProc("CoInitializeEx")
	procCoTaskMemFree    = guiOle32.NewProc("CoTaskMemFree")
	procDwmSetWinAttr    = guiDwmApi.NewProc("DwmSetWindowAttribute")
)

const (
	wmDestroy        = 0x0002
	wmSetFont        = 0x0030
	wmSetIcon        = 0x0080
	wmCommand        = 0x0111
	wmCtlColorEdit   = 0x0133
	wmCtlColorStatic = 0x0138
	wmAppUpdateUI    = 0x8001

	emSetSel       = 0x00B1
	emReplaceSel   = 0x00C2
	emScrollCaret  = 0x00B7
	wmGetTextLen   = 0x000E

	idBtnBrowse  = 1001
	idBtnStart   = 1002
	idBtnVerify  = 1003
	idBtnInspect = 1004
	idBtnCensus  = 1005
	idBtnShaders = 1006
	idBtnMap     = 1007
	idBtnConfig  = 1008
	idBtnClear   = 1009
)

type guiWndClassExW struct {
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

type guiMsg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	PtX     int32
	PtY     int32
}

type browseInfoW struct {
	HwndOwner      uintptr
	PidlRoot       uintptr
	DisplayName    *uint16
	Title          *uint16
	Flags          uint32
	Callback       uintptr
	LParam         uintptr
	Image          int32
	_pad           int32
}

// launcherState mémorise hors dépôt les vérifications déjà effectuées pour éviter de les relancer à chaque démarrage.
type launcherState struct {
	VerifiedDir string `json:"verified_dir"`
	VerifiedAt  string `json:"verified_at"`
	Summary     string `json:"summary"`
}

type guiApp struct {
	hwnd       uintptr
	lblStatus  uintptr
	edtGameDir uintptr
	btnStart   uintptr
	btnVerify  uintptr
	btnInspect uintptr
	btnCensus  uintptr
	btnShaders uintptr
	btnMap     uintptr
	btnConfig  uintptr
	btnBrowse  uintptr
	edtLog     uintptr

	brushBg     uintptr
	brushCard   uintptr
	fontTitle   uintptr
	fontSub     uintptr
	fontUI      uintptr
	fontBtnBold uintptr
	fontMono    uintptr

	mu        sync.Mutex
	cfg       core.Config
	gameDir   string
	statePath string
	state     launcherState
	busy      bool
	gameCmd   *exec.Cmd
	pending   []func()
}

// attachParentConsoleIfCLI rattache la console parente lorsqu'astral.exe (compilé en windowsgui)
// est lancé en ligne de commande depuis PowerShell/CMD sans redirection de pipe.
func attachParentConsoleIfCLI() {
	// Si stdout/stderr sont déjà un pipe ou un fichier (lanceur GUI ou redirection), ne rien toucher.
	const stdOutHandle = ^uintptr(10) // STD_OUTPUT_HANDLE (-11)
	hOut, _, _ := procGetStdHandle.Call(stdOutHandle)
	if hOut != 0 && hOut != ^uintptr(0) {
		ft, _, _ := procGetFileType.Call(hOut)
		if ft == 2 || ft == 3 || ft == 1 { // CHAR, PIPE, DISK déjà valides
			return
		}
	}
	const attachParentProcess = ^uintptr(0) // (DWORD)-1
	r, _, _ := procAttachConsole.Call(attachParentProcess)
	if r == 0 {
		return
	}
	if f, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
		os.Stdout = f
		os.Stderr = f
	}
}

func runDefaultUI() {
	hideStandaloneConsole()
	runLauncherGUI()
}

// hideStandaloneConsole masque la fenêtre console noire si astral.exe a été double-cliqué depuis l'Explorateur,
// tout en laissant intact le terminal si l'utilisateur l'a lancé depuis PowerShell/cmd.
func hideStandaloneConsole() {
	conWnd, _, _ := procGetConsoleWin.Call()
	if conWnd == 0 {
		return
	}
	var pids [4]uint32
	n, _, _ := procGetConsoleProcs.Call(uintptr(unsafe.Pointer(&pids[0])), 4)
	if n == 1 {
		_, _, _ = procShowWin.Call(conWnd, 0 /* SW_HIDE */)
	}
}

func runLauncherGUI() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	_, _, _ = procCoInitEx.Call(0, 2 /* COINIT_APARTMENTTHREADED */)

	cfg, _ := core.LoadConfig("")
	app := &guiApp{
		cfg:       cfg,
		gameDir:   cfg.Paths.GameDir,
		statePath: filepath.Join(cfg.Paths.DataDir, "launcher_state.json"),
	}
	app.loadState()

	// Couleurs sombres synthwave (BGR pour Win32 COLORREF : 0x00BBGGRR)
	app.brushBg, _, _ = procCreateSolidBrush.Call(0x001C1312)   // #12131C
	app.brushCard, _, _ = procCreateSolidBrush.Call(0x00281D1B) // #1B1D28
	app.fontTitle = makeFont("Segoe UI", 24, 700)
	app.fontSub = makeFont("Segoe UI", 14, 400)
	app.fontUI = makeFont("Segoe UI", 15, 400)
	app.fontBtnBold = makeFont("Segoe UI", 17, 700)
	app.fontMono = makeFont("Consolas", 14, 400)

	hInst, _, _ := procGetModHandle.Call(0)
	className, _ := syscall.UTF16PtrFromString("AstralLauncherWindowClass")
	titlePtr, _ := syscall.UTF16PtrFromString(fmt.Sprintf("GTA VI Astral %s — Lanceur", version))
	cursor, _, _ := procLoadCursor.Call(0, 32512 /* IDC_ARROW */)
	hIconBig, hIconSmall := gpu.WindowIcons()

	wndProc := syscall.NewCallback(func(hwnd, msg, wParam, lParam uintptr) uintptr {
		switch msg {
		case wmAppUpdateUI:
			app.drainPending()
			return 0
		case wmCommand:
			id := uint16(wParam & 0xFFFF)
			app.onCommand(id)
			return 0
		case wmCtlColorStatic:
			hdc := wParam
			ctrl := lParam
			_, _, _ = procSetBkMode.Call(hdc, 1 /* TRANSPARENT */)
			if ctrl == app.lblStatus {
				if app.isCurrentDirVerified() {
					_, _, _ = procSetTextColor.Call(hdc, 0x008CE050) // vert clair
				} else {
					_, _, _ = procSetTextColor.Call(hdc, 0x0060C8FF) // ambre/corail
				}
			} else {
				_, _, _ = procSetTextColor.Call(hdc, 0x00F4EAEA) // blanc cassé
			}
			_, _, _ = procSetBkColor.Call(hdc, 0x001C1312)
			return app.brushBg
		case wmCtlColorEdit:
			hdc := wParam
			_, _, _ = procSetTextColor.Call(hdc, 0x00F0E8E8)
			_, _, _ = procSetBkColor.Call(hdc, 0x00281D1B)
			return app.brushCard
		case wmDestroy:
			app.stopGameIfRunning()
			_, _, _ = procPostQuit.Call(0)
			return 0
		}
		r, _, _ := procDefWinProc.Call(hwnd, msg, wParam, lParam)
		return r
	})

	wc := guiWndClassExW{
		Size:       uint32(unsafe.Sizeof(guiWndClassExW{})),
		Style:      0x0003,
		WndProc:    wndProc,
		Instance:   hInst,
		Icon:       hIconBig,
		IconSm:     hIconSmall,
		Cursor:     cursor,
		Background: app.brushBg,
		ClassName:  className,
	}
	_, _, _ = procRegClassEx.Call(uintptr(unsafe.Pointer(&wc)))

	const winW, winH = 860, 650
	scrW, _, _ := procGetSysMetrics.Call(0)
	scrH, _, _ := procGetSysMetrics.Call(1)
	posX := (int(scrW) - winW) / 2
	posY := (int(scrH) - winH) / 2
	if posX < 0 {
		posX = 40
	}
	if posY < 0 {
		posY = 40
	}

	const wsFixedWindow = 0x00CA0000 | 0x02000000 // WS_OVERLAPPED|CAPTION|SYSMENU|MINIMIZEBOX|CLIPCHILDREN
	hwnd, _, _ := procCreateWinEx.Call(
		0,
		uintptr(unsafe.Pointer(className)),
		uintptr(unsafe.Pointer(titlePtr)),
		wsFixedWindow,
		uintptr(posX), uintptr(posY),
		winW, winH,
		0, 0, hInst, 0,
	)
	if hwnd == 0 {
		return
	}
	app.hwnd = hwnd

	if hIconBig != 0 {
		_, _, _ = procSendMsg.Call(hwnd, wmSetIcon, 1, hIconBig)
	}
	if hIconSmall != 0 {
		_, _, _ = procSendMsg.Call(hwnd, wmSetIcon, 0, hIconSmall)
	}
	// Barre de titre sombre Windows 10/11 (DWMWA_USE_IMMERSIVE_DARK_MODE = 20)
	dark := int32(1)
	_, _, _ = procDwmSetWinAttr.Call(hwnd, 20, uintptr(unsafe.Pointer(&dark)), 4)

	app.buildControls(hInst)
	app.refreshStatusUI()

	if app.gameDir == "" {
		app.appendLog("Bienvenue dans GTA VI Astral !\r\n")
		app.appendLog("Cliquez sur « Parcourir… » pour sélectionner le dossier de votre copie locale de GTA VI (Xbox Series).\r\n")
		app.appendLog("Le chemin et la vérification seront mémorisés automatiquement.\r\n")
	} else if app.isCurrentDirVerified() {
		app.appendLog(fmt.Sprintf("Prêt : copie vérifiée le %s (%s).\r\n", app.state.VerifiedAt, app.state.Summary))
		app.appendLog("Cliquez sur « DÉMARRER GTA VI » pour lancer le jeu immédiatement.\r\n")
	} else {
		app.appendLog(fmt.Sprintf("Dossier mémorisé : %s\r\n", app.gameDir))
		app.appendLog("Une vérification unique sera effectuée automatiquement au premier lancement.\r\n")
	}

	_, _, _ = procShowWin.Call(hwnd, 5 /* SW_SHOW */)
	_, _, _ = procUpdateWin.Call(hwnd)

	var m guiMsg
	for {
		ret, _, _ := procGetMsg.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		if int32(ret) <= 0 {
			break
		}
		_, _, _ = procTransMsg.Call(uintptr(unsafe.Pointer(&m)))
		_, _, _ = procDispMsg.Call(uintptr(unsafe.Pointer(&m)))
	}
}

func (a *guiApp) buildControls(hInst uintptr) {
	const (
		wsChild    = 0x40000000
		wsVisible  = 0x10000000
		wsTabStop  = 0x00010000
		wsVScroll  = 0x00200000
		wsBorder   = 0x00800000
		esReadOnly = 0x0800
		esMulti    = 0x0004
		esAutoVScr = 0x0040
		bsPush     = 0x0000
		bsDefPush  = 0x0001
	)

	// En-tête
	lblTitle := createCtrl(a.hwnd, hInst, "STATIC", "GTA VI Astral", wsChild|wsVisible, 24, 16, 400, 32, 0, a.fontTitle)
	_ = lblTitle
	a.lblStatus = createCtrl(a.hwnd, hInst, "STATIC", "", wsChild|wsVisible, 24, 50, 790, 22, 0, a.fontSub)

	// Ligne dossier du jeu
	_ = createCtrl(a.hwnd, hInst, "STATIC", "Dossier du jeu GTA VI (mémorisé dans astral.toml) :", wsChild|wsVisible, 24, 82, 500, 20, 0, a.fontSub)
	a.edtGameDir = createCtrl(a.hwnd, hInst, "EDIT", orNone(a.gameDir), wsChild|wsVisible|wsBorder|esReadOnly, 24, 106, 650, 28, 0, a.fontUI)
	a.btnBrowse = createCtrl(a.hwnd, hInst, "BUTTON", "Parcourir…", wsChild|wsVisible|wsTabStop|bsPush, 686, 105, 134, 30, idBtnBrowse, a.fontUI)

	// Bouton principal Démarrer le jeu
	a.btnStart = createCtrl(a.hwnd, hInst, "BUTTON", "▶   DÉMARRER GTA VI", wsChild|wsVisible|wsTabStop|bsDefPush, 24, 148, 796, 46, idBtnStart, a.fontBtnBold)

	// Grille d'outils (équivalents des options 2..8 du menu console)
	_ = createCtrl(a.hwnd, hInst, "STATIC", "Outils d'analyse & diagnostics :", wsChild|wsVisible, 24, 206, 400, 20, 0, a.fontSub)

	const bw, bh, gapX, gapY = 258, 34, 11, 10
	y1 := 230
	y2 := y1 + bh + gapY
	a.btnVerify = createCtrl(a.hwnd, hInst, "BUTTON", "Vérifier la copie (verify)", wsChild|wsVisible|wsTabStop|bsPush, 24, y1, bw, bh, idBtnVerify, a.fontUI)
	a.btnInspect = createCtrl(a.hwnd, hInst, "BUTTON", "Inspecter l'exécutable (inspect)", wsChild|wsVisible|wsTabStop|bsPush, 24+bw+gapX, y1, bw, bh, idBtnInspect, a.fontUI)
	a.btnCensus = createCtrl(a.hwnd, hInst, "BUTTON", "Recenser les imports (census)", wsChild|wsVisible|wsTabStop|bsPush, 24+2*(bw+gapX), y1, bw, bh, idBtnCensus, a.fontUI)

	a.btnShaders = createCtrl(a.hwnd, hInst, "BUTTON", "Analyser les shaders (shader-info)", wsChild|wsVisible|wsTabStop|bsPush, 24, y2, bw, bh, idBtnShaders, a.fontUI)
	a.btnMap = createCtrl(a.hwnd, hInst, "BUTTON", "Mapper TLS + IAT (run -map)", wsChild|wsVisible|wsTabStop|bsPush, 24+bw+gapX, y2, bw, bh, idBtnMap, a.fontUI)
	a.btnConfig = createCtrl(a.hwnd, hInst, "BUTTON", "Configuration (config)", wsChild|wsVisible|wsTabStop|bsPush, 24+2*(bw+gapX), y2, bw, bh, idBtnConfig, a.fontUI)

	// Journal en direct
	_ = createCtrl(a.hwnd, hInst, "STATIC", "Journal d'exécution :", wsChild|wsVisible, 24, 320, 300, 20, 0, a.fontSub)
	_ = createCtrl(a.hwnd, hInst, "BUTTON", "Effacer", wsChild|wsVisible|wsTabStop|bsPush, 730, 316, 90, 24, idBtnClear, a.fontSub)
	a.edtLog = createCtrl(a.hwnd, hInst, "EDIT", "", wsChild|wsVisible|wsBorder|wsVScroll|esMulti|esAutoVScr|esReadOnly, 24, 344, 796, 252, 0, a.fontMono)
}

func (a *guiApp) onCommand(id uint16) {
	switch id {
	case idBtnBrowse:
		a.pickGameFolder(false)
	case idBtnStart:
		a.mu.Lock()
		running := a.gameCmd != nil
		a.mu.Unlock()
		if running {
			a.stopGameIfRunning()
			return
		}
		a.startGameFlow()
	case idBtnVerify:
		if a.ensureGameDir() {
			a.runVerifyAsync(false)
		}
	case idBtnInspect:
		if a.ensureGameDir() {
			a.runToolAsync("Inspection PE32+", "inspect", filepath.Join(a.gameDir, game.ExpectedExecutable))
		}
	case idBtnCensus:
		if a.ensureGameDir() {
			a.runToolAsync("Recensement IAT", "census", filepath.Join(a.gameDir, game.ExpectedExecutable))
		}
	case idBtnShaders:
		if a.ensureGameDir() {
			a.runToolAsync("Analyse shaders RDNA2 (500 premiers)", "shader-info", "-limit", "500", filepath.Join(a.gameDir, "shaders.rpf"))
		}
	case idBtnMap:
		if a.ensureGameDir() {
			a.runToolAsync("Mappage natif TLS + IAT", "run", "-map", filepath.Join(a.gameDir, game.ExpectedExecutable))
		}
	case idBtnConfig:
		a.runToolAsync("Configuration active", "config")
	case idBtnClear:
		setText(a.edtLog, "")
	}
}

func (a *guiApp) ensureGameDir() bool {
	if a.gameDir != "" {
		return true
	}
	return a.pickGameFolder(false)
}

func (a *guiApp) pickGameFolder(autoLaunchAfter bool) bool {
	dir := browseForFolder(a.hwnd, "Sélectionnez le dossier contenant votre copie de GTA VI (game_scarlett_master.exe)")
	if dir == "" {
		return false
	}
	savedPath, err := core.SaveGameDir(dir)
	if err != nil {
		a.appendLog(fmt.Sprintf("Erreur sauvegarde astral.toml : %v\r\n", err))
		return false
	}
	a.gameDir = dir
	setText(a.edtGameDir, dir)
	a.appendLog(fmt.Sprintf("Dossier GTA VI mémorisé dans %s : %s\r\n", savedPath, dir))
	a.refreshStatusUI()

	if !a.isCurrentDirVerified() {
		a.runVerifyAsync(autoLaunchAfter)
	} else if autoLaunchAfter {
		a.launchGameProcess()
	}
	return true
}

func (a *guiApp) startGameFlow() {
	if a.gameDir == "" {
		a.pickGameFolder(true)
		return
	}
	if !a.isCurrentDirVerified() {
		a.appendLog("Première exécution pour ce dossier : vérification unique de la copie du jeu…\r\n")
		a.runVerifyAsync(true)
		return
	}
	a.launchGameProcess()
}

func (a *guiApp) runVerifyAsync(launchAfter bool) {
	a.mu.Lock()
	if a.busy {
		a.mu.Unlock()
		return
	}
	a.busy = true
	a.mu.Unlock()
	a.setToolsEnabled(false)

	dir := a.gameDir
	a.appendLog(fmt.Sprintf("\r\n--- Vérification de la copie du jeu (%s) ---\r\n", dir))
	go func() {
		rep, err := game.VerifyPackage(dir)
		var out strings.Builder
		if rep != nil {
			fmt.Fprintf(&out, "Dossier jeu    : %s\r\n", rep.GameDir)
			fmt.Fprintf(&out, "Cible          : %s\r\n", game.TargetVersion)
			if rep.Metadata != nil {
				fmt.Fprintf(&out, "TitleId        : %s | StoreId : %s\r\n", rep.Metadata.TitleID, rep.Metadata.StoreID)
			}
			fmt.Fprintf(&out, "Fichiers clés  : %d / %d présents (%.2f GiB)\r\n",
				len(rep.PresentFiles), len(game.CorePackageFiles), float64(rep.TotalBytes)/(1024*1024*1024))
			if len(rep.MissingFiles) > 0 {
				fmt.Fprintf(&out, "Manquants      : %s\r\n", strings.Join(rep.MissingFiles, ", "))
			}
		}
		ok := err == nil && rep != nil && len(rep.MissingFiles) == 0
		if ok {
			out.WriteString("Statut         : VALIDE (mémorisé, ne sera plus redemandé pour ce dossier)\r\n")
			a.mu.Lock()
			a.state = launcherState{
				VerifiedDir: normalizeDir(dir),
				VerifiedAt:  time.Now().Format("02/01/2006 15:04"),
				Summary:     fmt.Sprintf("%d/%d fichiers, %.2f GiB", len(rep.PresentFiles), len(game.CorePackageFiles), float64(rep.TotalBytes)/(1024*1024*1024)),
			}
			a.saveState()
			a.mu.Unlock()
		} else {
			if err != nil {
				fmt.Fprintf(&out, "Erreur         : %v\r\n", err)
			} else {
				out.WriteString("Statut         : COPIE INCOMPLÈTE\r\n")
			}
		}

		a.postUI(func() {
			a.appendLog(out.String())
			a.mu.Lock()
			a.busy = false
			a.mu.Unlock()
			a.setToolsEnabled(true)
			a.refreshStatusUI()
			if ok && launchAfter {
				a.launchGameProcess()
			}
		})
	}()
}

func (a *guiApp) runToolAsync(label string, args ...string) {
	a.mu.Lock()
	if a.busy {
		a.mu.Unlock()
		return
	}
	a.busy = true
	a.mu.Unlock()
	a.setToolsEnabled(false)

	a.appendLog(fmt.Sprintf("\r\n--- %s ---\r\n", label))
	go func() {
		self, err := os.Executable()
		if err != nil {
			a.postUI(func() {
				a.appendLog(fmt.Sprintf("Erreur exécutable : %v\r\n", err))
				a.finishTool()
			})
			return
		}
		cmd := exec.Command(self, args...)
		cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000 /* CREATE_NO_WINDOW */}
		outBytes, runErr := cmd.CombinedOutput()
		text := strings.ReplaceAll(strings.ReplaceAll(string(outBytes), "\r\n", "\n"), "\n", "\r\n")
		a.postUI(func() {
			if text != "" {
				a.appendLog(text)
			}
			if runErr != nil {
				a.appendLog(fmt.Sprintf("Terminé avec erreur : %v\r\n", runErr))
			}
			a.finishTool()
		})
	}()
}

func (a *guiApp) finishTool() {
	a.mu.Lock()
	a.busy = false
	a.mu.Unlock()
	a.setToolsEnabled(true)
}

func (a *guiApp) launchGameProcess() {
	self, err := os.Executable()
	if err != nil {
		a.appendLog(fmt.Sprintf("Erreur exécutable : %v\r\n", err))
		return
	}
	exePath := filepath.Join(a.gameDir, game.ExpectedExecutable)
	a.appendLog(fmt.Sprintf("\r\n=== Démarrage natif de GTA VI (%s) ===\r\n", exePath))

	cmd := exec.Command(self, "run", "-execute", "-unimplemented", "log", exePath)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000 /* CREATE_NO_WINDOW */}
	pr, pw := io.Pipe()
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		_ = pr.Close()
		_ = pw.Close()
		a.appendLog(fmt.Sprintf("Échec du lancement : %v\r\n", err))
		return
	}

	a.mu.Lock()
	a.gameCmd = cmd
	a.busy = true
	a.mu.Unlock()
	a.setToolsEnabled(false)
	setText(a.btnStart, "■   ARRÊTER GTA VI")
	_, _, _ = procEnableWin.Call(a.btnStart, 1)

	go func() {
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64<<10), 256<<10)
		for sc.Scan() {
			line := sc.Text() + "\r\n"
			a.postUI(func() { a.appendLog(line) })
		}
		_ = pr.Close()
	}()

	go func() {
		waitErr := cmd.Wait()
		_ = pw.Close()
		a.postUI(func() {
			if waitErr != nil {
				a.appendLog(fmt.Sprintf("Session GTA VI terminée (%v).\r\n", waitErr))
			} else {
				a.appendLog("Session GTA VI terminée proprement.\r\n")
			}
			a.mu.Lock()
			a.gameCmd = nil
			a.busy = false
			a.mu.Unlock()
			setText(a.btnStart, "▶   DÉMARRER GTA VI")
			a.setToolsEnabled(true)
		})
	}()
}

func (a *guiApp) stopGameIfRunning() {
	a.mu.Lock()
	cmd := a.gameCmd
	a.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

func (a *guiApp) setToolsEnabled(enabled bool) {
	v := uintptr(0)
	if enabled {
		v = 1
	}
	for _, h := range []uintptr{a.btnBrowse, a.btnStart, a.btnVerify, a.btnInspect, a.btnCensus, a.btnShaders, a.btnMap, a.btnConfig} {
		if h != 0 {
			_, _, _ = procEnableWin.Call(h, v)
		}
	}
}

func (a *guiApp) refreshStatusUI() {
	var txt string
	switch {
	case a.gameDir == "":
		txt = "Statut : Aucun dossier GTA VI configuré — cliquez sur « Parcourir… »"
	case a.isCurrentDirVerified():
		txt = fmt.Sprintf("Statut : Copie GTA VI vérifiée (%s — %s)", a.state.Summary, a.state.VerifiedAt)
	default:
		txt = "Statut : Dossier configuré — vérification unique requise au premier lancement"
	}
	setText(a.lblStatus, txt)
	_, _, _ = procInvalidateRect.Call(a.lblStatus, 0, 1)
}

func (a *guiApp) isCurrentDirVerified() bool {
	if a.gameDir == "" || a.state.VerifiedDir == "" {
		return false
	}
	return normalizeDir(a.gameDir) == normalizeDir(a.state.VerifiedDir)
}

func (a *guiApp) loadState() {
	b, err := os.ReadFile(a.statePath)
	if err != nil {
		return
	}
	_ = json.Unmarshal(b, &a.state)
}

func (a *guiApp) saveState() {
	_ = os.MkdirAll(filepath.Dir(a.statePath), 0o755)
	if b, err := json.MarshalIndent(a.state, "", "  "); err == nil {
		_ = os.WriteFile(a.statePath, b, 0o644)
	}
}

func (a *guiApp) postUI(fn func()) {
	a.mu.Lock()
	a.pending = append(a.pending, fn)
	a.mu.Unlock()
	_, _, _ = procPostMsg.Call(a.hwnd, wmAppUpdateUI, 0, 0)
}

func (a *guiApp) drainPending() {
	a.mu.Lock()
	fns := a.pending
	a.pending = nil
	a.mu.Unlock()
	for _, fn := range fns {
		fn()
	}
}

func (a *guiApp) appendLog(s string) {
	if a.edtLog == 0 || s == "" {
		return
	}
	length, _, _ := procSendMsg.Call(a.edtLog, wmGetTextLen, 0, 0)
	if length > 24000 {
		// Garde les logs rapides et réactifs sur les longues sessions
		setText(a.edtLog, "")
		length = 0
	}
	_, _, _ = procSendMsg.Call(a.edtLog, emSetSel, length, length)
	p, _ := syscall.UTF16PtrFromString(s)
	_, _, _ = procSendMsg.Call(a.edtLog, emReplaceSel, 0, uintptr(unsafe.Pointer(p)))
	_, _, _ = procSendMsg.Call(a.edtLog, emScrollCaret, 0, 0)
}

func normalizeDir(p string) string {
	return strings.ToLower(filepath.Clean(strings.TrimSpace(p)))
}

func setText(hwnd uintptr, s string) {
	if hwnd == 0 {
		return
	}
	p, _ := syscall.UTF16PtrFromString(s)
	_, _, _ = procSetWinText.Call(hwnd, uintptr(unsafe.Pointer(p)))
}

func makeFont(face string, height, weight int32) uintptr {
	p, _ := syscall.UTF16PtrFromString(face)
	h, _, _ := procCreateFont.Call(
		uintptr(-height), 0, 0, 0,
		uintptr(weight),
		0, 0, 0,
		1, // DEFAULT_CHARSET
		0, 0,
		5, // CLEARTYPE_QUALITY
		0,
		uintptr(unsafe.Pointer(p)),
	)
	return h
}

func createCtrl(parent, hInst uintptr, className, text string, style uint32, x, y, w, h int, id uint16, font uintptr) uintptr {
	cls, _ := syscall.UTF16PtrFromString(className)
	txt, _ := syscall.UTF16PtrFromString(text)
	hwnd, _, _ := procCreateWinEx.Call(
		0,
		uintptr(unsafe.Pointer(cls)),
		uintptr(unsafe.Pointer(txt)),
		uintptr(style),
		uintptr(x), uintptr(y), uintptr(w), uintptr(h),
		parent, uintptr(id), hInst, 0,
	)
	if hwnd != 0 && font != 0 {
		_, _, _ = procSendMsg.Call(hwnd, wmSetFont, font, 1)
	}
	return hwnd
}

func browseForFolder(owner uintptr, title string) string {
	var disp [260]uint16
	titlePtr, _ := syscall.UTF16PtrFromString(title)
	bi := browseInfoW{
		HwndOwner:   owner,
		DisplayName: &disp[0],
		Title:       titlePtr,
		Flags:       0x0001 | 0x0040, // BIF_RETURNONLYFSDIRS | BIF_NEWDIALOGSTYLE
	}
	pidl, _, _ := procSHBrowseFolder.Call(uintptr(unsafe.Pointer(&bi)))
	if pidl == 0 {
		return ""
	}
	defer procCoTaskMemFree.Call(pidl)
	var out [260]uint16
	ok, _, _ := procSHGetPathFromID.Call(pidl, uintptr(unsafe.Pointer(&out[0])))
	if ok == 0 {
		return ""
	}
	return syscall.UTF16ToString(out[:])
}
