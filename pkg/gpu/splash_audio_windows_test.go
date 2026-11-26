//go:build windows

package gpu

import (
	"bytes"
	"testing"

	"github.com/Archivist-Atlas/gta-vi-astral/assets"
	mp3 "github.com/hajimehoshi/go-mp3"
)

func TestLoadingMP3Decode(t *testing.T) {
	if len(assets.LoadingMP3) == 0 {
		t.Fatal("assets.LoadingMP3 est vide")
	}
	dec, err := mp3.NewDecoder(bytes.NewReader(assets.LoadingMP3))
	if err != nil {
		t.Fatalf("mp3.NewDecoder échoué : %v", err)
	}
	if sr := dec.SampleRate(); sr < 22050 || sr > 96000 {
		t.Fatalf("sample rate inattendu : %d", sr)
	}
	buf := make([]byte, 16384)
	n, err := dec.Read(buf)
	if err != nil || n == 0 {
		t.Fatalf("lecture PCM échouée : n=%d err=%v", n, err)
	}
	if !IsPointInAudioButton(1280-42, 38, 1280, 720) {
		t.Fatal("IsPointInAudioButton devrait détecter le centre du bouton")
	}
	if !IsPointInFullscreenButton(1280-98, 38, 1280, 720) {
		t.Fatal("IsPointInFullscreenButton devrait détecter le centre du bouton plein écran")
	}
	if IsPointInAudioButton(1280-98, 38, 1280, 720) {
		t.Fatal("les zones cliquables audio et plein écran ne doivent pas se chevaucher")
	}
}
