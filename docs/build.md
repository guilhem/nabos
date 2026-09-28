# Construire les images NabOS

Les deux cibles partent des images **officielles datées** de Raspberry Pi OS Lite Trixie. `image/sources.lock.json` fixe les URL, empreintes SHA-256 et révisions des pilotes, de U-Boot, de la bibliothèque LED et de Linux Voice Assistant. `genimage` assemble le disque ; RAUC signe le système préparé. Ni pi-gen, ni conteneur applicatif ne sont nécessaires sur le lapin.

| Cible | Hôte Ubuntu 24.04 | Système cible |
|---|---|---|
| `zero-armv6` | x86-64 + QEMU user (`arm1176`) | Raspberry Pi OS ARMv6, Rust `arm-unknown-linux-gnueabihf`, Go `GOARM=6` |
| `zero2-arm64` | ARM64 natif | Raspberry Pi OS ARM64, voix facultative |

## Commandes

Utiliser un hôte jetable avec `sudo` sans interaction, Go 1.27.1 et Rust 1.98.1, avec la cible Rust correspondante installée par `rustup target add`. Les commandes sont identiques dans GitHub Actions et en local :

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

La préparation se fait dans le système cible : installation APT, compilation des pilotes et de U-Boot, puis ajout du cœur Rust et du programme Go compilés en CI. Le linker Rust utilise la libc et libgcc de l'image ARMv6 ; les bibliothèques ARMv7 d'Ubuntu ne sont pas utilisées. La version du noyau vient des répertoires installés et de leurs symboles, jamais de `uname -r` dans le chroot.

Chaque image archive les `.deb` ajoutés/remplacés avec SHA-256 et inventaire. Les sources des pilotes, les dépendances Cargo/Go et les wheels Python ARM64 sont aussi archivés. Les fichiers Cargo.lock et go.sum sont vérifiés lors d'une reconstruction. Pour réutiliser les dépendances d'une release :

Les dépendances de Linux Voice Assistant et son backend de build sont verrouillés par URL de wheel et SHA-256 dans `image/lva-requirements.lock` (CPython 3.13 ARM64). Le paquet LVA est construit sans résolution supplémentaire ; l'installation dans l'image est ensuite faite hors ligne.

```sh
bash image/build.sh zero-armv6 v2.0.0 --development \
  --replay /chemin/build-inputs-zero-armv6.tar.xz
```

Le code et `sources.lock.json` doivent correspondre à cette release. L'image Raspberry Pi officielle est retéléchargée et vérifiée. Les opérations APT et pip dans le chroot utilisent alors les dépendances archivées, sans résolution sur un dépôt vivant. Cela reproduit les entrées logicielles ; les horodatages, signatures et identifiants de systèmes de fichiers ne sont pas déclarés reproductibles bit à bit.

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
