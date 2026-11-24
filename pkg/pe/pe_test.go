package pe

import (
	"math"
	"testing"
)

func TestShannonEntropy(t *testing.T) {
	zeros := make([]byte, 1024)
	if e := ShannonEntropy(zeros); e != 0 {
		t.Fatalf("entropie de zéros = %f, attendu 0", e)
	}
	uniform := make([]byte, 256)
	for i := range uniform {
		uniform[i] = byte(i)
	}
	if e := ShannonEntropy(uniform); math.Abs(e-8.0) > 1e-6 {
		t.Fatalf("entropie uniforme = %f, attendu 8.0", e)
	}
}

func TestParseInvalidData(t *testing.T) {
	if _, err := Parse([]byte("not a pe")); err != ErrNotPE {
		t.Fatalf("attendu ErrNotPE, obtenu %v", err)
	}
}
