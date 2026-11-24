# Architecture globale de gta-vi-astral

## 1. Principe fondamental : CPU natif x86-64 + Recompilation statique GPU RDNA2

La Xbox Series X|S repose sur un processeur **AMD Zen 2 x86-64** — la même architecture CPU qu'un PC Windows x64. Il n'est donc **pas nécessaire** de décompiler ou de traduire le code machine CPU de `game_scarlett_master.exe` : les instructions x86-64 s'exécutent nativement dans le processus hôte.

En revanche, **trois couches majeures** doivent être reconstruites pour faire tourner le titre sur PC :

1. **Chargeur PE32+ & Runtime HLE** (`pkg/pe`, `pkg/loader`, `pkg/hle`) : mappage de l'exécutable et des DLL embarquées, redirection TLS statique, tables d'exceptions `.pdata`, shims GDK hors-ligne, gestion mémoire `xmem`, système de fichiers virtuel `GAME:` avec overlay copy-on-write, DirectStorage, audio et manettes.
2. **Recompilation statique des shaders GPU** (`pkg/shader`, `pkg/pipeline`) : sur Xbox Series, le jeu embarque du code machine **AMD RDNA2 (GFX10)** précompilé (`SGD5 -> DXBC -> XDAT -> XBCH -> FSSM`). ASTRAL décode ce binaire vers une **représentation intermédiaire (IR)** indépendante de la cible, puis génère au choix du **DXIL SM 6.6** (D3D12) ou du **SPIR-V** (Vulkan).
3. **Couche graphique D3D12.X** (`pkg/gpu`) : traduction de l'ABI Xbox Series `d3d12_xs.dll` vers un backend hôte interchangeable (**D3D12 Windows** ou **Vulkan + XDXR BVH4**).

## 2. Pourquoi Go comme langage principal ?

ASTRAL démarre **100 % en Go** afin d'éviter la complexité prématurée d'un projet multi-langages :

- **Parallélisme natif (goroutines)** : idéal pour scanner, décompresser et traduire des dizaines de milliers de shaders en parallèle (`pkg/pipeline/runner.go`).
- **Parsing binaire sûr et rapide** : lecture claire des structures PE32+, SGD5, DXBC, XBCH, FSSM, RPF9 et AWC sans dépendances externes lourdes.
- **Compilation instantanée & binaire unique** : un seul exécutable `astral.exe` sous Windows.
- **Évolutivité hybride** : grâce au découpage strict par packages (`pkg/gpu`, `pkg/loader`, `pkg/shader`), un sous-module bas niveau critique (ex. bridge COM/C++ D3D12 ou Vulkan natif) pourra être branché en C++/Rust via CGO ou DLL dédiée uniquement lorsque les mesures de performance ou d'ABI le justifieront.

## 3. Synthèse du meilleur de VIctory et Leonida

| Domaine | Apport retenu de **VIctory** | Apport retenu de **Leonida** | Implémentation dans **ASTRAL** |
|---|---|---|---|
| Organisation | Architecture modulaire par responsabilités (`crates/*`) | Séparation nette outils / runtime / manifests | `pkg/{core,pe,formats,shader,pipeline,gpu,hle,loader,game}` |
| Sécurité / Clean-room | Garde-fou runtime `.victory-root` (`ensure_outside_repo`) | Zéro clé, zéro déchiffrement, vérification de copie | `.astral-root` + `pkg/core/policy.go` + `pkg/game` |
| Analyse binaire | Parsing PE32+ complet + CodeView RSDS | Entropie de Shannon par section + comptage `.pdata` | `pkg/pe/image.go` |
| Chargeur PE | Préparation en « DLL passive » + résolution 4 priorités | Overlay copy-on-write sur `GAME:` + traçage imports | `pkg/loader/` + `pkg/hle/files.go` |
| Shaders RDNA2 | Dépaquetage `SGD5`/`XDAT`/`XBCH`/`FSSM`, SSA IR, cible DXIL SM 6.6 | Couverture complète, émulation **wave64** (GPU 32-wide), cible SPIR-V | `pkg/shader/` (IR commune + double cible DXIL/SPIR-V) |
| Backend GPU | D3D12 hôte natif Windows, interception inline `Put==Limit==0` | Slots vtable `d3d12_xs` mesurés, Vulkan bindless, XDXR BVH4 | `pkg/gpu/abi.go` + `pkg/gpu/backend.go` |
| Cache & Pipeline | Cache disque atomique adressé par contenu | Exécution parallèle shardée et reprise sur interruption | `pkg/pipeline/cache.go` + `runner.go` |

## 4. Flux de données

```text
  Copie locale du jeu (hors dépôt)           gta-vi-astral (Go)
  ────────────────────────────────           ─────────────────────────────────────
  shaders.rpf / *.sgd5 ────────────────────► pkg/formats (SGD5) + pkg/shader
                                             (DXBC -> XDAT -> XBCH -> FSSM -> GFX10)
                                                        │
                                                        ▼
                                             pkg/pipeline (Cache SHA-256 atomique)
                                                        │
  game_scarlett_master.exe ────────────────► pkg/pe + pkg/loader (Passive-DLL, IAT)
                                                        │
                                                        ▼
                                             pkg/hle (GDK, XMem, VirtualFS, Audio, Net)
                                                        │ D3D12.X ABI
                                                        ▼
                                             pkg/gpu (D3D12 hôte OU Vulkan + XDXR BVH4)
```
