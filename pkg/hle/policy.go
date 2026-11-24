// Package hle implémente la couche High-Level Emulation des API Xbox Series / GDK,
// la politique de pass-through Windows (HostPolicy), le système de fichiers virtuel
// avec overlay copy-on-write, et l'isolation réseau stricte hors-ligne.
package hle

import "strings"

// HostPolicy définit quelles DLL Windows hôtes peuvent être utilisées en pass-through
// lorsqu'aucun shim HLE spécifique ne surcharge un symbole (approche VIctory).
type HostPolicy struct {
	Allow []string
}

// DefaultHostPolicy retourne la liste blanche par défaut des DLL système compatibles PC.
func DefaultHostPolicy() HostPolicy {
	return HostPolicy{
		Allow: []string{
			"api-ms-win-core-*",
			"api-ms-win-eventing-*",
			"api-ms-win-security-*",
			"api-ms-win-crt-*",
			"api-ms-win-shcore-*",
			"ext-ms-win-rtcore-ntuser-*",
			"kernel32.dll",
			"kernelbase.dll",
			"ntdll.dll",
			"ucrtbase.dll",
			"vcruntime140*",
			"msvcp140*",
			"msvcp_win.dll",
			"ole32.dll",
			"advapi32.dll",
			"bcrypt.dll",
			"crypt32.dll",
			"ncrypt.dll",
			"sspicli.dll",
			"cabinet.dll",
			"avrt.dll",
			"iphlpapi.dll",
			// Le trafic réseau sortant est bloqué en priorité par le registre HLE (connect/DNS).
			"ws2_32.dll",
			"dbghelp.dll",
			"xaudio2_9.dll",
			"mfplat.dll",
			"mfreadwrite.dll",
		},
	}
}

// Allows indique si une DLL est autorisée en pass-through vers System32.
func (p HostPolicy) Allows(dll string) bool {
	lower := strings.ToLower(dll)
	for _, pattern := range p.Allow {
		if strings.HasSuffix(pattern, "*") {
			if strings.HasPrefix(lower, strings.TrimSuffix(pattern, "*")) {
				return true
			}
		} else if lower == pattern {
			return true
		}
	}
	return false
}
