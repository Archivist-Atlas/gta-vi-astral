//go:build ignore

// Générateur autonome (Go pur) d'un fichier COFF x64 .syso contenant la ressource
// d'icône Windows (RT_GROUP_ICON + RT_ICON) construite depuis assets/astral.ico.
// Usage : go run cmd/astral/gen_syso.go
package main

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
)

type icoEntry struct {
	width     byte
	height    byte
	colors    byte
	reserved  byte
	planes    uint16
	bitCount  uint16
	bytesIn   uint32
	imgOffset uint32
	data      []byte
}

func main() {
	icoPath := filepath.Join("assets", "astral.ico")
	outPath := filepath.Join("cmd", "astral", "astral_windows_amd64.syso")

	raw, err := os.ReadFile(icoPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lecture %s : %v\n", icoPath, err)
		os.Exit(1)
	}
	if len(raw) < 6 {
		fmt.Fprintln(os.Stderr, "fichier .ico trop court")
		os.Exit(1)
	}
	count := int(binary.LittleEndian.Uint16(raw[4:6]))
	if count == 0 || len(raw) < 6+count*16 {
		fmt.Fprintln(os.Stderr, "en-tête .ico invalide")
		os.Exit(1)
	}

	entries := make([]icoEntry, count)
	for i := 0; i < count; i++ {
		off := 6 + i*16
		e := icoEntry{
			width:     raw[off+0],
			height:    raw[off+1],
			colors:    raw[off+2],
			reserved:  raw[off+3],
			planes:    binary.LittleEndian.Uint16(raw[off+4 : off+6]),
			bitCount:  binary.LittleEndian.Uint16(raw[off+6 : off+8]),
			bytesIn:   binary.LittleEndian.Uint32(raw[off+8 : off+12]),
			imgOffset: binary.LittleEndian.Uint32(raw[off+12 : off+16]),
		}
		if int(e.imgOffset)+int(e.bytesIn) > len(raw) {
			fmt.Fprintf(os.Stderr, "entrée icône %d hors limites\n", i)
			os.Exit(1)
		}
		e.data = raw[e.imgOffset : e.imgOffset+e.bytesIn]
		entries[i] = e
	}

	// Construction de RT_GROUP_ICON (GRPICONDIR + N * GRPICONDIRENTRY de 14 octets)
	var grp bytes.Buffer
	_ = binary.Write(&grp, binary.LittleEndian, uint16(0)) // idReserved
	_ = binary.Write(&grp, binary.LittleEndian, uint16(1)) // idType = ICON
	_ = binary.Write(&grp, binary.LittleEndian, uint16(count))
	for i, e := range entries {
		grp.WriteByte(e.width)
		grp.WriteByte(e.height)
		grp.WriteByte(e.colors)
		grp.WriteByte(0)
		_ = binary.Write(&grp, binary.LittleEndian, e.planes)
		_ = binary.Write(&grp, binary.LittleEndian, e.bitCount)
		_ = binary.Write(&grp, binary.LittleEndian, e.bytesIn)
		_ = binary.Write(&grp, binary.LittleEndian, uint16(i+1)) // nID = 1..N
	}
	grpBytes := grp.Bytes()

	// Construction de l'arborescence .rsrc :
	// - Root dir (2 types : 3 = RT_ICON, 14 = RT_GROUP_ICON)
	// - Type 3 dir (N noms : 1..N) -> N Lang dirs (0x0409) -> N DataEntries
	// - Type 14 dir (1 nom : 1)    -> 1 Lang dir (0x0409)  -> 1 DataEntry
	const (
		dirHdrSize   = 16
		dirEntrySize = 8
		dataEntSize  = 16
	)
	offRoot := uint32(0)
	offType3 := offRoot + dirHdrSize + 2*dirEntrySize
	offType14 := offType3 + dirHdrSize + uint32(count)*dirEntrySize
	offLang3Base := offType14 + dirHdrSize + 1*dirEntrySize
	langDirSize := uint32(dirHdrSize + 1*dirEntrySize)
	offLang14 := offLang3Base + uint32(count)*langDirSize
	offDataEnt3Base := offLang14 + langDirSize
	offDataEnt14 := offDataEnt3Base + uint32(count)*dataEntSize
	offPayload := (offDataEnt14 + dataEntSize + 7) &^ 7

	var rsrc bytes.Buffer
	writeDir := func(numID uint16, items [][2]uint32) {
		var hdr [16]byte
		binary.LittleEndian.PutUint16(hdr[14:16], numID)
		rsrc.Write(hdr[:])
		for _, it := range items {
			_ = binary.Write(&rsrc, binary.LittleEndian, it[0])
			_ = binary.Write(&rsrc, binary.LittleEndian, it[1])
		}
	}

	// 1. Root directory
	writeDir(2, [][2]uint32{
		{3, 0x80000000 | offType3},   // RT_ICON
		{14, 0x80000000 | offType14}, // RT_GROUP_ICON
	})
	// 2. Type 3 directory (RT_ICON)
	t3Items := make([][2]uint32, count)
	for i := 0; i < count; i++ {
		t3Items[i] = [2]uint32{uint32(i + 1), 0x80000000 | (offLang3Base + uint32(i)*langDirSize)}
	}
	writeDir(uint16(count), t3Items)
	// 3. Type 14 directory (RT_GROUP_ICON)
	writeDir(1, [][2]uint32{
		{1, 0x80000000 | offLang14},
	})
	// 4. Lang directories for RT_ICON
	for i := 0; i < count; i++ {
		writeDir(1, [][2]uint32{
			{0x0409, offDataEnt3Base + uint32(i)*dataEntSize},
		})
	}
	// 5. Lang directory for RT_GROUP_ICON
	writeDir(1, [][2]uint32{
		{0x0409, offDataEnt14},
	})

	// 6. Data entries + calcul des offsets de payload + relocations ADDR32NB
	var relocs []uint32
	curPay := offPayload
	payOffsets := make([]uint32, count)
	for i, e := range entries {
		relocs = append(relocs, uint32(rsrc.Len()))
		payOffsets[i] = curPay
		_ = binary.Write(&rsrc, binary.LittleEndian, curPay)
		_ = binary.Write(&rsrc, binary.LittleEndian, uint32(len(e.data)))
		_ = binary.Write(&rsrc, binary.LittleEndian, uint32(0))
		_ = binary.Write(&rsrc, binary.LittleEndian, uint32(0))
		curPay = (curPay + uint32(len(e.data)) + 7) &^ 7
	}
	relocs = append(relocs, uint32(rsrc.Len()))
	grpOff := curPay
	_ = binary.Write(&rsrc, binary.LittleEndian, grpOff)
	_ = binary.Write(&rsrc, binary.LittleEndian, uint32(len(grpBytes)))
	_ = binary.Write(&rsrc, binary.LittleEndian, uint32(0))
	_ = binary.Write(&rsrc, binary.LittleEndian, uint32(0))

	// 7. Payloads alignés sur 8 octets
	padTo := func(target uint32) {
		for uint32(rsrc.Len()) < target {
			rsrc.WriteByte(0)
		}
	}
	for i, e := range entries {
		padTo(payOffsets[i])
		rsrc.Write(e.data)
	}
	padTo(grpOff)
	rsrc.Write(grpBytes)
	padTo((uint32(rsrc.Len()) + 7) &^ 7)

	rsrcBytes := rsrc.Bytes()

	// Assemblage COFF x86-64 (.syso)
	const (
		coffHdrSize = 20
		secHdrSize  = 40
	)
	ptrRaw := uint32(coffHdrSize + secHdrSize)
	ptrReloc := ptrRaw + uint32(len(rsrcBytes))
	ptrSym := ptrReloc + uint32(len(relocs)*10)

	var coff bytes.Buffer
	// IMAGE_FILE_HEADER (20 bytes)
	_ = binary.Write(&coff, binary.LittleEndian, uint16(0x8664)) // Machine = AMD64
	_ = binary.Write(&coff, binary.LittleEndian, uint16(1))      // NumberOfSections
	_ = binary.Write(&coff, binary.LittleEndian, uint32(0))      // TimeDateStamp
	_ = binary.Write(&coff, binary.LittleEndian, ptrSym)         // PointerToSymbolTable
	_ = binary.Write(&coff, binary.LittleEndian, uint32(1))      // NumberOfSymbols
	_ = binary.Write(&coff, binary.LittleEndian, uint16(0))      // SizeOfOptionalHeader
	_ = binary.Write(&coff, binary.LittleEndian, uint16(0))      // Characteristics

	// IMAGE_SECTION_HEADER ".rsrc" (40 bytes)
	var secName [8]byte
	copy(secName[:], ".rsrc")
	coff.Write(secName[:])
	_ = binary.Write(&coff, binary.LittleEndian, uint32(0))              // VirtualSize
	_ = binary.Write(&coff, binary.LittleEndian, uint32(0))              // VirtualAddress
	_ = binary.Write(&coff, binary.LittleEndian, uint32(len(rsrcBytes))) // SizeOfRawData
	_ = binary.Write(&coff, binary.LittleEndian, ptrRaw)                 // PointerToRawData
	_ = binary.Write(&coff, binary.LittleEndian, ptrReloc)               // PointerToRelocations
	_ = binary.Write(&coff, binary.LittleEndian, uint32(0))              // PointerToLinenumbers
	_ = binary.Write(&coff, binary.LittleEndian, uint16(len(relocs)))    // NumberOfRelocations
	_ = binary.Write(&coff, binary.LittleEndian, uint16(0))              // NumberOfLinenumbers
	_ = binary.Write(&coff, binary.LittleEndian, uint32(0x40000040))     // CNT_INITIALIZED_DATA | MEM_READ

	// Section data
	coff.Write(rsrcBytes)

	// Relocations (IMAGE_REL_AMD64_ADDR32NB = 3)
	for _, rva := range relocs {
		_ = binary.Write(&coff, binary.LittleEndian, rva)
		_ = binary.Write(&coff, binary.LittleEndian, uint32(0)) // SymbolTableIndex = 0 (".rsrc")
		_ = binary.Write(&coff, binary.LittleEndian, uint16(3)) // IMAGE_REL_AMD64_ADDR32NB
	}

	// Symbol table (1 symbol = 18 bytes : ".rsrc")
	coff.Write(secName[:])
	_ = binary.Write(&coff, binary.LittleEndian, uint32(0)) // Value
	_ = binary.Write(&coff, binary.LittleEndian, int16(1))  // SectionNumber = 1
	_ = binary.Write(&coff, binary.LittleEndian, uint16(0)) // Type
	coff.WriteByte(3)                                       // IMAGE_SYM_CLASS_STATIC
	coff.WriteByte(0)                                       // NumberOfAuxSymbols

	// String table (vide = 4 octets)
	_ = binary.Write(&coff, binary.LittleEndian, uint32(4))

	if err := os.WriteFile(outPath, coff.Bytes(), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "écriture %s : %v\n", outPath, err)
		os.Exit(1)
	}
	fmt.Printf("OK : %s généré (%d octets, %d tailles d'icônes)\n", outPath, coff.Len(), count)
}
