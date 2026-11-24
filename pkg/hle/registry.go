package hle

import "strings"

// ModuleArea regroupe les sous-systèmes HLE par domaine fonctionnel.
type ModuleArea string

const (
	AreaGDK       ModuleArea = "gdk"
	AreaMemory    ModuleArea = "xmem"
	AreaGPU       ModuleArea = "d3d12x"
	AreaTexture   ModuleArea = "xg"
	AreaStorage   ModuleArea = "dstorage"
	AreaInput     ModuleArea = "gameinput"
	AreaAudio     ModuleArea = "audio"
	AreaNetwork   ModuleArea = "net-offline"
	AreaFilesystem ModuleArea = "files"
)

// ShimDescriptor décrit un symbole console implémenté (ou planifié) dans la couche HLE.
type ShimDescriptor struct {
	DLL         string
	Symbol      string // "*" signifie que toute la DLL console est prise en charge par ce module
	Area        ModuleArea
	Implemented bool
	Notes       string
}

// Registry centralise tous les shims console enregistrés dans gta-vi-astral.
type Registry struct {
	exact    map[string]ShimDescriptor
	dllWide  map[string]ShimDescriptor
	all      []ShimDescriptor
}

// NewDefaultRegistry construit le registre HLE combinant les surfaces couvertes
// par VIctory (GDK, XMem, D3D12.X, DirectStorage, AcpHal/XMA2, XAPU/Opus, GameInput, Net)
// et Leonida (overlay GAME:, WinHTTP offline, XMem physical pages).
func NewDefaultRegistry() *Registry {
	r := &Registry{
		exact:   make(map[string]ShimDescriptor),
		dllWide: make(map[string]ShimDescriptor),
	}

	defaults := []ShimDescriptor{
		// GDK Runtime hors-ligne (XUser, XGameSave, XStore, XPackage, XSystem, XTaskQueue/XAsync)
		{DLL: "xgameruntime.dll", Symbol: "*", Area: AreaGDK, Implemented: true, Notes: "QueryApiImpl hors-ligne, XUser, XGameSave atomique, XStore, XPackage, XAsync"},
		{DLL: "gameplatformservices.dll", Symbol: "*", Area: AreaGDK, Implemented: true, Notes: "Passerelle GDK/GamePlatformServices"},

		// Mémoire unifiée console
		{DLL: "xmem.dll", Symbol: "*", Area: AreaMemory, Implemented: true, Notes: "XMemAlloc/Free, XMemVirtualAlloc, pages physiques, WriteWatch"},

		// Graphique console D3D12.X & layouts textures XG
		{DLL: "d3d12_xs.dll", Symbol: "*", Area: AreaGPU, Implemented: true, Notes: "Proxy ABI Xbox D3D12.X vers backend D3D12 ou Vulkan"},
		{DLL: "xg_xs.dll", Symbol: "*", Area: AreaTexture, Implemented: true, Notes: "XGCreateTextureComputer / XGSuggestSwizzleMode (RDNA2 addrlib)"},

		// Streaming DirectStorage console (zlib + BCPack)
		{DLL: "xcrdapi.dll", Symbol: "*", Area: AreaStorage, Implemented: true, Notes: "DirectStorage console : files prioritaires + décompression zlib/BCPack"},
		{DLL: "dstorage_xs.dll", Symbol: "*", Area: AreaStorage, Implemented: true, Notes: "Factory DirectStorage Xbox Series"},

		// Entrées manette / clavier virtuel
		{DLL: "gameinput.dll", Symbol: "*", Area: AreaInput, Implemented: true, Notes: "XInput + clavier virtuel + support DualSense"},

		// Audio console (APU / XMA2 / Opus / DSP)
		{DLL: "acphal.dll", Symbol: "*", Area: AreaAudio, Implemented: true, Notes: "APU SHAPE + décodage XMA2"},
		{DLL: "xapu.dll", Symbol: "*", Area: AreaAudio, Implemented: true, Notes: "Décodage Opus matériel console"},
		{DLL: "chad.dll", Symbol: "*", Area: AreaAudio, Implemented: true, Notes: "DSP / réverbération (stub silencieux)"},

		// Isolation réseau stricte hors-ligne (bloque toute sortie hors loopback)
		{DLL: "winhttp.dll", Symbol: "*", Area: AreaNetwork, Implemented: true, Notes: "Isolation réseau hors-ligne"},
		{DLL: "ws2_32.dll", Symbol: "connect", Area: AreaNetwork, Implemented: true, Notes: "Autorise uniquement 127.0.0.0/8 et ::1"},
		{DLL: "ws2_32.dll", Symbol: "getaddrinfo", Area: AreaNetwork, Implemented: true, Notes: "Refuse la résolution DNS externe"},
		{DLL: "ws2_32.dll", Symbol: "GetAddrInfoW", Area: AreaNetwork, Implemented: true, Notes: "Refuse la résolution DNS externe"},
	}

	for _, d := range defaults {
		r.Register(d)
	}
	return r
}

// Register ajoute un shim dans le registre.
func (r *Registry) Register(d ShimDescriptor) {
	dll := strings.ToLower(d.DLL)
	d.DLL = dll
	r.all = append(r.all, d)
	if d.Symbol == "*" {
		r.dllWide[dll] = d
		return
	}
	r.exact[dll+"!"+strings.ToLower(d.Symbol)] = d
}

// Lookup recherche un shim exact puis un shim couvrant toute la DLL.
func (r *Registry) Lookup(dll, symbol string) (ShimDescriptor, bool) {
	dl := strings.ToLower(dll)
	if d, ok := r.exact[dl+"!"+strings.ToLower(symbol)]; ok {
		return d, true
	}
	if d, ok := r.dllWide[dl]; ok {
		return d, true
	}
	return ShimDescriptor{}, false
}

// All retourne la liste de tous les shims enregistrés.
func (r *Registry) All() []ShimDescriptor {
	out := make([]ShimDescriptor, len(r.all))
	copy(out, r.all)
	return out
}
