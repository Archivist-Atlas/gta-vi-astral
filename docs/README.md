# Documentation technique — gta-vi-astral

Bienvenue dans la documentation en français de **gta-vi-astral** (ASTRAL).

## Sommaire

1. [Architecture globale & choix du langage Go](architecture.md) — Découpage modulaire, synthèse **VIctory + Leonida**, CPU x86-64 natif vs recompilation statique GPU RDNA2.
2. [Guide de démarrage et commandes CLI](demarrage.md) — Installation, configuration (`astral.toml`, variables `ASTRAL_*`) et référence complète des commandes.
3. [Politique d'isolation des assets (Règle #1)](politique-assets.md) — Garde-fou `.astral-root`, séparation stricte du dépôt et des données dérivées.
4. [Pipeline Shaders (SGD5 / DXBC / FSSM / RDNA2)](pipeline-shaders.md) — Extraction sans TOC, dépaquetage `XDAT -> XBCH -> FSSM`, décodage GFX10, IR SSA et double cible DXIL / SPIR-V.
5. [Runtime, Chargeur PE32+ et couche HLE](runtime-hle-loader.md) — Préparation en DLL passive, résolution d'imports en 4 priorités, TLS statique, `.pdata`, GDK hors-ligne, XMem et VirtualFS overlay.
6. [Backend GPU & ABI Xbox Series D3D12.X](backend-gpu.md) — Slots vtable mesurés, interception inline sans patch binaire, double backend D3D12 (VIctory) et Vulkan + XDXR BVH4 (Leonida).
7. [Feuille de route](feuille-de-route.md) — Étapes techniques vers le démarrage complet, les menus et le rendu du monde.
