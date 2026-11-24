// Package pipeline fournit le cache adressé par contenu (inspiré de victory-pipeline)
// et l'orchestrateur parallèle multi-étapes (inspiré de Leonida make_game.py).
package pipeline

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"

	"github.com/Archivist-Atlas/gta-vi-astral/pkg/core"
)

// Converter représente une étape déterministe de conversion (ex: shader RDNA2 -> DXIL/SPIR-V).
type Converter interface {
	ID() string
	Version() uint32
	Convert(input []byte) ([]byte, error)
}

// CacheKey identifie de manière unique le résultat d'un Converter pour une entrée donnée.
type CacheKey [32]byte

// NewCacheKey calcule SHA-256(id + 0 + version + input).
func NewCacheKey(conv Converter, input []byte) CacheKey {
	h := sha256.New()
	h.Write([]byte(conv.ID()))
	h.Write([]byte{0})
	var ver [4]byte
	binary.LittleEndian.PutUint32(ver[:], conv.Version())
	h.Write(ver[:])
	h.Write(input)
	var out CacheKey
	copy(out[:], h.Sum(nil))
	return out
}

func (k CacheKey) Hex() string {
	return hex.EncodeToString(k[:])
}

// Cache stocke les artefacts convertis sur disque hors du dépôt, avec écriture atomique.
type Cache struct {
	root    string
	counter atomic.Uint64
}

// OpenCache ouvre (ou crée) un cache sur disque après vérification du garde-fou anti-fuite.
func OpenCache(root string) (*Cache, error) {
	if err := core.EnsureOutsideRepo(root); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("création du dossier cache %q : %w", root, err)
	}
	return &Cache{root: root}, nil
}

// PathFor retourne le chemin shardé (<root>/<hex[:2]>/<hex[2:]>) d'une clé.
func (c *Cache) PathFor(key CacheKey) string {
	h := key.Hex()
	return filepath.Join(c.root, h[:2], h[2:])
}

// GetOrConvert retourne le chemin de l'artefact en cache, ou le génère atomiquement.
func (c *Cache) GetOrConvert(conv Converter, input []byte) (string, bool, error) {
	key := NewCacheKey(conv, input)
	target := c.PathFor(key)
	if info, err := os.Stat(target); err == nil && !info.IsDir() {
		return target, true, nil
	}

	out, err := conv.Convert(input)
	if err != nil {
		return "", false, fmt.Errorf("convertisseur %s v%d : %w", conv.ID(), conv.Version(), err)
	}

	dir := filepath.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", false, err
	}

	tmp := filepath.Join(dir, fmt.Sprintf(".tmp-%d-%d", os.Getpid(), c.counter.Add(1)))
	if err := os.WriteFile(tmp, out, 0o644); err != nil {
		_ = os.Remove(tmp)
		return "", false, err
	}
	if err := os.Rename(tmp, target); err != nil {
		_ = os.Remove(tmp)
		// En cas de course entre deux workers, vérifier si la cible existe désormais.
		if info, statErr := os.Stat(target); statErr == nil && !info.IsDir() {
			return target, true, nil
		}
		return "", false, err
	}
	return target, false, nil
}
