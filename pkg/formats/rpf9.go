package formats

import (
	"encoding/binary"
	"fmt"
	"os"
)

// RPF9Header décrit les informations lisibles en clair de l'en-tête d'une archive RPF9.
// Conformément à la règle clean-room (Leonida & VIctory), aucun déchiffrement n'est effectué.
type RPF9Header struct {
	Magic      string
	EntryCount uint32
	FileSize   int64
}

// ProbeRPF9 lit uniquement l'en-tête d'un fichier .rpf pour vérifier sa signature.
func ProbeRPF9(path string) (RPF9Header, error) {
	f, err := os.Open(path)
	if err != nil {
		return RPF9Header{}, err
	}
	defer f.Close()

	st, err := f.Stat()
	if err != nil {
		return RPF9Header{}, err
	}
	var hdr [16]byte
	if _, err := f.ReadAt(hdr[:], 0); err != nil {
		return RPF9Header{}, fmt.Errorf("lecture en-tête RPF %q : %w", path, err)
	}
	magic := string(hdr[0:4])
	entryCount := binary.LittleEndian.Uint32(hdr[4:8])
	return RPF9Header{
		Magic:      magic,
		EntryCount: entryCount,
		FileSize:   st.Size(),
	}, nil
}
