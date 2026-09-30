Image NabOS pour un premier flash et des mises à jour signées de ce partitionnement. Aucune migration d'un ancien agencement ni repartitionnement par mise à jour n'est prévue. Raspberry Pi OS Lite Trixie, RAUC A/B, PipeWire et MQTT 5 ; carte microSD de 16 Go minimum. Choisir `zero-armv6` pour le Zero W original, `zero2-arm64` pour le Zero 2 W.

Créer d'abord une **prérelease** avec son changelog ; la CI y ajoute les artefacts selon la [procédure de release](build.md#créer-une-release). La réussite de la CI valide la fabrication ; elle ne valide pas le matériel. Le passage en **stable est bloqué** tant que les essais ci-dessous ne sont pas réussis et consignés sur **Zero W et Zero 2 W** réels, avec version, révision de carte, carte SD et journaux. Copier les journaux volatils sur `/data` avant chaque arrêt ou coupure contrôlée ; distinguer les inspections sur l'hôte des exécutions sur l'appareil.

Les [essais Zero 2 W du 30 septembre 2026](qualification-zero2-2026-09-30.md) consignent les mises à jour A → B → A et un rollback Linux contrôlé sur `v0.0.4-rc.1`. Ils utilisent le même bundle et des données persistantes déjà initialisées ; la qualification globale ci-dessous reste incomplète, notamment pour le Zero W, le premier flash corrigé et les coupures réelles.

- [ ] Premier démarrage, Wi-Fi depuis un téléphone, administration authentifiée, SSH fermé.
- [ ] Racine réellement en lecture seule et partition de démarrage non montée : premier démarrage avec `/data` vierge puis redémarrage avec état conservé ; point d’accès de device-core puis connexion Wi-Fi/DNS fonctionnels. Vérifier `/data` agrandie, `/dev/zram0` actif avec `/data/swap` comme fichier de soutien (via son périphérique loop), et Bluetooth/Cloud-init désactivés. Exercer les unités systemd effectives, y compris celles des paquets et leurs overrides, avec leurs restrictions et droits réels ; activer aussi les fonctions optionnelles dès le premier démarrage et relever `systemctl --failed`, `swapon --show` et toute erreur d'écriture.
- [ ] Clés SSH depuis les réglages : ajout, remplacement, refus d'une clé inconnue/des mots de passe/de root, retrait de toutes les clés ; `sudo -n id -u` renvoie `0` sous `nabos`. Clés autorisées et empreinte hôte conservées après redémarrage et bascule A/B.
- [ ] Démarrage autonome de PipeWire et des applications, sans connexion utilisateur.
- [ ] Oreilles, calibration, cinq LED, bouton, capture et lecture simultanées ; lecteurs CR14 et NFC ST25 testés séparément sur leurs cartes.
- [ ] Animations pendant le son ; arrêt et reprise après redémarrage de PipeWire.
- [ ] Reconnexion MQTT ; aucune commande expirée, retenue ou déjà exécutée ne se rejoue.
- [ ] Sur l'image fraîchement flashée, contrôler les deux environnements U-Boot bruts à 1 et 2 Mio (64 Kio chacun), les deux copies FAT identiques à 4 et 260 Mio (256 Mio chacune) et l'entrée MBR de p1. Démarrer matériellement depuis chacune des copies FAT en faisant pointer p1 vers elle.
- [ ] Installer un vrai bundle RAUC signé `verity`/Zstd sur l'appareil : `rootfs.ext4` vers la racine inactive puis `boot.vfat` via `boot-mbr-switch`. Vérifier p1 basculée entre 4 et 260 Mio, A → B puis B → A, et noyau, DTB, overlays et modules provenant du même slot racine.
- [ ] Refus de signatures incorrectes et de la mauvaise architecture ; interruption du téléchargement. Vérifier aussi qu'un système déjà installé sait ouvrir le bundle suivant ; si le lecteur change de manière incompatible, qualifier une release de transition.
- [ ] Coupures réelles pendant l'écriture de la racine, l'écriture de la copie FAT et avant/après le changement MBR ; redémarrage et état cohérent. Tester les quatre combinaisons ancien/nouveau démarrage × ancienne/nouvelle racine, puis l'échec du nouveau démarrage et le rollback racine après épuisement des tentatives, sans Internet. Une panne du firmware partagé ne bénéficie pas du contrôle de santé RAUC : vérifier la procédure de réparation de la carte SD.
- [ ] Watchdog : première alimentation par Linux moins de 16 secondes après U-Boot, y compris à froid sur Zero W ; blocage avant systemd et racine absente provoquent un redémarrage (délai de reprise de 300 secondes), puis un rollback.
- [ ] Réseau, identité, authentification, réglages et calibration conservés après mise à jour et rollback . Cette extraction ne migre pas les anciennes configurations ; qualifier le nouveau format par premier flash.
- [ ] Interface de mise à jour après le premier flash : choix d'une version, canaux Stable/Test, réglages conservés et automatique désactivé par défaut.
- [ ] Automatique nocturne : heure fiable, fin des lectures/radio/voix, créneau traversant minuit, désactivation avant redémarrage et une seule tentative par créneau.
- [ ] Reprise après redémarrage de device-core ou nab-service et coupure en fin d'installation : aucune seconde écriture ni redémarrage intempestif ; échec ou rollback visible dans l'interface, version exclue de l'automatique, réessai manuel possible.
- [ ] Horloge, sommeil, lecture et RFID utilisables sans Internet ni Home Assistant.
- [ ] Tai-chi et surprises : programmation, langues, déclenchement manuel, sommeil et redémarrage ; tags pynab existants et nouvellement écrits.
- [ ] Boule magique au clic puis maintien ; récupération administrateur uniquement après deux clics puis maintien de 10 secondes, sans extinction accidentelle.
- [ ] Livres : toutes les voix, navigation par oreille, interruption et exclusivité avec les annonces ; radio MP3 continue, arrêt au bouton et coupure réseau sans croissance mémoire.
- [ ] Webhooks, IFTTT et WAQI avec les comptes réels ; associations UID et secrets conservés, désactivation et erreurs visibles.
- [ ] Mastodon : OAuth, proposition/acceptation/refus/séparation et oreilles entre nabos et pynab ; coupure réseau, reconnexion et absence de doublons.
- [ ] Sur Zero 2 W : LVA activé au bouton, API périphériques accessible uniquement en boucle locale ; désactivation possible ; consommation mémoire et stabilité prolongée mesurées.
- [ ] Chaque artefact est inférieur à 2 Gio ; pic disque de chaque job relevé dans `disk-usage-*.txt`.

Les assets comprennent l'image de premier flash, le bundle RAUC signé, les manifestes des versions et sommes SHA-256, et les dépendances archivées. Le certificat de confiance est embarqué dans l'image ; la clé privée n'y figure jamais. Firmware Raspberry Pi, U-Boot et leur configuration sont communs aux racines A/B, mais les deux copies FAT sont mises à jour par RAUC. Un changement du partitionnement nécessite un nouveau flash.

## Qualification de l’extraction device-core

- [ ] Pin externe final : dépôt indépendant, commit, archive immuable et SHA-256 ; fabrication depuis cette archive pour ARMv6 et ARM64, distincte de la révision NabOS. Aucune valeur provisoire publiée.
- [ ] Replay hors résolution Internet : deux Cargo.lock, deux vendors et identités séparées ; refus d’une archive, d’un binaire, d’un verrou Cargo ou d’une architecture substitués.
- [ ] Image livrée : seulement les trois exécutables et leurs ressources/notices ; aucun checkout, outil Go/Cargo, cache ou vendor de fabrication.
- [ ] Racine réellement en lecture seule, montages de boot-init et restrictions systemd effectives : premier démarrage puis redémarrage ; `/data/device-core/settings.json` et `/data/nabos/application.json` séparés. Activer SSH et LVA dès ce premier démarrage, vérifier home/cache/préférences sous `/var/lib/nabos/lva`.
- [ ] device-core sans capability matérielle, PipeWire partagé ; droits NetworkManager/power/time/SSH/NTP/LVA accordés uniquement à son unité. Refus depuis nab-core, nab-service et une session du même compte ; présence D-Bus admise uniquement depuis nab-core.
- [ ] HTTP device-core désactivé : contrôle de santé dépendant de Ready D-Bus, de nab-service et des unités actives ; daemon arrêté ou non prêt ne doit pas confirmer le slot.
- [ ] Garder un FD de réseau ouvert, redémarrer device-core, vérifier inode inchangé et mutations toujours bloquées ; fermer le FD, vérifier reprise. Vérifier aussi nouveau generation/ancien generation rejeté.
- [ ] Audio interrompu, restart du core et disparition du client : aucun ancien ID ne stoppe une nouvelle lecture ; chorégraphies et calibration matérielle inchangées.
- [ ] Réseau sans Go/MQTT ; clients Go/nab-core connectés directement au même bus device-core. RAUC reste accessible à root pour la santé ; documenter la portée par compte partagé de sa policy D-Bus.

## Qualification Wi-Fi

- [ ] Sur ARMv6 et ARM64 : scan explicite pendant le hotspot, saisie manuelle, connexion avec mot de passe erroné et absence de DHCP, annulation et retour à la connexion précédente ou au hotspot.
- [ ] Confirmer la réussite sur un LAN sans Internet ; conserver les profils fonctionnels précédents.
- [ ] Premier démarrage et persistance après coupure de courant, redémarrage du cœur et de NetworkManager pendant une tentative, racine réellement en lecture seule et restrictions systemd effectives.
- [ ] Wi-Fi device-core sans Go ni MQTT ; matériel disponible malgré NetworkManager indisponible.
- [ ] Formulaire avant administration accessible uniquement par le hotspot réel avec confirmation physique liée à la réservation ; après administration, connexion obligatoire y compris sur le hotspot.
- [ ] Vérifier DNS captif et accès HTTP sur 80 ; `/healthz` distant reste inaccessible.
