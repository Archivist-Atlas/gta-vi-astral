# gta-vi-astral (ASTRAL)

Système modulaire en **Go** pour l'analyse, la recompilation de shaders et l'exécution native sur PC de **Grand Theft Auto VI (Xbox Series X|S, v1.0.2.1)** à partir de votre propre copie locale du jeu.

> **Règle #1 (Clean-Room)** : ce dépôt ne contient **aucun fichier du jeu, aucune clé, aucun exécutable ni aucun mécanisme de déchiffrement**. Toutes les données dérivées (caches, shaders traduits, sauvegardes, overlay, logs) sont strictement gardées **en dehors du dépôt** grâce au garde-fou `.astral-root` (`pkg/core/policy.go`).

> [!WARNING]
> **Projet expérimental en cours de développement — NON JOUABLE actuellement.**
> Le moteur démarre, charge toutes ses archives (`.rpf` + TTDB + licences), connecte l'utilisateur hors-ligne (`XUser` + `XGameSave`) et tourne en boucle continue **780+ frames `PresentX` sur 45 s (0 crash, 0 `WARN`)** avec recompilation live de ses shaders RDNA2 sur GPU Windows, mais le rendu 3D complet et l'audio ne sont **pas encore fonctionnels**.

## Avancement global : ~43 %

```text
Progression globale : [█████████░░░░░░░░░░░] 43 %
```

- **Boot natif continu + `XUser` + `XGameSave` + GPU Compute (780+ frames / 45 s, 0 crash)** : ✅ Terminé (`100 %`)
- **Premiers vrais pixels / écrans d'intro** : 🔄 En cours (`~82 %`)
- **Menus principaux navigables** : 🔄 En cours (`~55 %`)
- **Monde ouvert 3D jouable (audio, contrôles, ray tracing)** : ⏳ À venir (`~35 %`)

| Sous-système | Avancement | État actuel | Reste à faire |
| :--- | :---: | :--- | :--- |
| **Chargeur PE / Mémoire / CPU x64** | `90 %` | Mappage natif, `DllMain` des DLL du titre, 53 246 patchs TLS, VEH + émulation `CLZERO`/`MONITORX`/`MWAITX`, pools physiques 64 KiB (`xmem` 6,2 Go sans aliasing). | Ajustements ponctuels sur de nouveaux chemins CPU en jeu. |
| **Stockage & Streaming (`VirtualFS` / `DirectStorage`)** | `80 %` | 12 `.rpf`, `core_bvh4.rpf`, `core_licenses_data.rpf` et toutes les archives virtuelles TTDB `.ttpf` ouverts ; +9 500 requêtes `ReadAt` + `zlib` avec vtables COM partagées O(1). | Décompression matérielle **BCPack** (BC1–BC7) et `PAGE_DESTINATION` pour le streaming lourd du monde. |
| **OS Console / HLE (`xgameruntime`, `GameInput`, Audio)** | `75 %` | `xasync`, **`xuser` (connexion hors-ligne `AstralPlayer`)**, **`xgamesave` (fournisseur hors-ligne 256 Mo)**, **`xaccessibility`**, `xnetworking` (hors-ligne), `AcpHal`/`xapu`/`chad`, et **`GameInput` réel XInput + Clavier Windows**. | Persistance disque `XGameSave` et décodage audio réel (`xapu` Opus / `chad` XDsp). |
| **Recompilateur Shaders RDNA2 (`pkg/shader`)** | `30 %` | Extraction `DXBC->XDAT->XBCH->FSSM`, décodeur GFX10, IR SSA, `ISG1`/`OSG1`, et recompilation live RDNA2 -> HLSL -> `ID3D12PipelineState` (~93 % de couverture sur les 3 PSOs actifs du boot). | Flux de contrôle structuré (branches + masque `EXEC`), descripteurs bindless, `LDS`/`FLAT`, Mesh Shaders (`MS`/`AS`), Compute (`CS`), compilation asynchrone. |
| **Pont GPU Hôte D3D12 (`pkg/gpu` + `d3d12x`)** | `38 %` | Fenêtre Win32 unique, vtables COM partagées O(1) (zéro limite de callbacks Go), `ID3D12Device` matériel + `SwapChain3`, `PresentX` à 30 fps (780+ frames + 119 `Dispatch` détectés), rejeu `DrawInstanced`, upload VB + déréférencement `CBV1`, `CreatePlacedSRV` (`SRV2`) + détuilage RDNA2 (`4KB`/`64KB` & `R8`/`RGBA`/`BC1–BC7`), `GetDeviceRemovedReason`. | Exécution Compute (`CS`), rendu multi-passes (G-Buffer/ombres/post-process), `ExecuteIndirect`, BVH4. |

## Pourquoi ASTRAL ?

**gta-vi-astral** synthétise en Go les meilleures idées architecturales de deux projets de référence :

- **De VIctory** : l'architecture ultra-modulaire par responsabilités (`pkg/*` inspiré de `crates/*`), le garde-fou anti-fuite d'assets, la préparation PE32+ en « DLL passive », la résolution d'imports en 4 priorités, le cache atomique adressé par contenu, la couche HLE hors-ligne (GDK, XMem, DirectStorage, Audio XMA2/Opus) et le backend D3D12 hôte (DXIL SM 6.6).
- **De Leonida** : l'analyse exhaustive des archives et shaders RDNA2 (26 503 shaders), le décodage et l'émulation **wave64** pour les GPU 32-wide (NVIDIA/Intel), l'overlay copy-on-write sur `GAME:`, les contrats ABI mesurés sur `d3d12_xs.dll` et le backend portable Vulkan + constructeur BVH4 XDXR.

## Architecture des packages

```text
gta-vi-astral/
├── cmd/astral/       # Interface en ligne de commande unifiée
├── pkg/
│   ├── core/         # Configuration TOML/env, garde-fou anti-fuite (.astral-root), logs slog
│   ├── pe/           # Analyseur PE32+ x86-64 (sections, entropie, imports/exports, TLS, .pdata, CodeView)
│   ├── formats/      # SGD5, RPF9 (en-tête), layouts textures RDNA2 GFX10.3, AWC audio, JOAAT32
│   ├── shader/       # DXBC -> XDAT -> XBCH -> FSSM, décodeur RDNA2/GFX10, CFG/IR, cible DXIL ou SPIR-V
│   ├── pipeline/     # Cache atomique SHA-256 hors dépôt + orchestrateur parallèle par goroutines
│   ├── gpu/          # Contrats ABI Xbox D3D12.X + abstraction double backend (D3D12 / Vulkan)
│   ├── hle/          # Registre de shims console, HostPolicy, VirtualFS (overlay COW), XMem
│   ├── loader/       # Résolution d'imports (4 niveaux), recensement (census), préparation passive-DLL, crash log
│   └── game/         # Identité spécifique GTA VI Xbox Series 1.0.2.1 et lecture MicrosoftGame.Config
└── docs/             # Documentation technique complète en français
```

## Démarrage rapide

Prérequis : **Go 1.24+** (testé avec Go 1.27.1 sous Windows x64) et votre propre dossier GTA VI.

```powershell
# 1. Lancer les tests unitaires (aucun fichier de jeu requis)
go test ./...

# 2. Compiler l'outil CLI
go build -o astral.exe ./cmd/astral

# 3. Configurer vos chemins hors dépôt
copy astral.example.toml astral.toml
.\astral.exe config

# 4. Vérifier votre copie locale du jeu
.\astral.exe verify "C:\GTA_VI"

# 5. Inspecter l'exécutable et recenser ses imports
.\astral.exe inspect "C:\GTA_VI\game_scarlett_master.exe"
.\astral.exe census  "C:\GTA_VI\game_scarlett_master.exe"

# 6. Analyser les shaders RDNA2 de shaders.rpf
.\astral.exe shader-info -limit 500 "C:\GTA_VI\shaders.rpf"

# 7. Mapper l'exécutable dans Windows (TLS statique + binding IAT réel) sans exécuter
.\astral.exe run -map "C:\GTA_VI\game_scarlett_master.exe"

# 8. Lancer le point d'entrée natif (expérimental, journalisé avec VEH)
.\astral.exe run -execute -unimplemented log "C:\GTA_VI\game_scarlett_master.exe"
```

## Documentation

Toute la documentation technique en français est disponible dans [`docs/README.md`](docs/README.md).
