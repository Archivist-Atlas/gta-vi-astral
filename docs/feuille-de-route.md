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
- [x] Base d'émetteur binaire **SPIR-V 1.2** en Go pur (`pkg/shader/emit_spirv.go` : en-tête, capabilities, `OpEntryPoint`, `OpExecutionMode`, squelette fonctionnel).
- [ ] Structuration complète du flux de contrôle (élimination des masques `EXEC`) et abaissement ALU/mémoire/images.
- [ ] Analyse de flux de données pour les descripteurs et boucles waterfall (`v_readfirstlane_b32`).
- [ ] Émetteur binaire **DXIL SM 6.6** (LLVM 3.7 bitcode + conteneur DXBC signé).
- [ ] Validation `spirv-val` + mode `--wave64-emulate` + tests différentiels GPU / WARP.

## Phase 3 — Chargeur natif Windows & Shims HLE live
- [x] Mappage mémoire natif Windows (`LoadLibraryExW` sur image passive-DLL + exécution de `DllMain(DLL_PROCESS_ATTACH)` sur les DLL du titre + redirection des 53 246 accès TLS slot 0 + binding IAT réel).
- [x] Génération dynamique de thunks x86-64 pour les imports non implémentés (`log`/`zero`/`fail`/`trap`).
- [x] Gestionnaire d'exceptions VEH (`module+RVA`, adresse fautive, pile d'appels symbolisée, registres x64, octets `RIP` -> `crashes.log`) + émulation des instructions AMD Zen 2 `CLZERO` (`0F 01 FC`), `MONITORX` (`0F 01 FA`) et `MWAITX` (`0F 01 FB`) pour les CPU Intel.
- [x] Mémoire physique console complète (`pkg/hle/xmem_windows.go` : PFN alignés sur 16 frames 4 KiB = 64 KiB + trampolines natifs x64 `__XMemAllocationHooks__`, pool streaming 6,2 Go alloué en live sans aliasing parasite).
- [x] Moteur DirectStorage réel (`pkg/hle/dstorage_windows.go` : 12 `.rpf` + `core_bvh4.rpf` + toutes les archives virtuelles TTDB `.ttpf` ouverts, 17 queues, +7 000 lectures `ReadAt` + décompression `zlib`).
- [x] Calculateur de layouts/swizzles textures `xg_xs.dll` (`pkg/hle/xg_windows.go` : `XGSuggestSwizzleMode`, `XGCreateTextureComputer`, `IXGTextureAddressComputer::GetResourceTiling` avec séparation mips standards / mips packés RDNA2 64 KiB).
- [x] Sous-système audio APU `AcpHal.dll`, `xapu.dll` et `chad.dll` (`pkg/hle/acphal_windows.go` : `ApuCreateHeap` 455 Mo, `AcpHalAllocateShapeContexts`, `AcpHalCreate`/`IAcpHal`, `XApuConnect` 256 flux, `XDspConnectWithMaximumStreamLimit` 128 flux).
- [x] Shims exécutables live : `VirtualFS` (`CreateFileW/A/2`, `FindFirstFile*`, `fopen`/`_wfopen`/`_wstat64`, `GetModuleHandle`/`GetModuleFileName`, `G:\`/`T:\`), `xgameruntime` (`xasync` avec `DuplicateHandle`/`CloseHandle`/`Terminate`, `xnetworking`, `xuser`, `xgame`, `xsystem`, `xpackage`, `storage`, `display`), `GameInputCreate`, `OutputDebugString`, isolation réseau hors-ligne.
- [ ] Implémentation complète des codecs audio/texture temps réel (`BCPack` BC1–BC7, décodage matériel `xapu` Opus / `chad` XDsp) et sauvegardes `XGameSave`.

## Phase 4 — Exécution GPU (Boot -> Menus -> Monde)
- [x] Proxy COM Xbox `d3d12_xs.dll` (`pkg/hle/d3d12x_windows.go` : `ID3D12DeviceX` 200 slots, `IDXGIDevice`/`Adapter`/`Factory`/`Output`, `XboxBvhBuilder.dll` XDXR initialisé, `ID3D12CommandQueueX` 2026 + `PresentX` avec inspection du plan 0 `3840x2160` RGBA8, `ID3D12FenceX::GetAddress`, `GetFrameStatisticsX`, `AllocateUploadMemory`, `RegisterPagePool`, `CopyDescriptors`, `CopyMemoryX`, `D3D12Serialize*RootSignature` avec fallback PC, création des PSOs, suivi `Draw`/`Dispatch`/`ClearRenderTargetView`).
- [x] Fenêtre Win32 native (`pkg/gpu/window_windows.go`) + **`ID3D12Device` matériel Windows** et **SwapChain DXGI3 double-buffering** (`pkg/gpu/d3d12_host_windows.go` : RTV heap, `CommandAllocator`, `CommandList`, `Fence`, `UploadBuf`, `PresentClear` réel) atteignant **480+ frames `PresentX` en continu (0 crash, 0 WARN)** sur le titre retail.
- [ ] Enregistrement/rejeu complet des command lists (`PSO`, `RootSignature`, `Vertex/IndexBuffer`, `DrawInstanced`/`DrawIndexedX`) sur le `ID3D12Device` hôte + synchronisation mémoire unifiée (WriteWatch / shadow buffers) et détuilage textures RDNA2.
- [ ] Compilation asynchrone des PSO + persistance sur disque + garde-fou `ExecuteIndirect`.
- [ ] Support Mesh Shaders, 64-bit image atomics et constructeur XDXR BVH4 pour le rendu du monde.
