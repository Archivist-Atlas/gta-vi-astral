//go:build windows

package gpu

import (
	"bytes"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unsafe"

	"github.com/Archivist-Atlas/gta-vi-astral/assets"
	mp3 "github.com/hajimehoshi/go-mp3"
)

var (
	modWinMM                = syscall.NewLazyDLL("winmm.dll")
	procWaveOutOpen         = modWinMM.NewProc("waveOutOpen")
	procWaveOutPrepareHdr   = modWinMM.NewProc("waveOutPrepareHeader")
	procWaveOutUnprepareHdr = modWinMM.NewProc("waveOutUnprepareHeader")
	procWaveOutWrite        = modWinMM.NewProc("waveOutWrite")
	procWaveOutPause        = modWinMM.NewProc("waveOutPause")
	procWaveOutRestart      = modWinMM.NewProc("waveOutRestart")
	procWaveOutReset        = modWinMM.NewProc("waveOutReset")
	procWaveOutClose        = modWinMM.NewProc("waveOutClose")
)

type waveFormatEx struct {
	FormatTag      uint16
	Channels       uint16
	SamplesPerSec  uint32
	AvgBytesPerSec uint32
	BlockAlign     uint16
	BitsPerSample  uint16
	Size           uint16
}

type waveHdr struct {
	Data          uintptr
	BufferLength  uint32
	BytesRecorded uint32
	User          uintptr
	Flags         uint32
	Loops         uint32
	Next          uintptr
	Reserved      uintptr
}

const (
	whdrDone     = 0x00000001
	whdrPrepared = 0x00000002
	waveMapper   = 0xFFFFFFFF
)

var (
	loadingAudioOnce     sync.Once
	loadingAudioEnabled  atomic.Bool
	loadingAudioStopped  atomic.Bool
	introReachedAtomic   atomic.Bool
	loadingAudioMu       sync.Mutex
	loadingAudioOnToggle func(bool)
	loadingWaveOut       atomic.Uintptr
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

func startWaveOutLoop() {
	loadingAudioOnce.Do(func() {
		go func() {
			if len(assets.LoadingMP3) == 0 {
				return
			}
			dec, err := mp3.NewDecoder(bytes.NewReader(assets.LoadingMP3))
			if err != nil {
				slog.Warn("décodage loading.mp3 échoué", "err", err)
				return
			}
			sampleRate := uint32(dec.SampleRate())
			if sampleRate == 0 {
				sampleRate = 44100
			}
			wfx := waveFormatEx{
				FormatTag:      1, // WAVE_FORMAT_PCM
				Channels:       2, // go-mp3 produit toujours du PCM 16-bit stéréo
				SamplesPerSec:  sampleRate,
				BlockAlign:     4,
				BitsPerSample:  16,
				AvgBytesPerSec: sampleRate * 4,
			}

			var hwo uintptr
			r, _, _ := procWaveOutOpen.Call(
				uintptr(unsafe.Pointer(&hwo)),
				waveMapper,
				uintptr(unsafe.Pointer(&wfx)),
				0, 0, 0,
			)
			if r != 0 || hwo == 0 {
				slog.Warn("waveOutOpen loading.mp3 échoué", "code", r)
				return
			}
			loadingWaveOut.Store(hwo)
			defer func() {
				loadingWaveOut.Store(0)
				_, _, _ = procWaveOutReset.Call(hwo)
				_, _, _ = procWaveOutClose.Call(hwo)
			}()

			paused := false
			if !loadingAudioEnabled.Load() {
				_, _, _ = procWaveOutPause.Call(hwo)
				paused = true
			}

			const numBufs = 4
			const bufSize = 32768 // ~185 ms à 44.1 kHz stéréo 16-bit
			bufs := make([][]byte, numBufs)
			hdrs := make([]waveHdr, numBufs)
			inFlight := make([]bool, numBufs)
			for i := 0; i < numBufs; i++ {
				bufs[i] = make([]byte, bufSize)
			}

			readPCM := func(dst []byte) int {
				total := 0
				for total < len(dst) {
					n, rerr := dec.Read(dst[total:])
					total += n
					if rerr == io.EOF {
						if _, serr := dec.Seek(0, io.SeekStart); serr != nil {
							break
						}
						if n == 0 && total == 0 {
							break
						}
					} else if rerr != nil {
						break
					}
				}
				return total
			}

			defer func() {
				_, _, _ = procWaveOutReset.Call(hwo)
				for i := 0; i < numBufs; i++ {
					if hdrs[i].Flags&whdrPrepared != 0 {
						_, _, _ = procWaveOutUnprepareHdr.Call(hwo, uintptr(unsafe.Pointer(&hdrs[i])), unsafe.Sizeof(hdrs[i]))
					}
				}
			}()

			for !loadingAudioStopped.Load() && !introReachedAtomic.Load() {
				wantPlay := loadingAudioEnabled.Load()
				if !wantPlay && !paused {
					_, _, _ = procWaveOutPause.Call(hwo)
					paused = true
				} else if wantPlay && paused {
					_, _, _ = procWaveOutRestart.Call(hwo)
					paused = false
				}

				if paused {
					time.Sleep(30 * time.Millisecond)
					continue
				}

				submitted := false
				for i := 0; i < numBufs; i++ {
					if inFlight[i] {
						if hdrs[i].Flags&whdrDone == 0 {
							continue
						}
						_, _, _ = procWaveOutUnprepareHdr.Call(hwo, uintptr(unsafe.Pointer(&hdrs[i])), unsafe.Sizeof(hdrs[i]))
						inFlight[i] = false
					}
					n := readPCM(bufs[i])
					if n <= 0 {
						break
					}
					hdrs[i] = waveHdr{
						Data:         uintptr(unsafe.Pointer(&bufs[i][0])),
						BufferLength: uint32(n),
					}
					if pr, _, _ := procWaveOutPrepareHdr.Call(hwo, uintptr(unsafe.Pointer(&hdrs[i])), unsafe.Sizeof(hdrs[i])); pr == 0 {
						if wr, _, _ := procWaveOutWrite.Call(hwo, uintptr(unsafe.Pointer(&hdrs[i])), unsafe.Sizeof(hdrs[i])); wr == 0 {
							inFlight[i] = true
							submitted = true
						}
					}
				}
				if !submitted {
					time.Sleep(25 * time.Millisecond)
				}
			}
		}()
	})
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
	startWaveOutLoop()
}

// ToggleLoadingAudio active ou coupe le son de chargement, mémorise le choix et rafraîchit le splash.
func ToggleLoadingAudio() bool {
	if introReachedAtomic.Load() {
		return loadingAudioEnabled.Load()
	}
	next := !loadingAudioEnabled.Load()
	loadingAudioEnabled.Store(next)
	startWaveOutLoop()
	if hwo := loadingWaveOut.Load(); hwo != 0 {
		if next {
			_, _, _ = procWaveOutRestart.Call(hwo)
		} else {
			_, _, _ = procWaveOutPause.Call(hwo)
		}
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
		if hwo := loadingWaveOut.Load(); hwo != 0 {
			_, _, _ = procWaveOutReset.Call(hwo)
		}
	}
}
