# Guide de démarrage et commandes CLI

## 1. Prérequis

- **Windows 10/11 x64** (pour l'exécution et le backend D3D12 ; les outils d'analyse et tests tournent aussi sous Linux/macOS).
- **Go 1.24 ou plus récent** (`go version`).
- Votre propre copie locale de **GTA VI Xbox Series (v1.0.2.1)** avec ses fichiers lisibles sur disque (`MicrosoftGame.Config`, `game_scarlett_master.exe`, `*.rpf`).

## 2. Configuration

Copiez le modèle de configuration à la racine (le fichier `astral.toml` est ignoré par Git) :

```powershell
copy astral.example.toml astral.toml
```

Renseignez ensuite vos chemins **en dehors du dépôt** :

```toml
[paths]
game_dir = 'C:\GTA_VI'

[runtime]
console_profile = 'series-x'
gpu_backend = 'auto'
unimplemented = 'log'
wave64_emulate = 'auto'
jobs = 0
```

Les variables d'environnement ont toujours la priorité sur `astral.toml` :

| Variable | Rôle |
|---|---|
| `ASTRAL_CONFIG` | Chemin alternatif vers un fichier `.toml` |
| `ASTRAL_GAME_DIR` | Dossier contenant votre copie de GTA VI |
| `ASTRAL_CACHE_DIR` | Dossier de cache (défaut : `%LOCALAPPDATA%\gta-vi-astral\cache`) |
| `ASTRAL_DATA_DIR` | Sauvegardes, paramètres, `crashes.log` (défaut : `%LOCALAPPDATA%\gta-vi-astral\data`) |
| `ASTRAL_OVERLAY_DIR` | Overlay copy-on-write pour garder le dossier du jeu intact |
| `ASTRAL_GPU_BACKEND` | `auto`, `d3d12` ou `vulkan` |
| `ASTRAL_CONSOLE` | `series-x` ou `series-s` |
| `ASTRAL_UNIMPLEMENTED` | `log`, `zero`, `fail` ou `trap` |
| `ASTRAL_WAVE64_EMULATE` | `auto`, `on` ou `off` |
| `ASTRAL_JOBS` | Nombre de goroutines parallèles |
| `ASTRAL_LOG` | Niveau de logs (`debug`, `info`, `warn`, `error`) |

## 3. Compilation et tests

```powershell
go test -v ./...
go build -o astral.exe ./cmd/astral
```

## 4. Référence des commandes CLI

| Commande | Description |
|---|---|
| `astral config` | Affiche la configuration active et vérifie la politique anti-fuite |
| `astral verify [dossier_jeu]` | Vérifie la présence des fichiers clés GTA VI v1.0.2.1 et lit `MicrosoftGame.Config` |
| `astral inspect <exe\|dll> [--imports] [--exports]` | Analyse complète PE32+ : sections, entropie, `.pdata`, TLS, CodeView RSDS |
| `astral census <exe\|dll> [--missing]` | Classe chaque import (`HLE`, `Host`, `Title DLL`, `Manquant`) et calcule le taux de couverture |
| `astral shader-info <fichier> [--limit N]` | Extrait et décode les shaders RDNA2 (`SGD5/DXBC/XDAT/XBCH/FSSM`) avec statistiques par étage |
| `astral translate <fichier> [--target dxil\|spirv] [--wave64-emulate] [--limit N] [--jobs N]` | Lance le pipeline parallèle d'analyse/planification et écrit dans le cache SHA-256 |
| `astral run <exe> [--dry-run] [--backend auto\|d3d12\|vulkan]` | Prépare l'image passive-DLL, vérifie le recensement IAT, initialise VirtualFS et le backend GPU |
