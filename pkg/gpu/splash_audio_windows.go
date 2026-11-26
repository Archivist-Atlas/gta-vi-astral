//go:build windows

package gpu

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/Archivist-Atlas/gta-vi-astral/assets"
)

var (
	modWinMM          = syscall.NewLazyDLL("winmm.dll")
	procMCISendString = modWinMM.NewProc("mciSendStringW")
)

const (
	audioCmdPlay  = 1
	audioCmdPause = 2
	audioCmdStop  = 3
)

var (
	loadingAudioOnce     sync.Once
	loadingAudioCh       chan int
	loadingAudioEnabled  atomic.Bool
	loadingAudioStopped  atomic.Bool
	introReachedAtomic   atomic.Bool
	loadingAudioMu       sync.Mutex
	loadingAudioOnToggle func(bool)
)

func init() {
	loadingAudioEnabled.Store(true)
}

// IsLoadingAudioEnabled indique si la musique de chargement est actuellement activée.
func IsLoadingAudioEnabled() bool {
	return loadingAudioEnabled.Load()
}

// IsIntroReached indique si le jeu a quitté l'écran de chargement Astral.
func IsIntroReached() bool {
	return introReachedAtomic.Load()
}

func markIntroReached() {
	if introReachedAtomic.CompareAndSwap(false, true) {
		StopLoadingAudio()
	}
}

func mciExec(cmd string) uintptr {
	ptr, err := syscall.UTF16PtrFromString(cmd)
	if err != nil {
		return 1
	}
	r, _, _ := procMCISendString.Call(uintptr(unsafe.Pointer(ptr)), 0, 0, 0)
	return r
}

func ensureLoadingMP3File() (string, error) {
	if len(assets.LoadingMP3) == 0 {
		return "", fmt.Errorf("piste loading.mp3 vide")
	}
	dst := filepath.Join(os.TempDir(), "astral_loading.mp3")
	if st, err := os.Stat(dst); err == nil && st.Size() == int64(len(assets.LoadingMP3)) {
		return dst, nil
	}
	if err := os.WriteFile(dst, assets.LoadingMP3, 0o644); err != nil {
		return "", err
	}
	return dst, nil
}

func ensureAudioWorker() {
	loadingAudioOnce.Do(func() {
		loadingAudioCh = make(chan int, 16)
		go func() {
			// MCI exige que toutes les commandes pour un même alias proviennent du même thread OS.
			runtime.LockOSThread()
			defer runtime.UnlockOSThread()

			opened := false
			playing := false
			mp3Path := ""

			for cmd := range loadingAudioCh {
				switch cmd {
				case audioCmdPlay:
					if loadingAudioStopped.Load() || introReachedAtomic.Load() {
						continue
					}
					if !opened {
						p, err := ensureLoadingMP3File()
						if err != nil {
							slog.Warn("impossible d'extraire loading.mp3", "err", err)
							continue
						}
						mp3Path = p
						_ = mciExec("close astral_bgm")
						if r := mciExec(fmt.Sprintf(`open "%s" type mpegvideo alias astral_bgm`, mp3Path)); r != 0 {
							slog.Warn("MCI open loading.mp3 échoué", "code", r)
							continue
						}
						opened = true
					}
					if !playing {
						if r := mciExec("play astral_bgm repeat"); r != 0 {
							_ = mciExec("play astral_bgm")
						}
						playing = true
					}
				case audioCmdPause:
					if opened && playing {
						_ = mciExec("pause astral_bgm")
						playing = false
					}
				case audioCmdStop:
					if opened {
						_ = mciExec("stop astral_bgm")
						_ = mciExec("close astral_bgm")
						opened = false
						playing = false
					}
					return
				}
			}
		}()
	})
}

func sendAudioCmd(cmd int) {
	if loadingAudioCh == nil {
		return
	}
	select {
	case loadingAudioCh <- cmd:
	default:
	}
}

// StartLoadingAudio initialise et lance (si enabled=true) la musique de chargement en boucle.
// onToggle est appelé à chaque bascule utilisateur pour persister le choix dans astral.toml.
func StartLoadingAudio(enabled bool, onToggle func(bool)) {
	loadingAudioMu.Lock()
	loadingAudioOnToggle = onToggle
	loadingAudioMu.Unlock()

	loadingAudioEnabled.Store(enabled)
	if loadingAudioStopped.Load() || introReachedAtomic.Load() {
		return
	}
	ensureAudioWorker()
	if enabled {
		sendAudioCmd(audioCmdPlay)
	}
}

// ToggleLoadingAudio active ou coupe le son de chargement, mémorise le choix et rafraîchit le splash.
func ToggleLoadingAudio() bool {
	if introReachedAtomic.Load() {
		return loadingAudioEnabled.Load()
	}
	ensureAudioWorker()
	next := !loadingAudioEnabled.Load()
	loadingAudioEnabled.Store(next)
	if next {
		sendAudioCmd(audioCmdPlay)
	} else {
		sendAudioCmd(audioCmdPause)
	}

	loadingAudioMu.Lock()
	cb := loadingAudioOnToggle
	loadingAudioMu.Unlock()
	if cb != nil {
		go cb(next)
	}
	hostMu.Lock()
	h := hostInstance
	hostMu.Unlock()
	if h != nil && !introReachedAtomic.Load() {
		go func() { h.Present(0) }()
	}
	return next
}

// StopLoadingAudio arrête définitivement et libère la musique de chargement.
func StopLoadingAudio() {
	if loadingAudioStopped.CompareAndSwap(false, true) {
		sendAudioCmd(audioCmdStop)
	}
}
