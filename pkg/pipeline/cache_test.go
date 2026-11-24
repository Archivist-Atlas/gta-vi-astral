package pipeline

import (
	"bytes"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

type upperConverter struct {
	ver  uint32
	runs atomic.Int32
}

func (u *upperConverter) ID() string      { return "test.upper" }
func (u *upperConverter) Version() uint32 { return u.ver }
func (u *upperConverter) Convert(in []byte) ([]byte, error) {
	u.runs.Add(1)
	return bytes.ToUpper(in), nil
}

func TestCacheHitAndVersionInvalidation(t *testing.T) {
	root := filepath.Join(t.TempDir(), "cache")
	c, err := OpenCache(root)
	if err != nil {
		t.Fatalf("OpenCache échoué : %v", err)
	}

	conv := &upperConverter{ver: 1}
	p1, hit1, err := c.GetOrConvert(conv, []byte("astral"))
	if err != nil || hit1 {
		t.Fatalf("premier appel : hit=%v err=%v", hit1, err)
	}
	p2, hit2, err := c.GetOrConvert(conv, []byte("astral"))
	if err != nil || !hit2 || p1 != p2 {
		t.Fatalf("second appel : hit=%v p1=%s p2=%s err=%v", hit2, p1, p2, err)
	}
	if conv.runs.Load() != 1 {
		t.Fatalf("convertisseur appelé %d fois, attendu 1", conv.runs.Load())
	}
	data, _ := os.ReadFile(p1)
	if string(data) != "ASTRAL" {
		t.Fatalf("contenu = %q, attendu ASTRAL", data)
	}

	conv2 := &upperConverter{ver: 2}
	if NewCacheKey(conv, []byte("astral")) == NewCacheKey(conv2, []byte("astral")) {
		t.Fatal("le changement de version doit invalider la clé")
	}
}
