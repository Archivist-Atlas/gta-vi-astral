package formats

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"testing"
)

func TestJOAAT32(t *testing.T) {
	// Valeur de référence vérifiée par Leonida : "adder" == 0xB779A091
	if got := JOAAT32("adder"); got != 0xB779A091 {
		t.Fatalf("JOAAT32(adder) = 0x%08X, attendu 0xB779A091", got)
	}
	if got := JOAAT32("ADDER"); got != 0xB779A091 {
		t.Fatalf("JOAAT32(ADDER) = 0x%08X, attendu 0xB779A091", got)
	}
}

func TestScanAndUnpackSGD5(t *testing.T) {
	payload := []byte("DXBC_TEST_PAYLOAD_ASTRAL")
	var comp bytes.Buffer
	w, err := flate.NewWriter(&comp, flate.DefaultCompression)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.Write(payload)
	_ = w.Close()

	name := "VS_Test_0011aabb_Wrapped"
	n := uint32(comp.Len() + 12)

	var buf bytes.Buffer
	buf.Write(SGD5Magic)
	buf.Write([]byte{0x01, 0x00, 0x00, 0x00})
	buf.WriteByte(byte(len(name) + 1))
	buf.WriteString(name)
	buf.WriteByte(0)
	buf.WriteByte(0x32)
	_ = binary.Write(&buf, binary.LittleEndian, n)
	_ = binary.Write(&buf, binary.LittleEndian, uint32(0x000DDCE7))
	_ = binary.Write(&buf, binary.LittleEndian, n)
	_ = binary.Write(&buf, binary.LittleEndian, uint32(len(payload)))
	buf.Write(comp.Bytes())

	data := buf.Bytes()
	if !IsSGD5(data) {
		t.Fatal("IsSGD5 a retourné false")
	}
	recs := ScanSGD5(data)
	if len(recs) != 1 {
		t.Fatalf("attendu 1 enregistrement SGD5, obtenu %d", len(recs))
	}
	if recs[0].Name != name || recs[0].Kind != 0x32 {
		t.Fatalf("métadonnées inattendues : %+v", recs[0])
	}
	unpacked, err := recs[0].Unpack()
	if err != nil {
		t.Fatalf("Unpack échoué : %v", err)
	}
	if !bytes.Equal(unpacked, payload) {
		t.Fatalf("payload décompressé = %q, attendu %q", unpacked, payload)
	}
}

func TestComputeTextureLayout(t *testing.T) {
	layout, err := ComputeTextureLayout(TextureDesc{
		Dimension:    Tex2D,
		Width:        1024,
		Height:       1024,
		Depth:        1,
		Mips:         1,
		ElementBytes: 16, // BC7 = 16 octets par bloc 4x4
		Block:        4,
		Samples:      1,
		Swizzle:      Swizzle64KB_S,
	})
	if err != nil {
		t.Fatalf("ComputeTextureLayout échoué : %v", err)
	}
	if layout.Alignment != 65536 {
		t.Errorf("alignement = %d, attendu 65536", layout.Alignment)
	}
	if layout.TotalBytes != 1048576 {
		t.Errorf("TotalBytes = %d, attendu 1048576", layout.TotalBytes)
	}
}
