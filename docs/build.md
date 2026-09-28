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

La CI utilise les secrets GitHub `RAUC_SIGNING_KEY` et `RAUC_SIGNING_CERT` (contenus PEM). Les constructions hors tag n'ont pas accès à ces secrets. Sur un tag `v*`, les deux jobs doivent réussir avant la création du brouillon GitHub Release avec `gh release`. La [qualification matérielle](release-checklist.md) précède sa publication. Les nouvelles versions ne sont installées qu'à la demande dans l'interface locale.

## Sources et dépendances

Le workflow réutilisable `components.yml` croise les projets `[go, rust, uboot]` et les plateformes `[zero-armv6, zero2-arm64]` : six jobs indépendants, en parallèle des tests. `actions/setup-go` gère les modules et objets Go avec son cache intégré ; `actions-rust-lang/setup-rust-toolchain` installe Rust et gère le cache Cargo et sysroot ; U-Boot utilise ccache. Les caches sont séparés par cible et chaîne de compilation. Le job d'image attend leurs succès, récupère les archives de la même exécution et vérifie leur cible, leur révision et la version du service avant installation.

Go et Rust sont cross-compilés sur x86-64. Rust utilise un petit sysroot dont les quatre paquets sont verrouillés par URL et SHA-256 dans `image/rust-sysroots.lock.json` : libc, fichiers de démarrage et libgcc. Les paquets ARMv6 viennent de Raspbian, jamais de Debian/Ubuntu ARMv7. U-Boot ARMv6 est cross-compilé sur x86-64 avec sa libgcc privée ; U-Boot ARM64 est construit sur un runner ARM64. Ses options A/B et watchdog ainsi que l'architecture de l'ELF sont vérifiées avant publication de l'artefact.

La préparation dans le système cible conserve APT et les petits composants C/pilotes qui dépendent de son noyau et de ses bibliothèques. La version du noyau vient des répertoires installés et de leurs symboles, jamais de `uname -r`. Les tests d'intégration exécutent les binaires livrés contre le système cible (QEMU ARM1176 pour ARMv6), puis le sandbox U-Boot vérifie le démarrage avant assemblage et signature. La compression de l'image et des entrées utilise `xz -T0`, avec des horodatages séparés dans les logs.

Pour assembler des composants déjà construits, placer les trois fichiers `go-<cible>.tar`, `rust-<cible>.tar` et `uboot-<cible>.tar` dans un répertoire, puis passer `--components /chemin/composants` à `image/build.sh`. Rust et les compilateurs cross ne sont alors pas nécessaires au job d'image. Sans cette option, le script appelle `image/build-go.sh`, `image/build-rust.sh` et `image/build-uboot.sh` lui-même.

La compilation du service reste un `go build` avec `CGO_ENABLED=0`, `GOOS=linux` et `GOARCH=arm GOARM=6` ou `GOARCH=arm64`. Son court script ajoute la version et archive les dépendances pour le replay. Les scripts Rust et U-Boot préparent leur chaîne de compilation et vérifient la compatibilité ARMv6 ; ils sont partagés par la CI et la reconstruction locale.

Chaque image archive les `.deb` ajoutés/remplacés avec SHA-256 et inventaire. Les sources des pilotes, les dépendances Cargo/Go, le sysroot Rust, les paquets du compilateur U-Boot et les wheels Python ARM64 sont aussi archivés. Les verrous Cargo, Go et sysroot sont vérifiés lors d'une reconstruction. Les caches de téléchargement restent une optimisation : une disparition des anciens paquets des miroirs exige de mettre à jour le verrou ou de fournir les entrées archivées. Le répertoire APT `inputs/debs` n'est pas mis en cache : son manifeste active le mode replay et figerait la résolution des paquets.

Les dépendances de Linux Voice Assistant et son backend de build sont verrouillés par URL de wheel et SHA-256 dans `image/lva-requirements.lock` (CPython 3.13 ARM64). Le paquet LVA est construit sans résolution supplémentaire ; l'installation dans l'image est ensuite faite hors ligne.

```sh
bash image/build.sh zero-armv6 v2.0.0 --development \
  --replay /chemin/build-inputs-zero-armv6.tar.xz
```

Le code et les fichiers de verrouillage doivent correspondre à cette release. Utiliser la même architecture d'hôte que la CI de la cible pour exécuter le compilateur U-Boot archivé : x86-64 pour ARMv6, ARM64 pour ARM64. L'image Raspberry Pi officielle est retéléchargée et vérifiée. Les compilations des composants et les opérations APT/pip dans le chroot utilisent alors les dépendances archivées, sans résolution sur un dépôt vivant. Les utilitaires de l'hôte restent ceux d'Ubuntu 24.04. Cela reproduit les entrées logicielles ; les horodatages, signatures et identifiants de systèmes de fichiers ne sont pas déclarés reproductibles bit à bit.

## Partitionnement et démarrage

| Zone | Contenu |
|---|---|
| 1 Mio et 2 Mio, hors partitions | Deux copies de l'environnement U-Boot |
| Partition 1, à partir de 4 Mio | Firmware Raspberry Pi, U-Boot, script de sélection A/B |
| Partition 2 | Système A, 6 Gio, prérempli au flash |
| Partition 3 | Système B, 6 Gio, vide avant la première mise à jour |
| Partition 4 | Données ext4, étendue une seule fois au premier démarrage |

U-Boot lit noyau et Device Tree dans le slot choisi. Overlays et modules restent dans ce même système. L'état RAUC est conservé dans `/data`. Un nouveau slot n'est confirmé qu'après le contrôle local des services essentiels. Un échec de démarrage consomme une tentative puis ramène au dernier slot valide. La partition de firmware partagée reste fixe dans cette version.

Le système racine est monté en lecture seule ; identité, connexion réseau, réglages et calibration sont persistants. Journaux et fichiers temporaires sont volatils. Aucun serveur SQL n'est installé : la configuration applicative est un fichier JSON versionné écrit atomiquement. Les évolutions de schéma doivent rester lisibles par la version précédente pour permettre le rollback.

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
