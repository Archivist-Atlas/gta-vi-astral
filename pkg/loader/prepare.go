package loader

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/Archivist-Atlas/gta-vi-astral/pkg/core"
	"github.com/Archivist-Atlas/gta-vi-astral/pkg/pe"
)

const (
	imageFileDLL      uint16 = 0x2000
	dllForceIntegrity uint16 = 0x0080

	dirBoundImport       = 11
	importDescriptorSize = 20
	tls64CallbacksOffset = 24
	relocTypeDir64       = 10
)

// PreparedImage contient une copie en mémoire d'un PE32+ préparé en « DLL passive »
// (approche victory-loader::prepare) pour être mappé proprement par le chargeur Windows
// tout en gardant le contrôle complet des imports, du point d'entrée et des callbacks TLS.
type PreparedImage struct {
	Info         *pe.ImageInfo
	OriginalHash string
	Bytes        []byte
	EntryRVA     uint32
}

// FileName retourne un nom de fichier unique pour le cache disque (hors dépôt).
func (p *PreparedImage) FileName() string {
	return fmt.Sprintf("astral-image-%s.dll", p.OriginalHash)
}

// PreparePassiveDLL transforme une image PE32+ en DLL passive sans modifier le fichier source :
// - active IMAGE_FILE_DLL ;
// - met à zéro AddressOfEntryPoint et CheckSum, retire FORCE_INTEGRITY ;
// - pointe le répertoire d'imports sur son terminateur nul (pour que Windows alloue le TLS statique) ;
// - efface les bound imports ;
// - met à zéro TLS.AddressOfCallBacks et neutralise sa relocation DIR64.
func PreparePassiveDLL(original []byte) (*PreparedImage, error) {
	info, err := pe.Parse(original)
	if err != nil {
		return nil, err
	}
	buf := make([]byte, len(original))
	copy(buf, original)

	peOff := int(binary.LittleEndian.Uint32(buf[0x3C:0x40]))
	fhOff := peOff + 4
	optOff := fhOff + 20
	numDirs := int(binary.LittleEndian.Uint32(buf[optOff+108 : optOff+112]))

	dirOffset := func(idx int) (int, bool) {
		if idx >= numDirs {
			return 0, false
		}
		off := optOff + 112 + idx*8
		if off+8 > len(buf) {
			return 0, false
		}
		return off, true
	}

	// 1. IMAGE_FILE_DLL + zéro EntryPoint / CheckSum + retrait FORCE_INTEGRITY
	chars := binary.LittleEndian.Uint16(buf[fhOff+18 : fhOff+20])
	binary.LittleEndian.PutUint16(buf[fhOff+18:fhOff+20], chars|imageFileDLL)
	binary.LittleEndian.PutUint32(buf[optOff+16:optOff+20], 0)
	binary.LittleEndian.PutUint32(buf[optOff+64:optOff+68], 0)
	dllChars := binary.LittleEndian.Uint16(buf[optOff+70 : optOff+72])
	binary.LittleEndian.PutUint16(buf[optOff+70:optOff+72], dllChars&^dllForceIntegrity)

	// 2. Pointe DirImport sur le terminateur vide (20 octets nuls) pour préserver le TLS statique Windows
	if at, ok := dirOffset(pe.DirImport); ok {
		impRVA := binary.LittleEndian.Uint32(buf[at : at+4])
		if impRVA != 0 {
			termRVA := impRVA + uint32(len(info.Imports))*importDescriptorSize
			binary.LittleEndian.PutUint32(buf[at:at+4], termRVA)
			binary.LittleEndian.PutUint32(buf[at+4:at+8], importDescriptorSize)
		}
	}

	// 3. Efface DirBoundImport
	if at, ok := dirOffset(dirBoundImport); ok {
		for i := 0; i < 8; i++ {
			buf[at+i] = 0
		}
	}

	// 4. Neutralise TLS.AddressOfCallBacks et sa relocation DIR64
	if info.TLS != nil && info.TLS.CallbacksVA != 0 {
		tlsOff, ok := pe.RVAToOffset(info.Sections, info.TLS.DirectoryRVA)
		if !ok || tlsOff+tls64CallbacksOffset+8 > len(buf) {
			return nil, fmt.Errorf("répertoire TLS non mappé sur fichier")
		}
		binary.LittleEndian.PutUint64(buf[tlsOff+tls64CallbacksOffset:tlsOff+tls64CallbacksOffset+8], 0)

		fieldRVA := info.TLS.DirectoryRVA + tls64CallbacksOffset
		if relOff, okRel := dirOffset(pe.DirBaseReloc); okRel {
			relRVA := binary.LittleEndian.Uint32(buf[relOff : relOff+4])
			relSize := binary.LittleEndian.Uint32(buf[relOff+4 : relOff+8])
			neutralizeDir64Reloc(buf, info.Sections, relRVA, relSize, fieldRVA)
		}
	}

	sum := sha256.Sum256(original)
	return &PreparedImage{
		Info:         info,
		OriginalHash: hex.EncodeToString(sum[:8]),
		Bytes:        buf,
		EntryRVA:     info.EntryRVA,
	}, nil
}

func neutralizeDir64Reloc(buf []byte, sections []pe.SectionInfo, relRVA, relSize, targetRVA uint32) {
	if relRVA == 0 || relSize == 0 {
		return
	}
	start, ok := pe.RVAToOffset(sections, relRVA)
	if !ok {
		return
	}
	end := start + int(relSize)
	if end > len(buf) {
		end = len(buf)
	}
	at := start
	for at+8 <= end {
		page := binary.LittleEndian.Uint32(buf[at : at+4])
		block := int(binary.LittleEndian.Uint32(buf[at+4 : at+8]))
		if block < 8 || at+block > end {
			break
		}
		for eOff := at + 8; eOff+2 <= at+block; eOff += 2 {
			entry := binary.LittleEndian.Uint16(buf[eOff : eOff+2])
			if entry>>12 == relocTypeDir64 && page+uint32(entry&0xFFF) == targetRVA {
				binary.LittleEndian.PutUint16(buf[eOff:eOff+2], 0) // IMAGE_REL_BASED_ABSOLUTE
			}
		}
		at += block
	}
}

// AppendCrashLog enregistre un rapport d'incident dans <dataDir>/crashes.log
// après validation du garde-fou anti-fuite de dépôt.
func AppendCrashLog(dataDir, summary string) error {
	if err := core.EnsureOutsideRepo(dataDir); err != nil {
		return err
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	logPath := filepath.Join(dataDir, "crashes.log")
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "[%s] %s\n", time.Now().Format(time.RFC3339), summary)
	return err
}
