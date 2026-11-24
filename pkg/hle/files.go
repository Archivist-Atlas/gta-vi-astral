package hle

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Archivist-Atlas/gta-vi-astral/pkg/core"
)

// VirtualFS mappe les lecteurs virtuels Xbox Series (G: / GAME: -> dossier jeu en
// lecture seule avec overlay copy-on-write, T: / PLS -> stockage persistant sous data_dir).
// Combine l'overlay COW de Leonida et la réécriture de lecteurs virtuels de VIctory.
type VirtualFS struct {
	GameDir    string
	OverlayDir string
	DataDir    string
}

// NewVirtualFS crée et valide un système de fichiers virtuel (hors dépôt).
func NewVirtualFS(gameDir, overlayDir, dataDir string) (*VirtualFS, error) {
	for _, p := range []string{overlayDir, dataDir} {
		if p == "" {
			continue
		}
		if err := core.EnsureOutsideRepo(p); err != nil {
			return nil, err
		}
	}
	return &VirtualFS{
		GameDir:    gameDir,
		OverlayDir: overlayDir,
		DataDir:    dataDir,
	}, nil
}

// ResolveRead traduit un chemin console (ex: "G:\common.rpf" ou "GAME:\data.rpf")
// en vérifiant d'abord l'overlay copy-on-write puis le dossier du jeu.
func (v *VirtualFS) ResolveRead(consolePath string) (string, bool) {
	rel, area := splitConsolePath(consolePath)
	switch area {
	case "game":
		if v.OverlayDir != "" {
			cand := filepath.Join(v.OverlayDir, rel)
			if _, err := os.Stat(cand); err == nil {
				return cand, true
			}
		}
		if v.GameDir != "" {
			return filepath.Join(v.GameDir, rel), true
		}
	case "pls":
		if v.DataDir != "" {
			return filepath.Join(v.DataDir, "pls", rel), true
		}
	}
	return consolePath, false
}

// ResolveWrite dirige toute écriture destinée à GAME:/G: vers l'overlay pour ne jamais
// altérer la copie originale du jeu.
func (v *VirtualFS) ResolveWrite(consolePath string) (string, error) {
	rel, area := splitConsolePath(consolePath)
	switch area {
	case "game":
		if v.OverlayDir == "" {
			return "", fmt.Errorf("overlay non configuré : écriture refusée sur %q", consolePath)
		}
		return filepath.Join(v.OverlayDir, rel), nil
	case "pls":
		if v.DataDir == "" {
			return "", fmt.Errorf("data_dir non configuré : écriture refusée sur %q", consolePath)
		}
		return filepath.Join(v.DataDir, "pls", rel), nil
	default:
		return consolePath, nil
	}
}

func splitConsolePath(p string) (rel string, area string) {
	norm := strings.ReplaceAll(strings.TrimSpace(p), "/", `\`)
	upper := strings.ToUpper(norm)
	switch {
	case strings.HasPrefix(upper, `GAME:\`):
		return filepath.Clean(norm[6:]), "game"
	case strings.HasPrefix(upper, `G:\`):
		return filepath.Clean(norm[3:]), "game"
	case strings.HasPrefix(upper, `T:\`):
		return filepath.Clean(norm[3:]), "pls"
	default:
		return norm, ""
	}
}
