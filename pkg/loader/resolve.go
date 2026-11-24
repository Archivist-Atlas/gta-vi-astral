// Package loader gère la résolution des imports en 4 niveaux, le recensement (census),
// la préparation passive-DLL des exécutables PE32+ (approche VIctory) et le rapport de crash.
package loader

import (
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Archivist-Atlas/gta-vi-astral/pkg/hle"
	"github.com/Archivist-Atlas/gta-vi-astral/pkg/pe"
)

// ResolutionKind indique comment un import du jeu sera satisfait au chargement.
type ResolutionKind string

const (
	ResolveHLE           ResolutionKind = "hle"
	ResolveHost          ResolutionKind = "host"
	ResolveTitleDLL      ResolutionKind = "title-dll"
	ResolveUnimplemented ResolutionKind = "unimplemented"
)

// ResolvedImport associe un import d'une image PE à sa résolution.
type ResolvedImport struct {
	DLL       string
	Symbol    string
	DelayLoad bool
	Kind      ResolutionKind
	Area      hle.ModuleArea
}

// CensusReport synthétise la couverture d'imports d'un exécutable ou d'une DLL du jeu.
type CensusReport struct {
	ImagePath     string
	Total         int
	ByKind        map[ResolutionKind]int
	MissingByDLL  map[string][]string
	Resolved      []ResolvedImport
}

// CoveragePercent retourne le pourcentage d'imports satisfaits (HLE + Host + TitleDLL).
func (c CensusReport) CoveragePercent() float64 {
	if c.Total == 0 {
		return 100.0
	}
	ok := c.ByKind[ResolveHLE] + c.ByKind[ResolveHost] + c.ByKind[ResolveTitleDLL]
	return float64(ok) * 100.0 / float64(c.Total)
}

// Resolver implémente l'ordre de priorité strict (VIctory + Leonida) :
// 1. HLE (toujours prioritaire)
// 2. HostPolicy (DLL système Windows autorisées)
// 3. DLL fournie par le titre dans son dossier (ex: Bink2Scarlett.dll, XCurl.dll)
// 4. Stub / Unimplemented
type Resolver struct {
	HLE       *hle.Registry
	Host      hle.HostPolicy
	TitleDir  string
	titleDLLs map[string]bool
}

// NewResolver crée un résolveur d'imports pour un dossier de titre optionnel.
func NewResolver(reg *hle.Registry, host hle.HostPolicy, titleDir string) *Resolver {
	r := &Resolver{
		HLE:       reg,
		Host:      host,
		TitleDir:  titleDir,
		titleDLLs: make(map[string]bool),
	}
	if titleDir != "" {
		if entries, err := os.ReadDir(titleDir); err == nil {
			for _, e := range entries {
				if !e.IsDir() && strings.EqualFold(filepath.Ext(e.Name()), ".dll") {
					r.titleDLLs[strings.ToLower(e.Name())] = true
				}
			}
		}
	}
	return r
}

// Resolve classe un import unique (dll!symbol).
func (r *Resolver) Resolve(dll, symbol string) (ResolutionKind, hle.ModuleArea) {
	if r.HLE != nil {
		if d, ok := r.HLE.Lookup(dll, symbol); ok && d.Implemented {
			return ResolveHLE, d.Area
		}
	}
	if r.Host.Allows(dll) {
		return ResolveHost, ""
	}
	if r.titleDLLs[strings.ToLower(dll)] {
		return ResolveTitleDLL, ""
	}
	return ResolveUnimplemented, ""
}

// Census analyse tous les imports (directs et différés) d'une image PE32+.
func (r *Resolver) Census(imagePath string, info *pe.ImageInfo) CensusReport {
	rep := CensusReport{
		ImagePath:    imagePath,
		ByKind:       make(map[ResolutionKind]int),
		MissingByDLL: make(map[string][]string),
	}

	visit := func(mods []pe.ImportModule, delay bool) {
		for _, m := range mods {
			for _, e := range m.Entries {
				sym := e.DisplayName()
				kind, area := r.Resolve(m.DLL, sym)
				rep.Total++
				rep.ByKind[kind]++
				rep.Resolved = append(rep.Resolved, ResolvedImport{
					DLL:       m.DLL,
					Symbol:    sym,
					DelayLoad: delay,
					Kind:      kind,
					Area:      area,
				})
				if kind == ResolveUnimplemented {
					rep.MissingByDLL[m.DLL] = append(rep.MissingByDLL[m.DLL], sym)
				}
			}
		}
	}

	visit(info.Imports, false)
	visit(info.DelayImports, true)

	for dll := range rep.MissingByDLL {
		sort.Strings(rep.MissingByDLL[dll])
	}
	return rep
}
