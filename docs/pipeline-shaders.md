# Pipeline Shaders (SGD5 / DXBC / FSSM / RDNA2)

Sur Xbox Series X|S, les shaders de GTA VI ne sont pas distribués en bytecode portable : ils sont précompilés en code machine **AMD RDNA2 (GFX10)** encapsulé dans plusieurs couches successives.

## 1. Chaîne d'extraction

```text
shaders.rpf (archive RPF9, sans besoin du TOC chiffré)
  └─► Enregistrements SGD5 (marqueur 0x000DDCE7, nom ASCII + flux raw-deflate)
        └─► Conteneur DXBC ("DXBC", parts ISG1, OSG1, PSG1, PSV0, RTS0, DXIL)
              └─► Index XDAT (+0x18 dans la part DXIL, entrée kind = 0x0D)
                    └─► Blob XBCH (version 3, flux zlib)
                          └─► Payload FSSM (version 0x101)
                                ├─► Métadonnées (stage LS/HS/ES/GS/VS/PS/MS/CS, wave32/wave64, VGPR/SGPR, LDS)
                                ├─► Tables Root Layout & Used Resources (user SGPRs)
                                └─► Code machine natif RDNA2 (GFX10)
```

- Implémentation : [`pkg/formats/sgd5.go`](../pkg/formats/sgd5.go), [`pkg/shader/dxbc.go`](../pkg/shader/dxbc.go) et [`pkg/shader/native.go`](../pkg/shader/native.go).

## 2. Décodage GFX10 et Représentation Intermédiaire (IR)

1. **Décodage binaire (`pkg/shader/gfx10.go`)** : décode les encodages 32 et 64 bits RDNA2 (`VOP1/2/3/3P/C`, `SOP1/2/C/K/P`, `VINTRP`, `SMEM`, `DS`, `MUBUF`, `MTBUF`, `MIMG`, `EXP`, `FLAT`) y compris les littéraux 32 bits (`src == 0xFF`) jusqu'à `s_endpgm`.
2. **Découpage CFG & SSA (`pkg/shader/translator.go`)** : construit les blocs de base pour préparer l'élimination du masque `EXEC` (structuration du flux de contrôle à la manière de VIctory et Leonida).
3. **Garde-fou anti-TDR** : reprend l'option `LoopLimit` de VIctory afin d'éviter qu'une boucle de shader mal traduite ne fige le pilote graphique Windows.

## 3. Double cible interchangeable : DXIL vs SPIR-V

| Cible | Backend associé | Points forts |
|---|---|---|
| **DXIL SM 6.6** (`--target dxil`) | D3D12 hôte (VIctory) | Zéro couche de traduction supplémentaire sous Windows, indexation directe des descriptor heaps SM 6.6 |
| **SPIR-V 1.2** (`--target spirv`) | Vulkan bindless (Leonida) | Portable (Windows / Linux / Wine), validation stricte `spirv-val`, mode `--wave64-emulate` exact en compute/mesh sur GPU 32-wide (NVIDIA / Intel) |

## 4. Cache atomique parallèle

Le package [`pkg/pipeline`](../pkg/pipeline) hache chaque shader avec `SHA-256(id_convertisseur + version + octets_dxbc)` et écrit le résultat dans un fichier temporaire avant renommage atomique (`os.Rename`). Le parallélisme par goroutines (`RunParallel`) permet d'exploiter tous les cœurs CPU sans condition de course.
