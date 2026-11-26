//go:build windows

package hle

import (
	"testing"
	"time"
)

func TestHIDGamepadScanAndNorm(t *testing.T) {
	// Vérifie la normalisation d'axes et de gâchettes HID
	r := hidAxisRange{min: 0, max: 255, present: true}
	if v := normHIDAxis(128, r, false); v != 0 {
		t.Fatalf("centre attendu 0 (deadzone), obtenu %v", v)
	}
	if v := normHIDAxis(255, r, false); v < 0.99 {
		t.Fatalf("max attendu ~1.0, obtenu %v", v)
	}
	if v := normHIDAxis(0, r, true); v < 0.99 {
		t.Fatalf("min inversé attendu ~1.0, obtenu %v", v)
	}
	if v := normHIDTrigger(255, r); v < 0.99 {
		t.Fatalf("trigger max attendu 1.0, obtenu %v", v)
	}

	// Vérifie que le scan HID universel s'exécute sans erreur (et détecte la manette branchée si présente)
	hidManager.start()
	time.Sleep(50 * time.Millisecond)
	_, detected := hidManager.mergedState()
	t.Logf("HID universel actif, manette détectée=%v", detected)
}
