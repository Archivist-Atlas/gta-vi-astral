package shader

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"testing"
)

func TestDXBCAndFSSMExtraction(t *testing.T) {
	// Programme minimal RDNA2 : s_nop 0 + s_endpgm
	code := make([]byte, 8)
	binary.LittleEndian.PutUint32(code[0:4], 0xBF800000) // s_nop 0 (SOPP op 0)
	binary.LittleEndian.PutUint32(code[4:8], 0xBF810000) // s_endpgm (SOPP op 1)

	// Construit un payload FSSM minimal avec un VS à l'offset 0x70 et code à 0x100
	payload := make([]byte, 0x108)
	for _, slot := range fssmSlots {
		binary.LittleEndian.PutUint64(payload[slot.offset:slot.offset+8], fssmAbsentSlot)
	}
	recOff := 0x70
	binary.LittleEndian.PutUint64(payload[0x28:0x30], uint64(recOff)) // StageVS
	binary.LittleEndian.PutUint64(payload[recOff+0x00:recOff+0x08], 0x100)
	binary.LittleEndian.PutUint64(payload[recOff+0x08:recOff+0x10], uint64(len(code)))
	binary.LittleEndian.PutUint32(payload[recOff+0x14:recOff+0x18], 0) // wave64
	binary.LittleEndian.PutUint32(payload[recOff+0x50:recOff+0x54], 16|(32<<20))
	copy(payload[0x100:], code)

	var fssm bytes.Buffer
	fssm.WriteString("FSSM")
	_ = binary.Write(&fssm, binary.LittleEndian, uint32(fssmVersion))
	_ = binary.Write(&fssm, binary.LittleEndian, uint64(len(payload)))
	fssm.Write(payload)

	var zbuf bytes.Buffer
	zw := zlib.NewWriter(&zbuf)
	_, _ = zw.Write(fssm.Bytes())
	_ = zw.Close()

	var xbch bytes.Buffer
	xbch.WriteString("XBCH")
	_ = binary.Write(&xbch, binary.LittleEndian, uint32(xbchVersion))
	_ = binary.Write(&xbch, binary.LittleEndian, uint64(zbuf.Len()))
	_ = binary.Write(&xbch, binary.LittleEndian, uint64(fssm.Len()))
	xbch.Write(zbuf.Bytes())

	// Bloc DXIL avec XDAT à +0x18
	var dxil bytes.Buffer
	dxil.Write(make([]byte, xdatOffset))
	dxil.WriteString("XDAT")
	_ = binary.Write(&dxil, binary.LittleEndian, uint32(1))
	_ = binary.Write(&dxil, binary.LittleEndian, uint32(0x24))
	blobOff := uint32(xdatOffset + 12 + 12)
	_ = binary.Write(&dxil, binary.LittleEndian, uint32(xdatKindNative))
	_ = binary.Write(&dxil, binary.LittleEndian, blobOff)
	_ = binary.Write(&dxil, binary.LittleEndian, uint32(xbch.Len()))
	dxil.Write(xbch.Bytes())

	rawDXBC := BuildDXBC([]Part{{FourCC: FourCCDXIL, Data: dxil.Bytes()}})
	container, err := ParseDXBC(rawDXBC)
	if err != nil {
		t.Fatalf("ParseDXBC échoué : %v", err)
	}

	progs, err := ExtractNativePrograms(container)
	if err != nil {
		t.Fatalf("ExtractNativePrograms échoué : %v", err)
	}
	if len(progs) != 1 {
		t.Fatalf("attendu 1 programme, obtenu %d", len(progs))
	}
	p := progs[0]
	if p.Stage != StageVS || !p.Wave64 || p.VGPRs != 16 || p.SGPRs != 32 {
		t.Fatalf("métadonnées inattendues : %+v", p)
	}

	insts := DecodeGFX10(p.Code)
	if len(insts) != 2 || !insts[1].IsEndPgm {
		t.Fatalf("décodage GFX10 inattendu : %+v", insts)
	}
}
