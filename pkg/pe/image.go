// Package pe fournit une analyse en lecture seule des images PE32+ x86-64
// (inspirée de victory-pe et de Leonida pe_inspect.py) : en-têtes, sections,
// entropie de Shannon, imports, delay-imports, exports, TLS et CodeView RSDS.
package pe

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"os"
	"strings"
)

const (
	MachineAMD64 = 0x8664
	MagicPE32P   = 0x020B

	DirExport        = 0
	DirImport        = 1
	DirException     = 3
	DirBaseReloc     = 5
	DirDebug         = 6
	DirTLS           = 9
	DirLoadConfig    = 10
	DirDelayImport   = 13
	DebugTypeCodeView = 2
)

var (
	ErrNotPE       = errors.New("fichier non PE32+")
	ErrUnsupported = errors.New("architecture ou format PE non supporté")
	ErrTruncated   = errors.New("image PE tronquée ou malformée")
)

// ImageInfo rassemble toutes les métadonnées utiles d'une image PE32+ AMD64.
type ImageInfo struct {
	Machine            uint16
	Characteristics    uint16
	Timestamp          uint32
	Subsystem          uint16
	DLLCharacteristics uint16
	ImageBase          uint64
	SizeOfImage        uint32
	EntryRVA           uint32
	StackReserve       uint64
	StackCommit        uint64
	Sections           []SectionInfo
	Imports            []ImportModule
	DelayImports       []ImportModule
	Exports            []ExportInfo
	TLS                *TLSInfo
	HasRelocations     bool
	HasExceptionTable  bool
	ExceptionCount     int
	HasLoadConfig      bool
	CodeView           *CodeViewInfo
}

// SectionInfo décrit une section PE et son entropie de Shannon (détection chiffrement/compression).
type SectionInfo struct {
	Name            string
	RVA             uint32
	VirtualSize     uint32
	FileOffset      uint32
	FileSize        uint32
	Characteristics uint32
	Entropy         float64
}

// ImportModule regroupe les symboles importés depuis une même DLL.
type ImportModule struct {
	DLL     string
	Entries []ImportEntry
}

// ImportEntry associe un symbole importé à son slot IAT (RVA).
type ImportEntry struct {
	Name       string
	Hint       uint16
	Ordinal    uint16
	ByOrdinal  bool
	IATSlotRVA uint32
}

// DisplayName retourne le nom lisible du symbole ("Func" ou "#123").
func (e ImportEntry) DisplayName() string {
	if e.ByOrdinal {
		return fmt.Sprintf("#%d", e.Ordinal)
	}
	return e.Name
}

// ExportInfo décrit un symbole exporté (ou transféré / forwarder).
type ExportInfo struct {
	Name    string
	Ordinal uint16
	RVA     uint32
	Forward string
}

// TLSInfo décrit le répertoire TLS statique et les callbacks TLS.
type TLSInfo struct {
	DirectoryRVA uint32
	CallbacksVA  uint64
	CallbackRVAs []uint32
}

// CodeViewInfo identifie exactement le build via son enregistrement RSDS.
type CodeViewInfo struct {
	PDBPath string
	GUID    [16]byte
	Age     uint32
}

// SymbolKey retourne la clé GUID+Age au format serveur de symboles Microsoft.
func (c CodeViewInfo) SymbolKey() string {
	d1 := binary.LittleEndian.Uint32(c.GUID[0:4])
	d2 := binary.LittleEndian.Uint16(c.GUID[4:6])
	d3 := binary.LittleEndian.Uint16(c.GUID[6:8])
	return fmt.Sprintf("%08X%04X%04X%02X%02X%02X%02X%02X%02X%02X%02X%X",
		d1, d2, d3,
		c.GUID[8], c.GUID[9], c.GUID[10], c.GUID[11],
		c.GUID[12], c.GUID[13], c.GUID[14], c.GUID[15],
		c.Age,
	)
}

type dataDir struct {
	rva  uint32
	size uint32
}

// ParseFile lit et analyse un fichier PE32+ depuis le disque.
func ParseFile(path string) (*ImageInfo, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse analyse une image PE32+ x86-64 complète en une passe.
func Parse(data []byte) (*ImageInfo, error) {
	if len(data) < 0x40 || data[0] != 'M' || data[1] != 'Z' {
		return nil, ErrNotPE
	}
	peOff := int(binary.LittleEndian.Uint32(data[0x3C:0x40]))
	if peOff < 0 || peOff+24 > len(data) || !bytes.Equal(data[peOff:peOff+4], []byte{'P', 'E', 0, 0}) {
		return nil, ErrNotPE
	}

	fhOff := peOff + 4
	machine := binary.LittleEndian.Uint16(data[fhOff : fhOff+2])
	if machine != MachineAMD64 {
		return nil, fmt.Errorf("%w : machine 0x%04x (attendu AMD64 0x8664)", ErrUnsupported, machine)
	}
	numSections := int(binary.LittleEndian.Uint16(data[fhOff+2 : fhOff+4]))
	timestamp := binary.LittleEndian.Uint32(data[fhOff+4 : fhOff+8])
	optSize := int(binary.LittleEndian.Uint16(data[fhOff+16 : fhOff+18]))
	characteristics := binary.LittleEndian.Uint16(data[fhOff+18 : fhOff+20])

	optOff := fhOff + 20
	if optSize < 112 || optOff+optSize > len(data) {
		return nil, ErrTruncated
	}
	if binary.LittleEndian.Uint16(data[optOff:optOff+2]) != MagicPE32P {
		return nil, fmt.Errorf("%w : pas PE32+ (magic != 0x20b)", ErrUnsupported)
	}

	info := &ImageInfo{
		Machine:            machine,
		Characteristics:    characteristics,
		Timestamp:          timestamp,
		EntryRVA:           binary.LittleEndian.Uint32(data[optOff+16 : optOff+20]),
		ImageBase:          binary.LittleEndian.Uint64(data[optOff+24 : optOff+32]),
		SizeOfImage:        binary.LittleEndian.Uint32(data[optOff+56 : optOff+60]),
		Subsystem:          binary.LittleEndian.Uint16(data[optOff+68 : optOff+70]),
		DLLCharacteristics: binary.LittleEndian.Uint16(data[optOff+70 : optOff+72]),
		StackReserve:       binary.LittleEndian.Uint64(data[optOff+72 : optOff+80]),
		StackCommit:        binary.LittleEndian.Uint64(data[optOff+80 : optOff+88]),
	}

	numRvaAndSizes := int(binary.LittleEndian.Uint32(data[optOff+108 : optOff+112]))
	if numRvaAndSizes > 16 {
		numRvaAndSizes = 16
	}
	dirs := make([]dataDir, 16)
	for i := 0; i < numRvaAndSizes; i++ {
		dOff := optOff + 112 + i*8
		if dOff+8 > len(data) {
			break
		}
		dirs[i] = dataDir{
			rva:  binary.LittleEndian.Uint32(data[dOff : dOff+4]),
			size: binary.LittleEndian.Uint32(data[dOff+4 : dOff+8]),
		}
	}

	secOff := optOff + optSize
	if secOff+numSections*40 > len(data) {
		return nil, ErrTruncated
	}
	info.Sections = make([]SectionInfo, 0, numSections)
	for i := 0; i < numSections; i++ {
		s := data[secOff+i*40 : secOff+(i+1)*40]
		name := strings.TrimRight(string(s[0:8]), "\x00")
		vsize := binary.LittleEndian.Uint32(s[8:12])
		rva := binary.LittleEndian.Uint32(s[12:16])
		fsize := binary.LittleEndian.Uint32(s[16:20])
		foff := binary.LittleEndian.Uint32(s[20:24])
		chars := binary.LittleEndian.Uint32(s[36:40])

		var entropy float64
		if fsize > 0 && uint64(foff)+uint64(fsize) <= uint64(len(data)) {
			entropy = ShannonEntropy(data[foff : foff+fsize])
		}
		info.Sections = append(info.Sections, SectionInfo{
			Name:            name,
			RVA:             rva,
			VirtualSize:     vsize,
			FileOffset:      foff,
			FileSize:        fsize,
			Characteristics: chars,
			Entropy:         entropy,
		})
	}

	info.HasRelocations = dirs[DirBaseReloc].rva != 0 && dirs[DirBaseReloc].size > 0
	info.HasExceptionTable = dirs[DirException].rva != 0 && dirs[DirException].size > 0
	if info.HasExceptionTable {
		// Chaque entrée RUNTIME_FUNCTION (.pdata) fait 12 octets.
		info.ExceptionCount = int(dirs[DirException].size / 12)
	}
	info.HasLoadConfig = dirs[DirLoadConfig].rva != 0 && dirs[DirLoadConfig].size > 0

	info.Imports = parseImports(data, info.Sections, dirs[DirImport])
	info.DelayImports = parseDelayImports(data, info.Sections, dirs[DirDelayImport], info.ImageBase)
	info.Exports = parseExports(data, info.Sections, dirs[DirExport])
	info.TLS = parseTLS(data, info.Sections, dirs[DirTLS], info.ImageBase)
	info.CodeView = parseCodeView(data, info.Sections, dirs[DirDebug])

	return info, nil
}

// TotalImports retourne le nombre total de fonctions importées (directes + différées).
func (i *ImageInfo) TotalImports() int {
	n := 0
	for _, m := range i.Imports {
		n += len(m.Entries)
	}
	for _, m := range i.DelayImports {
		n += len(m.Entries)
	}
	return n
}

// RVAToOffset convertit une RVA en offset fichier dans l'image.
func RVAToOffset(sections []SectionInfo, rva uint32) (int, bool) {
	for _, s := range sections {
		span := s.VirtualSize
		if s.FileSize > span {
			span = s.FileSize
		}
		if rva >= s.RVA && rva < s.RVA+span {
			off := uint64(s.FileOffset) + uint64(rva-s.RVA)
			if rva-s.RVA >= s.FileSize {
				return 0, false
			}
			return int(off), true
		}
	}
	return 0, false
}

// ShannonEntropy calcule l'entropie en bits/octet d'un bloc (proche de 8.0 = chiffré/compressé).
func ShannonEntropy(data []byte) float64 {
	if len(data) == 0 {
		return 0
	}
	var counts [256]uint64
	for _, b := range data {
		counts[b]++
	}
	total := float64(len(data))
	var h float64
	for _, c := range counts {
		if c == 0 {
			continue
		}
		p := float64(c) / total
		h -= p * math.Log2(p)
	}
	return h
}

func readCString(data []byte, off int) string {
	if off < 0 || off >= len(data) {
		return ""
	}
	end := bytes.IndexByte(data[off:], 0)
	if end < 0 {
		return string(data[off:])
	}
	return string(data[off : off+end])
}

func parseImports(data []byte, sections []SectionInfo, dir dataDir) []ImportModule {
	if dir.rva == 0 || dir.size == 0 {
		return nil
	}
	off, ok := RVAToOffset(sections, dir.rva)
	if !ok {
		return nil
	}

	var modules []ImportModule
	for off+20 <= len(data) {
		origFirstThunk := binary.LittleEndian.Uint32(data[off : off+4])
		nameRVA := binary.LittleEndian.Uint32(data[off+12 : off+16])
		firstThunk := binary.LittleEndian.Uint32(data[off+16 : off+20])
		off += 20

		if origFirstThunk == 0 && nameRVA == 0 && firstThunk == 0 {
			break
		}
		nameOff, ok := RVAToOffset(sections, nameRVA)
		if !ok {
			continue
		}
		dllName := readCString(data, nameOff)
		thunkRVA := origFirstThunk
		if thunkRVA == 0 {
			thunkRVA = firstThunk
		}
		entries := parseThunkList64(data, sections, thunkRVA, firstThunk, 0)
		modules = append(modules, ImportModule{DLL: dllName, Entries: entries})
	}
	return modules
}

func parseDelayImports(data []byte, sections []SectionInfo, dir dataDir, imageBase uint64) []ImportModule {
	if dir.rva == 0 || dir.size == 0 {
		return nil
	}
	off, ok := RVAToOffset(sections, dir.rva)
	if !ok {
		return nil
	}

	var modules []ImportModule
	for off+32 <= len(data) {
		attrs := binary.LittleEndian.Uint32(data[off : off+4])
		nameVal := binary.LittleEndian.Uint32(data[off+4 : off+8])
		iatVal := binary.LittleEndian.Uint32(data[off+12 : off+16])
		intVal := binary.LittleEndian.Uint32(data[off+16 : off+20])
		off += 32

		if nameVal == 0 && iatVal == 0 && intVal == 0 {
			break
		}
		// attrs bit 0 = 1 => adresses relatives (RVA), sinon VA
		var vaBase uint64
		if attrs&1 == 0 {
			vaBase = imageBase
		}
		nameRVA := nameVal
		iatRVA := iatVal
		intRVA := intVal
		if vaBase != 0 {
			nameRVA = uint32(uint64(nameVal) - vaBase)
			iatRVA = uint32(uint64(iatVal) - vaBase)
			intRVA = uint32(uint64(intVal) - vaBase)
		}
		nameOff, ok := RVAToOffset(sections, nameRVA)
		if !ok {
			continue
		}
		dllName := readCString(data, nameOff)
		entries := parseThunkList64(data, sections, intRVA, iatRVA, vaBase)
		modules = append(modules, ImportModule{DLL: dllName, Entries: entries})
	}
	return modules
}

func parseThunkList64(data []byte, sections []SectionInfo, thunkRVA, iatRVA uint32, vaBase uint64) []ImportEntry {
	tOff, ok := RVAToOffset(sections, thunkRVA)
	if !ok {
		return nil
	}
	var entries []ImportEntry
	for idx := uint32(0); tOff+8 <= len(data); idx++ {
		val := binary.LittleEndian.Uint64(data[tOff : tOff+8])
		tOff += 8
		if val == 0 {
			break
		}
		slotRVA := iatRVA + idx*8
		if val&(1<<63) != 0 {
			entries = append(entries, ImportEntry{
				Ordinal:    uint16(val & 0xFFFF),
				ByOrdinal:  true,
				IATSlotRVA: slotRVA,
			})
			continue
		}
		hintNameRVA := uint32(val)
		if vaBase != 0 && val >= vaBase {
			hintNameRVA = uint32(val - vaBase)
		}
		hnOff, ok := RVAToOffset(sections, hintNameRVA)
		if !ok || hnOff+2 >= len(data) {
			continue
		}
		hint := binary.LittleEndian.Uint16(data[hnOff : hnOff+2])
		name := readCString(data, hnOff+2)
		entries = append(entries, ImportEntry{
			Name:       name,
			Hint:       hint,
			IATSlotRVA: slotRVA,
		})
	}
	return entries
}

func parseExports(data []byte, sections []SectionInfo, dir dataDir) []ExportInfo {
	if dir.rva == 0 || dir.size < 40 {
		return nil
	}
	off, ok := RVAToOffset(sections, dir.rva)
	if !ok || off+40 > len(data) {
		return nil
	}
	ordBase := binary.LittleEndian.Uint32(data[off+16 : off+20])
	numFuncs := binary.LittleEndian.Uint32(data[off+20 : off+24])
	numNames := binary.LittleEndian.Uint32(data[off+24 : off+28])
	addrTableRVA := binary.LittleEndian.Uint32(data[off+28 : off+32])
	nameTableRVA := binary.LittleEndian.Uint32(data[off+32 : off+36])
	ordTableRVA := binary.LittleEndian.Uint32(data[off+36 : off+40])

	addrOff, ok := RVAToOffset(sections, addrTableRVA)
	if !ok || numFuncs > 65536 {
		return nil
	}
	nameByIndex := make(map[uint32]string, numNames)
	if nameOff, okN := RVAToOffset(sections, nameTableRVA); okN {
		if ordOff, okO := RVAToOffset(sections, ordTableRVA); okO {
			for i := uint32(0); i < numNames; i++ {
				if nameOff+int(i*4)+4 > len(data) || ordOff+int(i*2)+2 > len(data) {
					break
				}
				nRVA := binary.LittleEndian.Uint32(data[nameOff+int(i*4) : nameOff+int(i*4)+4])
				ordIdx := uint32(binary.LittleEndian.Uint16(data[ordOff+int(i*2) : ordOff+int(i*2)+2]))
				if strOff, okS := RVAToOffset(sections, nRVA); okS {
					nameByIndex[ordIdx] = readCString(data, strOff)
				}
			}
		}
	}

	exports := make([]ExportInfo, 0, numFuncs)
	for i := uint32(0); i < numFuncs; i++ {
		if addrOff+int(i*4)+4 > len(data) {
			break
		}
		fnRVA := binary.LittleEndian.Uint32(data[addrOff+int(i*4) : addrOff+int(i*4)+4])
		if fnRVA == 0 {
			continue
		}
		exp := ExportInfo{
			Name:    nameByIndex[i],
			Ordinal: uint16(ordBase + i),
		}
		if fnRVA >= dir.rva && fnRVA < dir.rva+dir.size {
			if fwdOff, okF := RVAToOffset(sections, fnRVA); okF {
				exp.Forward = readCString(data, fwdOff)
			}
		} else {
			exp.RVA = fnRVA
		}
		exports = append(exports, exp)
	}
	return exports
}

func parseTLS(data []byte, sections []SectionInfo, dir dataDir, imageBase uint64) *TLSInfo {
	if dir.rva == 0 || dir.size < 40 {
		return nil
	}
	off, ok := RVAToOffset(sections, dir.rva)
	if !ok || off+40 > len(data) {
		return nil
	}
	cbVA := binary.LittleEndian.Uint64(data[off+24 : off+32])
	tls := &TLSInfo{
		DirectoryRVA: dir.rva,
		CallbacksVA:  cbVA,
	}
	if cbVA > imageBase {
		cbRVA := uint32(cbVA - imageBase)
		if cbOff, okCB := RVAToOffset(sections, cbRVA); okCB {
			for count := 0; count < 1024 && cbOff+8 <= len(data); count++ {
				fnVA := binary.LittleEndian.Uint64(data[cbOff : cbOff+8])
				cbOff += 8
				if fnVA == 0 || fnVA < imageBase {
					break
				}
				tls.CallbackRVAs = append(tls.CallbackRVAs, uint32(fnVA-imageBase))
			}
		}
	}
	return tls
}

func parseCodeView(data []byte, sections []SectionInfo, dir dataDir) *CodeViewInfo {
	if dir.rva == 0 || dir.size < 28 {
		return nil
	}
	off, ok := RVAToOffset(sections, dir.rva)
	if !ok {
		return nil
	}
	count := int(dir.size / 28)
	for i := 0; i < count; i++ {
		eOff := off + i*28
		if eOff+28 > len(data) {
			break
		}
		dbgType := binary.LittleEndian.Uint32(data[eOff+12 : eOff+16])
		rawSize := binary.LittleEndian.Uint32(data[eOff+16 : eOff+20])
		rawPtr := binary.LittleEndian.Uint32(data[eOff+24 : eOff+28])
		if dbgType != DebugTypeCodeView || rawSize < 24 || uint64(rawPtr)+uint64(rawSize) > uint64(len(data)) {
			continue
		}
		cv := data[rawPtr : rawPtr+rawSize]
		if !bytes.Equal(cv[0:4], []byte("RSDS")) {
			continue
		}
		var guid [16]byte
		copy(guid[:], cv[4:20])
		age := binary.LittleEndian.Uint32(cv[20:24])
		pdb := readCString(cv, 24)
		return &CodeViewInfo{PDBPath: pdb, GUID: guid, Age: age}
	}
	return nil
}
