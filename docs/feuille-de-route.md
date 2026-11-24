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
- [ ] Passe SSA complète et structuration du flux de contrôle (élimination des masques `EXEC`).
- [ ] Analyse de flux de données pour les descripteurs et boucles waterfall (`v_readfirstlane_b32`).
- [ ] Émetteur binaire **DXIL SM 6.6** (LLVM 3.7 bitcode + conteneur DXBC signé).
- [ ] Émetteur **SPIR-V 1.2** + mode `--wave64-emulate` validé par `spirv-val`.
- [ ] Suite de tests sémantiques et différentiels sur GPU / WARP.

## Phase 3 — Chargeur natif Windows & Shims HLE live
- [ ] Mappage mémoire natif Windows (`LoadLibraryExW` sur image préparée + redirection TLS slot 0 + `RtlAddFunctionTable`).
- [ ] Génération dynamique de thunks x86-64 pour les imports non implémentés (`log`/`zero`/`fail`/`trap`).
- [ ] Implémentation live des shims `xgameruntime`, `xmem`, `xcrdapi` (DirectStorage + décodeur BCPack BC1–BC7), `gameinput`, `AcpHal` (XMA2) et `xapu` (Opus).
- [ ] Journalisation automatique des crashs (`module+RVA` + stack unwind) dans `crashes.log`.

## Phase 4 — Exécution GPU (Boot -> Menus -> Monde)
- [ ] Proxy COM Xbox `d3d12_xs.dll` connecté au backend D3D12 hôte (et/ou Vulkan bindless).
- [ ] Synchronisation mémoire unifiée (WriteWatch / shadow buffers) et détuilage textures RDNA2.
- [ ] Compilation asynchrone des PSO + persistance sur disque + garde-fou `ExecuteIndirect`.
- [ ] Support Mesh Shaders, 64-bit image atomics et constructeur XDXR BVH4 pour le rendu du monde.
