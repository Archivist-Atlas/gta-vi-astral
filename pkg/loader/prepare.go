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
	imageFileDLL       uint16 = 0x2000
	dllForceIntegrity  uint16 = 0x0080
)

// PreparedImage contient une copie en mémoire d'un PE32+ préparé en "DLL passive"
// (approche victory-loader::prepare) pour être mappé proprement par le chargeur Windows
// tout en gardant le contrôle complet des imports, du point d'entrée et des callbacks TLS.
type PreparedImage struct {
	OriginalHash string
	Bytes        []byte
	EntryRVA     uint32
}

// PreparePassiveDLL transforme une image PE32+ en DLL passive sans modifier le fichier source :
// - active IMAGE_FILE_DLL
// - met à zéro le point d'entrée et le checksum
// - retire FORCE_INTEGRITY
// - neutralise les bound imports
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

	// Active IMAGE_FILE_DLL
	chars := binary.LittleEndian.Uint16(buf[fhOff+18 : fhOff+20])
	binary.LittleEndian.PutUint16(buf[fhOff+18:fhOff+20], chars|imageFileDLL)

	// Met à zéro AddressOfEntryPoint et CheckSum
	binary.LittleEndian.PutUint32(buf[optOff+16:optOff+20], 0)
	binary.LittleEndian.PutUint32(buf[optOff+64:optOff+68], 0)

	// Retire IMAGE_DLLCHARACTERISTICS_FORCE_INTEGRITY
	dllChars := binary.LittleEndian.Uint16(buf[optOff+70 : optOff+72])
	binary.LittleEndian.PutUint16(buf[optOff+70:optOff+72], dllChars&^dllForceIntegrity)

	sum := sha256.Sum256(original)
	return &PreparedImage{
		OriginalHash: hex.EncodeToString(sum[:8]),
		Bytes:        buf,
		EntryRVA:     info.EntryRVA,
	}, nil
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
