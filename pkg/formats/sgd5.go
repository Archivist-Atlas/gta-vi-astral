package formats

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"fmt"
	"io"
)

// SGD5Magic est la signature des conteneurs de shaders RAGE (GTA VI).
var SGD5Magic = []byte("SGD5")

var sgd5Marker = []byte{0xE7, 0xDC, 0x0D, 0x00} // 0x000DDCE7 en little-endian

const sgd5MaxUnpacked = 64 << 20 // 64 MiB max par conteneur

// SGD5Record décrit un enregistrement shader trouvé dans un fichier ou une archive SGD5.
type SGD5Record struct {
	Offset       int
	Name         string
	Kind         byte
	Packed       []byte
	UnpackedSize int
}

// Unpack décompresse le flux raw-deflate de l'enregistrement et vérifie sa taille exacte.
func (r SGD5Record) Unpack() ([]byte, error) {
	rd := flate.NewReader(bytes.NewReader(r.Packed))
	defer rd.Close()

	limited := io.LimitReader(rd, int64(r.UnpackedSize)+1)
	out, err := io.ReadAll(limited)
	if err != nil {
		return nil, err
	}
	if len(out) != r.UnpackedSize {
		return nil, fmt.Errorf("taille décompressée SGD5 incohérente : obtenu %d, attendu %d", len(out), r.UnpackedSize)
	}
	return out, nil
}

// IsSGD5 indique si le buffer contient un en-tête SGD5.
func IsSGD5(data []byte) bool {
	return bytes.Contains(data, SGD5Magic)
}

// ScanSGD5 parcourt data (fichier SGD5 ou archive RPF le contenant) et retourne
// tous les enregistrements valides dans l'ordre du fichier (sans avoir besoin du TOC).
func ScanSGD5(data []byte) []SGD5Record {
	var out []SGD5Record
	pos := 0
	for {
		idx := bytes.Index(data[pos:], sgd5Marker)
		if idx < 0 {
			break
		}
		at := pos + idx
		pos = at + 4
		if rec, ok := parseSGD5Record(data, at); ok {
			out = append(out, rec)
		}
	}
	return out
}

func parseSGD5Record(data []byte, at int) (SGD5Record, bool) {
	if at < 6 || at+12 > len(data) {
		return SGD5Record{}, false
	}
	n := int(binary.LittleEndian.Uint32(data[at-4 : at]))
	n2 := int(binary.LittleEndian.Uint32(data[at+4 : at+8]))
	unpacked := int(binary.LittleEndian.Uint32(data[at+8 : at+12]))
	if n != n2 || n < 12 || unpacked <= 0 || unpacked > sgd5MaxUnpacked || at+n > len(data) {
		return SGD5Record{}, false
	}

	var name string
	nul := at - 6
	if data[nul] == 0 {
		name = extractSGD5Name(data, nul)
	}
	return SGD5Record{
		Offset:       at,
		Name:         name,
		Kind:         data[at-5],
		Packed:       data[at+12 : at+n],
		UnpackedSize: unpacked,
	}, true
}

func extractSGD5Name(data []byte, nul int) string {
	run := nul
	for run > 0 && data[run-1] >= 0x21 && data[run-1] <= 0x7E {
		run--
	}
	start := run
	if start > 0 {
		start--
	}
	for l := start; l < nul; l++ {
		if int(data[l]) == nul-l {
			return string(data[l+1 : nul])
		}
	}
	return ""
}
