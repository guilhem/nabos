# NabOS

Logiciel libre pour les Nabaztag équipés d'une carte **TagTagTag 2019/2021** ou **NFC 2022**, avec Raspberry Pi Zero ou Zero 2.

[![Images](https://github.com/guilhem/nabos/actions/workflows/images.yml/badge.svg)](https://github.com/guilhem/nabos/actions/workflows/images.yml)

NabOS utilise **Raspberry Pi OS Lite Trixie + RAUC**, avec PipeWire et des API D-Bus typées. `nab-hardware` garde uniquement le matériel en Rust. `nabos` possède en Go les états, la file média, les chorégraphies, l’interface et les applications. Le dépôt indépendant `device-core` fournit les services Linux en Rust : réseau, audio, configuration système, horloge, SSH, voix et mises à jour. Les réglages système vivent dans `/data/device-core/settings.json`, les réglages applicatifs dans `/data/nabos/application.json`. MQTT sert uniquement à Home Assistant via son broker configuré ; aucun broker local n’est installé.

Les outils de fabrication et les tests propres à NabOS sont aussi en Go. Linux Voice Assistant conserve ses dépendances Python.

La qualification du démarrage, des pilotes et du rollback sur les deux matériels est requise avant diffusion en release stable. Les constructions de développement et les tests de simulation ne constituent pas cette qualification.

## Installation

1. Télécharger l'image `.img.xz` correspondant au matériel dans une [release qualifiée](https://github.com/guilhem/nabos/releases) : `zero-armv6` pour le Zero original, `zero2-arm64` pour le Zero 2.
2. Vérifier `SHA256SUMS`, puis flasher une carte microSD de **16 Go minimum**.
3. Démarrer le lapin, rejoindre son point d'accès Nabaztag et ouvrir `http://10.41.0.1`. Appuyer sur le bouton quand le formulaire le demande, puis configurer le Wi-Fi.
4. Ouvrir `http://nabaztag.local` et terminer la configuration de l'administration avec le bouton du lapin.

L'installation se fait par flash d'une carte SD. L'administration locale utilise HTTP sur le réseau de confiance.

### Connexion SSH

SSH est désactivé par défaut. Dans **Réglages → Accès SSH**, coller une ou plusieurs **clés publiques**, une par ligne, puis cliquer sur **Enregistrer les clés SSH**. Sur Linux, le contenu à copier s'affiche avec `cat ~/.ssh/id_ed25519.pub` ; si aucune clé n'existe encore, en créer une avec `ssh-keygen -t ed25519`.

```sh
ssh nabos@nabaztag.local
```

OpenSSH utilise le fichier standard `authorized_keys`. La connexion fonctionne uniquement par clé, sous le compte `nabos`, avec **sudo sans mot de passe** pour administrer le système ; ni le mot de passe de l'interface ni une connexion SSH directe en root ne sont acceptés. Les clés autorisées et l'identité SSH du lapin sont conservées après redémarrage et mise à jour. Vider le champ puis enregistrer désactive les nouvelles connexions ; les sessions déjà ouvertes restent actives.

Les options SSH de Raspberry Pi Imager et les fichiers `ssh`/`userconf.txt` ne sont pas utilisés par NabOS.

La page **Mises à jour** liste les nouvelles versions GitHub et leurs notes de publication. Elle permet de vérifier à la demande, de proposer les mises à jour chaque jour (réglage initial), ou d'activer leur installation automatique. Le canal **Stable** est sélectionné par défaut ; le canal **Test** inclut les préversions et s'applique aussi à l'automatique. Chaque version disponible peut être installée manuellement, puis activée avec le bouton de redémarrage.

L'automatique attend le créneau réglable (03:00–05:00 par défaut, dans le fuseau du lapin), une heure fiable et la fin des lectures et interactions. Il ne redémarre que les installations qu'il a déclenchées. Désactiver l'automatique laisse une écriture déjà commencée se terminer, puis conserve le redémarrage manuel. Une release dont les fichiers sont encore en fabrication apparaît **En préparation**.

RAUC vérifie la signature et la compatibilité, écrit le slot inactif, puis le contrôle de santé confirme le nouveau système. Les données et réglages sont conservés. Après un rollback, la version fautive est exclue de l'automatique ; un réessai manuel reste possible. Une coupure au résultat indéterminé suspend l'automatique jusqu'à une reprise manuelle. Firmware Raspberry Pi et U-Boot sont livrés dans les deux copies FAT mises à jour par RAUC.

Cette extraction utilise de nouveaux contrats et fichiers de configuration. Aucune migration des anciens réglages ni rétrocompatibilité des anciennes API n’est fournie. Le système racine reste en lecture seule ; le home et les préférences LVA restent sous `/var/lib/nabos`, lié à `/data/system` au démarrage.

## Composants

| Composant | Rôle |
|---|---|
| `core/` — `nab-hardware` | Oreilles, LED, bouton et RFID/NFC ; [API D-Bus matérielle](docs/hardware-dbus.md) |
| `device-core` — dépôt indépendant | NetworkManager, audio, réglages système, horloge, SSH, voix, RAUC ; API D-Bus, HTTP facultatif désactivé dans NabOS |
| `services/` — `nabos` | États, médias, chorégraphies, interface et applications ; clients D-Bus de hardware/device-core |
| PipeWire + WirePlumber | Lecture et capture ALSA ; compatibilité PulseAudio pour la voix |
| NetworkManager | Radio Wi-Fi, profils et secrets ; [API D-Bus de device-core](docs/network-dbus.md) |
| RAUC + U-Boot | Installation signée A/B et retour à la version précédente |

Linux Voice Assistant est préinstallé uniquement sur ARM64 et **désactivé par défaut**. Son activation utilise Home Assistant pour la reconnaissance et la synthèse. Le bouton précède la qualification du mot d'activation et de l'annulation d'écho. L'API de périphériques reste en boucle locale.

Les pilotes oreilles, WM8960, CR14 et ST25R391x et la bibliothèque `rpi_ws281x` commandent le matériel. Un seul lecteur RFID est activé selon la carte détectée. Les sons et chorégraphies sont rangés dans `assets/`.

## Services

L’onglet **Services** configure le tai-chi, les surprises (langues, anniversaires et messages saisonniers), la boule magique, la qualité de l’air, IFTTT, les webhooks, la radio, les livres et le jumelage Mastodon. L’horloge et la météo restent dans **Réglages**. Les formats de tags et les médias proviennent de pynab `f24d3e1` ; leurs attributions sont dans [NOTICE](NOTICE).

- Une fréquence de zéro suspend le tai-chi ou les surprises automatiques ; les déclenchements manuels restent disponibles. Les prochaines échéances sont conservées après redémarrage et les annonces périmées ne sont pas rejouées.
- La qualité de l’air demande un jeton WAQI personnel et reprend la localisation météo, ou la géolocalisation par adresse IP. L’interface affiche les erreurs des services connectés.
- **Étiquettes** programme les anciens formats RFID. Pour la radio, IFTTT et les webhooks, l’adresse ou l’événement est enregistré localement par UID. « Associer sans réécrire » permet de conserver un tag existant, même verrouillé. Il faut configurer l’association sur chaque lapin.
- La radio accepte une adresse directe de flux **MP3** (pas une liste M3U/HLS ou de l’AAC). Go relaie le flux sans fichier temporaire, avec un tampon borné. Un clic, « Interrompre », le sommeil ou une autre annonce l’arrête.
- Pendant un livre, l’oreille gauche recule d’un chapitre, la droite avance et un clic interrompt. Les annonces attendent la fin de la session. Le catalogue et les voix fournis par pynab sont inclus ; ses consignes de lecture sont en français.
- Un clic puis maintien active l’écoute de la boule magique ; le relâchement déclenche la réponse. Pour réinitialiser l’administration, faire **deux clics puis maintenir un troisième appui pendant 10 secondes**. Trois clics brefs restent l’extinction.
- Mastodon utilise OAuth et les messages directs `NabPairing`, compatibles avec pynab. Le jumelage, les positions et le curseur des messages sont persistants ; le flux se reconnecte et rattrape les messages après une coupure.

Home Assistant expose les annonces comme boutons, utilisables depuis LVA. LVA et NabBlockly restent des projets externes.

## Développement

La [documentation de fabrication](docs/build.md) décrit les commandes locales, les runners GitHub standards, les dépendances archivées, les secrets de signature et le partitionnement. Le mainteneur crée la release avec son statut et son changelog ; la CI y ajoute les artefacts. La [fiche de qualification](docs/release-checklist.md) accompagne le passage en stable.

```sh
cargo test --locked --manifest-path core/Cargo.toml
(cd services && go test -race ./...)
(cd services && NABOS_INTEGRATION=1 go test -race -count=1 ./tests/integration ./cmd/nabos)
```

Les simulations utilisent un bus D-Bus privé explicite (`NABOS_DEVICE_BUS_ADDRESS`) ; Mosquitto et ses clients servent uniquement aux fixtures Home Assistant. `DEVICE_CORE_BIN` désigne le binaire externe de simulation (voir le guide de fabrication). Les règles de contribution sont dans [CONTRIBUTING.md](CONTRIBUTING.md).

Le projet est distribué sous GPL-3.0-only ; les attributions des éléments réutilisés sont dans [NOTICE](NOTICE). Les firmwares binaires nécessaires au Raspberry Pi restent une exception fournie par Raspberry Pi OS.
