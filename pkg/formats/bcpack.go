// Décodeur pur Go du format matériel Xbox Series BCPack (BC1, BC3, BC4, BC5, BC6H, BC7).
// Utilisé par DirectStorage (xcrdapi.dll) pour décompresser les textures BCn sans perte.
package formats

import (
	_ "embed"
	"encoding/binary"
	"errors"
	"sync"
)

//go:embed bcpack_tables.bin
var bcpackTablesRaw []byte

const (
	bcModels     = 128
	bcBuckets    = 9
	bcHistory    = 256
	bcDictionary = 255
	bcLow        = uint32(1 << 24)
	bcRoundBytes = 8

	// BCPackTablesSize est la taille exacte du fichier de tables statiques BCPack.
	BCPackTablesSize = bcModels*bcBuckets*2 +
		256 + 64 + 256 + 64 + 64 + 512*2 +
		bcDictionary*(6+16+8+32+2+16+4+32+25+33+50+66)
)

type bcPlain struct {
	ep [8]uint8
	ix [32]uint8
}

type bc6Ep struct {
	e [4][3]uint16
}

type bc6Ix struct {
	mode uint8
	ix   [16]int16
}

type bc6Entry struct {
	ep bc6Ep
	ix bc6Ix
}

type bc7Ep struct {
	ctrl uint16
	rows [3][8]uint16
}

type bc7Ix struct {
	bits [2]uint8
	ix   [2][16]int16
}

type bc7Entry struct {
	ep bc7Ep
	ix bc7Ix
}

// BCPackTables regroupe les modèles rANS, tables de rang, codes de contrôle BC7
// et dictionnaires initiaux (255 entrées par format) de BCPack.
type BCPackTables struct {
	freq   [bcModels][bcBuckets]uint32
	start  [bcModels][bcBuckets + 1]uint32
	bucket [bcModels][256]uint8
	recip  [bcModels][bcBuckets]uint64
	rank1  [256]uint8
	rank3a [64]uint8
	rank3c [256]uint8
	rank4  [64]uint8
	rank5  [64]uint8
	ctrl7  [512]uint16
	bc1    []bcPlain
	bc3    []bcPlain
	bc4    []bcPlain
	bc5    []bcPlain
	bc6    []bc6Entry
	bc7    []bc7Entry
}

var (
	builtinTablesOnce sync.Once
	builtinTables     *BCPackTables
	builtinTablesErr  error
)

// BuiltinBCPackTables retourne les tables BCPack embarquées (initialisées une seule fois).
func BuiltinBCPackTables() (*BCPackTables, error) {
	builtinTablesOnce.Do(func() {
		builtinTables, builtinTablesErr = ParseBCPackTables(bcpackTablesRaw)
	})
	return builtinTables, builtinTablesErr
}

// ParseBCPackTables analyse et valide un bloc de tables statiques BCPack.
func ParseBCPackTables(data []byte) (*BCPackTables, error) {
	if len(data) != BCPackTablesSize {
		return nil, errors.New("bcpack: taille du fichier de tables invalide")
	}
	t := &BCPackTables{}
	off := 0
	readU8 := func() uint8 {
		v := data[off]
		off++
		return v
	}
	readU16 := func() uint16 {
		v := binary.LittleEndian.Uint16(data[off : off+2])
		off += 2
		return v
	}

	for m := 0; m < bcModels; m++ {
		for b := 0; b < bcBuckets; b++ {
			t.freq[m][b] = uint32(readU16())
		}
		used := m/16 + 2
		at := uint32(0)
		for b := 0; b < used; b++ {
			f := t.freq[m][b]
			if f == 0 {
				return nil, errors.New("bcpack: bucket vide dans un modèle")
			}
			t.start[m][b] = at
			t.recip[m][b] = ((1 << 32) + uint64(f) - 1) / uint64(f)
			shift := uint(0)
			if b > 0 {
				shift = uint(b - 1)
			}
			at += f << shift
		}
		if at != (1 << 16) {
			return nil, errors.New("bcpack: un modèle ne couvre pas 65536")
		}
		for b := used; b <= bcBuckets; b++ {
			t.start[m][b] = 1 << 16
		}
		for w := 0; w < 256; w++ {
			first := uint32(w) << 8
			cnt := uint8(0)
			for b := 1; b <= bcBuckets; b++ {
				if t.start[m][b] <= first {
					cnt++
				}
			}
			t.bucket[m][w] = cnt
		}
	}

	copy(t.rank1[:], data[off:off+256])
	off += 256
	copy(t.rank3a[:], data[off:off+64])
	off += 64
	copy(t.rank3c[:], data[off:off+256])
	off += 256
	copy(t.rank4[:], data[off:off+64])
	off += 64
	copy(t.rank5[:], data[off:off+64])
	off += 64
	for i := 0; i < 512; i++ {
		t.ctrl7[i] = readU16()
	}

	readPlain := func(epLen, ixLen int) []bcPlain {
		out := make([]bcPlain, bcDictionary)
		for i := 0; i < bcDictionary; i++ {
			for k := 0; k < epLen; k++ {
				out[i].ep[k] = readU8()
			}
			for k := 0; k < ixLen; k++ {
				out[i].ix[k] = readU8()
			}
		}
		return out
	}
	t.bc1 = readPlain(6, 16)
	t.bc3 = readPlain(8, 32)
	t.bc4 = readPlain(2, 16)
	t.bc5 = readPlain(4, 32)

	t.bc6 = make([]bc6Entry, bcDictionary)
	for i := 0; i < bcDictionary; i++ {
		_ = readU8() // mode endpoint inutilisé
		for e := 0; e < 4; e++ {
			for c := 0; c < 3; c++ {
				t.bc6[i].ep.e[e][c] = readU16()
			}
		}
		t.bc6[i].ix.mode = readU8()
		for p := 0; p < 16; p++ {
			t.bc6[i].ix.ix[p] = int16(readU16())
		}
	}

	t.bc7 = make([]bc7Entry, bcDictionary)
	for i := 0; i < bcDictionary; i++ {
		t.bc7[i].ep.ctrl = readU16()
		for r := 0; r < 3; r++ {
			for c := 0; c < 8; c++ {
				t.bc7[i].ep.rows[r][c] = readU16()
			}
		}
		t.bc7[i].ix.bits[0] = readU8()
		t.bc7[i].ix.bits[1] = readU8()
		for s := 0; s < 2; s++ {
			for p := 0; p < 16; p++ {
				t.bc7[i].ix.ix[s][p] = int16(readU16())
			}
		}
	}
	return t, nil
}

// DecodeBCPack décompresse un flux BCPack (bc = 1, 3, 4, 5, 6 ou 7) dans dst.
// La longueur de dst détermine le nombre de blocs décodés.
func DecodeBCPack(bc uint8, src, dst []byte, t *BCPackTables) error {
	var blockSize int
	switch bc {
	case 1, 4:
		blockSize = 8
	case 3, 5, 6, 7:
		blockSize = 16
	default:
		return errors.New("bcpack: format BC non supporté")
	}
	if len(dst) == 0 || len(dst)%blockSize != 0 {
		return errors.New("bcpack: taille destination non multiple d'un bloc")
	}
	if t == nil {
		var err error
		t, err = BuiltinBCPackTables()
		if err != nil {
			return err
		}
	}
	blocks := len(dst) / blockSize
	var widths []uint8
	var per int
	switch bc {
	case 1:
		widths, per = []uint8{7, 7, 4, 5, 4, 4, 5, 4, 7, 7, 7, 7}, 1
	case 3:
		widths, per = []uint8{3, 3, 7, 5, 5, 5, 5, 4, 5, 4, 7, 7}, 2
	case 4:
		widths, per = []uint8{7, 7, 7, 7, 5, 5, 5, 5, 5, 5, 5, 5}, 1
	case 5:
		widths, per = []uint8{3, 3, 7, 7, 5, 5, 5, 5, 5, 5, 5, 5}, 2
	case 6:
		widths, per = []uint8{3, 3, 7, 7, 7, 4, 4, 4, 5, 5, 5, 7, 7, 7, 7, 4}, 2
	default:
		widths, per = []uint8{3, 3, 7, 7, 7, 7, 7, 6, 6, 4, 5, 7, 7, 7, 7, 4}, 2
	}
	stride := len(widths) * per
	records := make([]uint8, blocks*stride)
	srcPos := 0
	fetch := func(_ int) (uint8, bool) {
		if srcPos >= len(src) {
			return 0, false
		}
		b := src[srcPos]
		srcPos++
		return b, true
	}
	if err := decodeBCPackLanes(t, widths, per, blocks, fetch, records); err != nil {
		return err
	}
	switch bc {
	case 1:
		return decodePlainBlocks(records, stride, dst, blockSize, t.bc1, &bc1Widths, func(s []uint8) bcPlain { return bc1Residual(t, s) }, bc1Pack)
	case 3:
		return decodePlainBlocks(records, stride, dst, blockSize, t.bc3, &bc3Widths, func(s []uint8) bcPlain { return bc3Residual(t, s) }, bc3Pack)
	case 4:
		return decodePlainBlocks(records, stride, dst, blockSize, t.bc4, &bc4Widths, func(s []uint8) bcPlain { return bc4Residual(t, s) }, bc4Pack)
	case 5:
		return decodePlainBlocks(records, stride, dst, blockSize, t.bc5, &bc5Widths, func(s []uint8) bcPlain { return bc5Residual(t, s) }, bc5Pack)
	case 6:
		return decodeBC6Blocks(records, stride, dst, t.bc6)
	default:
		return decodeBC7Blocks(records, stride, dst, t)
	}
}

// --- Lanes rANS ---

type bcLane struct {
	width    uint8
	phase    uint8 // 0 header, 1 length low, 2 length high, 3 payload
	long     bool
	reset    bool
	model    uint8
	need     bool
	hasLatch bool
	latch    uint8
	hasOut   bool
	out      uint8
	stalled  bool
	x        uint32
	left     uint32
}

func bcBucketOf(t *BCPackTables, model uint8, slot uint32) int {
	start := &t.start[model]
	b := int(t.bucket[model][slot>>8])
	for start[b+1] <= slot {
		b++
	}
	return b
}

func (l *bcLane) step(t *BCPackTables) {
	if l.phase != 3 {
		if !l.hasLatch {
			return
		}
		b := l.latch
		l.hasLatch = false
		l.need = true
		switch l.phase {
		case 0:
			group := l.width - (b >> 6)
			l.model = group*16 + (b & 15)
			l.reset = (b & 0x10) != 0
			l.long = (b & 0x20) != 0
			l.left = 0
			l.phase = 1
		case 1:
			if l.long {
				l.left = uint32(b)
				l.phase = 2
			} else {
				l.left = uint32(b) + 1
				l.phase = 3
			}
		default:
			l.left = (uint32(b)<<8 | l.left) + 257
			l.phase = 3
		}
		return
	}
	x := l.x
	if x < bcLow && l.hasLatch {
		b := l.latch
		l.hasLatch = false
		l.left--
		x = (x << 8) | uint32(b)
	}
	l.need = l.left != 0 && !l.hasLatch
	if x > bcLow {
		freq := &t.freq[l.model]
		start := &t.start[l.model]
		slot := x & 0xFFFF
		b := bcBucketOf(t, l.model, slot)
		var sym, f, rem uint32
		if b == 0 {
			sym = 0
			f = freq[0] - 1
			rem = slot - 1
		} else {
			offset := slot - start[b]
			q := uint32((uint64(offset) * t.recip[l.model][b]) >> 32)
			sym = (uint32(1) << uint(b-1)) + q
			f = freq[b]
			rem = offset - q*f
		}
		l.x = (x>>16)*f + rem
		l.out = uint8(sym)
		l.hasOut = true
	} else if l.left != 0 {
		l.x = x
	} else {
		l.phase = 0
		l.need = !l.hasLatch
		if l.reset {
			l.x = 0
		}
	}
}

func decodeBCPackLanes(t *BCPackTables, widths []uint8, per, blocks int, fetch func(int) (uint8, bool), records []uint8) error {
	var lanesArr [16]bcLane
	nLanes := len(widths)
	lanes := lanesArr[:nLanes]
	for j := 0; j < nLanes; j++ {
		lanes[j].width = widths[j]
		lanes[j].need = true
	}
	stride := nLanes * per
	full := uint32(1<<uint(nLanes)) - 1
	pos, done, idle, at := 0, uint32(0), 0, 0
	total := blocks * per
	for pos < total {
		taken, moved := 0, false
		for j := 0; j < nLanes; j++ {
			lane := &lanes[j]
			bit := uint32(1) << uint(j)
			before := taken
			if !lane.stalled {
				lane.step(t)
			}
			waiting := lane.hasOut && (done&bit != 0)
			if lane.need && !waiting && taken < bcRoundBytes {
				if b, ok := fetch(j); ok {
					lane.latch = b
					lane.hasLatch = true
					taken++
				}
			}
			lane.stalled = (lane.need && before >= bcRoundBytes) || waiting
			if !lane.stalled && lane.hasOut {
				records[at+j*per] = lane.out
				lane.hasOut = false
				done |= bit
				moved = true
			}
		}
		if done == full {
			pos++
			if pos%per == 0 {
				at += stride - per + 1
			} else {
				at++
			}
			done = 0
			moved = true
		}
		if taken > 0 || moved {
			idle = 0
		} else {
			idle++
			if idle > 3 {
				return errors.New("bcpack: flux tronqué ou corrompu")
			}
		}
	}
	return nil
}

// --- Blocs BC1 / BC3 / BC4 / BC5 ---

func bcInterleave(a, b uint8) uint8 {
	spread := func(v uint8) uint8 {
		var acc uint8
		for k := uint(0); k < 4; k++ {
			acc |= ((v >> k) & 1) << (2 * k)
		}
		return acc
	}
	return spread(a) | (spread(b) << 1)
}

func bcReferences(s []uint8, paired bool) (int, int) {
	var d, ix uint8
	if paired {
		d = bcInterleave(s[0], s[1])
		ix = bcInterleave(s[2], s[3])
	} else {
		d = s[0]
		ix = s[1]
	}
	delta := (d >> 1) ^ (-(d & 1))
	return int(ix + delta), int(ix)
}

func bcPick[T any](i, r int, history []T, dictionary []T, zero *T) *T {
	if r == 0 {
		return zero
	}
	if r <= i {
		return &history[(i-r)%bcHistory]
	}
	return &dictionary[r-i-1]
}

func bcZZ(v uint8, bits uint) uint8 {
	mask := uint8((uint32(1) << bits) - 1)
	if v&1 != 0 {
		return ((v >> 1) ^ mask) & mask
	}
	return (v >> 1) & mask
}

type bcPlainWidths struct {
	ep     [8]uint8
	ix     [32]uint8
	paired bool
}

func makePlainWidths(ep [8]uint8, ix3, ix2 int, paired bool) bcPlainWidths {
	w := bcPlainWidths{ep: ep, paired: paired}
	for k := 0; k < ix3+ix2; k++ {
		if k < ix3 {
			w.ix[k] = 3
		} else {
			w.ix[k] = 2
		}
	}
	return w
}

var (
	bc1Widths = makePlainWidths([8]uint8{5, 6, 5, 5, 6, 5}, 0, 16, false)
	bc3Widths = makePlainWidths([8]uint8{5, 6, 5, 5, 6, 5, 8, 8}, 16, 16, true)
	bc4Widths = makePlainWidths([8]uint8{8, 8}, 16, 0, false)
	bc5Widths = makePlainWidths([8]uint8{8, 8, 8, 8}, 32, 0, true)
)

func decodePlainBlocks(
	records []uint8, stride int,
	dst []byte, blockSize int,
	dictionary []bcPlain,
	w *bcPlainWidths,
	residual func([]uint8) bcPlain,
	pack func(*bcPlain, []byte),
) error {
	var history [bcHistory]bcPlain
	var zero bcPlain
	blocks := len(dst) / blockSize
	for i := 0; i < blocks; i++ {
		s := records[i*stride : (i+1)*stride]
		epRefIdx, ixRefIdx := bcReferences(s, w.paired)
		epRef := &bcPick(i, epRefIdx, history[:], dictionary, &zero).ep
		ixRef := &bcPick(i, ixRefIdx, history[:], dictionary, &zero).ix
		v := residual(s)
		for k := 0; k < 8; k++ {
			if w.ep[k] > 0 {
				v.ep[k] = (v.ep[k] + epRef[k]) & uint8((uint16(1)<<w.ep[k])-1)
			}
		}
		for k := 0; k < 32; k++ {
			if w.ix[k] > 0 {
				v.ix[k] = (v.ix[k] + ixRef[k]) & uint8((uint16(1)<<w.ix[k])-1)
			}
		}
		history[i%bcHistory] = v
		pack(&v, dst[i*blockSize:(i+1)*blockSize])
	}
	return nil
}

func bcQuads(rank *[256]uint8, s []uint8, ix []uint8) {
	quadPos := [4]int{0, 1, 4, 5}
	for q, sym := range s {
		v := rank[sym]
		base := (q>>1)*8 + (q&1)*2
		for k, p := range quadPos {
			ix[base+p] = (v >> uint(2*k)) & 3
		}
	}
}

func bcPairs(rank *[64]uint8, s []uint8, ix []uint8) {
	for k, sym := range s {
		v := rank[sym&63]
		ix[2*k] = v & 7
		ix[2*k+1] = (v >> 4) & 7
	}
}

func bc1Residual(t *BCPackTables, s []uint8) bcPlain {
	var v bcPlain
	epBits := [6]uint{5, 6, 5, 5, 6, 5}
	for k, bits := range epBits {
		v.ep[k] = bcZZ(s[2+k], bits)
	}
	bcQuads(&t.rank1, s[8:12], v.ix[:])
	return v
}

func bc3Residual(t *BCPackTables, s []uint8) bcPlain {
	v := bcPlain{
		ep: [8]uint8{
			bcZZ(s[14], 5), bcZZ(s[16], 6), bcZZ(s[18], 5),
			bcZZ(s[15], 5), bcZZ(s[17], 6), bcZZ(s[19], 5),
			bcZZ(s[4], 8), bcZZ(s[5], 8),
		},
	}
	bcPairs(&t.rank3a, s[6:14], v.ix[:16])
	bcQuads(&t.rank3c, s[20:24], v.ix[16:])
	return v
}

func bc4Residual(t *BCPackTables, s []uint8) bcPlain {
	var v bcPlain
	v.ep[0] = bcZZ(s[2], 8)
	v.ep[1] = bcZZ(s[3], 8)
	bcPairs(&t.rank4, s[4:12], v.ix[:16])
	return v
}

func bc5Residual(t *BCPackTables, s []uint8) bcPlain {
	var v bcPlain
	for k := 0; k < 4; k++ {
		v.ep[k] = bcZZ(s[4+k], 8)
	}
	bcPairs(&t.rank5, s[8:24], v.ix[:])
	return v
}

var (
	bc1Code = [4]uint32{0, 2, 3, 1}
	bc4Code = [8]uint64{0, 2, 3, 4, 5, 6, 7, 1}
)

func bc1Color(ep, ix []uint8, out []byte) {
	c0 := uint16(ep[0])<<11 | uint16(ep[1])<<5 | uint16(ep[2])
	c1 := uint16(ep[3])<<11 | uint16(ep[4])<<5 | uint16(ep[5])
	var bits uint32
	for p := uint(0); p < 16; p++ {
		bits |= bc1Code[ix[p]&3] << (2 * p)
	}
	binary.LittleEndian.PutUint16(out[0:2], c0)
	binary.LittleEndian.PutUint16(out[2:4], c1)
	binary.LittleEndian.PutUint32(out[4:8], bits)
}

func bc4Alpha(e0, e1 uint8, ix []uint8, out []byte) {
	var bits uint64
	for p := uint(0); p < 16; p++ {
		bits |= bc4Code[ix[p]&7] << (16 + 3*p)
	}
	v := uint64(e0^0x80) | uint64(e1^0x80)<<8 | bits
	binary.LittleEndian.PutUint64(out[0:8], v)
}

func bc1Pack(v *bcPlain, out []byte) { bc1Color(v.ep[:], v.ix[:], out) }
func bc3Pack(v *bcPlain, out []byte) {
	bc4Alpha(v.ep[6], v.ep[7], v.ix[:16], out[0:8])
	bc1Color(v.ep[:], v.ix[16:], out[8:16])
}
func bc4Pack(v *bcPlain, out []byte) { bc4Alpha(v.ep[0], v.ep[1], v.ix[:], out) }
func bc5Pack(v *bcPlain, out []byte) {
	bc4Alpha(v.ep[0], v.ep[1], v.ix[:16], out[0:8])
	bc4Alpha(v.ep[2], v.ep[3], v.ix[16:], out[8:16])
}

// --- Utilitaires bits 128-bit & indices signés (BC6H / BC7) ---

func bcNibbles(b uint8, oddFirst bool) [2]int16 {
	half := func(v uint8) uint8 {
		var acc uint8
		for k := uint(0); k < 4; k++ {
			acc |= ((v >> (2 * k)) & 1) << k
		}
		return acc
	}
	z := func(u uint8) int16 { return int16(u>>1) ^ -int16(u&1) }
	even, odd := z(half(b)), z(half(b>>1))
	if oddFirst {
		return [2]int16{odd, even}
	}
	return [2]int16{even, odd}
}

func bcAddIndex(cur int16, bits uint8, refv int16, refBits uint8) int16 {
	up := func(v int16, b uint8) uint16 {
		if b == 0 || b > 16 {
			return 0
		}
		return uint16(v) << uint(16-b)
	}
	sum := int16(up(cur, bits) + up(refv, refBits))
	if bits == 0 {
		return 0
	}
	return sum >> uint(16-bits)
}

type bcBits128 struct {
	lo uint64
	hi uint64
	at uint32
}

func (w *bcBits128) put(val uint64, n uint32) {
	if n > 0 && w.at < 128 {
		var mask uint64
		if n >= 64 {
			mask = ^uint64(0)
		} else {
			mask = (uint64(1) << n) - 1
		}
		v := val & mask
		if w.at < 64 {
			w.lo |= v << w.at
			if w.at+n > 64 {
				w.hi |= v >> (64 - w.at)
			}
		} else {
			w.hi |= v << (w.at - 64)
		}
	}
	w.at += n
}

func (w *bcBits128) writeTo(out []byte) {
	binary.LittleEndian.PutUint64(out[0:8], w.lo)
	binary.LittleEndian.PutUint64(out[8:16], w.hi)
}

// --- BC7 ---

var (
	bcAnchor2 = [64]uint8{
		15, 15, 15, 15, 15, 15, 15, 15, 15, 15, 15, 15, 15, 15, 15, 15,
		15, 2, 8, 2, 2, 8, 8, 15, 2, 8, 2, 2, 8, 8, 2, 2,
		15, 15, 6, 8, 2, 8, 15, 15, 2, 8, 2, 2, 2, 15, 15, 6,
		6, 2, 6, 8, 15, 15, 2, 2, 15, 15, 15, 15, 15, 2, 2, 15,
	}
	bcAnchor3A = [64]uint8{
		3, 3, 15, 15, 8, 3, 15, 15, 8, 8, 6, 6, 6, 5, 3, 3,
		3, 3, 8, 15, 3, 3, 6, 10, 5, 8, 8, 6, 8, 5, 15, 15,
		8, 15, 3, 5, 6, 10, 8, 15, 15, 3, 15, 5, 15, 15, 15, 15,
		3, 15, 5, 5, 5, 8, 5, 10, 5, 10, 8, 13, 15, 12, 3, 3,
	}
	bcAnchor3B = [64]uint8{
		15, 8, 8, 3, 15, 15, 3, 8, 15, 15, 15, 15, 15, 15, 15, 8,
		15, 8, 15, 3, 15, 8, 15, 8, 3, 15, 6, 10, 15, 15, 10, 8,
		15, 3, 15, 10, 10, 8, 9, 10, 6, 15, 8, 15, 3, 6, 6, 8,
		15, 3, 15, 15, 15, 15, 15, 15, 15, 15, 15, 15, 3, 15, 15, 8,
	}
	bc7Bits = [8][4]uint8{
		{4, 4, 4, 0}, {6, 6, 6, 0}, {5, 5, 5, 0}, {7, 7, 7, 0},
		{5, 5, 5, 6}, {7, 7, 7, 8}, {7, 7, 7, 7}, {5, 5, 5, 5},
	}
	bc7IndexBits = [8][2]uint8{
		{3, 0}, {3, 0}, {2, 0}, {2, 0}, {3, 2}, {2, 2}, {4, 0}, {2, 0},
	}
)

func bc7HasPBits(mode uint16) bool {
	return mode == 0 || mode == 1 || mode == 3 || mode == 6 || mode == 7
}

func bc7Precision(ctrl uint16) [4]uint8 {
	mode := ctrl & 7
	rot := int((ctrl >> 3) & 3)
	p := bc7Bits[mode]
	if rot != 0 {
		p[3], p[rot-1] = p[rot-1], p[3]
	} else if bc7HasPBits(mode) {
		p[0]++
	}
	return p
}

func decodeBC7Blocks(records []uint8, stride int, dst []byte, t *BCPackTables) error {
	var history [bcHistory]bc7Entry
	var zero bc7Entry
	blocks := len(dst) / 16
	setOffsets := [2][4]int{{22, 24, 26, 28}, {12, 14, 16, 20}}
	for i := 0; i < blocks; i++ {
		s := records[i*stride : (i+1)*stride]
		epRefIdx, ixRefIdx := bcReferences(s, true)
		ctrlIdx := (int(s[31]&15) << 5) | int(s[30]&31)
		ctrl := t.ctrl7[ctrlIdx]
		if ctrl == 0 {
			return errors.New("bcpack: code de contrôle BC7 inutilisé")
		}
		mode := int(ctrl & 7)
		subsets := int((ctrl & 0xFF) >> 6)
		prec := bc7Precision(ctrl)
		three := subsets == 3
		or0 := func(v uint8) uint8 {
			if three {
				return 0
			}
			return v
		}
		if3 := func(v uint8) uint8 {
			if three {
				return v
			}
			return 0
		}
		stored := [3][8]uint8{
			{s[4], s[5], s[6], s[7], s[8], s[9], or0(s[10]), or0(s[11])},
			{s[12], s[13], s[14], s[15], s[16], s[17], or0(s[18]), or0(s[19])},
			{s[20], s[21], if3(s[10]), if3(s[11]), if3(s[18]), if3(s[19]), 0, 0},
		}
		r := &bcPick(i, epRefIdx, history[:], t.bc7, &zero).ep
		ep := bc7Ep{ctrl: ctrl}
		refSubsets := int((r.ctrl & 0xFF) >> 6)
		common := subsets
		if refSubsets < common {
			common = refSubsets
		}
		cols := 6
		if prec[3] > 0 {
			cols = 8
		}
		for c := 0; c < cols; c++ {
			p := prec[c/2]
			if p == 0 {
				continue
			}
			mask := uint16(0xFFFF) << uint(16-p)
			for row := 0; row < subsets; row++ {
				v := stored[row][c]
				var sign uint16
				if v&1 != 0 {
					sign = 0xFFFF
				}
				residual := (uint16(v>>1) ^ sign) << uint(16-p)
				srcRow := 0
				if row < common {
					srcRow = row
				}
				base := r.rows[srcRow][c] & mask
				ep.rows[row][c] = base + residual
			}
		}
		bits := bc7IndexBits[mode]
		rx := &bcPick(i, ixRefIdx, history[:], t.bc7, &zero).ix
		ix := bc7Ix{bits: bits}
		for set := 0; set < 2; set++ {
			if bits[set] == 0 {
				continue
			}
			for k, o := range setOffsets[set] {
				for b := 0; b < 2; b++ {
					pair := bcNibbles(s[o+b], k < 3)
					for h := 0; h < 2; h++ {
						p := 4*k + 2*b + h
						ix.ix[set][p] = bcAddIndex(pair[h], bits[set], rx.ix[set][p], rx.bits[set])
					}
				}
			}
		}
		history[i%bcHistory] = bc7Entry{ep: ep, ix: ix}
		bc7Pack(&ep, &ix, prec, dst[i*16:(i+1)*16])
	}
	return nil
}

func bc7Pack(ep *bc7Ep, ix *bc7Ix, prec [4]uint8, out []byte) {
	ctrl := ep.ctrl
	mode := ctrl & 7
	subsets := int((ctrl & 0xFF) >> 6)
	part := int((ctrl >> 8) & 63)
	rot := int((ctrl >> 3) & 3)
	var w bcBits128
	w.put(uint64(1)<<mode, uint32(mode)+1)
	switch mode {
	case 0:
		w.put(uint64(part), 4)
	case 1, 2, 3, 7:
		w.put(uint64(part), 6)
	case 4:
		w.put(uint64(rot), 2)
		w.put(uint64((ctrl>>5)&1), 1)
	case 5:
		w.put(uint64(rot), 2)
	}
	ends := 2 * subsets
	field := func(row, col int, p uint8) uint64 {
		return uint64(ep.rows[row][col] >> uint(16-p))
	}
	order := [4]int{0, 1, 2, 3}
	if rot != 0 {
		order[3], order[rot-1] = order[rot-1], order[3]
	}
	for _, ch := range order {
		p := prec[ch]
		if p == 0 {
			continue
		}
		drop := uint32(0)
		if ch == 0 && bc7HasPBits(mode) {
			drop = 1
		}
		for e := 0; e < ends; e++ {
			w.put(field(e/2, 2*ch+e%2, p)>>drop, uint32(p)-drop)
		}
	}
	if mode == 1 {
		for row := 0; row < subsets; row++ {
			w.put(field(row, 0, prec[0])&1, 1)
		}
	} else if bc7HasPBits(mode) {
		for e := 0; e < ends; e++ {
			w.put(field(e/2, e%2, prec[0])&1, 1)
		}
	}
	isAnchor := func(p uint8) bool {
		if p == 0 {
			return true
		}
		if subsets == 2 {
			return p == bcAnchor2[part]
		}
		if subsets == 3 {
			return p == bcAnchor3A[part] || p == bcAnchor3B[part]
		}
		return false
	}
	putIndices := func(set int) {
		bits := uint32(ix.bits[set])
		for p := 0; p < 16; p++ {
			n := bits
			if isAnchor(uint8(p)) {
				n--
			}
			w.put(uint64(uint16(ix.ix[set][p])), n)
		}
	}
	switch mode {
	case 4:
		putIndices(1)
		putIndices(0)
	case 5:
		putIndices(0)
		putIndices(1)
	default:
		putIndices(0)
	}
	w.writeTo(out)
}

// --- BC6H ---

var (
	bc6Bits = [15][4]uint8{
		{0, 0, 0, 0},
		{10, 5, 5, 5}, {7, 6, 6, 6}, {11, 5, 4, 4}, {11, 4, 5, 4}, {11, 4, 4, 5},
		{9, 5, 5, 5}, {8, 6, 5, 5}, {8, 5, 6, 5}, {8, 5, 5, 6}, {6, 6, 6, 6},
		{10, 10, 10, 10}, {11, 9, 9, 9}, {12, 8, 8, 8}, {16, 4, 4, 4},
	}
	bc6Code = [15]struct {
		code uint64
		n    uint32
	}{
		{0, 0}, {0, 2}, {1, 2}, {2, 5}, {6, 5}, {10, 5}, {14, 5}, {18, 5},
		{22, 5}, {26, 5}, {30, 5}, {3, 5}, {7, 5}, {11, 5}, {15, 5},
	}
)

type bc6Run struct {
	ch, ep, first, length uint8
}

const (
	bcR uint8 = 0
	bcG uint8 = 1
	bcB uint8 = 2
)

var bc6PartRun = bc6Run{3, 0, 0, 5}

var bc6Layout = [15][]bc6Run{
	nil,
	{
		{bcG, 2, 4, 1}, {bcB, 2, 4, 1}, {bcB, 3, 4, 1}, {bcR, 0, 0, 10}, {bcG, 0, 0, 10}, {bcB, 0, 0, 10},
		{bcR, 1, 0, 5}, {bcG, 3, 4, 1}, {bcG, 2, 0, 4}, {bcG, 1, 0, 5}, {bcB, 3, 0, 1}, {bcG, 3, 0, 4},
		{bcB, 1, 0, 5}, {bcB, 3, 1, 1}, {bcB, 2, 0, 4}, {bcR, 2, 0, 5}, {bcB, 3, 2, 1}, {bcR, 3, 0, 5},
		{bcB, 3, 3, 1}, bc6PartRun,
	},
	{
		{bcG, 2, 5, 1}, {bcG, 3, 4, 1}, {bcG, 3, 5, 1}, {bcR, 0, 0, 7}, {bcB, 3, 0, 1}, {bcB, 3, 1, 1},
		{bcB, 2, 4, 1}, {bcG, 0, 0, 7}, {bcB, 2, 5, 1}, {bcB, 3, 2, 1}, {bcG, 2, 4, 1}, {bcB, 0, 0, 7},
		{bcB, 3, 3, 1}, {bcB, 3, 5, 1}, {bcB, 3, 4, 1}, {bcR, 1, 0, 6}, {bcG, 2, 0, 4}, {bcG, 1, 0, 6},
		{bcG, 3, 0, 4}, {bcB, 1, 0, 6}, {bcB, 2, 0, 4}, {bcR, 2, 0, 6}, {bcR, 3, 0, 6}, bc6PartRun,
	},
	{
		{bcR, 0, 0, 10}, {bcG, 0, 0, 10}, {bcB, 0, 0, 10}, {bcR, 1, 0, 5}, {bcR, 0, 10, 1}, {bcG, 2, 0, 4},
		{bcG, 1, 0, 4}, {bcG, 0, 10, 1}, {bcB, 3, 0, 1}, {bcG, 3, 0, 4}, {bcB, 1, 0, 4}, {bcB, 0, 10, 1},
		{bcB, 3, 1, 1}, {bcB, 2, 0, 4}, {bcR, 2, 0, 5}, {bcB, 3, 2, 1}, {bcR, 3, 0, 5}, {bcB, 3, 3, 1}, bc6PartRun,
	},
	{
		{bcR, 0, 0, 10}, {bcG, 0, 0, 10}, {bcB, 0, 0, 10}, {bcR, 1, 0, 4}, {bcR, 0, 10, 1}, {bcG, 3, 4, 1},
		{bcG, 2, 0, 4}, {bcG, 1, 0, 5}, {bcG, 0, 10, 1}, {bcG, 3, 0, 4}, {bcB, 1, 0, 4}, {bcB, 0, 10, 1},
		{bcB, 3, 1, 1}, {bcB, 2, 0, 4}, {bcR, 2, 0, 4}, {bcB, 3, 0, 1}, {bcB, 3, 2, 1}, {bcR, 3, 0, 4},
		{bcG, 2, 4, 1}, {bcB, 3, 3, 1}, bc6PartRun,
	},
	{
		{bcR, 0, 0, 10}, {bcG, 0, 0, 10}, {bcB, 0, 0, 10}, {bcR, 1, 0, 4}, {bcR, 0, 10, 1}, {bcB, 2, 4, 1},
		{bcG, 2, 0, 4}, {bcG, 1, 0, 4}, {bcG, 0, 10, 1}, {bcB, 3, 0, 1}, {bcG, 3, 0, 4}, {bcB, 1, 0, 5},
		{bcB, 0, 10, 1}, {bcB, 2, 0, 4}, {bcR, 2, 0, 4}, {bcB, 3, 1, 1}, {bcB, 3, 2, 1}, {bcR, 3, 0, 4},
		{bcB, 3, 4, 1}, {bcB, 3, 3, 1}, bc6PartRun,
	},
	{
		{bcR, 0, 0, 9}, {bcB, 2, 4, 1}, {bcG, 0, 0, 9}, {bcG, 2, 4, 1}, {bcB, 0, 0, 9}, {bcB, 3, 4, 1},
		{bcR, 1, 0, 5}, {bcG, 3, 4, 1}, {bcG, 2, 0, 4}, {bcG, 1, 0, 5}, {bcB, 3, 0, 1}, {bcG, 3, 0, 4},
		{bcB, 1, 0, 5}, {bcB, 3, 1, 1}, {bcB, 2, 0, 4}, {bcR, 2, 0, 5}, {bcB, 3, 2, 1}, {bcR, 3, 0, 5},
		{bcB, 3, 3, 1}, bc6PartRun,
	},
	{
		{bcR, 0, 0, 8}, {bcG, 3, 4, 1}, {bcB, 2, 4, 1}, {bcG, 0, 0, 8}, {bcB, 3, 2, 1}, {bcG, 2, 4, 1},
		{bcB, 0, 0, 8}, {bcB, 3, 3, 1}, {bcB, 3, 4, 1}, {bcR, 1, 0, 6}, {bcG, 2, 0, 4}, {bcG, 1, 0, 5},
		{bcB, 3, 0, 1}, {bcG, 3, 0, 4}, {bcB, 1, 0, 5}, {bcB, 3, 1, 1}, {bcB, 2, 0, 4}, {bcR, 2, 0, 6},
		{bcR, 3, 0, 6}, bc6PartRun,
	},
	{
		{bcR, 0, 0, 8}, {bcB, 3, 0, 1}, {bcB, 2, 4, 1}, {bcG, 0, 0, 8}, {bcG, 2, 5, 1}, {bcG, 2, 4, 1},
		{bcB, 0, 0, 8}, {bcG, 3, 5, 1}, {bcB, 3, 4, 1}, {bcR, 1, 0, 5}, {bcG, 3, 4, 1}, {bcG, 2, 0, 4},
		{bcG, 1, 0, 6}, {bcG, 3, 0, 4}, {bcB, 1, 0, 5}, {bcB, 3, 1, 1}, {bcB, 2, 0, 4}, {bcR, 2, 0, 5},
		{bcB, 3, 2, 1}, {bcR, 3, 0, 5}, {bcB, 3, 3, 1}, bc6PartRun,
	},
	{
		{bcR, 0, 0, 8}, {bcB, 3, 1, 1}, {bcB, 2, 4, 1}, {bcG, 0, 0, 8}, {bcB, 2, 5, 1}, {bcG, 2, 4, 1},
		{bcB, 0, 0, 8}, {bcB, 3, 5, 1}, {bcB, 3, 4, 1}, {bcR, 1, 0, 5}, {bcG, 3, 4, 1}, {bcG, 2, 0, 4},
		{bcG, 1, 0, 5}, {bcB, 3, 0, 1}, {bcG, 3, 0, 4}, {bcB, 1, 0, 6}, {bcB, 2, 0, 4}, {bcR, 2, 0, 5},
		{bcB, 3, 2, 1}, {bcR, 3, 0, 5}, {bcB, 3, 3, 1}, bc6PartRun,
	},
	{
		{bcR, 0, 0, 6}, {bcG, 3, 4, 1}, {bcB, 3, 0, 1}, {bcB, 3, 1, 1}, {bcB, 2, 4, 1}, {bcG, 0, 0, 6},
		{bcG, 2, 5, 1}, {bcB, 2, 5, 1}, {bcB, 3, 2, 1}, {bcG, 2, 4, 1}, {bcB, 0, 0, 6}, {bcG, 3, 5, 1},
		{bcB, 3, 3, 1}, {bcB, 3, 5, 1}, {bcB, 3, 4, 1}, {bcR, 1, 0, 6}, {bcG, 2, 0, 4}, {bcG, 1, 0, 6},
		{bcG, 3, 0, 4}, {bcB, 1, 0, 6}, {bcB, 2, 0, 4}, {bcR, 2, 0, 6}, {bcR, 3, 0, 6}, bc6PartRun,
	},
	{
		{bcR, 0, 0, 10}, {bcG, 0, 0, 10}, {bcB, 0, 0, 10}, {bcR, 1, 0, 10}, {bcG, 1, 0, 10}, {bcB, 1, 0, 10},
	},
	{
		{bcR, 0, 0, 10}, {bcG, 0, 0, 10}, {bcB, 0, 0, 10}, {bcR, 1, 0, 9}, {bcR, 0, 10, 1}, {bcG, 1, 0, 9},
		{bcG, 0, 10, 1}, {bcB, 1, 0, 9}, {bcB, 0, 10, 1},
	},
	{
		{bcR, 0, 0, 10}, {bcG, 0, 0, 10}, {bcB, 0, 0, 10}, {bcR, 1, 0, 8}, {bcR, 0, 10, 2}, {bcG, 1, 0, 8},
		{bcG, 0, 10, 2}, {bcB, 1, 0, 8}, {bcB, 0, 10, 2},
	},
	{
		{bcR, 0, 0, 10}, {bcG, 0, 0, 10}, {bcB, 0, 0, 10}, {bcR, 1, 0, 4}, {bcR, 0, 10, 6}, {bcG, 1, 0, 4},
		{bcG, 0, 10, 6}, {bcB, 1, 0, 4}, {bcB, 0, 10, 6},
	},
}

func bc6IndexBits(mode uint8) uint8 {
	if mode >= 1 && mode <= 10 {
		return 3
	}
	if mode >= 11 && mode <= 14 {
		return 4
	}
	return 0
}

func decodeBC6Blocks(records []uint8, stride int, dst []byte, dictionary []bc6Entry) error {
	var history [bcHistory]bc6Entry
	zero := bc6Entry{ix: bc6Ix{mode: 11}}
	z := func(v uint16) uint16 { return (v >> 1) ^ (-(v & 1)) }
	wide := func(a, b uint8) uint16 {
		var acc uint16
		for k := uint(0); k < 8; k++ {
			acc |= uint16((a>>k)&1)<<(2*k) | uint16((b>>k)&1)<<(2*k+1)
		}
		return acc
	}
	blocks := len(dst) / 16
	for i := 0; i < blocks; i++ {
		s := records[i*stride : (i+1)*stride]
		epRefIdx, ixRefIdx := bcReferences(s, true)
		mode := s[30] + 1
		if mode < 1 || mode > 14 {
			return errors.New("bcpack: mode BC6H hors limites")
		}
		two := mode < 11
		var shift [4]uint
		for k := 0; k < 4; k++ {
			shift[k] = uint(16 - bc6Bits[mode][k])
		}
		e0 := [3]uint16{z(wide(s[4], s[5])), z(wide(s[6], s[7])), z(wide(s[8], s[9]))}
		e1 := [3]uint16{z(wide(s[10], s[11])), z(wide(s[12], s[13])), z(wide(s[14], s[15]))}
		var e2, e3 [3]uint16
		if two {
			e2 = [3]uint16{z(uint16(s[16])), z(uint16(s[18])), z(uint16(s[20]))}
			e3 = [3]uint16{z(uint16(s[17])), z(uint16(s[19])), z(uint16(s[21]))}
		} else {
			e2, e3 = e0, e1
		}
		r := &bcPick(i, epRefIdx, history[:], dictionary, &zero).ep
		var ep bc6Ep
		for c := 0; c < 3; c++ {
			ep.e[0][c] = (r.e[0][c] + (e0[c] << shift[0])) & (uint16(0xFFFF) << shift[0])
		}
		resList := [3][3]uint16{e1, e2, e3}
		limit := 1
		if two {
			limit = 3
		}
		for k := 0; k < limit; k++ {
			for c := 0; c < 3; c++ {
				ep.e[k+1][c] = (r.e[k+1][c] + (resList[k][c] << shift[1+c])) & (uint16(0xFFFF) << shift[1+c])
			}
		}
		if !two {
			for c := 0; c < 3; c++ {
				ep.e[2][c] = (ep.e[0][c] >> shift[0]) << shift[1]
			}
			ep.e[3] = ep.e[1]
		}
		rx := &bcPick(i, ixRefIdx, history[:], dictionary, &zero).ix
		bits := bc6IndexBits(mode)
		ix := bc6Ix{mode: mode}
		for k := 0; k < 8; k++ {
			pair := bcNibbles(s[22+k], k < 6)
			for h := 0; h < 2; h++ {
				p := 2*k + h
				ix.ix[p] = bcAddIndex(pair[h], bits, rx.ix[p], bc6IndexBits(rx.mode))
			}
		}
		history[i%bcHistory] = bc6Entry{ep: ep, ix: ix}

		field := func(k, c int) uint64 {
			sh := shift[0]
			if k > 0 {
				sh = shift[1+c]
			}
			return uint64((ep.e[k][c] ^ 0x8000) >> sh)
		}
		var w bcBits128
		cc := bc6Code[mode]
		w.put(cc.code, cc.n)
		part := s[31] & 31
		for _, run := range bc6Layout[mode] {
			if run.ch == 3 {
				w.put(uint64(part), 5)
			} else {
				w.put(field(int(run.ep), int(run.ch))>>run.first, uint32(run.length))
			}
		}
		anchor := uint8(0)
		if two {
			anchor = bcAnchor2[part]
		}
		for p := 0; p < 16; p++ {
			short := p == 0 || p == int(anchor)
			v := uint16(ix.ix[p]) + (uint16(1) << uint(bits-1))
			n := uint32(bits)
			if short {
				n--
			}
			w.put(uint64(v), n)
		}
		w.writeTo(dst[i*16 : (i+1)*16])
	}
	return nil
}
