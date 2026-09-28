# NabOS

Logiciel libre pour les Nabaztag équipés d'une carte **TagTagTag 2019/2021** ou **NFC 2022**, avec Raspberry Pi Zero ou Zero 2.

[![Images](https://github.com/guilhem/nabos/actions/workflows/images.yml/badge.svg)](https://github.com/guilhem/nabos/actions/workflows/images.yml)

NabOS utilise **Raspberry Pi OS Lite Trixie + RAUC**, avec PipeWire et un bus MQTT 5 local. Le cœur matériel est en Rust ; l'interface, la configuration et les services sont en Go. Les réglages sont enregistrés atomiquement dans un fichier JSON versionné : aucun serveur de base de données n'est nécessaire.

Les outils de fabrication et les tests propres à NabOS sont aussi en Go. Les projets externes Comitup et Linux Voice Assistant sont conservés avec leurs dépendances Python.

La qualification du démarrage, des pilotes et du rollback sur les deux matériels est requise avant publication. Les constructions de développement et les tests de simulation ne constituent pas cette qualification.

## Installation

1. Télécharger l'image `.img.xz` correspondant au matériel dans une [release qualifiée](https://github.com/guilhem/nabos/releases) : `zero-armv6` pour le Zero original, `zero2-arm64` pour le Zero 2.
2. Vérifier `SHA256SUMS`, puis flasher une carte microSD de **16 Go minimum**.
3. Démarrer le lapin et configurer le Wi-Fi depuis le point d'accès Comitup.
4. Ouvrir `http://nabaztag.local:8080` et terminer la configuration de l'administration avec le bouton du lapin.

L'installation se fait par flash d'une carte SD. SSH est désactivé par défaut ; l'administration locale utilise HTTP sur le réseau de confiance.

Les mises à jour sont proposées dans l'interface après une recherche quotidienne sur GitHub Releases. L'installation est déclenchée par l'utilisateur. RAUC vérifie la signature et la compatibilité, écrit le slot inactif, puis valide le nouveau système après contrôle des services locaux. Les données et réglages sont conservés. Firmware Raspberry Pi et U-Boot restent ceux du flash initial.

## Composants

| Composant | Rôle |
|---|---|
| `core/` — `nab-core` | Matériel, états, séquences, chorégraphies, synchronisation avec le son |
| `services/` — `nab-service` | Interface locale, réglages, horloge, météo, ressources, Home Assistant, mises à jour |
| Mosquitto | Transport MQTT 5 local ; [contrat JSON v1](docs/protocol-v1.md) |
| PipeWire + WirePlumber | Lecture et capture ALSA ; compatibilité PulseAudio pour la voix |
| NetworkManager + Comitup | Connexion Wi-Fi et configuration initiale |
| RAUC + U-Boot | Installation signée A/B et retour à la version précédente |

Linux Voice Assistant est préinstallé uniquement sur ARM64 et **désactivé par défaut**. Son activation utilise Home Assistant pour la reconnaissance et la synthèse. Le bouton précède la qualification du mot d'activation et de l'annulation d'écho. L'API de périphériques reste en boucle locale.

Les pilotes oreilles, WM8960, CR14 et ST25R391x et la bibliothèque `rpi_ws281x` commandent le matériel. Un seul lecteur RFID est activé selon la carte détectée. Les sons et chorégraphies sont rangés dans `assets/`.

## Développement

La [documentation de fabrication](docs/build.md) décrit les commandes locales, les runners GitHub standards, les dépendances archivées, les secrets de signature et le partitionnement. La [fiche de qualification](docs/release-checklist.md) accompagne les brouillons de release.

```sh
cargo test --locked --manifest-path core/Cargo.toml
(cd services && go test -race ./...)
(cd services && NABOS_INTEGRATION=1 go test -count=1 ./tests/integration)
```

Le dernier contrôle requiert Mosquitto et ses clients. Les règles de contribution sont dans [CONTRIBUTING.md](CONTRIBUTING.md).

Le projet est distribué sous GPL-3.0-only ; les attributions des éléments réutilisés sont dans [NOTICE](NOTICE). Les firmwares binaires nécessaires au Raspberry Pi restent une exception fournie par Raspberry Pi OS.
