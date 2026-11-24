# Politique d'isolation des assets (Règle #1)

Inspirée directement de la « Rule #1 » de **VIctory** et des règles clean-room de **Leonida**, cette règle est non négociable dans **gta-vi-astral** :

> **Aucune donnée provenant du jeu ou dérivée du jeu ne doit jamais être écrite, copiée ou versionnée à l'intérieur du dépôt Git `gta-vi-astral`.**

## Ce qui est strictement interdit dans le dépôt

- Les fichiers originaux du jeu (`*.exe`, `*.dll`, `*.rpf`, `*.ttpf`, `*.xvd`, `MicrosoftGame.Config`).
- Les artefacts dérivés : shaders extraits ou traduits (`*.dxbc`, `*.dxil`, `*.spv`), textures détuilées (`*.dds`), modèles (`*.obj`, `*.glb`), audio (`*.awc`, `*.ogg`, `*.wav`), sauvegardes, dumps mémoire ou `crashes.log`.
- Toute clé de chiffrement ou routine de contournement de protection.

## Mécanismes de protection intégrés

1. **Marqueur `.astral-root`** : présent à la racine du dépôt.
2. **Garde-fou runtime (`pkg/core/policy.go`)** : la fonction `core.EnsureOutsideRepo(path)` résout les chemins absolus, les `..` et les liens symboliques, puis remonte l'arborescence pour vérifier qu'aucun dossier parent ne contient `.astral-root`.
3. **Vérification systématique avant toute écriture** : `pipeline.OpenCache`, `hle.NewVirtualFS`, `loader.AppendCrashLog` et `Config.ValidatePolicy` refusent immédiatement toute opération qui ciblerait l'intérieur du dépôt.
4. **`.gitignore` défensif** : bloque `astral.toml`, les binaires compilés et toutes les extensions d'assets connus.
