# NabOS

Logiciel libre pour les Nabaztag équipés d'une carte **TagTagTag 2019/2021** ou **NFC 2022**, avec Raspberry Pi Zero ou Zero 2.

[![Images](https://github.com/guilhem/nabos/actions/workflows/images.yml/badge.svg)](https://github.com/guilhem/nabos/actions/workflows/images.yml)

NabOS utilise **Raspberry Pi OS Lite Trixie + RAUC**, avec PipeWire et un bus MQTT 5 local. Le cœur matériel est en Rust ; l'interface, la configuration et les services sont en Go. Les réglages sont enregistrés atomiquement dans un fichier JSON versionné : aucun serveur de base de données n'est nécessaire.

Les outils de fabrication et les tests propres à NabOS sont aussi en Go. Les projets externes Comitup et Linux Voice Assistant sont conservés avec leurs dépendances Python.

La qualification du démarrage, des pilotes et du rollback sur les deux matériels est requise avant diffusion en release stable. Les constructions de développement et les tests de simulation ne constituent pas cette qualification.

## Installation

1. Télécharger l'image `.img.xz` correspondant au matériel dans une [release qualifiée](https://github.com/guilhem/nabos/releases) : `zero-armv6` pour le Zero original, `zero2-arm64` pour le Zero 2.
2. Vérifier `SHA256SUMS`, puis flasher une carte microSD de **16 Go minimum**.
3. Démarrer le lapin et configurer le Wi-Fi depuis le point d'accès Comitup.
4. Ouvrir `http://nabaztag.local:8080` et terminer la configuration de l'administration avec le bouton du lapin.

L'installation se fait par flash d'une carte SD. L'administration locale utilise HTTP sur le réseau de confiance.

### Connexion SSH

SSH est désactivé par défaut. Dans **Réglages → Accès SSH**, coller une ou plusieurs **clés publiques**, une par ligne, puis cliquer sur **Enregistrer les clés SSH**. Sur Linux, le contenu à copier s'affiche avec `cat ~/.ssh/id_ed25519.pub` ; si aucune clé n'existe encore, en créer une avec `ssh-keygen -t ed25519`.

```sh
ssh nabos@nabaztag.local
```

OpenSSH utilise le fichier standard `authorized_keys`. La connexion fonctionne uniquement par clé, sous le compte `nabos`, avec **sudo sans mot de passe** pour administrer le système ; ni le mot de passe de l'interface ni une connexion SSH directe en root ne sont acceptés. Les clés autorisées et l'identité SSH du lapin sont conservées après redémarrage et mise à jour. Vider le champ puis enregistrer désactive les nouvelles connexions ; les sessions déjà ouvertes restent actives.

Cette fonction nécessite une image qui l'intègre, ou sa mise à jour RAUC. Les anciennes images qui masquent SSH doivent être mises à jour auparavant. Les options SSH de Raspberry Pi Imager et les fichiers `ssh`/`userconf.txt` ne sont pas utilisés par NabOS.

La page **Mises à jour** liste les nouvelles versions GitHub et leurs notes de publication. Elle permet de vérifier à la demande, de proposer les mises à jour chaque jour (réglage initial), ou d'activer leur installation automatique. Le canal **Stable** est sélectionné par défaut ; le canal **Test** inclut les préversions et s'applique aussi à l'automatique. Chaque version disponible peut être installée manuellement, puis activée avec le bouton de redémarrage.

L'automatique attend le créneau réglable (03:00–05:00 par défaut, dans le fuseau du lapin), une heure fiable et la fin des lectures et interactions. Il ne redémarre que les installations qu'il a déclenchées. Désactiver l'automatique laisse une écriture déjà commencée se terminer, puis conserve le redémarrage manuel. Une release dont les fichiers sont encore en fabrication apparaît **En préparation**.

RAUC vérifie la signature et la compatibilité, écrit le slot inactif, puis le contrôle de santé confirme le nouveau système. Les données et réglages sont conservés. Après un rollback, la version fautive est exclue de l'automatique ; un réessai manuel reste possible. Une coupure au résultat indéterminé suspend l'automatique jusqu'à une reprise manuelle. Firmware Raspberry Pi et U-Boot restent ceux du flash initial.

## Composants

| Composant | Rôle |
|---|---|
| `core/` — `nab-core` | Matériel, états, séquences, chorégraphies, synchronisation avec le son |
| `services/` — `nab-service` | Interface locale, réglages, services pynab en Go, Home Assistant, mises à jour |
| Mosquitto | Transport MQTT 5 local ; [contrat JSON v1](docs/protocol-v1.md) |
| PipeWire + WirePlumber | Lecture et capture ALSA ; compatibilité PulseAudio pour la voix |
| NetworkManager + Comitup | Connexion Wi-Fi et configuration initiale |
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

Home Assistant expose les annonces comme boutons, utilisables depuis LVA. LVA, Comitup et NabBlockly restent des projets externes.

## Développement

La [documentation de fabrication](docs/build.md) décrit les commandes locales, les runners GitHub standards, les dépendances archivées, les secrets de signature et le partitionnement. Le mainteneur crée la release avec son statut et son changelog ; la CI y ajoute les artefacts. La [fiche de qualification](docs/release-checklist.md) accompagne le passage en stable.

```sh
cargo test --locked --manifest-path core/Cargo.toml
(cd services && go test -race ./...)
(cd services && NABOS_INTEGRATION=1 go test -race -count=1 ./tests/integration ./cmd/nab-service)
```

Le dernier contrôle requiert Mosquitto et ses clients. Les règles de contribution sont dans [CONTRIBUTING.md](CONTRIBUTING.md).

Le projet est distribué sous GPL-3.0-only ; les attributions des éléments réutilisés sont dans [NOTICE](NOTICE). Les firmwares binaires nécessaires au Raspberry Pi restent une exception fournie par Raspberry Pi OS.
