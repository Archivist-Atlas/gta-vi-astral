// Package game regroupe tout ce qui est spécifique à Grand Theft Auto VI
// (Xbox Series X|S, version 1.0.2.1), en gardant les autres packages 100 % génériques
// (séparation inspirée de victory-game et Leonida game/).
package game

import (
	"encoding/xml"
	"fmt"
	"os"
	"path/filepath"
)

const (
	ExpectedExecutable = "game_scarlett_master.exe"
	ExpectedConfigFile = "MicrosoftGame.Config"
	TargetVersion      = "Xbox Series 1.0.2.1"
)

// CorePackageFiles liste les fichiers essentiels attendus dans un dossier de jeu valide.
var CorePackageFiles = []string{
	"MicrosoftGame.Config",
	"game_scarlett_master.exe",
	"shaders.rpf",
	"common.rpf",
	"data.rpf",
	"levels.rpf",
	"audio.rpf",
	"script.rpf",
	"textures.rpf",
	"Bink2Scarlett.dll",
	"XboxBvhBuilder.dll",
	"amd_fidelityfx_upscaler_xbox.dll",
}

// TitleMetadata contient les informations extraites de MicrosoftGame.Config.
type TitleMetadata struct {
	TitleID    string
	StoreID    string
	Executable string
	PLSSizeMB  int
}

// VerificationReport résume la vérification d'une copie locale de GTA VI.
type VerificationReport struct {
	GameDir      string
	Executable   string
	Metadata     *TitleMetadata
	PresentFiles []string
	MissingFiles []string
	TotalBytes   int64
}

// OK retourne true si tous les fichiers essentiels sont présents.
func (r VerificationReport) OK() bool {
	return len(r.MissingFiles) == 0
}

// VerifyPackage vérifie qu'un dossier contient bien une copie lisible de GTA VI Xbox Series.
func VerifyPackage(gameDir string) (*VerificationReport, error) {
	st, err := os.Stat(gameDir)
	if err != nil {
		return nil, fmt.Errorf("dossier du jeu inaccessible %q : %w", gameDir, err)
	}
	if !st.IsDir() {
		return nil, fmt.Errorf("%q n'est pas un dossier", gameDir)
	}

	rep := &VerificationReport{
		GameDir:    gameDir,
		Executable: filepath.Join(gameDir, ExpectedExecutable),
	}

	for _, rel := range CorePackageFiles {
		full := filepath.Join(gameDir, rel)
		info, statErr := os.Stat(full)
		if statErr != nil || info.IsDir() {
			rep.MissingFiles = append(rep.MissingFiles, rel)
			continue
		}
		rep.PresentFiles = append(rep.PresentFiles, rel)
		rep.TotalBytes += info.Size()
	}

	cfgPath := filepath.Join(gameDir, ExpectedConfigFile)
	if meta, err := ParseMicrosoftGameConfig(cfgPath); err == nil {
		rep.Metadata = meta
	}

	return rep, nil
}

type xmlGameConfig struct {
	TitleID string `xml:"TitleId"`
	StoreID string `xml:"StoreId"`
	ExeList struct {
		Exes []struct {
			Name string `xml:"Name,attr"`
		} `xml:"Executable"`
	} `xml:"ExecutableList"`
	PLS struct {
		SizeMB int `xml:"SizeMB"`
	} `xml:"PersistentLocalStorage"`
}

// ParseMicrosoftGameConfig lit les métadonnées clés de MicrosoftGame.Config.
func ParseMicrosoftGameConfig(path string) (*TitleMetadata, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc xmlGameConfig
	if err := xml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	meta := &TitleMetadata{
		TitleID:   doc.TitleID,
		StoreID:   doc.StoreID,
		PLSSizeMB: doc.PLS.SizeMB,
	}
	if len(doc.ExeList.Exes) > 0 {
		meta.Executable = doc.ExeList.Exes[0].Name
	}
	return meta, nil
}
