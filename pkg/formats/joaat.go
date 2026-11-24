// Package formats implémente les formats binaires et codecs plateforme/moteur
// (SGD5 de VIctory, RPF9/AWC/JOAAT de Leonida, layouts GPU RDNA2 GFX10.3).
package formats

import "strings"

// JOAAT32 calcule le hash Jenkins One-At-A-Time 32 bits (utilisé par RAGE pour
// les noms d'assets, maps, archétypes et schémas PSIN).
// Par convention RAGE, la plupart des noms sont passés en minuscules.
func JOAAT32(s string) uint32 {
	return JOAAT32Bytes([]byte(strings.ToLower(s)))
}

// JOAAT32CaseSensitive calcule le hash JOAAT32 sans mise en minuscules
// (utilisé notamment pour les noms de champs dans les sections LSCH/LENM de PSIN).
func JOAAT32CaseSensitive(s string) uint32 {
	return JOAAT32Bytes([]byte(s))
}

// JOAAT32Bytes calcule JOAAT32 sur une tranche d'octets brute.
func JOAAT32Bytes(data []byte) uint32 {
	var hash uint32
	for _, b := range data {
		hash += uint32(b)
		hash += hash << 10
		hash ^= hash >> 6
	}
	hash += hash << 3
	hash ^= hash >> 11
	hash += hash << 15
	return hash
}
