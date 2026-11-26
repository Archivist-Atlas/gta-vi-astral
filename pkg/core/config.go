package core

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// ConsoleProfile identifie le matériel Xbox Series simulé (inspiré de VIctory).
type ConsoleProfile string

const (
	ConsoleSeriesX ConsoleProfile = "series-x"
	ConsoleSeriesS ConsoleProfile = "series-s"
)

// GPUBackend sélectionne le backend graphique hôte.
type GPUBackend string

const (
	BackendAuto   GPUBackend = "auto"
	BackendD3D12  GPUBackend = "d3d12"  // Natif Windows D3D12 (approche VIctory)
	BackendVulkan GPUBackend = "vulkan" // Portable Vulkan + XDXR BVH4 (approche Leonida)
)

// UnimplementedPolicy définit le comportement lors d'un appel d'import non implémenté.
type UnimplementedPolicy string

const (
	UnimplLog  UnimplementedPolicy = "log"  // Journalise une fois et retourne 0
	UnimplZero UnimplementedPolicy = "zero" // Retourne silencieusement 0
	UnimplFail UnimplementedPolicy = "fail" // Retourne E_NOTIMPL / erreur
	UnimplTrap UnimplementedPolicy = "trap" // Interrompt l'exécution / breakpoint
)

// PathsConfig regroupe tous les chemins externes au dépôt.
type PathsConfig struct {
	GameDir    string
	CacheDir   string
	DataDir    string
	OverlayDir string
}

// LauncherMode définit le comportement au lancement d'astral.exe sans argument.
type LauncherMode string

const (
	ModeNormal LauncherMode = "normal" // Configuration guidée au 1er lancement, puis démarrage direct d'Astral
	ModeDev    LauncherMode = "dev"    // Affiche le lanceur complet avec outils de diagnostic
)

// RuntimeConfig regroupe les paramètres d'exécution et de traduction.
type RuntimeConfig struct {
	Mode           LauncherMode
	LoadingSound   bool
	Fullscreen     bool
	ConsoleProfile ConsoleProfile
	GPUBackend     GPUBackend
	Unimplemented  UnimplementedPolicy
	Wave64Emulate  string // "auto", "on", "off"
	Jobs           int
}

// Config représente la configuration complète de gta-vi-astral.
type Config struct {
	ConfigFile string
	Paths      PathsConfig
	Runtime    RuntimeConfig
}

// DefaultConfig retourne une configuration par défaut sûre (stockée sous %LOCALAPPDATA%).
func DefaultConfig() Config {
	base := defaultUserBaseDir()
	return Config{
		Paths: PathsConfig{
			CacheDir:   filepath.Join(base, "cache"),
			DataDir:    filepath.Join(base, "data"),
			OverlayDir: filepath.Join(base, "overlay"),
		},
		Runtime: RuntimeConfig{
			Mode:           ModeNormal,
			LoadingSound:   true,
			ConsoleProfile: ConsoleSeriesX,
			GPUBackend:     BackendAuto,
			Unimplemented:  UnimplLog,
			Wave64Emulate:  "auto",
			Jobs:           max(1, runtime.NumCPU()/2),
		},
	}
}

func defaultUserBaseDir() string {
	if localAppData := os.Getenv("LOCALAPPDATA"); localAppData != "" {
		return filepath.Join(localAppData, "gta-vi-astral")
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".gta-vi-astral")
	}
	return filepath.Join(os.TempDir(), "gta-vi-astral")
}

// LoadConfig charge la configuration depuis astral.toml (ou ASTRAL_CONFIG),
// puis applique les surcharges des variables d'environnement ASTRAL_*.
func LoadConfig(explicitPath string) (Config, error) {
	cfg := DefaultConfig()

	configPath := explicitPath
	if configPath == "" {
		configPath = os.Getenv("ASTRAL_CONFIG")
	}
	if configPath == "" {
		if _, err := os.Stat("astral.toml"); err == nil {
			configPath = "astral.toml"
		} else if exe, exeErr := os.Executable(); exeErr == nil {
			beside := filepath.Join(filepath.Dir(exe), "astral.toml")
			if _, err := os.Stat(beside); err == nil {
				configPath = beside
			}
		}
	}

	if configPath != "" {
		if err := parseSimpleTOML(configPath, &cfg); err != nil {
			return cfg, fmt.Errorf("lecture de %q : %w", configPath, err)
		}
		cfg.ConfigFile = configPath
	}

	// Variables d'environnement prioritaires (comme VIctory et Leonida)
	if v := os.Getenv("ASTRAL_GAME_DIR"); v != "" {
		cfg.Paths.GameDir = v
	}
	if v := os.Getenv("ASTRAL_CACHE_DIR"); v != "" {
		cfg.Paths.CacheDir = v
	}
	if v := os.Getenv("ASTRAL_DATA_DIR"); v != "" {
		cfg.Paths.DataDir = v
	}
	if v := os.Getenv("ASTRAL_OVERLAY_DIR"); v != "" {
		cfg.Paths.OverlayDir = v
	}
	if v := os.Getenv("ASTRAL_MODE"); v != "" {
		if strings.EqualFold(v, string(ModeDev)) {
			cfg.Runtime.Mode = ModeDev
		} else {
			cfg.Runtime.Mode = ModeNormal
		}
	}
	if v := os.Getenv("ASTRAL_LOADING_SOUND"); v != "" {
		cfg.Runtime.LoadingSound = parseBoolLoose(v, true)
	}
	if v := os.Getenv("ASTRAL_FULLSCREEN"); v != "" {
		cfg.Runtime.Fullscreen = parseBoolLoose(v, false)
	}
	if v := os.Getenv("ASTRAL_CONSOLE"); v != "" {
		cfg.Runtime.ConsoleProfile = ConsoleProfile(strings.ToLower(v))
	}
	if v := os.Getenv("ASTRAL_GPU_BACKEND"); v != "" {
		cfg.Runtime.GPUBackend = GPUBackend(strings.ToLower(v))
	}
	if v := os.Getenv("ASTRAL_UNIMPLEMENTED"); v != "" {
		cfg.Runtime.Unimplemented = UnimplementedPolicy(strings.ToLower(v))
	}
	if v := os.Getenv("ASTRAL_WAVE64_EMULATE"); v != "" {
		cfg.Runtime.Wave64Emulate = strings.ToLower(v)
	}
	if v := os.Getenv("ASTRAL_JOBS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			cfg.Runtime.Jobs = n
		}
	}

	if cfg.Runtime.Jobs <= 0 {
		cfg.Runtime.Jobs = max(1, runtime.NumCPU()/2)
	}

	return cfg, nil
}

// SaveGameDir mémorise gameDir en mode normal dans le fichier local astral.toml (ignoré par Git).
func SaveGameDir(gameDir string) (string, error) {
	return SaveLauncherConfig(gameDir, ModeNormal)
}

// SaveLauncherConfig mémorise gameDir et le mode (normal / dev) dans astral.toml (ignoré par Git).
func SaveLauncherConfig(gameDir string, mode LauncherMode) (string, error) {
	if err := EnsureOutsideRepo(gameDir); err != nil {
		return "", err
	}
	if mode != ModeDev {
		mode = ModeNormal
	}
	loadingSound := true
	fullscreen := false
	if cur, err := LoadConfig(""); err == nil {
		loadingSound = cur.Runtime.LoadingSound
		fullscreen = cur.Runtime.Fullscreen
	}
	return writeLocalConfig(gameDir, mode, loadingSound, fullscreen)
}

// SaveLoadingSound mémorise l'état du son de chargement (activé / coupé) dans astral.toml.
func SaveLoadingSound(enabled bool) error {
	cur, _ := LoadConfig("")
	mode := cur.Runtime.Mode
	if mode != ModeDev {
		mode = ModeNormal
	}
	_, err := writeLocalConfig(cur.Paths.GameDir, mode, enabled, cur.Runtime.Fullscreen)
	return err
}

// SaveFullscreen mémorise l'état plein écran (activé / fenêtré) dans astral.toml.
func SaveFullscreen(enabled bool) error {
	cur, _ := LoadConfig("")
	mode := cur.Runtime.Mode
	if mode != ModeDev {
		mode = ModeNormal
	}
	_, err := writeLocalConfig(cur.Paths.GameDir, mode, cur.Runtime.LoadingSound, enabled)
	return err
}

func writeLocalConfig(gameDir string, mode LauncherMode, loadingSound, fullscreen bool) (string, error) {
	target := "astral.toml"
	if exe, err := os.Executable(); err == nil {
		target = filepath.Join(filepath.Dir(exe), "astral.toml")
	}
	content := fmt.Sprintf(
		"# Configuration locale générée par gta-vi-astral (ignorée par Git)\n"+
			"# Passez mode = 'dev' pour afficher le lanceur complet avec les outils de diagnostic.\n\n"+
			"[paths]\ngame_dir = '%s'\n\n"+
			"[runtime]\nmode = '%s'\nloading_sound = %t\nfullscreen = %t\n",
		gameDir, mode, loadingSound, fullscreen,
	)
	if err := os.WriteFile(target, []byte(content), 0o644); err != nil {
		return "", err
	}
	return target, nil
}

func parseBoolLoose(s string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "1", "yes", "on", "oui":
		return true
	case "false", "0", "no", "off", "non":
		return false
	default:
		return fallback
	}
}

// ValidatePolicy vérifie qu'aucun chemin de données dérivées ne pointe dans le dépôt.
func (c Config) ValidatePolicy() error {
	for _, p := range []struct {
		label string
		path  string
	}{
		{"game_dir", c.Paths.GameDir},
		{"cache_dir", c.Paths.CacheDir},
		{"data_dir", c.Paths.DataDir},
		{"overlay_dir", c.Paths.OverlayDir},
	} {
		if p.path == "" {
			continue
		}
		if err := EnsureOutsideRepo(p.path); err != nil {
			return fmt.Errorf("%s invalide : %w", p.label, err)
		}
	}
	return nil
}

// parseSimpleTOML lit les sections [paths] et [runtime] de astral.toml sans dépendance externe.
func parseSimpleTOML(path string, cfg *Config) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	section := ""
	scanner := bufio.NewScanner(f)
	lineNum := 0
	for scanner.Scan() {
		lineNum++
		line := strings.TrimSpace(stripTOMLComment(scanner.Text()))
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(strings.TrimSpace(line[1 : len(line)-1]))
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return fmt.Errorf("ligne %d : syntaxe TOML invalide (%q)", lineNum, line)
		}
		key := strings.ToLower(strings.TrimSpace(k))
		val := unquoteTOML(strings.TrimSpace(v))

		switch section {
		case "paths":
			switch key {
			case "game_dir":
				cfg.Paths.GameDir = val
			case "cache_dir":
				cfg.Paths.CacheDir = val
			case "data_dir":
				cfg.Paths.DataDir = val
			case "overlay_dir":
				cfg.Paths.OverlayDir = val
			}
		case "runtime":
			switch key {
			case "mode":
				if strings.EqualFold(val, string(ModeDev)) {
					cfg.Runtime.Mode = ModeDev
				} else {
					cfg.Runtime.Mode = ModeNormal
				}
			case "loading_sound":
				cfg.Runtime.LoadingSound = parseBoolLoose(val, true)
			case "fullscreen":
				cfg.Runtime.Fullscreen = parseBoolLoose(val, false)
			case "console_profile":
				cfg.Runtime.ConsoleProfile = ConsoleProfile(strings.ToLower(val))
			case "gpu_backend":
				cfg.Runtime.GPUBackend = GPUBackend(strings.ToLower(val))
			case "unimplemented":
				cfg.Runtime.Unimplemented = UnimplementedPolicy(strings.ToLower(val))
			case "wave64_emulate":
				cfg.Runtime.Wave64Emulate = strings.ToLower(val)
			case "jobs":
				if n, convErr := strconv.Atoi(val); convErr == nil && n >= 0 {
					cfg.Runtime.Jobs = n
				}
			}
		}
	}
	return scanner.Err()
}

func stripTOMLComment(s string) string {
	inSingle := false
	inDouble := false
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle && (i == 0 || s[i-1] != '\\') {
				inDouble = !inDouble
			}
		case '#':
			if !inSingle && !inDouble {
				return s[:i]
			}
		}
	}
	return s
}

func unquoteTOML(v string) string {
	if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
		return v[1 : len(v)-1]
	}
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		if unq, err := strconv.Unquote(v); err == nil {
			return unq
		}
		return v[1 : len(v)-1]
	}
	return v
}
