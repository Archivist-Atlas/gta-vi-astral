package shader

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"fmt"
	"io"
)

const (
	xdatOffset        = 0x18
	maxXDATEntries    = 64
	xdatKindNative    = 0x0D
	xbchVersion       = 3
	xbchHeaderSize    = 0x18
	fssmVersion       = 0x101
	fssmHeaderSize    = 0x10
	fssmAbsentSlot    = uint64(0xDEADBEEFDEADBEEF)
	fssmRecordSize    = 0x54
	fssmFlagWave32    = uint32(1 << 5)
	fssmEntrySize     = 20
	maxUnpackedFSSM   = 16 << 20
)

// HwStage représente l'étage matériel GFX10 / RDNA2 d'un programme natif Xbox Series.
type HwStage uint8

const (
	StageLS HwStage = iota
	StageHS
	StageES
	StageGS
	StageVS
	StagePS
	StageMS
	StageCS
)

func (s HwStage) String() string {
	switch s {
	case StageLS:
		return "LS"
	case StageHS:
		return "HS"
	case StageES:
		return "ES"
	case StageGS:
		return "GS"
	case StageVS:
		return "VS"
	case StagePS:
		return "PS"
	case StageMS:
		return "MS"
	case StageCS:
		return "CS"
	default:
		return fmt.Sprintf("Stage(%d)", s)
	}
}

// UserSGPR retourne l'indice du SGPR contenant le mot user-data demandé.
// Les étages fusionnés et Mesh Shaders réservent 8 SGPRs système en tête (règle VIctory/RDNA2).
func (s HwStage) UserSGPR(index uint8) uint8 {
	switch s {
	case StageLS, StageHS, StageES, StageGS, StageMS:
		return index + 8
	default:
		return index
	}
}

// RootParam décrit un paramètre de root signature mappé dans les user SGPRs.
type RootParam struct {
	RootType       uint32
	TableOffset    uint32
	UserSGPRs      uint32
	BaseRegister   uint32
	FirstUserDword uint8
	RootSlot       uint8
}

// UsedResource décrit un descripteur consommé par le shader.
type UsedResource struct {
	ResourceType   uint8
	RootType       uint8
	TableOffset    uint32
	LowerBound     uint32
	Stride         uint32
	DescDwords     uint8
	FirstUserDword uint8
	RootSlot       uint8
}

// NativeProgram représente un programme binaire RDNA2 (GFX10) extrait d'un bloc FSSM.
type NativeProgram struct {
	Stage          HwStage
	Wave64         bool
	VGPRs          uint16
	SGPRs          uint16
	LDSBytes       uint32
	ThreadGroup    [3]uint32
	Layout         []RootParam
	UsedResources  []UsedResource
	Code           []byte
}

var fssmSlots = []struct {
	stage  HwStage
	offset int
}{
	{StageLS, 0x08},
	{StageHS, 0x10},
	{StageES, 0x18},
	{StageGS, 0x20},
	{StageVS, 0x28},
	{StagePS, 0x30},
	{StageMS, 0x48},
	{StageCS, 0x60},
}

// ExtractNativePrograms extrait tous les programmes RDNA2 natifs d'un conteneur DXBC
// en suivant la chaîne console : DXIL -> XDAT -> XBCH (zlib) -> FSSM.
func ExtractNativePrograms(c *Container) ([]NativeProgram, error) {
	dxilPart, ok := c.Part(FourCCDXIL)
	if !ok {
		return nil, fmt.Errorf("%w : bloc DXIL absent", ErrMalformed)
	}
	return ExtractNativeFromDXILPart(dxilPart.Data)
}

// ExtractNativeFromDXILPart parcourt l'index XDAT dans un bloc DXIL console.
func ExtractNativeFromDXILPart(part []byte) ([]NativeProgram, error) {
	if len(part) < xdatOffset+12 || !bytes.Equal(part[xdatOffset:xdatOffset+4], []byte("XDAT")) {
		return nil, fmt.Errorf("%w : en-tête XDAT introuvable", ErrMalformed)
	}
	entryCount := int(binary.LittleEndian.Uint32(part[xdatOffset+4 : xdatOffset+8]))
	if entryCount > maxXDATEntries {
		return nil, fmt.Errorf("%w : trop d'entrées XDAT (%d)", ErrMalformed, entryCount)
	}

	var programs []NativeProgram
	tableOff := xdatOffset + 12
	for i := 0; i < entryCount; i++ {
		eOff := tableOff + i*12
		if eOff+12 > len(part) {
			break
		}
		kind := binary.LittleEndian.Uint32(part[eOff : eOff+4])
		off := int(binary.LittleEndian.Uint32(part[eOff+4 : eOff+8]))
		size := int(binary.LittleEndian.Uint32(part[eOff+8 : eOff+12]))
		if kind != xdatKindNative {
			continue
		}
		if off < 0 || off+size > len(part) {
			return nil, fmt.Errorf("%w : entrée XDAT %d hors limites", ErrMalformed, i)
		}
		fssm, err := unpackXBCH(part[off : off+size])
		if err != nil {
			return nil, err
		}
		progs, err := parseFSSM(fssm)
		if err != nil {
			return nil, err
		}
		programs = append(programs, progs...)
	}
	return programs, nil
}

func unpackXBCH(blob []byte) ([]byte, error) {
	if len(blob) < xbchHeaderSize || !bytes.Equal(blob[0:4], []byte("XBCH")) {
		return nil, fmt.Errorf("%w : signature XBCH absente", ErrMalformed)
	}
	ver := binary.LittleEndian.Uint32(blob[4:8])
	if ver != xbchVersion {
		return nil, fmt.Errorf("%w : version XBCH non supportée (%d)", ErrMalformed, ver)
	}
	packedSize := binary.LittleEndian.Uint64(blob[8:16])
	unpackedSize := binary.LittleEndian.Uint64(blob[16:24])
	if unpackedSize == 0 || unpackedSize > maxUnpackedFSSM {
		return nil, fmt.Errorf("%w : taille XBCH décompressée invalide (%d)", ErrMalformed, unpackedSize)
	}
	if uint64(xbchHeaderSize)+packedSize > uint64(len(blob)) {
		return nil, fmt.Errorf("%w : flux XBCH tronqué", ErrMalformed)
	}

	zr, err := zlib.NewReader(bytes.NewReader(blob[xbchHeaderSize : xbchHeaderSize+int(packedSize)]))
	if err != nil {
		return nil, fmt.Errorf("ouverture zlib XBCH : %w", err)
	}
	defer zr.Close()

	out, err := io.ReadAll(io.LimitReader(zr, int64(unpackedSize)+1))
	if err != nil {
		return nil, fmt.Errorf("décompression zlib XBCH : %w", err)
	}
	if uint64(len(out)) != unpackedSize {
		return nil, fmt.Errorf("%w : taille FSSM (%d != %d)", ErrMalformed, len(out), unpackedSize)
	}
	return out, nil
}

func parseFSSM(data []byte) ([]NativeProgram, error) {
	if len(data) < fssmHeaderSize || !bytes.Equal(data[0:4], []byte("FSSM")) {
		return nil, fmt.Errorf("%w : signature FSSM absente", ErrMalformed)
	}
	ver := binary.LittleEndian.Uint32(data[4:8])
	if ver != fssmVersion {
		return nil, fmt.Errorf("%w : version FSSM non supportée (0x%x)", ErrMalformed, ver)
	}
	payloadSize := int(binary.LittleEndian.Uint64(data[8:16]))
	if fssmHeaderSize+payloadSize > len(data) {
		return nil, fmt.Errorf("%w : payload FSSM tronqué", ErrMalformed)
	}
	payload := data[fssmHeaderSize : fssmHeaderSize+payloadSize]
	if len(payload) < 0x68 {
		return nil, fmt.Errorf("%w : en-tête payload FSSM trop court", ErrMalformed)
	}

	var out []NativeProgram
	for _, slot := range fssmSlots {
		recOff := binary.LittleEndian.Uint64(payload[slot.offset : slot.offset+8])
		if recOff == fssmAbsentSlot || recOff == 0 {
			continue
		}
		prog, err := parseFSSMRecord(payload, int(recOff), slot.stage)
		if err != nil {
			return nil, err
		}
		out = append(out, prog)
	}
	return out, nil
}

func parseFSSMRecord(payload []byte, recOff int, stage HwStage) (NativeProgram, error) {
	if recOff < 0 || recOff+fssmRecordSize > len(payload) {
		return NativeProgram{}, fmt.Errorf("%w : record FSSM %s hors limites", ErrMalformed, stage)
	}
	rec := payload[recOff:]
	codeOff := int(binary.LittleEndian.Uint64(rec[0x00:0x08]))
	codeSize := int(binary.LittleEndian.Uint64(rec[0x08:0x10]))
	flags := binary.LittleEndian.Uint32(rec[0x14:0x18])
	layoutOff := int(binary.LittleEndian.Uint64(rec[0x28:0x30]))
	usedOff := int(binary.LittleEndian.Uint64(rec[0x30:0x38]))
	layoutCount := int(binary.LittleEndian.Uint32(rec[0x38:0x3C]))
	usedCount := int(binary.LittleEndian.Uint32(rec[0x3C:0x40]))
	ldsBytes := binary.LittleEndian.Uint32(rec[0x4C:0x50])
	regsWord := binary.LittleEndian.Uint32(rec[0x50:0x54])

	if codeOff < 0 || codeSize < 0 || codeOff+codeSize > len(payload) {
		return NativeProgram{}, fmt.Errorf("%w : code RDNA2 %s hors limites", ErrMalformed, stage)
	}

	code := make([]byte, codeSize)
	copy(code, payload[codeOff:codeOff+codeSize])

	prog := NativeProgram{
		Stage:    stage,
		Wave64:   (flags & fssmFlagWave32) == 0,
		VGPRs:    uint16(regsWord & 0x3FF),
		SGPRs:    uint16((regsWord >> 20) & 0xFF),
		LDSBytes: ldsBytes,
		Code:     code,
	}

	if layoutCount > 0 && layoutCount <= 256 && layoutOff >= 0 && layoutOff+layoutCount*fssmEntrySize <= len(payload) {
		prog.Layout = make([]RootParam, 0, layoutCount)
		for i := 0; i < layoutCount; i++ {
			e := payload[layoutOff+i*fssmEntrySize : layoutOff+(i+1)*fssmEntrySize]
			prog.Layout = append(prog.Layout, RootParam{
				RootType:       binary.LittleEndian.Uint32(e[0:4]),
				TableOffset:    binary.LittleEndian.Uint32(e[4:8]),
				UserSGPRs:      binary.LittleEndian.Uint32(e[8:12]),
				BaseRegister:   binary.LittleEndian.Uint32(e[12:16]),
				FirstUserDword: e[16],
				RootSlot:       e[17],
			})
		}
	}

	if usedCount > 0 && usedCount <= 512 && usedOff >= 0 && usedOff+usedCount*fssmEntrySize <= len(payload) {
		prog.UsedResources = make([]UsedResource, 0, usedCount)
		for i := 0; i < usedCount; i++ {
			e := payload[usedOff+i*fssmEntrySize : usedOff+(i+1)*fssmEntrySize]
			prog.UsedResources = append(prog.UsedResources, UsedResource{
				ResourceType:   e[0],
				RootType:       e[1],
				TableOffset:    binary.LittleEndian.Uint32(e[4:8]),
				LowerBound:     binary.LittleEndian.Uint32(e[8:12]),
				Stride:         binary.LittleEndian.Uint32(e[12:16]),
				DescDwords:     e[16],
				FirstUserDword: e[17],
				RootSlot:       e[18],
			})
		}
	}

	// Taille de thread-group pour les compute shaders (record + 0x54 + 0x28)
	if stage == StageCS {
		tgOff := recOff + fssmRecordSize + 0x28
		if tgOff+12 <= len(payload) {
			prog.ThreadGroup = [3]uint32{
				binary.LittleEndian.Uint32(payload[tgOff : tgOff+4]),
				binary.LittleEndian.Uint32(payload[tgOff+4 : tgOff+8]),
				binary.LittleEndian.Uint32(payload[tgOff+8 : tgOff+12]),
			}
		}
	}

	return prog, nil
}
