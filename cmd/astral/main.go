// Commande astral : interface CLI unifiée du système de recompilation gta-vi-astral.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Archivist-Atlas/gta-vi-astral/pkg/core"
	"github.com/Archivist-Atlas/gta-vi-astral/pkg/formats"
	"github.com/Archivist-Atlas/gta-vi-astral/pkg/game"
	"github.com/Archivist-Atlas/gta-vi-astral/pkg/gpu"
	"github.com/Archivist-Atlas/gta-vi-astral/pkg/hle"
	"github.com/Archivist-Atlas/gta-vi-astral/pkg/loader"
	"github.com/Archivist-Atlas/gta-vi-astral/pkg/pe"
	"github.com/Archivist-Atlas/gta-vi-astral/pkg/pipeline"
	"github.com/Archivist-Atlas/gta-vi-astral/pkg/shader"
)

const version = "0.1.0-dev"

func main() {
	if len(os.Args) >= 2 {
		attachParentConsoleIfCLI()
	}
	if len(os.Args) < 2 {
		core.InitLogger(os.Stderr)
		runDefaultUI()
		return
	}
	if os.Args[1] == "run" {
		if lf, lerr := os.Create(filepath.Join(os.TempDir(), "astral_last.log")); lerr == nil {
			defer lf.Close()
			core.InitLogger(io.MultiWriter(lf, os.Stderr))
		} else {
			core.InitLogger(os.Stderr)
		}
	} else {
		core.InitLogger(os.Stderr)
	}

	var err error
	switch os.Args[1] {
	case "config":
		err = cmdConfig(os.Args[2:])
	case "verify":
		err = cmdVerify(os.Args[2:])
	case "inspect":
		err = cmdInspect(os.Args[2:])
	case "census":
		err = cmdCensus(os.Args[2:])
	case "shader-info":
		err = cmdShaderInfo(os.Args[2:])
	case "translate":
		err = cmdTranslate(os.Args[2:])
	case "run":
		err = cmdRun(os.Args[2:])
	case "version", "--version", "-v":
		fmt.Printf("gta-vi-astral %s\n", version)
		return
	case "help", "--help", "-h":
		printUsage()
		return
	default:
		fmt.Fprintf(os.Stderr, "commande inconnue : %q\n\n", os.Args[1])
		printUsage()
		os.Exit(2)
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "erreur : %v\n", err)
		os.Exit(1)
	}
}

func printUsage() {
	fmt.Printf(`gta-vi-astral %s — Système modulaire de recompilation GTA VI (Xbox Series -> PC)

Usage :
  astral <commande> [options]

Commandes :
  config                     Affiche la configuration active et vérifie la politique anti-fuite
  verify [dossier_jeu]       Vérifie une copie locale de GTA VI Xbox Series (v1.0.2.1)
  inspect <exe|dll>          Analyse un binaire PE32+ (sections, entropie, TLS, imports, CodeView)
  census <exe|dll>           Recense tous les imports (HLE / Host / Title DLL / Manquants)
  shader-info <fichier>      Analyse les shaders SGD5/DXBC/XDAT/XBCH/FSSM et le code RDNA2
  translate <fichier>        Exécute le pipeline parallèle d'analyse/planification en cache
  run <exe>                  Prépare et vérifie l'environnement d'exécution (mode --dry-run)
  version                    Affiche la version d'ASTRAL
`, version)
}

func cmdConfig(args []string) error {
	fs := flag.NewFlagSet("config", flag.ContinueOnError)
	cfgFile := fs.String("config", "", "chemin vers astral.toml")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := core.LoadConfig(*cfgFile)
	if err != nil {
		return err
	}
	fmt.Printf("Fichier config : %s\n", orNone(cfg.ConfigFile))
	fmt.Printf("game_dir       : %s\n", orNone(cfg.Paths.GameDir))
	fmt.Printf("cache_dir      : %s\n", cfg.Paths.CacheDir)
	fmt.Printf("data_dir       : %s\n", cfg.Paths.DataDir)
	fmt.Printf("overlay_dir    : %s\n", cfg.Paths.OverlayDir)
	fmt.Printf("mode           : %s\n", cfg.Runtime.Mode)
	fmt.Printf("console        : %s\n", cfg.Runtime.ConsoleProfile)
	fmt.Printf("gpu_backend    : %s\n", cfg.Runtime.GPUBackend)
	fmt.Printf("unimplemented  : %s\n", cfg.Runtime.Unimplemented)
	fmt.Printf("wave64_emulate : %s\n", cfg.Runtime.Wave64Emulate)
	fmt.Printf("jobs           : %d\n", cfg.Runtime.Jobs)

	if err := cfg.ValidatePolicy(); err != nil {
		return fmt.Errorf("politique anti-fuite violée : %w", err)
	}
	fmt.Println("Politique      : OK (tous les chemins sont hors du dépôt)")
	return nil
}

func cmdVerify(args []string) error {
	fs := flag.NewFlagSet("verify", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	gameDir := fs.Arg(0)
	if gameDir == "" {
		cfg, err := core.LoadConfig("")
		if err != nil {
			return err
		}
		gameDir = cfg.Paths.GameDir
	}
	if gameDir == "" {
		return fmt.Errorf("précisez le dossier du jeu en argument ou via ASTRAL_GAME_DIR / astral.toml")
	}

	rep, err := game.VerifyPackage(gameDir)
	if err != nil {
		return err
	}
	fmt.Printf("Dossier jeu    : %s\n", rep.GameDir)
	fmt.Printf("Cible          : %s\n", game.TargetVersion)
	if rep.Metadata != nil {
		fmt.Printf("TitleId        : %s\n", rep.Metadata.TitleID)
		fmt.Printf("StoreId        : %s\n", rep.Metadata.StoreID)
		fmt.Printf("Exécutable XML : %s\n", rep.Metadata.Executable)
		if rep.Metadata.PLSSizeMB > 0 {
			fmt.Printf("PLS            : %d MiB\n", rep.Metadata.PLSSizeMB)
		}
	}
	fmt.Printf("Fichiers clés  : %d / %d présents (%.2f GiB)\n",
		len(rep.PresentFiles), len(game.CorePackageFiles), float64(rep.TotalBytes)/(1024*1024*1024))
	if len(rep.MissingFiles) > 0 {
		fmt.Printf("Manquants      : %s\n", strings.Join(rep.MissingFiles, ", "))
		return fmt.Errorf("copie incomplète (%d fichier(s) manquant(s))", len(rep.MissingFiles))
	}
	fmt.Println("Statut         : VALIDE")
	return nil
}

func cmdInspect(args []string) error {
	fs := flag.NewFlagSet("inspect", flag.ContinueOnError)
	showImports := fs.Bool("imports", false, "afficher la liste des imports")
	showExports := fs.Bool("exports", false, "afficher la liste des exports")
	if err := fs.Parse(args); err != nil {
		return err
	}
	target := fs.Arg(0)
	if target == "" {
		return fmt.Errorf("usage : astral inspect <exe|dll> [--imports] [--exports]")
	}
	info, err := pe.ParseFile(target)
	if err != nil {
		return err
	}

	fmt.Printf("Image          : %s\n", target)
	fmt.Printf("Architecture   : AMD64 (0x%04x) | Subsystem: %d\n", info.Machine, info.Subsystem)
	fmt.Printf("ImageBase      : 0x%016X | SizeOfImage: 0x%X | EntryRVA: 0x%X\n", info.ImageBase, info.SizeOfImage, info.EntryRVA)
	fmt.Printf("Relocs / .pdata: relocs=%v | .pdata=%v (%d fonctions) | load_config=%v\n",
		info.HasRelocations, info.HasExceptionTable, info.ExceptionCount, info.HasLoadConfig)
	if info.TLS != nil {
		fmt.Printf("TLS statique   : dir=0x%X | callbacks=%d\n", info.TLS.DirectoryRVA, len(info.TLS.CallbackRVAs))
	}
	if info.CodeView != nil {
		fmt.Printf("CodeView RSDS  : %s (key=%s)\n", info.CodeView.PDBPath, info.CodeView.SymbolKey())
	}
	fmt.Printf("Imports        : %d modules directs, %d différés (%d symboles au total)\n",
		len(info.Imports), len(info.DelayImports), info.TotalImports())
	fmt.Printf("Exports        : %d\n\n", len(info.Exports))

	fmt.Printf("%-10s %-10s %-10s %-10s %-8s\n", "Section", "RVA", "VSize", "FSize", "Entropie")
	for _, s := range info.Sections {
		fmt.Printf("%-10s 0x%08X 0x%08X 0x%08X %.3f\n", s.Name, s.RVA, s.VirtualSize, s.FileSize, s.Entropy)
	}

	if *showImports {
		fmt.Println("\n--- Imports directs ---")
		for _, m := range info.Imports {
			fmt.Printf("  %s (%d)\n", m.DLL, len(m.Entries))
			for _, e := range m.Entries {
				fmt.Printf("    - %s (IAT 0x%X)\n", e.DisplayName(), e.IATSlotRVA)
			}
		}
	}
	if *showExports && len(info.Exports) > 0 {
		fmt.Println("\n--- Exports ---")
		for _, e := range info.Exports {
			if e.Forward != "" {
				fmt.Printf("  #%d %s -> %s\n", e.Ordinal, e.Name, e.Forward)
			} else {
				fmt.Printf("  #%d %s (RVA 0x%X)\n", e.Ordinal, e.Name, e.RVA)
			}
		}
	}
	return nil
}

func cmdCensus(args []string) error {
	fs := flag.NewFlagSet("census", flag.ContinueOnError)
	showMissing := fs.Bool("missing", true, "lister les imports non implémentés")
	if err := fs.Parse(args); err != nil {
		return err
	}
	target := fs.Arg(0)
	if target == "" {
		return fmt.Errorf("usage : astral census <exe|dll> [--missing]")
	}
	info, err := pe.ParseFile(target)
	if err != nil {
		return err
	}

	res := loader.NewResolver(hle.NewDefaultRegistry(), hle.DefaultHostPolicy(), filepath.Dir(target))
	rep := res.Census(target, info)

	fmt.Printf("Recensement    : %s\n", rep.ImagePath)
	fmt.Printf("Total imports  : %d (couverture : %.1f%%)\n", rep.Total, rep.CoveragePercent())
	fmt.Printf("  HLE          : %d\n", rep.ByKind[loader.ResolveHLE])
	fmt.Printf("  Host Windows : %d\n", rep.ByKind[loader.ResolveHost])
	fmt.Printf("  DLL du titre : %d\n", rep.ByKind[loader.ResolveTitleDLL])
	fmt.Printf("  Manquants    : %d\n", rep.ByKind[loader.ResolveUnimplemented])

	if *showMissing && len(rep.MissingByDLL) > 0 {
		fmt.Println("\n--- Imports restant à implémenter ---")
		dlls := make([]string, 0, len(rep.MissingByDLL))
		for d := range rep.MissingByDLL {
			dlls = append(dlls, d)
		}
		sort.Strings(dlls)
		for _, dll := range dlls {
			syms := rep.MissingByDLL[dll]
			fmt.Printf("  %s (%d) : %s\n", dll, len(syms), previewList(syms, 8))
		}
	}
	return nil
}

func cmdShaderInfo(args []string) error {
	fs := flag.NewFlagSet("shader-info", flag.ContinueOnError)
	limit := fs.Int("limit", 0, "limiter le nombre de shaders analysés (0 = tous)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	target := fs.Arg(0)
	if target == "" {
		return fmt.Errorf("usage : astral shader-info <shaders.rpf|fichier.sgd5|shader.dxbc> [--limit N]")
	}

	start := time.Now()
	data, err := os.ReadFile(target)
	if err != nil {
		return err
	}

	var containers [][]byte
	if c, dxbcErr := shader.ParseDXBC(data); dxbcErr == nil && c != nil {
		containers = [][]byte{data}
	} else {
		recs := formats.ScanSGD5(data)
		fmt.Printf("Enregistrements SGD5 détectés : %d (lecture en %s)\n", len(recs), time.Since(start).Round(time.Millisecond))
		for _, r := range recs {
			if *limit > 0 && len(containers) >= *limit {
				break
			}
			if raw, uErr := r.Unpack(); uErr == nil {
				containers = append(containers, raw)
			}
		}
	}

	stageCounts := make(map[shader.HwStage]int)
	var totalProgs, wave64Count, wave32Count, totalInsts, decodeErrs int

	for _, raw := range containers {
		c, err := shader.ParseDXBC(raw)
		if err != nil {
			decodeErrs++
			continue
		}
		progs, err := shader.ExtractNativePrograms(c)
		if err != nil {
			decodeErrs++
			continue
		}
		for _, p := range progs {
			totalProgs++
			stageCounts[p.Stage]++
			if p.Wave64 {
				wave64Count++
			} else {
				wave32Count++
			}
			totalInsts += len(shader.DecodeGFX10(p.Code))
		}
	}

	fmt.Printf("Conteneurs DXBC analysés : %d (erreurs : %d)\n", len(containers), decodeErrs)
	fmt.Printf("Programmes RDNA2 extraits: %d (wave64: %d | wave32: %d)\n", totalProgs, wave64Count, wave32Count)
	fmt.Printf("Instructions GFX10       : %d décodées en %s\n", totalInsts, time.Since(start).Round(time.Millisecond))
	for _, st := range []shader.HwStage{shader.StageVS, shader.StagePS, shader.StageMS, shader.StageCS, shader.StageGS, shader.StageHS, shader.StageLS, shader.StageES} {
		if n := stageCounts[st]; n > 0 {
			fmt.Printf("  %-4s : %d\n", st, n)
		}
	}
	return nil
}

type shaderPlanConverter struct {
	opts shader.TranslateOptions
}

func (c shaderPlanConverter) ID() string      { return "astral.shader.plan." + string(c.opts.Target) }
func (c shaderPlanConverter) Version() uint32 { return 1 }
func (c shaderPlanConverter) Convert(input []byte) ([]byte, error) {
	cont, err := shader.ParseDXBC(input)
	if err != nil {
		return nil, err
	}
	progs, err := shader.ExtractNativePrograms(cont)
	if err != nil || len(progs) == 0 {
		return nil, fmt.Errorf("aucun programme natif RDNA2 dans le conteneur : %v", err)
	}
	_, out, err := shader.AnalyzeAndPlan(progs[0], c.opts)
	return out, err
}

func cmdTranslate(args []string) error {
	fs := flag.NewFlagSet("translate", flag.ContinueOnError)
	targetIR := fs.String("target", "dxil", "cible IR : dxil ou spirv")
	wave64 := fs.Bool("wave64-emulate", false, "activer l'émulation wave64 (GPU 32-wide)")
	limit := fs.Int("limit", 100, "nombre max de shaders à traiter (0 = tous)")
	jobs := fs.Int("jobs", 0, "nombre de goroutines parallèles (0 = auto)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	src := fs.Arg(0)
	if src == "" {
		return fmt.Errorf("usage : astral translate <shaders.rpf|fichier.sgd5> [--target dxil|spirv] [--limit N]")
	}

	cfg, err := core.LoadConfig("")
	if err != nil {
		return err
	}
	if *jobs <= 0 {
		*jobs = cfg.Runtime.Jobs
	}
	cache, err := pipeline.OpenCache(filepath.Join(cfg.Paths.CacheDir, "shaders"))
	if err != nil {
		return err
	}

	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	recs := formats.ScanSGD5(data)
	if *limit > 0 && len(recs) > *limit {
		recs = recs[:*limit]
	}

	conv := shaderPlanConverter{
		opts: shader.TranslateOptions{
			Target:        shader.TargetIR(strings.ToLower(*targetIR)),
			Wave64Emulate: *wave64,
			LoopLimit:     1 << 20,
		},
	}

	var hits, misses, errs atomic.Int64
	start := time.Now()
	_ = pipeline.RunParallel(context.Background(), *jobs, recs, func(_ context.Context, _ int, r formats.SGD5Record) error {
		raw, uErr := r.Unpack()
		if uErr != nil {
			errs.Add(1)
			return nil
		}
		_, hit, cErr := cache.GetOrConvert(conv, raw)
		if cErr != nil {
			errs.Add(1)
			return nil
		}
		if hit {
			hits.Add(1)
		} else {
			misses.Add(1)
		}
		return nil
	})

	fmt.Printf("Traduction terminée en %s (%d workers, cible=%s)\n", time.Since(start).Round(time.Millisecond), *jobs, conv.opts.Target)
	fmt.Printf("  Traités : %d (cache hits: %d | générés: %d | ignorés/erreurs: %d)\n",
		len(recs), hits.Load(), misses.Load(), errs.Load())
	fmt.Printf("  Cache   : %s\n", filepath.Join(cfg.Paths.CacheDir, "shaders"))
	return nil
}

func cmdRun(args []string) error {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	dryRun := fs.Bool("dry-run", false, "vérification statique uniquement sans mappage mémoire")
	mapOnly := fs.Bool("map", false, "mapper l'image + TLS + IAT dans Windows sans exécuter le point d'entrée")
	execute := fs.Bool("execute", false, "mapper l'image et exécuter le point d'entrée natif du jeu")
	backendFlag := fs.String("backend", "", "backend GPU : auto, d3d12, vulkan")
	unimplFlag := fs.String("unimplemented", "", "politique imports non implémentés : log, zero, fail, trap")
	if err := fs.Parse(args); err != nil {
		return err
	}
	exePath := fs.Arg(0)
	if exePath == "" {
		return fmt.Errorf("usage : astral run [--dry-run|--map|--execute] [--backend auto|d3d12|vulkan] [--unimplemented log|zero|fail|trap] <game_scarlett_master.exe>")
	}
	if !*dryRun && !*mapOnly && !*execute {
		*mapOnly = true
	}

	cfg, err := core.LoadConfig("")
	if err != nil {
		return err
	}
	if err := cfg.ValidatePolicy(); err != nil {
		return err
	}
	if *backendFlag != "" {
		cfg.Runtime.GPUBackend = core.GPUBackend(strings.ToLower(*backendFlag))
	}
	if *unimplFlag != "" {
		cfg.Runtime.Unimplemented = core.UnimplementedPolicy(strings.ToLower(*unimplFlag))
	}

	backend := gpu.SelectBackend(cfg.Runtime.GPUBackend, cfg.Runtime.Wave64Emulate)
	if err := backend.Init(); err != nil {
		return err
	}
	defer backend.Shutdown()
	caps := backend.Capabilities()

	vfs, err := hle.NewVirtualFS(filepath.Dir(exePath), cfg.Paths.OverlayDir, cfg.Paths.DataDir)
	if err != nil {
		return err
	}

	fmt.Printf("Backend GPU    : %s (shaders=%s, wave64_natif=%v, mesh=%v, xdxr_bvh4=%v)\n",
		caps.Name, caps.ShaderTarget, caps.NativeWave64, caps.MeshShaders, caps.XDXRBVH4Builder)
	fmt.Printf("VirtualFS      : GAME:=%s | OVERLAY=%s | DATA=%s\n", vfs.GameDir, vfs.OverlayDir, vfs.DataDir)

	if *dryRun {
		raw, err := os.ReadFile(exePath)
		if err != nil {
			return err
		}
		info, err := pe.Parse(raw)
		if err != nil {
			return err
		}
		prepared, err := loader.PreparePassiveDLL(raw)
		if err != nil {
			return err
		}
		res := loader.NewResolver(hle.NewDefaultRegistry(), hle.DefaultHostPolicy(), filepath.Dir(exePath))
		census := res.Census(exePath, info)
		fmt.Printf("Image préparée : %s (hash=%s, EntryRVA=0x%X)\n", filepath.Base(exePath), prepared.OriginalHash, prepared.EntryRVA)
		fmt.Printf("Couverture IAT : %.1f%% (%d HLE, %d Host, %d TitleDLL, %d manquants)\n",
			census.CoveragePercent(),
			census.ByKind[loader.ResolveHLE],
			census.ByKind[loader.ResolveHost],
			census.ByKind[loader.ResolveTitleDLL],
			census.ByKind[loader.ResolveUnimplemented],
		)
		fmt.Println("Mode -dry-run  : vérifications statiques réussies.")
		return nil
	}

	if *execute {
		openGameWindowEarly(cfg.Runtime.LoadingSound, cfg.Runtime.Fullscreen)
	}

	// Mappage natif Windows (LoadLibraryExW + redirection TLS statique + binding IAT réel)
	sess, err := loader.MapAndBind(loader.MapOptions{
		ExePath:       exePath,
		CacheDir:      cfg.Paths.CacheDir,
		DataDir:       cfg.Paths.DataDir,
		Unimplemented: cfg.Runtime.Unimplemented,
		LiveHLE:       hle.NewLiveTable(cfg.Runtime.ConsoleProfile, vfs),
		HostPolicy:    hle.DefaultHostPolicy(),
	})
	if err != nil {
		return err
	}
	fmt.Printf("Image mappée   : %s (TLS slot=%d, %d accès TLS patchés)\n", filepath.Base(exePath), sess.TLSSlot, sess.TLSPatched)
	fmt.Printf("Binding IAT    : %d HLE live | %d Host System32 | %d TitleDLL | %d stubs x86-64\n",
		sess.BoundHLE, sess.BoundHost, sess.BoundTitleDLL, sess.BoundStubs)

	if !*execute {
		fmt.Println("Mode -map      : mappage mémoire, TLS et binding IAT réussis (ajoutez -execute pour lancer le point d'entrée).")
		return nil
	}

	exitCode, err := sess.RunEntryPoint()
	if err != nil {
		return err
	}
	fmt.Printf("Point d'entrée terminé avec le code de sortie : %d (0x%X)\n", exitCode, exitCode)
	return nil
}

func orNone(s string) string {
	if s == "" {
		return "(non défini)"
	}
	return s
}

func previewList(items []string, maxItems int) string {
	if len(items) <= maxItems {
		return strings.Join(items, ", ")
	}
	return fmt.Sprintf("%s ... (+%d)", strings.Join(items[:maxItems], ", "), len(items)-maxItems)
}

func clearScreen() {
	if runtime.GOOS == "windows" {
		cmd := exec.Command("cmd", "/c", "cls")
		cmd.Stdout = os.Stdout
		_ = cmd.Run()
		return
	}
	fmt.Print("\033[H\033[2J")
}

func runInteractiveMenu() {
	reader := bufio.NewReader(os.Stdin)
	cfg, _ := core.LoadConfig("")
	gameDir := cfg.Paths.GameDir

	for {
		clearScreen()
		fmt.Println("============================================================")
		fmt.Printf("  gta-vi-astral %s — Menu interactif\n", version)
		fmt.Println("============================================================")
		fmt.Printf("  Dossier jeu : %s\n\n", orNone(gameDir))
		fmt.Println("  1) Définir / changer le dossier du jeu")
		fmt.Println("  2) Vérifier la copie du jeu (verify)")
		fmt.Println("  3) Inspecter game_scarlett_master.exe (inspect)")
		fmt.Println("  4) Recenser les imports IAT (census)")
		fmt.Println("  5) Analyser les shaders RDNA2 (shader-info, 500 premiers)")
		fmt.Println("  6) Mapper l'exécutable + TLS + IAT (run -map)")
		fmt.Println("  7) Lancer le démarrage natif expérimental (run -execute)")
		fmt.Println("  8) Afficher la configuration (config)")
		fmt.Println("  0) Quitter")
		fmt.Print("\nChoix : ")

		line, err := reader.ReadString('\n')
		if err != nil {
			return
		}
		choice := strings.TrimSpace(line)
		fmt.Println()

		var runErr error
		switch choice {
		case "0", "q", "Q":
			return
		case "1":
			fmt.Print("Chemin complet du dossier GTA VI : ")
			p, _ := reader.ReadString('\n')
			entered := strings.Trim(strings.TrimSpace(p), `"'`)
			if entered != "" {
				if savedPath, saveErr := core.SaveGameDir(entered); saveErr != nil {
					runErr = saveErr
				} else {
					gameDir = entered
					fmt.Printf("Dossier mémorisé dans %s.\n", savedPath)
				}
			}
		case "2":
			if gameDir == "" {
				runErr = fmt.Errorf("définissez d'abord le dossier du jeu (option 1) ou astral.toml")
			} else {
				runErr = cmdVerify([]string{gameDir})
			}
		case "3":
			if gameDir == "" {
				runErr = fmt.Errorf("définissez d'abord le dossier du jeu (option 1)")
			} else {
				runErr = cmdInspect([]string{filepath.Join(gameDir, game.ExpectedExecutable)})
			}
		case "4":
			if gameDir == "" {
				runErr = fmt.Errorf("définissez d'abord le dossier du jeu (option 1)")
			} else {
				runErr = cmdCensus([]string{filepath.Join(gameDir, game.ExpectedExecutable)})
			}
		case "5":
			if gameDir == "" {
				runErr = fmt.Errorf("définissez d'abord le dossier du jeu (option 1)")
			} else {
				runErr = cmdShaderInfo([]string{"-limit", "500", filepath.Join(gameDir, "shaders.rpf")})
			}
		case "6":
			if gameDir == "" {
				runErr = fmt.Errorf("définissez d'abord le dossier du jeu (option 1)")
			} else {
				runErr = cmdRun([]string{"-map", filepath.Join(gameDir, game.ExpectedExecutable)})
			}
		case "7":
			if gameDir == "" {
				runErr = fmt.Errorf("définissez d'abord le dossier du jeu (option 1)")
			} else if self, exeErr := os.Executable(); exeErr != nil {
				runErr = exeErr
			} else {
				// Lance l'exécution native dans un processus enfant pour que le menu
				// reste ouvert et lisible même si le binaire console déclenche une exception.
				child := exec.Command(self, "run", "-execute", "-unimplemented", "log", filepath.Join(gameDir, game.ExpectedExecutable))
				child.Stdout = os.Stdout
				child.Stderr = os.Stderr
				child.Stdin = os.Stdin
				if err := child.Run(); err != nil {
					fmt.Printf("\nProcessus jeu terminé : %v\n", err)
				}
			}
		case "8":
			runErr = cmdConfig(nil)
		default:
			fmt.Println("Choix inconnu.")
		}

		if runErr != nil {
			fmt.Fprintf(os.Stderr, "\nerreur : %v\n", runErr)
		}
		fmt.Print("\nAppuyez sur Entrée pour revenir au menu...")
		_, _ = reader.ReadString('\n')
		fmt.Println()
	}
}
