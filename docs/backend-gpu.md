# Backend GPU & ABI Xbox Series D3D12.X

Le package [`pkg/gpu`](../pkg/gpu) sépare strictement les **spécificités ABI de la Xbox Series** (`abi.go`) du **backend d'exécution hôte** (`backend.go`).

## 1. Contrats ABI Xbox Series D3D12.X mesurés

Les recherches empiriques de **VIctory** et **Leonida** ont mis en évidence plusieurs écarts majeurs entre `d3d12_xs.dll` (console) et `d3d12.dll` (PC) :

- **Interception sans patch binaire des command lists inlinées** : en maintenant `PutPointer == LimitPointer == 0` (`NewInlineCommandListTrap`), tout appel inliné du moteur passe par la vtable et est intercepté proprement.
- **Root Signatures** : l'octet haut (`0xFF000000`, ex. `0x03000000`) contient des drapeaux propres à la Xbox et doit être nettoyé (`SanitizeRootSignatureFlags`) avant sérialisation PC.
- **Écarts de slots vtable (`pkg/gpu/abi.go`)** :
  - `ID3D12Device` slot `26` (`GetCustomHeapProperties`) : ordre `(return_buf, this, ...)` inversé par rapport à MSVC PC.
  - `ID3D12Device` slots `48` (`CreateCommandQueueX`), `62` (`CreateComputePipelineState` + 2 mots), `67` (compute PSO natif), `73` (`CreateCommandListX`), `76` (création ressource), `78–80` (événements de trame), `133` (`CreatePlacedResourceX` avec VA brute).
  - `ID3D12DescriptorHeap` slots `9` et `10` : retournent le handle 8 octets directement dans `RAX` (au lieu d'un pointeur caché dans `RDX`).
  - `ID3D12CommandQueue` slots `30` / `32` : `PresentX`.
  - `ID3D12GraphicsCommandList` slot `148` : `AllocateUploadMemory`.

## 2. Double backend interchangeable

| Backend | Option | Inspiration | Caractéristiques |
|---|---|---|---|
| **D3D12 Hôte** | `--backend d3d12` | VIctory | Natif Windows, shaders **DXIL SM 6.6**, suivi des pages sales (WriteWatch + shadow memory), support **WARP** logiciel pour déboguer sans TDR |
| **Vulkan Bindless** | `--backend vulkan` | Leonida | Portable Windows/Linux/Wine, shaders **SPIR-V**, émulation **wave64** (NVIDIA/Intel), Buffer Device Address (BDA), émulation D24S8 sur D32S8, constructeur GPU **XDXR BVH4** pour le ray tracing |

## 3. Layouts de textures RDNA2 (`pkg/formats/texture.go`)

Les textures console utilisent les modes de tuilage matériel AMD GFX10.3 (`SW_4KB_S`, `SW_64KB_S`, `SW_64KB_D`, modes XOR `_T`/`_X`). `ComputeTextureLayout` calcule les dimensions de blocs, l'alignement et les tailles de surfaces correspondantes.
