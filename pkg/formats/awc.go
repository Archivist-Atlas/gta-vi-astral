package formats

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// AudioCodec identifie les codecs rencontrés dans les banques AWC de GTA VI (Leonida).
type AudioCodec uint8

const (
	CodecPCM16   AudioCodec = 0x00
	CodecPCM32F  AudioCodec = 0x02
	CodecXMA2    AudioCodec = 0x05
	CodecOpus    AudioCodec = 0x0B
)

func (c AudioCodec) String() string {
	switch c {
	case CodecPCM16:
		return "PCM16"
	case CodecPCM32F:
		return "PCM32F"
	case CodecXMA2:
		return "XMA2"
	case CodecOpus:
		return "Opus"
	default:
		return fmt.Sprintf("Codec(0x%02X)", uint8(c))
	}
}

// AWCHeader représente l'en-tête de base d'une banque audio ADAT (AWC).
type AWCHeader struct {
	Magic       string
	Version     uint16
	Flags       uint16
	StreamCount uint32
	DataOffset  uint32
}

var ErrNotAWC = errors.New("en-tête AWC/ADAT invalide")

// ParseAWCHeader lit les 16 premiers octets d'une banque audio AWC.
func ParseAWCHeader(data []byte) (AWCHeader, error) {
	if len(data) < 16 || string(data[0:4]) != "ADAT" {
		return AWCHeader{}, ErrNotAWC
	}
	return AWCHeader{
		Magic:       "ADAT",
		Version:     binary.LittleEndian.Uint16(data[4:6]),
		Flags:       binary.LittleEndian.Uint16(data[6:8]),
		StreamCount: binary.LittleEndian.Uint32(data[8:12]),
		DataOffset:  binary.LittleEndian.Uint32(data[12:16]),
	}, nil
}
