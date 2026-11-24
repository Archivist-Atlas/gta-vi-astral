# Runtime, Chargeur PE32+ et couche HLE

## 1. Analyse PE32+ (`pkg/pe`)

Avant toute exécution, `pkg/pe` extrait en lecture seule :
- Les en-têtes PE32+ AMD64 (`ImageBase`, `SizeOfImage`, `EntryRVA`, `Subsystem`, `StackReserve`).
- Les sections et leur **entropie de Shannon** (une entropie proche de `8.0` indique une section compressée ou chiffrée).
- Les imports directs, les delay-imports, les exports (y compris forwarders), la table d'exceptions `.pdata` (nombre de `RUNTIME_FUNCTION`), le répertoire TLS statique et l'empreinte **CodeView RSDS** (GUID + Age + chemin PDB).

## 2. Préparation « DLL passive » (`pkg/loader/prepare.go`)

Technique issue de **VIctory** : plutôt que de réimplémenter intégralement le chargeur noyau de Windows, l'image du jeu est clonée en mémoire (ou dans le cache hors dépôt) avec quelques ajustements ciblés :
- Activation du flag `IMAGE_FILE_DLL`.
- Mise à zéro de `AddressOfEntryPoint` et `CheckSum`.
- Suppression de `IMAGE_DLLCHARACTERISTICS_FORCE_INTEGRITY`.

Le chargeur Windows s'occupe ainsi nativement des relocations, des tables de déroulement SEH (`.pdata`) et de la visibilité dans les débogueurs, tout en laissant à ASTRAL le contrôle total du binding IAT, du TLS statique (redirection du slot 0) et du point d'entrée.

## 3. Résolution d'imports en 4 priorités (`pkg/loader/resolve.go`)

Pour chaque symbole `dll!fonction` importé par le jeu, ASTRAL applique l'ordre strict suivant :

1. **HLE (`pkg/hle/registry.go`)** : toujours prioritaire (permet aussi de surcharger des appels Win32 précis comme `ws2_32!connect`).
2. **HostPolicy (`pkg/hle/policy.go`)** : pass-through vers la DLL Windows officielle (chargée depuis `System32` pour prévenir le DLL planting).
3. **DLL du titre** : DLL fournie à côté de l'exécutable (ex. `Bink2Scarlett.dll`, `XboxBvhBuilder.dll`, `amd_fidelityfx_upscaler_xbox.dll`).
4. **Stub / Unimplemented** : géré selon `unimplemented = log | zero | fail | trap`.

## 4. Sous-systèmes HLE (`pkg/hle`)

- **GDK hors-ligne** (`xgameruntime.dll`, `gameplatformservices.dll`) : utilisateur local hors-ligne (`XUser`), sauvegardes atomiques versionnées (`XGameSave`), licence locale (`XStore`), package (`XPackage`), files asynchrones (`XTaskQueue` / `XAsync`).
- **Mémoire unifiée** (`xmem.dll` — `pkg/hle/xmem.go`) : nettoyage des flags `MEM_GRAPHICS` / `MEM_TITLE` et traduction des protections `PAGE_GRAPHICS_*`.
- **Système de fichiers virtuel** (`pkg/hle/files.go`) : `GAME:` / `G:` redirigé vers le dossier du jeu en lecture seule avec **overlay copy-on-write** (approche Leonida), et `T:` redirigé vers le stockage persistant sous `data_dir` (approche VIctory).
- **Isolation réseau stricte** : blocage de toute connexion hors loopback (`127.0.0.0/8`, `::1`) et refus de la résolution DNS externe.
- **Audio & Entrées** : `AcpHal.dll` (XMA2), `xapu.dll` (Opus), `chad.dll` (DSP silencieux), `gameinput.dll` (XInput + clavier virtuel + manettes).
