// Package core fournit les fondations de gta-vi-astral : configuration,
// garde-fou anti-fuite d'assets (asset-policy) et journalisation structurée.
package core

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// RootMarker est le fichier sentinelle placé à la racine du dépôt gta-vi-astral.
// Il empêche toute écriture accidentelle de données dérivées du jeu dans le repo.
const RootMarker = ".astral-root"

// ErrInsideRepo est retournée si un chemin cible se trouve à l'intérieur du dépôt.
var ErrInsideRepo = errors.New("chemin interdit : à l'intérieur du dépôt gta-vi-astral")

// EnsureOutsideRepo vérifie que path se trouve strictement en dehors de tout
// dépôt gta-vi-astral (identifié par le marqueur .astral-root).
// Le chemin n'a pas besoin d'exister sur disque.
func EnsureOutsideRepo(path string) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("chemin vide")
	}

	resolved, err := resolvePath(path)
	if err != nil {
		return fmt.Errorf("résolution de %q : %w", path, err)
	}

	cur := resolved
	for {
		marker := filepath.Join(cur, RootMarker)
		if info, statErr := os.Stat(marker); statErr == nil && !info.IsDir() {
			return fmt.Errorf("%w : %q est dans %q (gardez les fichiers du jeu, caches et sauvegardes hors du dépôt)", ErrInsideRepo, path, cur)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	return nil
}

// resolvePath normalise le plus long préfixe existant (avec résolution de liens
// symboliques) puis ré-attache la fin non existante.
func resolvePath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)

	existing := abs
	var tail []string
	for {
		if _, statErr := os.Lstat(existing); statErr == nil {
			canon, evalErr := filepath.EvalSymlinks(existing)
			if evalErr != nil {
				canon = existing
			}
			for i := len(tail) - 1; i >= 0; i-- {
				canon = filepath.Join(canon, tail[i])
			}
			return filepath.Clean(canon), nil
		}
		parent := filepath.Dir(existing)
		if parent == existing {
			return abs, nil
		}
		tail = append(tail, filepath.Base(existing))
		existing = parent
	}
}
