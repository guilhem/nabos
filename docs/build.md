# Construire les images NabOS

Les deux cibles partent des images **officielles datées** de Raspberry Pi OS Lite Trixie. `image/sources.lock.json` fixe les URL, empreintes SHA-256 et révisions des pilotes, de U-Boot, de la bibliothèque LED et de Linux Voice Assistant. `genimage` assemble le disque ; RAUC signe le système préparé. Ni pi-gen, ni conteneur applicatif ne sont nécessaires sur le lapin.

| Cible | Hôte Ubuntu 24.04 | Système cible |
|---|---|---|
| `zero-armv6` | x86-64 + QEMU user (`arm1176`) | Raspberry Pi OS ARMv6, Rust `arm-unknown-linux-gnueabihf`, Go `GOARM=6` |
| `zero2-arm64` | ARM64 natif | Raspberry Pi OS ARM64, voix facultative |

## Commandes

Utiliser un hôte jetable avec `sudo` sans interaction, Go 1.27.1 et Rust 1.98.1, avec la cible Rust correspondante installée par `rustup target add`. Pour une compilation locale complète, installer aussi le compilateur U-Boot (`gcc-arm-linux-gnueabihf` pour ARMv6, `gcc-aarch64-linux-gnu` pour ARM64), `bc` et `python3-pyelftools` sur l'hôte. Les scripts sont les mêmes en CI et en local :

```sh
bash image/host-deps.sh
bash image/build.sh zero-armv6 dev-local --development
# Sur un hôte ARM64 :
bash image/build.sh zero2-arm64 dev-local --development
```

Les sorties sont dans `dist/<cible>/`. Le travail temporaire est dans `build/iot/`. La fabrication utilise un espace de noms de montage privé (`unshare`) pour isoler le chroot des services de l'hôte. Les images et partitions sont des fichiers creux ; les périphériques loop sont alloués au processus puis libérés, y compris en cas d'erreur. Le script affiche le répertoire de travail conservé pour diagnostic. `disk-usage-<cible>.txt` échantillonne l'espace disque pendant la fabrication.

`--development` crée un certificat éphémère valable sept jours. Cette image sert aux essais ; les futures releases officielles ne seront pas acceptées par cette chaîne de confiance. Ne pas diffuser ces images comme des releases utilisables en production.

Pour une release, fournir deux fichiers PEM, en conservant la même autorité de confiance pour les versions suivantes :

```sh
RAUC_KEY=/chemin/prive/key.pem RAUC_CERT=/chemin/cert.pem \
  bash image/build.sh zero-armv6 v2.0.0
```

La CI utilise les secrets GitHub `RAUC_SIGNING_KEY` et `RAUC_SIGNING_CERT` (contenus PEM). Les constructions hors tag n'ont pas accès à ces secrets.

## Créer une release

1. Dans GitHub Releases, créer la release avec un tag `vX.Y.Z` (ou un tag SemVer comme `vX.Y.Z-rc.1` pour une préversion) sur le commit voulu, son titre, son changelog et son statut (prérelease ou release stable). Le commit choisi doit contenir ce workflow. Une version portant un suffixe de préversion reste réservée au canal Test même si le statut GitHub est stable.
2. Publier la release : l'événement `release: published` lance les tests et la fabrication signée pour `zero-armv6` et `zero2-arm64`. Pousser seulement un tag ne lance plus la fabrication.
3. Après le succès des deux cibles, la CI ajoute les artefacts à cette release existante. Elle conserve le titre, le changelog, le statut et le choix de dernière version. Le fichier global `SHA256SUMS` est ajouté en dernier, après les images et bundles.

Pour une première qualification, choisir une **prérelease**, puis compléter la [fiche matérielle](release-checklist.md) avant de passer en stable. Une release publiée reste visible pendant la fabrication ; attendre la réussite du workflow et la présence de tous les artefacts avant de la diffuser. L'interface des appareils parcourt les releases publiées, les trie par version SemVer et propose celles plus récentes que le système installé. Le canal Stable exclut les préversions ; le canal Test les inclut. Les fichiers incomplets ne sont pas installables. Lorsque l'utilisateur active l'automatique, la version installable la plus élevée du canal choisi est installée pendant son créneau nocturne. Le choix GitHub « latest » ne remplace pas ces règles.

GitHub ne déclenche pas Actions lors de la création d'un **brouillon**. Pour le remplir avant publication, créer d'abord le tag Git sur le commit voulu, puis le brouillon associé, et lancer manuellement le workflow sur ce tag existant :

```sh
gh workflow run images.yml --repo guilhem/nabos --ref v0.1.0
```

Le brouillon reste un brouillon. Sa publication déclenche aussi le workflow. Une relance remplace les artefacts de même nom (`gh release upload --clobber`) ; elle ne modifie pas les informations de la release. L'ancien `SHA256SUMS` est retiré avant le remplacement des fichiers et rétabli seulement si tous les envois réussissent ; la recherche de mise à jour échoue tant que ce manifeste manque. Pour ajouter les artefacts après publication, les releases immuables doivent être désactivées dans les paramètres du dépôt.

## Sources et dépendances

Les workflows réutilisables `go.yml`, `rust.yml` et `uboot.yml` ont chacun leur matrice de plateformes `[zero-armv6, zero2-arm64]` : six jobs indépendants, en parallèle des tests. `actions/setup-go` gère les modules et objets Go avec son cache intégré ; `actions-rust-lang/setup-rust-toolchain` installe Rust et gère le cache Cargo et sysroot ; U-Boot utilise ccache. Les caches sont séparés par cible et chaîne de compilation. Le job d'image attend leurs succès, récupère les archives de la même exécution et vérifie leur cible, leur révision et la version du service avant installation.

Go et Rust sont cross-compilés sur x86-64. Rust utilise un petit sysroot dont les quatre paquets sont verrouillés par URL et SHA-256 dans `image/rust-sysroots.lock.json` : libc, fichiers de démarrage et libgcc. Les paquets ARMv6 viennent de Raspbian, jamais de Debian/Ubuntu ARMv7. U-Boot ARMv6 est cross-compilé sur x86-64 avec sa libgcc privée ; U-Boot ARM64 est construit sur un runner ARM64. Ses options A/B et watchdog ainsi que l'architecture de l'ELF sont vérifiées avant publication de l'artefact.

La fabrication sépare compilation, livraison et tests :

1. Une copie jetable de l'image Raspberry Pi reçoit les outils de compilation et les en-têtes du noyau. Elle compile les pilotes, le mixer, la bibliothèque LED et les wheels Python, puis est démontée et supprimée. Seuls les modules, overlays, exécutables, bibliothèque partagée et ressources nécessaires au fonctionnement sont conservés.
2. L'image à livrer repart de la base intacte. APT y installe uniquement les paquets nécessaires au fonctionnement depuis le dépôt local archivé par la compilation, avec les mêmes versions de bibliothèques. Les outils de compilation déjà présents dans l'image officielle sont retirés ; les dépendances de nos compilations restent dans la copie jetable. Les wheels sont installés hors ligne, sans compilation ni cache pip. Les versions des paquets communs et du noyau doivent correspondre à celles de l'environnement de compilation ; les en-têtes et le noyau doivent aussi provenir de la même version de paquet.
3. Après assemblage du disque SD, `image/test.sh` en crée une copie jetable. Les tests d'intégration utilisent les binaires et ressources de cette copie (QEMU ARM1176 pour ARMv6). Le sandbox U-Boot est compilé sur l'hôte et vérifie les scénarios A/B avec les fichiers de démarrage livrés. La copie est démontée et supprimée même si les tests échouent ; l'empreinte de l'original est contrôlée avant et après. La signature du bundle et la compression ne commencent qu'après leur succès.

Il n'y a pas de deuxième résolution de paquets sur Internet pour l'image livrée, ni de reconstruction après validation. Ces tests exécutent les services en simulation et le script de démarrage dans le sandbox ; ils ne démarrent pas un noyau Raspberry Pi complet. Les essais matériels restent nécessaires. La compression de l'image et des entrées utilise `xz -T0`, avec des horodatages séparés dans les logs.

Les bundles `.raucb` sont signés, au format RAUC `verity`, et utilisent SquashFS avec Zstd niveau 15. Ils contiennent `rootfs.ext4` pour la classe `rootfs`, puis `boot.vfat` pour la classe `bootloader` ; RAUC installe la racine inactive avant de basculer la copie FAT. La fabrication vérifie `CONFIG_SQUASHFS=y` ou `m` et `CONFIG_SQUASHFS_ZSTD=y` dans la configuration des en-têtes correspondant au noyau livré. Pour une mise à jour, le noyau déjà démarré sur le lapin doit aussi prendre en charge SquashFS/Zstd pour ouvrir le bundle ; le support dans le nouveau noyau seul ne suffit pas. Si un futur format de bundle ou une autre exigence du lecteur devient incompatible avec la version installée, publier d'abord une release de transition que l'ancien système peut lire.

Pour assembler des composants déjà construits, placer les trois fichiers `go-<cible>.tar`, `rust-<cible>.tar` et `uboot-<cible>.tar` dans un répertoire, puis passer `--components /chemin/composants` à `image/build.sh`. Rust et les compilateurs cross ne sont alors pas nécessaires au job d'image. Sans cette option, le script appelle les cibles `go`, `rust` et `uboot` du Makefile.

Le Makefile appelle directement `go build`, `cargo build` et le Makefile d'U-Boot, qui gèrent leurs compilations incrémentales. Il prépare aussi les entrées verrouillées et vérifie la compatibilité ARMv6. Les mêmes cibles servent en CI et en local :

```sh
make go TARGET=zero-armv6 VERSION=dev-local
make rust TARGET=zero-armv6
make uboot TARGET=zero-armv6
# Ajouter l'archive pour le job d'assemblage :
make package-go TARGET=zero-armv6 VERSION=dev-local
```

Les sorties sont dans `build/<composant>/<cible>/` et les archives dans `build/components/`. Les cibles `package-rust` et `package-uboot` suivent la même convention. `OUT=/chemin/sortie` change le répertoire de sortie ; `INPUTS=/chemin/entrees-archivees` active le replay sans téléchargement des dépendances. La compilation Go utilise `CGO_ENABLED=0`, `GOOS=linux` et `GOARCH=arm GOARM=6` ou `GOARCH=arm64`.

Chaque image archive les `.deb` ajoutés/remplacés avec SHA-256 et inventaire. Les sources des pilotes, les dépendances Cargo/Go, le sysroot Rust, les paquets du compilateur U-Boot et les wheels Python ARM64 sont aussi archivés. Les verrous Cargo, Go et sysroot sont vérifiés lors d'une reconstruction. Les caches de téléchargement restent une optimisation : une disparition des anciens paquets des miroirs exige de mettre à jour le verrou ou de fournir les entrées archivées. Le répertoire APT `inputs/debs` n'est pas mis en cache : son manifeste active le mode replay et figerait la résolution des paquets.

Les dépendances de Linux Voice Assistant et son backend de build sont verrouillés par URL de wheel et SHA-256 dans `image/lva-requirements.lock` (CPython 3.13 ARM64). Le paquet LVA est construit sans résolution supplémentaire ; l'installation dans l'image est ensuite faite hors ligne.

```sh
bash image/build.sh zero-armv6 v2.0.0 --development \
  --replay /chemin/build-inputs-zero-armv6.tar.xz
```

Le code et les fichiers de verrouillage doivent correspondre à cette release. Utiliser la même architecture d'hôte que la CI de la cible pour exécuter le compilateur U-Boot archivé : x86-64 pour ARMv6, ARM64 pour ARM64. L'image Raspberry Pi officielle est retéléchargée et vérifiée. Les compilations des composants et les opérations APT/pip dans le chroot utilisent alors les dépendances archivées, sans résolution sur un dépôt vivant. Les utilitaires de l'hôte restent ceux d'Ubuntu 24.04. Cela reproduit les entrées logicielles ; les horodatages, signatures et identifiants de systèmes de fichiers ne sont pas déclarés reproductibles bit à bit.

## Partitionnement et démarrage

| Zone | Contenu au premier flash |
|---|---|
| 1 Mio et 2 Mio, hors partitions | Deux copies identiques de l'environnement U-Boot, 64 Kio chacune |
| 4–516 Mio | Deux copies FAT de 256 Mio, initialement identiques : 4–260 Mio et 260–516 Mio |
| Partition 1 | Entrée MBR pointant vers la copie FAT à 4 Mio ou à 260 Mio ; initialement 4 Mio |
| Partition 2, à partir de 516 Mio | Racine A ext4, 6 Gio, préremplie |
| Partition 3, à partir de 6660 Mio | Racine B ext4, 6 Gio, vide jusqu'à sa première installation RAUC |
| Partition 4, à partir de 12804 Mio | Données ext4, 1 Gio puis agrandies au premier démarrage |

RAUC utilise nativement [`boot-mbr-switch`](https://rauc.readthedocs.io/en/v1.11.3/advanced.html#update-bootloader-partition-in-mbr) sur la région 4–516 Mio : il écrit la copie FAT inactive, puis change l'entrée MBR de la partition 1. Le handler de fin d'installation exécute `sync /dev/mmcblk0` ; une erreur fait échouer l'installation. Les deux copies contiennent le firmware Raspberry Pi, U-Boot, sa configuration et les DTB nécessaires avant Linux ; elles peuvent être mises à jour par bundle. U-Boot lit ensuite le noyau et le Device Tree dans la racine A ou B choisie. Les modules et overlays Linux restent dans cette même racine. À la fabrication, ce Device Tree reçoit le profil `image/nabos-overlay.dts`, qui désactive Bluetooth (UART0), VCHIQ, framebuffer, USB et régulateurs caméra ; la copie FAT garde le DTB d'origine pour U-Boot. La partition de démarrage n'est jamais montée sous Linux.

L'état RAUC est conservé dans `/data`. Un nouveau slot racine n'est confirmé qu'après le contrôle local des services essentiels ; un échec de démarrage consomme une tentative puis ramène au dernier slot valide. Ce contrôle ne valide pas le firmware partagé : une copie FAT défectueuse peut empêcher le démarrage des deux racines et nécessiter une réparation de la carte SD. Avant chaque future release, tester les combinaisons ancien/nouveau démarrage × ancienne/nouvelle racine, car une coupure peut survenir avant ou après le changement de l'entrée MBR. Ce plan concerne le premier flash et les mises à jour de ce format ; aucune migration d'un ancien partitionnement n'est prévue. Changer le partitionnement nécessite un nouveau flash.

Le système racine est monté en lecture seule ; identité, connexion réseau, réglages et calibration restent sur `/data`. Journaux et fichiers temporaires sont volatils. Aucun serveur SQL n'est installé : la configuration applicative est un fichier JSON versionné écrit atomiquement. N'introduire une migration de schéma que lorsqu'elle est nécessaire, et vérifier alors la lecture des données par la version précédente après rollback.

SSH utilise le service OpenSSH fourni par Raspberry Pi OS, conditionné par un fichier `/data/nabos/ssh/authorized_keys` non vide et des données persistantes disponibles. L'interface authentifiée valide les clés avec `ssh-keygen`, écrit ce fichier atomiquement et demande uniquement `start` ou `stop` sur `ssh.service` via Polkit. Les clés hôtes sont créées dans `/data/system/ssh/etc/ssh` au premier démarrage du service ; la configuration OpenSSH reste dans le slot pour recevoir les mises à jour. Le compte `nabos` a un shell, conserve son mot de passe verrouillé et dispose de sudo sans mot de passe. Gérer ses clés permet donc d'accorder un accès administrateur au système. Une ancienne version sans cette fonction ferme SSH en cas de rollback ; les clés restent sur `/data` pour le retour à une version compatible.

## Validation locale

Les outils propres au dépôt sont regroupés dans `services/cmd/nab-image`.
`image/build.sh` compile cet utilitaire Go sur l'hôte pour lire les verrous,
télécharger et vérifier les entrées, puis extraire les archives. Les composants
externes et leurs dépendances Python restent inchangés.

```sh
cargo test --locked --manifest-path core/Cargo.toml
(cd services && go test -race ./...)
(cd services && NABOS_INTEGRATION=1 go test -count=1 ./tests/integration)
```

Le test d'intégration requiert Mosquitto et ses clients. Les tests de simulation ne remplacent pas les essais des pilotes, de l'audio, de l'alimentation et du rollback sur de vrais appareils.

Pour vérifier les sources verrouillées et les pilotes séparément :

```sh
(cd services && go build -o ../build/nab-image ./cmd/nab-image)
build/nab-image fetch image/sources.lock.json zero-armv6 build/inputs --sources-only
build/nab-image drivers image/sources.lock.json --archives build/inputs
# Ajouter --kernel CHEMIN_DES_ENTETES pour vérifier aussi les modules.
```

Les tests d'image sont inclus dans `go test ./...`. Pour exécuter aussi le script
de démarrage dans le sandbox U-Boot, fournir `NABOS_UBOOT_SANDBOX`,
`NABOS_SOURCES` et `NABOS_VENDOR_DTBS` ; la fabrication des images le fait
automatiquement.

La CI exécute aussi `TestRaucBootMBRIntegration` avec
`NABOS_RAUC_INTEGRATION=1`, sous root dans un espace de montage privé : RAUC
installe de vrais bundles signés sur un périphérique loop jetable, avec les
environnements U-Boot et le partitionnement du dépôt. Ce test couvre les bascules
dans les deux sens et les refus d'installation ; il ne simule pas les propriétés
électriques d'une carte SD ni le démarrage du firmware Raspberry Pi.
