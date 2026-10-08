# Construire les images NabOS

**NixOS est l’unique constructeur d’images NabOS.** `flake.lock` fixe les
entrées Nixpkgs et nixos-hardware ; `nix/` décrit les paquets, le système et son
payload. Aucun constructeur Raspberry Pi OS ni rpi-image-gen n’est maintenu.
Le passage d’une ancienne image à NixOS demande un **nouveau flash SD** :
sauvegarder les données avant de reflasher. Les identifiants RAUC
`nabos-nixos-zero-armv6` et `nabos-nixos-zero2-arm64` empêchent les anciennes
images d’accepter ces bundles. Aucune migration OTA n’est qualifiée.

## Construire et vérifier

Installer Nix avec les fonctions `nix-command flakes` sur un hôte Linux jetable.
Prévoir l’espace pour le store, les racines ext4 de 6 Gio, le disque SD, les
copies de test et la compression ; aucun minimum d’espace constructeur n’est
attesté sans mesure. Le dev shell fournit les outils d’assemblage et de test.

| Cible | Hôte constructeur | Appareil |
|---|---|---|
| `zero-armv6` | x86-64 Linux, compilation croisée ARMv6 | Raspberry Pi Zero W, ARM1176 |
| `zero2-arm64` | x86-64 Linux, compilation croisée ARM64 | Raspberry Pi Zero 2 W, voix facultative |

Les mêmes commandes servent en local et en CI :

```sh
make image TARGET=zero-armv6 VERSION=dev-local DEVELOPMENT=1
# Équivalent :
bash image/build.sh zero-armv6 dev-local --development
# Sur le même hôte x86-64 Linux :
bash image/build.sh zero2-arm64 dev-local --development
```

Nix réalise le système et U-Boot ; l’assembleur signe **hors du store Nix**,
amorce la partition de données, assemble le disque et le compresse. Les sorties
sont dans `dist/<cible>/`. Les révisions externes, dont device-core, les pilotes,
U-Boot et LVA, restent verrouillées par les expressions Nix et
`image/sources.lock.json`. Les verrous Cargo et Go fixent leurs dépendances.
`lib.mkImage` est la fonction d’évaluation commune ; le verrou livré avec chaque
image fait autorité pour ses entrées. Ces verrous ne promettent pas une image
ou une signature reproductible bit à bit.

| Fichier dans `dist/<cible>/` | Usage |
|---|---|
| `nabos-<cible>.img.xz` | Premier flash SD |
| `nabos-<cible>.raucb` | Bundle RAUC signé |
| `ca-<cible>.cert.pem` | Certificat public utilisé pour la signature |
| `flake-<cible>.lock` | Entrées Nix verrouillées |
| `boot-<cible>.cmd` | Script de démarrage livré |
| `cache-roots-<cible>` | Racines store du système, d’U-Boot et des outils de compilation sélectionnés |
| `build-<cible>.json` | Cible, version, révisions et durées de fabrication |
| `SHA256SUMS-<cible>` | Empreintes des fichiers livrés |

```sh
(cd dist/zero-armv6 && sha256sum -c SHA256SUMS-zero-armv6)
EXPECTED_VERSION=dev-local EXPECTED_DEVELOPMENT=true \
  bash image/test-artifact.sh zero-armv6 dist/zero-armv6
```

La construction locale lance `image/test.sh` avant compression. Son interface
prend la cible, le disque, la racine, le démarrage, le bundle et le certificat :

```sh
# WORK est le répertoire de travail affiché par le constructeur.
bash image/test.sh zero-armv6 \
  WORK/images/sdcard.img WORK/images/rootfs.ext4 WORK/images/boot.vfat \
  dist/zero-armv6/nabos-zero-armv6.raucb \
  dist/zero-armv6/ca-zero-armv6.cert.pem
```

Ces contrôles vérifient le partitionnement, le contenu racine/démarrage,
l’initrd, les propriétaires, les empreintes du bundle et sa signature.
`image/test-artifact.sh TARGET dist/<cible>` vérifie les fichiers exactement
téléchargés et exerce des copies jetables, sans modifier les originaux.
Il exige un checkout propre correspondant au commit livré, la version attendue
et le flag de développement. Pour une sortie de production, fournir aussi
`EXPECTED_RAUC_CERT` avec le chemin du certificat public attendu.
`--defer-tests` reporte les tests à cette étape explicite ; ce flag ne valide
rien. Les inspections d’artefacts ne démarrent ni un noyau Raspberry Pi ni les
services sous leurs restrictions réelles sur l’appareil.

## Signature et confiance

La clé publique Cachix permet au **constructeur** de vérifier les binaires Nix.
La confiance **RAUC sur le lapin** repose sur `/data/rauc/ca.cert.pem`, amorcé
dans la partition de données au premier flash. Les mises à jour conservent
cette partition et son autorité de confiance.

`--development` crée une autorité temporaire valable sept jours. Une image
ainsi flashée rejette les bundles signés par une autre autorité, dont les
releases officielles. Pour une chaîne durable, conserver l’autorité RAUC et
fournir deux fichiers PEM :

```sh
RAUC_KEY=/chemin/prive/key.pem RAUC_CERT=/chemin/public/cert.pem \
  bash image/build.sh zero-armv6 v2.0.0
```

La clé privée ne doit jamais entrer dans une dérivation Nix, un cache public
ou un artefact. Les releases utilisent les secrets GitHub existants
`RAUC_SIGNING_KEY` et `RAUC_SIGNING_CERT` (contenus PEM) ; les constructions de
PR utilisent une signature de développement. Les pushes de `main` utilisent
également cette signature jusqu’à l’activation du canal Edge ; ensuite ils
utilisent l’autorité officielle.

## Créer une release

Le workflow principal `images.yml` construit automatiquement **les deux cibles**
sur PR, push et publication de release. Les deux cibles sont compilées sur
x86-64, comme les constructions locales qui alimentent Cachix. Les dérivations
croisées diffèrent des dérivations natives ; utiliser le même hôte constructeur
pour réutiliser le cache. Les tests d’artefacts téléchargent les sorties
de chaque construction. La publication attend le **succès des constructions et
des tests d’artefacts des deux cibles**, puis livre ces mêmes fichiers.

FFTW conserve son API C pour l’audio, sans interfaces ni documentation Fortran.
LVA réutilise la bibliothèque Fortran embarquée dans son wheel NumPy verrouillé.
Ces choix évitent de compiler des toolchains Fortran pour les images.
LVA charge directement `libmpv`, sans le wrapper mpv et ses dépendances yt-dlp/Deno.
Ses bindings Python utilisent les chemins Nix de MPV et PulseAudio, sans Binutils au runtime.

1. Créer une release GitHub sur le commit voulu, avec un tag `vX.Y.Z`, un titre
   et un changelog ; commencer par une prérelease comme `vX.Y.Z-rc.1`.
2. Publier la release pour déclencher la fabrication signée avec l’autorité
   RAUC officielle.
3. Attendre le succès du workflow et tous les fichiers et manifestes
   `SHA256SUMS-<cible>` avant diffusion.
4. Consigner les essais de la [fiche de qualification](release-checklist.md)
   sur les deux appareils avant le passage en stable.

La release peut être visible pendant sa fabrication. Une CI réussie ne vaut
pas qualification matérielle. Le canal Stable exclut les préversions ; le
canal Test inclut les préversions habituelles et exclut Edge. Le choix GitHub « latest » ne remplace pas la sélection
SemVer de l’appareil. L’acceptation des assets par device-core doit aussi être
vérifiée sur la nouvelle image.

### Canal Edge

Edge propose uniquement les builds automatiques de `main`. Le tag brut
`edge-X.Y.Z.RUN` reste la version de GitHub, de l’image, du manifeste RAUC et
du journal de reprise. Le comparateur le traite comme `X.Y.Z-edge.RUN`.
`X.Y.Z` est le plus grand triplet des releases publiées hors Edge, préversions
comprises, avec son patch incrémenté ; `RUN` est le numéro du workflow Images.
Un changement de canal attend une version strictement supérieure : il ne
réinstalle pas une ancienne version pour revenir vers Stable ou Test.

Avant d’activer la publication automatique :

1. Fusionner les PR des sources device-core et du mixeur, puis NabOS avec leurs
   commits épinglés ; qualifier le client compatible dans une release Stable
   ou Test signée officiellement, publiée volontairement.
2. Installer cette version de base sur les appareils de test et vérifier les
   trois canaux ainsi que la transition N → N+1 → rollback N avec les données
   conservées. Les anciennes images Raspberry Pi OS exigent un reflash NixOS.
3. Définir la variable de dépôt `NABOS_EDGE_ENABLED=true`.

Chaque push de `main` termine sa propre construction, sans annulation par un
push suivant. Après réussite de tous les tests et des deux images, le job Edge
crée une release brouillon, y charge les octets testés, puis la publie comme
préversion. Les événements de release `edge-*` ne relancent pas le constructeur.
Une publication interrompue conserve son brouillon : relancer les jobs échoués
du même run réutilise les artefacts précédemment testés. Un asset déjà publié
est immuable ; une reconstruction différente ne remplace pas ses octets.
La rétention supprime uniquement les Edge publiées au-delà des 30 versions les
plus élevées. Stable et Test conservent leur procédure de publication volontaire.

Pour arrêter Edge, retirer la variable puis annuler les jobs de publication
déjà autorisés : la variable ne révoque pas un job en cours.

## Lire et alimenter Cachix

La lecture de `https://nabos.cachix.org` est publique et anonyme. Configurer Nix
sur l’hôte constructeur avec un utilisateur autorisé à définir les substituters :

```ini
extra-substituters = https://nabos.cachix.org
extra-trusted-public-keys = nabos.cachix.org-1:jLoce+DvPr6ejhFfvmEKXznQLVKxZ6zCP5N7dirR/JQ=
```

Publier uniquement les racines listées dans `cache-roots-<cible>` : le système,
les sorties d’exécution demandées pour assembler son profil, U-Boot,
les en-têtes préparés du noyau, les sorties natives `gtk4.dev` et
`gobject-introspection.dev` et le compilateur Go, avec leurs dépendances.
Ces outils et certaines sorties masquées par les collisions du profil sont
nécessaires à la reconstruction sur un hôte neuf mais absents de la closure
d’exécution. Cachix ignore les objets déjà présents dans le cache officiel.
Le payload d’assemblage, les
images, bundles, certificats et dev shells ne sont pas des racines de
publication ; les images et releases sont conservées hors Cachix.
La publication globale du store et `watch-store` ne sont pas utilisés.
La liste est préparée avant la construction. Même si le packaging échoue,
la CI publie les racines effectivement réalisées. Un échec Cachix est signalé
et les images et rapports disponibles restent téléchargeables ; les benchmarks
continuent d’exiger la disponibilité complète de leur closure.

La version de l’image est inscrite dans NabOS et ses métadonnées. Les assets et
`nab-hardware` gardent une identité indépendante de cette version ; Nix invalide
leur cache quand leurs sources ou dépendances changent. Changer uniquement la
version de l’image ne recompile donc pas les deux services Rust.
Le profil système sélectionne explicitement la sortie d’exécution de chaque
paquet : les sorties de manuels non installées ne déclenchent pas de compilation.

Le job `device-integration` lit également ce cache pour les trois paquets natifs
`device-core-native`, `nab-hardware-native` et `nabos-native`. Après réussite des
tests, il publie leurs racines sur `main`, lors d’une release, d’un lancement
manuel sur `main` ou d’une PR issue d’une branche de ce dépôt, et vérifie leur
disponibilité publique. Les builds d’images publient également leurs racines
sur les PR internes. Les deux jobs de tests ciblent `core/target` dans leur cache
Rust ; Go conserve son cache de modules et de compilation. Les fixtures
d’intégration sont exécutées à chaque run, même lorsque les binaires sont cachés.

`CACHIX_AUTH_TOKEN` est un secret de publication CI, transmis uniquement à
l’étape autorisée à écrire dans le cache `nabos`. Sa valeur ne doit jamais
apparaître dans le dépôt, les documents ou les journaux. Les PR internes peuvent
publier avec ce secret ; les PR de forks et celles ouvertes par Dependabot
conservent la lecture anonyme. Le jeton est retiré de l’environnement avant les
vérifications du cache qui appellent Nix. Vérifier les dépendances effectivement
disponibles dans les caches publics après publication.

## Mesurer le cache et les constructions

Les outils canoniques sont `image/benchmark.sh` et `image/cache-report.py`.
Les résultats sont conservés sous `dist/measurements/`, séparés des images.
Les phases mesurent **les racines du cache sélectionnées par le constructeur**,
outils de compilation compris, sans réassembler le SD :

| Phase | Conditions |
|---|---|
| `cold` | Hôte neuf ; cache NabOS désactivé, cache officiel NixOS autorisé |
| `warm` | Autre hôte neuf ; mêmes commit/version/pin, après publication vérifiée de `cold` |
| `version` | Hôte neuf ; même pin, version applicative suffixée `.next` |
| `nixpkgs` | Hôte neuf ; version initiale, nouveau pin Nixpkgs explicite |

```sh
bash image/benchmark.sh cold zero-armv6 dev-local
# Sur un AUTRE hôte neuf, après publication des racines cold :
bash image/benchmark.sh warm zero-armv6 dev-local
bash image/benchmark.sh version zero-armv6 dev-local.next
```

Répéter sur x86-64 pour `zero2-arm64`. Le nom de phase n’isole pas un store
déjà rempli : conserver des hôtes neufs pour comparer. Pour `nixpkgs`, modifier
explicitement le pin dans un checkout de mesure et conserver le verrou utilisé.
Sans nouveau pin, aucune mesure de changement Nixpkgs n’est possible.
`.next` mesure un changement de version, pas une modification de logique métier.

Chaque phase conserve le manifeste `build-<cible>.json`, le verrou, les sorties
Nix et les journaux. Son `build_seconds` mesure les racines sélectionnées ; celui d’une
image complète mesure le **payload Nix**, génération ext4/FAT comprise.
Le rapport des images distingue également `durations_seconds.nix_build`,
`sd_assembly`, `rauc_bundle`, `xz` et `tests`. Une valeur `tests: null` signifie
que les tests sont différés. Le job de test conserve son propre rapport
`test-<cible>.json`, sans modifier les fichiers testés ; la publication Cachix
conserve `cache-timings-<cible>.json`. Relever aussi le pic disque des jobs.

Les images utilisent XZ 3 et Zstd RAUC 6. La comparaison locale du 8 octobre
2026, avec huit threads sur un i7-1265U et les mêmes octets d'entrée par cible,
a donné les résultats suivants :

| Cible | XZ 6 → 3 | Zstd 15 → 6 | Taille image / bundle supplémentaire |
| --- | --- | --- | --- |
| ARMv6 | 140 → 74 s | 33 → 19 s | +5,8 % / +4,9 % |
| ARM64 | 131 → 81 s | 41 → 19 s | +5,9 % / +4,8 % |

Les hashes des images décompressées et des payloads extraits des bundles
correspondent aux sources ; les signatures sont vérifiées. Ces mesures sur un
hôte partagé ne prédisent pas la durée CI. Le format XZ et les bundles verity
restent identiques ; les tests complets des artefacts et la limite de 2 Gio
restent obligatoires.

```sh
python3 image/cache-report.py --self-test
python3 image/cache-report.py dist/zero-armv6/cache-roots-zero-armv6 \
  --output dist/measurements/cache-before-zero-armv6.json
# Après publication autorisée des racines :
python3 image/cache-report.py dist/zero-armv6/cache-roots-zero-armv6 \
  --require-published --output dist/measurements/cache-after-zero-armv6.json
```

Le rapport interroge la closure locale et les métadonnées publiques `.narinfo` :

- `closure_nar_bytes` additionne les tailles NAR des chemins store distincts ;
  ce n’est ni la taille ext4 ni une taille compressée.
- `upstream_missing_path_count` compte les chemins absents du cache officiel.
- `cachix_compressed_bytes` additionne les `FileSize` publiés, dédupliqués par
  URL NAR. `unpublished_paths` et `publication_complete` signalent les manques ;
  aucune taille absente n’est extrapolée depuis `narSize`.

Fusionner les rapports **après publication** pour mesurer les deux cibles ou
plusieurs versions, plutôt qu’additionner leurs totaux :

```sh
python3 image/cache-report.py --merge \
  dist/measurements/cache-after-zero-armv6.json \
  dist/measurements/cache-after-zero2-arm64.json \
  --output dist/measurements/cache-union.json
```

La fusion réinterroge les caches et détecte les objets évincés ou supprimés
avec `previously_cached_missing_paths`. Seul HTTP 404 représente une absence ;
une erreur réseau fait échouer la mesure. Cette union décrit les fichiers
référencés à cet instant, pas l’ensemble du compte Cachix ni sa facturation.

## Maintenance des pins

Les merges applicatifs conservent `flake.lock`. Le workflow `nix-update.yml`
propose chaque mois une PR brouillon actualisant ensemble Nixpkgs et
nixos-hardware ; son lancement manuel permet d’avancer un correctif de sécurité.
Les révisions actuellement verrouillées restent inchangées dans cette
optimisation. La branche `codex/nix-inputs-update` appartient à ce workflow.

La création de PR exige le réglage GitHub autorisant GitHub Actions à créer des
pull requests. La CI Images est également déclenchée explicitement sur cette
branche ; approuver les workflows de PR ordinaires s’ils attendent une
autorisation GitHub, et exiger leurs checks avant la review ou le merge.
Les builds compilent et testent les deux cibles, publient leurs racines Cachix
et comparent leur union avec les derniers rapports réussis de `main`.
Ces rapports sont conservés 45 jours. Si les rapports de référence ont expiré,
la comparaison échoue explicitement ; reconstruire la référence avant d’accepter
la mise à jour. Relever le volume compressé et les évictions de la closure
courante avec le candidat avant d’accepter cette PR.
La rétention de versions anciennes augmente l’occupation. Ajouter des
toolchains au cache seulement si les journaux montrent des recompilations
coûteuses, puis mesurer leur coût compressé.

**Les mesures cold/warm/version/nixpkgs ne sont pas encore complètes.** Les
timings locaux et les CI avec cache ne suffisent pas à établir la rétention de
plusieurs générations dans une enveloppe de 5 Go.

## Partitionnement, persistance et responsabilités

| Zone | Contenu au premier flash |
|---|---|
| 1 et 2 Mio, hors partitions | Environnements U-Boot redondants de 64 Kio |
| 4–516 Mio | Deux copies FAT de 256 Mio, initialement identiques |
| Partition 1 | Entrée MBR vers la copie FAT à 4 ou 260 Mio |
| Partition 2, à 516 Mio | Racine A ext4 de 6 Gio, préremplie |
| Partition 3, à 6660 Mio | Racine B ext4 de 6 Gio, pour installation RAUC |
| Partition 4, à 12804 Mio | `/data`, 1 Gio puis agrandie au premier démarrage |

Le bundle RAUC `verity`, SquashFS/Zstd, contient `rootfs.ext4` puis `boot.vfat`.
RAUC écrit la racine inactive, puis `boot-mbr-switch` écrit la copie FAT inactive
et bascule l’entrée MBR. Le lecteur déjà installé doit savoir ouvrir le prochain
bundle ; le support dans le nouveau noyau seul ne suffit pas. Un changement de
partitionnement nécessite un nouveau flash.

U-Boot charge le noyau, l’initrd et le DTB depuis le slot racine sélectionné ;
les modules et overlays correspondent à ce noyau. Le démarrage FAT n’est pas
monté sous Linux. Le contrôle de santé confirme le slot seulement avec les
unités essentielles actives, les propriétés Ready D-Bus de device-core et
nab-hardware, la santé de nabos et les périphériques audio. Il ne garantit pas
la récupération d’un firmware FAT défectueux avant Linux : prévoir la
réparation SD et qualifier les combinaisons ancien/nouveau démarrage avec
ancienne/nouvelle racine.

`nix/system.nix` et `nix/runtime/initrd-persist.sh` déclarent la racine en lecture
seule, `/etc` immuable, `/var` volatile et les montages persistants avant les
services. Les unités et politiques applicatives encore utilisées dans
`image/rootfs/` sont des sources déclaratives partagées, adaptées aux chemins
Nix ; leur présence ne constitue pas un deuxième constructeur.

| Propriétaire | État et permissions |
|---|---|
| `nab-hardware` | GPIO, LED sysfs, I²C CR14/ST25R391x ; aucune capability ni accès aux données applicatives |
| `device-core` | Services Linux, réseau, audio, SSH, voix, mises à jour ; `/data/device-core/settings.json`, sans capability matérielle |
| `nabos`, compte `nab-app` | États, médias, chorégraphies, interface ; `/data/nabos/application.json`, HTTP avec `CAP_NET_BIND_SERVICE` |
| `nab-audio` | PipeWire/WirePlumber et LVA ARM64, sans permissions système |
| `nabos`, compte SSH | Administration par clé, sudo sans mot de passe |

Les profils réseau, l’horloge, le mixer et `/var/lib/nabos` sont liés à
`/data/system`. Le home, les préférences et caches LVA restent sous
`/var/lib/nabos/lva` et le home SSH est `/var/lib/nabos/admin` ; leur parent
appartient à root, chaque home à son compte. Les clés SSH autorisées sont sous `/data/device-core/ssh`,
les clés hôtes sous `/data/system/ssh/etc/ssh`. Le secours `/data/.volatile`
ne garantit aucune persistance. Les temporaires des services restent sous
`/run` avec `RuntimeDirectory` et `WorkingDirectory` adaptés.

D-Bus et Polkit séparent les propriétaires et réservent les opérations système
à device-core ; RAUC reste accessible à root pour le contrôle de santé.
PipeWire est partagé via son socket audio ; Mosquitto sert uniquement aux
fixtures Home Assistant, aucun broker n’est livré. Ne pas rendre la racine
inscriptible ni retirer le confinement pour résoudre un chemin erroné :
`ReadWritePaths` ne rend pas inscriptible un système de fichiers monté en lecture
seule. Vérifier aussi HOME, XDG, caches et écritures des sous-processus.

## Tests locaux et qualification

Les sorties flake natives `device-core-native`, `nab-hardware-native`,
`nabos-native` et `uboot-sandbox` servent aux tests sur l’hôte. Elles sont
distinctes des exécutables ARM livrés ; les simulations utilisent un bus D-Bus
privé et Mosquitto uniquement pour Home Assistant. Le sandbox U-Boot exécute le
script A/B sans démarrer le firmware ARM ou le noyau Raspberry Pi.

Le check `.#checks.x86_64-linux.audio-boot` démarre le module audio partagé avec
systemd PID 1 dans une VM x86-64 : racine et `/etc` en lecture seule, `/var`
volatile, linger déclaratif et aucune connexion utilisateur. Il attend les
trois processus et sockets audio, puis vérifie leurs droits et
`NoNewPrivileges`. Ce test complète les contrôles des unités composées dans
chaque image ; il ne qualifie pas les pilotes ni le démarrage des Raspberry Pi.

```sh
nix develop
mkdir -p build
nix build .#device-core-native --out-link build/device-core-native
nix build .#nab-hardware-native --out-link build/nab-hardware-native
nix build .#nabos-native --out-link build/nabos-native
nix build .#uboot-sandbox --out-link build/uboot-sandbox
```

Utiliser les exécutables natifs pour `DEVICE_CORE_BIN`, `NABOS_HARDWARE_BIN`,
`NABOS_BIN` et `NABOS_UBOOT_SANDBOX` selon le test. Le scénario d’intégration
complet avec trois UID et les installations RAUC sur loop demandent root,
des namespaces privés et un hôte jetable ; les inspections d’image seules ne
prouvent pas l’exécution de ces scénarios.

La [fiche de release](release-checklist.md) couvre le premier démarrage avec
`/data` vierge, la persistance, Wi-Fi, SSH, audio, LED, oreilles, les deux lecteurs,
LVA ARM64, la mémoire disponible, RAUC A/B, les coupures et le rollback.
Les [essais du 30 septembre](qualification-zero2-2026-09-30.md) concernent une
ancienne image Raspberry Pi OS : **ils ne qualifient pas NixOS**.

### Qualification des oreilles userspace

Sur les deux appareils, avec l’image réelle, la racine en lecture seule et les
restrictions systemd effectives : exercer les 17 positions dans les deux sens,
chaque oreille puis les deux, au repos et sous charge ; vérifier calibration
et temporisations. Interrompre par `SIGKILL`, `SIGABRT`, `SIGSTOP` et pendant
la calibration ; vérifier watchdog, helper d’arrêt et redémarrage avec
`initializing` et `Ready=false`. Tester aussi `SIGTERM` pendant une écriture NFC
admise. Mesurer l’état électrique des sorties moteurs après fermeture des FD
puis après le helper : leur succès logiciel ne prouve pas l’arrêt physique.

Pour chaque preuve, préciser commit, cible, runner ou carte, protocole et
rapports. Distinguer inspection des sources, tests locaux, CI et exécution
matérielle. Recueillir les journaux volatils avant arrêt ou préparer leur
collecte sur `/data`. Aucune qualification matérielle NixOS n’est attestée ici.
