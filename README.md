# gta-vi-astral (ASTRAL)

Système modulaire en **Go** pour l'analyse, la recompilation de shaders et l'exécution native sur PC de **Grand Theft Auto VI (Xbox Series X|S, v1.0.2.1)** à partir de votre propre copie locale du jeu.

> **Règle #1 (Clean-Room)** : ce dépôt ne contient **aucun fichier du jeu, aucune clé, aucun exécutable ni aucun mécanisme de déchiffrement**. Toutes les données dérivées (caches, shaders traduits, sauvegardes, overlay, logs) sont strictement gardées **en dehors du dépôt** grâce au garde-fou `.astral-root` (`pkg/core/policy.go`).

> [!WARNING]
> **Projet expérimental en cours de développement — NON JOUABLE actuellement.**
> Le moteur démarre, charge toutes ses archives (`.rpf` + TTDB + licences), connecte l'utilisateur hors-ligne (`XUser` + `XGameSave`), lit la **vidéo d'introduction 4K Bink2 avec son audio** et affiche les **menus de configuration** en boucle continue avec recompilation live de ses shaders RDNA2 sur GPU Windows, mais le **rendu 3D temps réel du monde ouvert** n'est pas encore fonctionnel.

## Avancement global : ~54 %

```text
Progression globale : [███████████░░░░░░░░░] 54 %
```

- **Boot natif continu + `XUser` + `XGameSave` + `XAsyncRun` + `XStore` + DirectStorage (`story000.rpf`) + BCPack pur Go** : ✅ Opérationnel (`92 %`)
- **Vidéo d'introduction 4K Bink2 + Audio** : ✅ Opérationnel (`100 %` — passe matérielle D3D12 YUV420p BT.709 fluide, couleurs fidèles, sans trames noires)
- **Interfaces 2D / menus de configuration (`ASTRALPLAYER (A)` -> `TAILLE DE L'INTERFACE` -> `LUMINOSITÉ` -> `MENTIONS LÉGALES`)** : 🔄 En cours (`~82 %` — navigation manette, dégradés et fonds Vice City nets ; courbe gamma luminosité et quelques glyphes courts à corriger)
- **Audio console (`xapu` Opus + audio d'intro + `AcpHal` XMA2)** : 🔄 En cours (`~55 %` — audio d'intro, sons UI Opus 48 kHz pur Go et musique de chargement avec bouton mute persisté opérationnels ; décodage XMA2 in-game à compléter)
- **Rendu 3D temps réel & monde ouvert jouable (shaders 3D bindless, G-Buffer, Depth-Stencil, ray tracing)** : ⏳ Début (`~18 %`)

| Sous-système | Avancement | État actuel | Reste à faire |
| :--- | :---: | :--- | :--- |
| **Chargeur PE / Mémoire / CPU x64** | `90 %` | Mappage natif, `DllMain` des DLL du titre, 53 246 patchs TLS, VEH + émulation AMD Zen 2 (`CLZERO`, `MONITORX`, `MWAITX`, `RDPRU`, `CLWB`, SSE4a `INSERTQ`/`EXTRQ`), pools physiques 64 KiB (`xmem` 6,2 Go) + `xmemMapAlias` + `xmemWritePhysical`. | Cas limites CPU/mémoire en monde ouvert. |
| **Stockage & Streaming (`VirtualFS` / `DirectStorage` / `BCPack`)** | `85 %` | 20+ `.rpf` (jusqu'à `story000.rpf` / `story000_script.rpf`) et TTDB `.ttpf` ouverts ; file `Submit()` FIFO stricte, `zlib`, `PAGE_DESTINATION` (pages 64 KiB), **décodeur BCPack pur Go (BC1/BC3/BC4/BC5/BC6H/BC7)** et continuation multi-pages. | Exécution **asynchrone** multi-thread et cas limites streaming monde ouvert. |
| **OS Console / HLE (`xgameruntime`, `GameInput`)** | `88 %` | `xasync`/`XTaskQueue` (ports partagés composites), `xuser` (`AstralPlayer`), `xgamesave` 30 slots sur disque, `xstore`/`xgameui`/`xnetworking` hors-ligne, `OEM\ConsoleMode`, `GameInput` complet (DualSense PS5 / XInput / clavier). | APIs secondaires rencontrées en jeu. |
| **Audio (`AcpHal` / `xapu` / `chad`)** | `55 %` | Audio d'intro fonctionnel, allocation/mapping APU (`ApuMapVirtualAddress`), décodeur **Opus 48 kHz pur Go** (`xapu.dll`, sons UI), pacing 60 Hz (`timeBeginPeriod(1)`), musique d'écran de chargement (`go-mp3` + `waveOut`) avec bouton mute persisté. | Décodage **XMA2** (`AcpHal` SHAPE 350 contextes) pour les sons et dialogues en jeu. |
| **Recompilateur Shaders RDNA2 (`pkg/shader`)** | `32 %` | Extraction `DXBC->XDAT->XBCH->FSSM`, décodeur GFX10 partiel, recompilation live HLSL SM 5.0 suffisante pour l'UI 2D Scaleform et plusieurs Compute Shaders. | Masque `EXEC` par lane, descripteurs **bindless**, `LDS`/`FLAT`, émulation wave64, **DXIL SM 6.6**, Mesh/Amplification Shaders (indispensables pour la 3D). |
| **Pont GPU Hôte D3D12 (`pkg/gpu` + `d3d12x`)** | `45 %` | Fenêtre Win32 unique 1280×720 à lancement instantané, **vidéo d'intro 4K Bink2 (PSO BT.709 YUV420p→RGB)** 100 % fluide, `PresentX`, RenderTargets offscreen, détuilage RDNA2 aligné sur tuiles, splash synthwave + barre néon + bouton audio. | **G-Buffer / MRT**, **Depth-Stencil (`DSV`)**, passe composite gamma, tas de descripteurs bindless, `ExecuteIndirect`, synchronisation inter-files (`Wait64BitValueX`), rendu 3D complet. |

## Captures d'avancement

Les captures d'écran illustrant les jalons techniques sont stockées dans [`docs/screenshots/`](docs/screenshots/).

### Écran de chargement / préchargeur (splash D3D12 + barre néon temps réel)

![Écran de chargement GTA VI Astral](docs/screenshots/01-ecran-chargement-preloader.jpg)

*Écran de préchargement natif dans la fenêtre `gta-vi-astral` (icône synthwave dédiée, bouton audio mute/unmute persisté et barre de progression néon pilotée en temps réel par DirectStorage, `XUser` et `XGameSave`).*

### Profil `ASTRALPLAYER (A)` actif & sélectionnable à la manette

![Sélecteur de profil et AstralPlayer (A)](docs/screenshots/02-selecteur-profil-astralplayer.png)

*Profil hors-ligne `ASTRALPLAYER (A)` affiché en temps réel suite à la résolution complète de `XUSER_GAMERTAG_IID`, `XUSER_DEVICE` et de la vtable 30 slots `XGameSave`.*

### Vidéo d'introduction 4K Bink2 fonctionnelle (image + audio)

![Vidéo d'introduction GTA VI](docs/screenshots/03-video-intro.jpg)

*Cinématique d'introduction 4K Bink2 décodée et affichée en temps réel via la passe matérielle D3D12 YUV420p BT.709→RGB avec son audio synchronisé.*

## Pourquoi ASTRAL ?

**gta-vi-astral** synthétise en Go les meilleures idées architecturales de deux projets de référence :

- **De VIctory** : l'architecture ultra-modulaire par responsabilités (`pkg/*` inspiré de `crates/*`), le garde-fou anti-fuite d'assets, la préparation PE32+ en « DLL passive », la résolution d'imports en 4 priorités, le cache atomique adressé par contenu, la couche HLE hors-ligne (GDK, XMem, DirectStorage, Audio XMA2/Opus) et le backend D3D12 hôte (DXIL SM 6.6).
- **De Leonida** : l'analyse exhaustive des archives et shaders RDNA2 (26 503 shaders), le décodage et l'émulation **wave64** pour les GPU 32-wide (NVIDIA/Intel), l'overlay copy-on-write sur `GAME:`, les contrats ABI mesurés sur `d3d12_xs.dll` et le backend portable Vulkan + constructeur BVH4 XDXR.

## Architecture des packages

```text
gta-vi-astral/
├── cmd/astral/       # Lanceur graphique Windows natif (GUI) + interface CLI unifiée
├── pkg/
│   ├── core/         # Configuration TOML/env, garde-fou anti-fuite (.astral-root), logs slog
│   ├── pe/           # Analyseur PE32+ x86-64 (sections, entropie, imports/exports, TLS, .pdata, CodeView)
│   ├── formats/      # SGD5, RPF9 (en-tête), BCPack pur Go (BC1–BC7), layouts textures RDNA2 GFX10.3, AWC audio, JOAAT32
│   ├── shader/       # DXBC -> XDAT -> XBCH -> FSSM, décodeur RDNA2/GFX10, CFG/IR, cible DXIL ou SPIR-V
│   ├── pipeline/     # Cache atomique SHA-256 hors dépôt + orchestrateur parallèle par goroutines
│   ├── gpu/          # Contrats ABI Xbox D3D12.X + abstraction double backend (D3D12 / Vulkan)
│   ├── hle/          # Registre de shims console, HostPolicy, VirtualFS (overlay COW), XMem
│   ├── loader/       # Résolution d'imports (4 niveaux), recensement (census), préparation passive-DLL, crash log
│   └── game/         # Identité spécifique GTA VI Xbox Series 1.0.2.1 et lecture MicrosoftGame.Config
└── docs/             # Documentation technique complète en français
```

## Documentation

Toute la documentation technique en français est disponible dans [`docs/README.md`](docs/README.md).

---

## Vision du projet

### Présentation

**GTA VI Astral** (développé par Archivist-Atlas) est un projet indépendant de recherche et de développement logiciel consacré à l'étude des techniques de recompilation, de compatibilité multiplateforme et d'intégration de systèmes d'intelligence artificielle dans les environnements vidéoludiques.

L'objectif à long terme est d'explorer les possibilités offertes par la recompilation logicielle afin d'étudier l'intégration de fonctionnalités indépendantes qui ne sont pas nécessairement prévues dans l'environnement d'exécution d'origine.

GTA VI constitue une cible de recherche envisagée pour expérimenter ces technologies.

### Objectif principal : des personnages non joueurs pilotés par IA

L'une des ambitions principales du projet est d'étudier le développement d'un système indépendant d'intelligence artificielle destiné à enrichir les interactions entre les joueurs et les personnages non joueurs (PNJ).

Les systèmes traditionnels reposent souvent sur des dialogues prédéfinis, des arbres de conversation et des comportements scriptés. **GTA VI Astral** souhaite explorer des approches permettant de rendre ces interactions plus dynamiques, contextuelles et cohérentes.

Les axes de recherche envisagés comprennent :

- **Conversations dynamiques** : permettre aux PNJ de générer des réponses adaptées au contexte des échanges avec le joueur.
- **Mémoire persistante** : étudier la possibilité pour les PNJ de conserver des informations pertinentes issues de leurs interactions précédentes.
- **Personnalités individuelles** : développer des profils comportementaux distincts, avec des préférences, des habitudes et des styles de communication propres à chaque personnage.
- **Réactions contextuelles** : permettre aux personnages de prendre en compte les événements environnants et les situations auxquelles ils sont confrontés.
- **Comportements adaptatifs** : explorer des systèmes dans lesquels les réactions et les interactions évoluent en fonction des expériences simulées des personnages.
- **Interactions sociales** : étudier la possibilité de faire évoluer les relations entre les PNJ en fonction des événements et des informations qu'ils partagent.

L'objectif n'est pas simplement de remplacer les dialogues existants par un modèle conversationnel, mais d'étudier une architecture capable de combiner mémoire, personnalité, contexte et comportement.

### Recompilation et compatibilité PC

La recherche sur la recompilation constitue un axe technique du projet. Elle vise à étudier les mécanismes nécessaires à l'analyse de logiciels compilés, à la traduction de code machine et à la génération de code adapté à différents environnements d'exécution.

Les travaux envisagés comprennent notamment :

- L'analyse des structures des exécutables.
- L'étude des architectures processeur et des instructions machine.
- La création de représentations intermédiaires du code.
- L'exploration de techniques de traduction et de génération de code natif.
- L'étude des interfaces entre les logiciels et les systèmes d'exploitation.
- Le développement de composants indépendants de compatibilité et d'interopérabilité.

Cette recherche doit également permettre d'étudier les possibilités d'intégration d'un moteur d'intelligence artificielle indépendant avec un environnement logiciel compatible. La recompilation et l'intégration IA constituent deux axes techniques distincts, dont les interfaces et les contraintes devront être étudiées et documentées.

### Architecture d'intelligence artificielle envisagée

L'architecture envisagée repose sur plusieurs composants indépendants :

- **Moteur conversationnel** : génération de dialogues adaptés au contexte.
- **Système de mémoire** : conservation des informations pertinentes relatives aux interactions.
- **Gestion des personnalités** : définition des caractéristiques et comportements propres à chaque PNJ.
- **Moteur décisionnel** : interprétation des situations et sélection de réactions cohérentes.
- **Couche d'intégration** : communication entre le moteur IA et un environnement logiciel compatible.

L'exécution de ces composants pourra être étudiée localement sur PC ou au moyen de services externes, selon les contraintes de performances, de confidentialité et d'architecture.

### Indépendance du développement

**GTA VI Astral** (maintenu par Archivist-Atlas) développe ses propres outils et composants logiciels. Le projet distingue clairement les technologies développées indépendamment des logiciels tiers étudiés dans le cadre de ses recherches.

Il n'a pas pour objectif de distribuer le code source propriétaire, les exécutables, les ressources graphiques, les fichiers audio ou les autres contenus protégés appartenant aux éditeurs des logiciels concernés. Toute utilisation de fichiers tiers reste soumise aux droits et autorisations applicables.

### Cadre juridique

Les travaux de recherche sont conduits dans le respect des législations applicables en matière de propriété intellectuelle, de logiciels et de protection des mesures techniques.

Les références juridiques et les précisions relatives aux activités envisagées sont présentées dans le fichier [`LEGAL.md`](LEGAL.md).

Le présent projet ne revendique aucune autorisation générale de décompilation, de reproduction ou de modification de logiciels propriétaires.

### Indépendance et absence d'affiliation

**GTA VI Astral** est un projet indépendant, non officiel et sans affiliation avec Rockstar Games, Rockstar North ou Take-Two Interactive.

Les noms de produits et marques éventuellement mentionnés sont utilisés uniquement à des fins descriptives et restent la propriété de leurs détenteurs respectifs.
