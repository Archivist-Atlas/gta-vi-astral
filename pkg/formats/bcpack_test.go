package formats

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func makeSyntheticBCPackTables() []byte {
	var out bytes.Buffer
	var tmp2 [2]byte
	for m := 0; m < bcModels; m++ {
		g := m / 16
		for b := 0; b < bcBuckets; b++ {
			var f uint16
			if b < g+2 {
				f = uint16(1 << uint(15-g))
			}
			binary.LittleEndian.PutUint16(tmp2[:], f)
			out.Write(tmp2[:])
		}
	}
	for i := 0; i < 256; i++ {
		out.WriteByte(uint8(i & 0x77)) // rank1
	}
	for i := 0; i < 64; i++ {
		out.WriteByte(uint8(i)) // rank3a
	}
	for i := 0; i < 256; i++ {
		out.WriteByte(uint8(i)) // rank3c
	}
	for i := 0; i < 64; i++ {
		out.WriteByte(uint8((i & 7) | ((i >> 3) << 4))) // rank4
	}
	for i := 0; i < 64; i++ {
		out.WriteByte(uint8(i)) // rank5
	}
	for code := 0; code < 512; code++ {
		var ctrl uint16
		if code == 0 {
			ctrl = 0x46 // mode 6, 1 subset
		}
		binary.LittleEndian.PutUint16(tmp2[:], ctrl)
		out.Write(tmp2[:])
	}
	buf := out.Bytes()
	if len(buf) < BCPackTablesSize {
		buf = append(buf, make([]byte, BCPackTablesSize-len(buf))...)
	}
	return buf
}

func encodeTestFragment(width uint8, syms []uint8) []byte {
	f := uint32(1) << uint(15-width)
	x := bcLow
	var raw []byte
	for i := len(syms) - 1; i >= 0; i-- {
		s := syms[i]
		var freq, start uint32
		if s == 0 {
			freq = f - 1
			start = 1
		} else {
			freq = f
			start = uint32(s) * f
		}
		for x >= (freq << 16) {
			raw = append(raw, byte(x))
			x >>= 8
		}
		x = ((x / freq) << 16) + (x % freq) + start
	}
	var xb [4]byte
	binary.LittleEndian.PutUint32(xb[:], x)
	raw = append(raw, xb[:]...)
	for i, j := 0, len(raw)-1; i < j; i, j = i+1, j-1 {
		raw[i], raw[j] = raw[j], raw[i]
	}
	out := []byte{0}
	if len(raw) > 256 {
		out[0] |= 0x20
		n := len(raw) - 257
		out = append(out, byte(n), byte(n>>8))
	} else {
		out = append(out, byte(len(raw)-1))
	}
	return append(out, raw...)
}

func encodeTestStream(t *testing.T, tbl *BCPackTables, widths []uint8, per int, records [][]uint8) []byte {
	t.Helper()
	queues := make([][]byte, len(widths))
	for j := range widths {
		var syms []uint8
		for _, r := range records {
			syms = append(syms, r[j*per:j*per+per]...)
		}
		b := encodeTestFragment(widths[j], syms)
		b = append(b, 0, 1, 0, 0)
		queues[j] = b
	}
	var stream []byte
	check := make([]uint8, len(records)*len(widths)*per)
	fetch := func(j int) (uint8, bool) {
		if len(queues[j]) == 0 {
			t.Fatalf("lane %d a demandé des octets après fin", j)
		}
		b := queues[j][0]
		queues[j] = queues[j][1:]
		stream = append(stream, b)
		return b, true
	}
	if err := decodeBCPackLanes(tbl, widths, per, len(records), fetch, check); err != nil {
		t.Fatalf("decodeBCPackLanes échoué : %v", err)
	}
	return stream
}

func testZigZag(v int) uint8 {
	if v >= 0 {
		return uint8(2 * v)
	}
	return uint8(-2*v - 1)
}

func TestBCPackBuiltinTables(t *testing.T) {
	tbl, err := BuiltinBCPackTables()
	if err != nil {
		t.Fatalf("BuiltinBCPackTables échoué : %v", err)
	}
	if len(tbl.bc7) != bcDictionary {
		t.Fatalf("dictionnaire BC7 inattendu : %d", len(tbl.bc7))
	}
	// Vérifie que la table de fenêtres 256-slots correspond exactement au scan linéaire
	for m := 0; m < bcModels; m++ {
		for slot := uint32(0); slot < 65536; slot += 137 {
			got := bcBucketOf(tbl, uint8(m), slot)
			want := 0
			for b := 1; b < bcBuckets; b++ {
				if tbl.start[m][b] <= slot {
					want++
				}
			}
			if got != want {
				t.Fatalf("model %d slot %d : bucket=%d want=%d", m, slot, got, want)
			}
		}
	}
}

func TestBCPackBC1AndBC7Mode6(t *testing.T) {
	tbl, err := ParseBCPackTables(makeSyntheticBCPackTables())
	if err != nil {
		t.Fatalf("ParseBCPackTables échoué : %v", err)
	}

	// BC1 exact reference vector
	recs1 := [][]uint8{
		{0, 0, 2, 1, 4, 3, 6, 2, 0x13, 0x27, 0x54, 0x71},
		{2, 0, 3, 2, 1, 4, 1, 3, 0x71, 0x54, 0x27, 0x13},
	}
	src1 := encodeTestStream(t, tbl, []uint8{7, 7, 4, 5, 4, 4, 5, 4, 7, 7, 7, 7}, 1, recs1)
	dst1 := make([]byte, 16)
	if err := DecodeBCPack(1, src1, dst1, tbl); err != nil {
		t.Fatalf("DecodeBCPack(BC1) échoué : %v", err)
	}
	var want0, want1 [8]byte
	binary.LittleEndian.PutUint64(want0[:], 0x9a283291f0610fe2)
	binary.LittleEndian.PutUint64(want1[:], 0x2319a982005ff801)
	if !bytes.Equal(dst1[:8], want0[:]) || !bytes.Equal(dst1[8:], want1[:]) {
		t.Fatalf("BC1 mismatch : got %x, want %x%x", dst1, want0, want1)
	}

	// BC7 mode 6 exact reference vector
	s7 := make([]uint8, 32)
	s7[4] = testZigZag(0x55)
	s7[5] = testZigZag(-1)
	s7[6] = testZigZag(3)
	s7[10] = testZigZag(0x7f >> 1)
	src7 := encodeTestStream(t, tbl, []uint8{3, 3, 7, 7, 7, 7, 7, 6, 6, 4, 5, 7, 7, 7, 7, 4}, 2, [][]uint8{s7})
	dst7 := make([]byte, 16)
	if err := DecodeBCPack(7, src7, dst7, tbl); err != nil {
		t.Fatalf("DecodeBCPack(BC7) échoué : %v", err)
	}
	var w bcBits128
	w.put(1<<6, 7)
	for _, f := range []uint64{0x2a, 0x7f, 3, 0, 0, 0, 0x3f, 0} {
		w.put(f, 7)
	}
	w.put(1, 1)
	w.put(1, 1)
	var want7 [16]byte
	w.writeTo(want7[:])
	if !bytes.Equal(dst7, want7[:]) {
		t.Fatalf("BC7 mode 6 mismatch : got %x, want %x", dst7, want7)
	}
}
