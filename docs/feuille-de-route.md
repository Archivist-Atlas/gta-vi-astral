# Feuille de route — gta-vi-astral

## Phase 1 — Fondations & Outillage (Terminé)
- [x] Architecture modulaire Go (`cmd/astral`, `pkg/{core,pe,formats,shader,pipeline,gpu,hle,loader,game}`).
- [x] Garde-fou anti-fuite d'assets (`.astral-root` + `EnsureOutsideRepo`).
- [x] Analyseur PE32+ complet (sections, entropie Shannon, imports/delay-imports, `.pdata`, TLS, CodeView RSDS).
- [x] Vérification du package GTA VI Xbox Series v1.0.2.1 (`astral verify`).
- [x] Extraction `SGD5 -> DXBC -> XDAT -> XBCH -> FSSM` et décodeur binaire RDNA2/GFX10 (`astral shader-info`).
- [x] Cache disque atomique SHA-256 + orchestration parallèle par goroutines (`astral translate`).
- [x] Résolution d'imports en 4 priorités et recensement (`astral census`).
- [x] Documentation technique complète en français (`docs/`).

## Phase 2 — Traducteur Shaders complet (IR SSA -> DXIL & SPIR-V)
- [x] Levée 1:1 en IR SSA avec filtrage des ops de planification et suivi des gardes `EXEC` (`pkg/shader/ssa.go`).
- [ ] Structuration complète du flux de contrôle (élimination des masques `EXEC`).
- [ ] Analyse de flux de données pour les descripteurs et boucles waterfall (`v_readfirstlane_b32`).
- [ ] Émetteur binaire **DXIL SM 6.6** (LLVM 3.7 bitcode + conteneur DXBC signé).
- [ ] Émetteur **SPIR-V 1.2** + mode `--wave64-emulate` validé par `spirv-val`.
- [ ] Suite de tests sémantiques et différentiels sur GPU / WARP.

## Phase 3 — Chargeur natif Windows & Shims HLE live
- [x] Mappage mémoire natif Windows (`LoadLibraryExW` sur image passive-DLL + redirection des 53 246 accès TLS slot 0 + binding IAT réel + chargement récursif des DLL du titre).
- [x] Génération dynamique de thunks x86-64 pour les imports non implémentés (`log`/`zero`/`fail`/`trap`).
- [x] Gestionnaire d'exceptions VEH (`module+RVA` -> `crashes.log`).
- [x] Premiers shims exécutables live : `XMemVirtualAlloc` (`VirtualAlloc2` avec plages 40-bit/32-bit), `XMemAlloc/Free`, `D3D12ConfigureVirtualMemoryX`, `LoadLibrary*/GetProcAddress` console, `xgameruntime` (`InitializeApiImpl*`, `QueryApiImpl` avec décodage GUID), `GameInputCreate`, isolation réseau hors-ligne.
- [ ] Implémentation complète des interfaces COM `QueryApiImpl` (`XUser`, `XGameSave`, `XStore`, `XPackage`, `XAsync`), `xcrdapi` (DirectStorage + BCPack BC1–BC7), `AcpHal` (XMA2) et `xapu` (Opus).

## Phase 4 — Exécution GPU (Boot -> Menus -> Monde)
- [ ] Proxy COM Xbox `d3d12_xs.dll` connecté au backend D3D12 hôte (et/ou Vulkan bindless).
- [ ] Synchronisation mémoire unifiée (WriteWatch / shadow buffers) et détuilage textures RDNA2.
- [ ] Compilation asynchrone des PSO + persistance sur disque + garde-fou `ExecuteIndirect`.
- [ ] Support Mesh Shaders, 64-bit image atomics et constructeur XDXR BVH4 pour le rendu du monde.
