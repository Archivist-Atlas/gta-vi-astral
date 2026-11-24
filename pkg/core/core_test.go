package core

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureOutsideRepoRejectsMarkerAncestors(t *testing.T) {
	tmp := t.TempDir()
	marker := filepath.Join(tmp, RootMarker)
	if err := os.WriteFile(marker, []byte("marker"), 0o644); err != nil {
		t.Fatal(err)
	}

	inside := filepath.Join(tmp, "cache", "shaders")
	err := EnsureOutsideRepo(inside)
	if !errors.Is(err, ErrInsideRepo) {
		t.Fatalf("attendu ErrInsideRepo pour %q, obtenu: %v", inside, err)
	}

	dotdot := filepath.Join(tmp, "pkg", "..", "nonexistent", "out")
	err = EnsureOutsideRepo(dotdot)
	if !errors.Is(err, ErrInsideRepo) {
		t.Fatalf("attendu ErrInsideRepo pour %q, obtenu: %v", dotdot, err)
	}
}

func TestEnsureOutsideRepoAcceptsExternalDir(t *testing.T) {
	tmp := t.TempDir()
	outside := filepath.Join(tmp, "external-cache", "shaders")
	if err := EnsureOutsideRepo(outside); err != nil {
		t.Fatalf("chemin externe refusé à tort: %v", err)
	}
}

func TestParseSimpleTOML(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "astral.toml")
	content := `
[paths]
game_dir = 'D:\Games\GTA6'
cache_dir = 'D:\Cache\Astral' # commentaire

[runtime]
console_profile = 'series-s'
gpu_backend = 'vulkan'
unimplemented = 'fail'
wave64_emulate = 'on'
jobs = 6
`
	if err := os.WriteFile(cfgPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadConfig(cfgPath)
	if err != nil {
		t.Fatalf("LoadConfig échoué: %v", err)
	}
	if cfg.Paths.GameDir != `D:\Games\GTA6` {
		t.Errorf("GameDir = %q", cfg.Paths.GameDir)
	}
	if cfg.Paths.CacheDir != `D:\Cache\Astral` {
		t.Errorf("CacheDir = %q", cfg.Paths.CacheDir)
	}
	if cfg.Runtime.ConsoleProfile != ConsoleSeriesS {
		t.Errorf("ConsoleProfile = %q", cfg.Runtime.ConsoleProfile)
	}
	if cfg.Runtime.GPUBackend != BackendVulkan {
		t.Errorf("GPUBackend = %q", cfg.Runtime.GPUBackend)
	}
	if cfg.Runtime.Unimplemented != UnimplFail {
		t.Errorf("Unimplemented = %q", cfg.Runtime.Unimplemented)
	}
	if cfg.Runtime.Wave64Emulate != "on" || cfg.Runtime.Jobs != 6 {
		t.Errorf("Wave64=%q Jobs=%d", cfg.Runtime.Wave64Emulate, cfg.Runtime.Jobs)
	}
}
