Image NabOS NixOS pour un premier flash et des mises à jour signées de ce partitionnement. Le passage depuis une ancienne image demande un nouveau flash, après sauvegarde ; aucune migration OTA ni repartitionnement par mise à jour n’est qualifié. RAUC A/B, PipeWire et D-Bus ; carte microSD de 16 Go minimum. Choisir `zero-armv6` pour le Zero W original, `zero2-arm64` pour le Zero 2 W.

Créer d'abord une **prérelease** avec son changelog ; la CI y ajoute les artefacts selon la [procédure de release](build.md#créer-une-release). La réussite de la CI valide la fabrication ; elle ne valide pas le matériel. Le passage en **stable est bloqué** tant que les essais ci-dessous ne sont pas réussis et consignés sur **Zero W et Zero 2 W** réels, avec version, révision de carte, carte SD et journaux. Copier les journaux volatils sur `/data` avant chaque arrêt ou coupure contrôlée ; distinguer les inspections sur l'hôte des exécutions sur l'appareil.

Les [essais Zero 2 W du 30 septembre 2026](qualification-zero2-2026-09-30.md) consignent les mises à jour A → B → A et un rollback Linux contrôlé sur `v0.0.4-rc.1`, sous Raspberry Pi OS. Ils utilisent le même bundle et des données persistantes déjà initialisées ; **ils ne qualifient pas NixOS**. Tous les essais ci-dessous restent à consigner pour les nouvelles images.

- [ ] Premier démarrage, Wi-Fi depuis un téléphone, administration authentifiée, SSH fermé.
- [ ] Racine et `/etc` réellement en lecture seule, partition de démarrage non montée : premier démarrage avec `/data` vierge puis redémarrage avec état conservé ; point d’accès de device-core puis connexion Wi-Fi/DNS fonctionnels. Vérifier `/data` agrandie, les montages persistants de l’initrd NixOS, `/var` volatile et Bluetooth désactivé. Exercer les unités systemd effectives, y compris celles des paquets et leurs overrides, avec leurs restrictions et droits réels ; activer aussi les fonctions optionnelles dès le premier démarrage et relever `systemctl --failed`, `swapon --show`, la mémoire disponible et toute erreur d’écriture. Ne pas déduire une politique de swap NixOS de l’ancienne image.
- [ ] Clés SSH depuis les réglages : ajout, remplacement, refus d'une clé inconnue/des mots de passe/de root, retrait de toutes les clés ; `sudo -n id -u` renvoie `0` sous `nabos`. Clés autorisées et empreinte hôte conservées après redémarrage et bascule A/B.
- [ ] Démarrage autonome de PipeWire et des applications, sans connexion utilisateur.
- [ ] Unité `user@1004.service` composée avec le template et son drop-in, linger déclaré par NixOS, ModemManager désactivé ; vérifier les unités générées dans les deux artefacts puis leur démarrage réel sous systemd.
- [ ] Canaux Stable / Test / Edge séparés, identité Edge brute conservée dans RAUC et le journal ; changement de canal sans downgrade, installation strictement supérieure et reprise après redémarrage.
- [ ] Publication Edge interrompue pendant le brouillon puis reprise avec les mêmes artefacts testés ; assets publiés immuables, rétention des 30 versions les plus élevées sans suppression Stable/Test.
- [ ] Transition de versions N → N+1 → rollback N sur les deux appareils, données applicatives et profils Wi-Fi conservés ; activation Edge après livraison et qualification du client compatible.
- [ ] Oreilles, calibration, cinq LED, bouton, capture et lecture simultanées ; lecteurs CR14 et NFC ST25 testés séparément sur leurs cartes.
- [ ] Animations pendant le son ; arrêt et reprise après redémarrage de PipeWire.
- [ ] Reconnexion D-Bus après restart hardware/application ; aucun mouvement ancien ne se rejoue, LED nettoyées et oreilles/RFID réellement au repos avant une nouvelle prise de contrôle.
- [ ] Sur l'image fraîchement flashée, contrôler les deux environnements U-Boot bruts à 1 et 2 Mio (64 Kio chacun), les deux copies FAT identiques à 4 et 260 Mio (256 Mio chacune) et l'entrée MBR de p1. Démarrer matériellement depuis chacune des copies FAT en faisant pointer p1 vers elle.
- [ ] Installer un vrai bundle RAUC signé `verity`/Zstd sur l'appareil : `rootfs.ext4` vers la racine inactive puis `boot.vfat` via `boot-mbr-switch`. Vérifier p1 basculée entre 4 et 260 Mio, A → B puis B → A, et noyau, DTB, overlays et modules provenant du même slot racine.
- [ ] Refus de signatures incorrectes et de la mauvaise architecture ; interruption du téléchargement. Vérifier aussi qu'un système déjà installé sait ouvrir le bundle suivant ; si le lecteur change de manière incompatible, qualifier une release de transition.
- [ ] Coupures réelles pendant l'écriture de la racine, l'écriture de la copie FAT et avant/après le changement MBR ; redémarrage et état cohérent. Tester les quatre combinaisons ancien/nouveau démarrage × ancienne/nouvelle racine, puis l'échec du nouveau démarrage et le rollback racine après épuisement des tentatives, sans Internet. Une panne du firmware partagé ne bénéficie pas du contrôle de santé RAUC : vérifier la procédure de réparation de la carte SD.
- [ ] Watchdog : première alimentation par Linux moins de 16 secondes après U-Boot, y compris à froid sur Zero W ; blocage avant systemd et racine absente provoquent un redémarrage (délai de reprise de 300 secondes), puis un rollback.
- [ ] Réseau, identité, authentification, réglages et calibration conservés après mise à jour et rollback NixOS, entre versions différentes ; qualifier le nouveau format par premier flash. Les migrations de données persistantes doivent permettre le retour à la version précédente.
- [ ] Interface de mise à jour après le premier flash : choix d'une version, canaux Stable/Test, réglages conservés et automatique désactivé par défaut.
- [ ] Automatique nocturne : heure fiable, fin des lectures/radio/voix, créneau traversant minuit, désactivation avant redémarrage et une seule tentative par créneau.
- [ ] Reprise après redémarrage de device-core ou nabos et coupure en fin d'installation : aucune seconde écriture ni redémarrage intempestif ; échec ou rollback visible dans l'interface, version exclue de l'automatique, réessai manuel possible.
- [ ] Horloge, sommeil, lecture et RFID utilisables sans Internet ni Home Assistant.
- [ ] Tai-chi et surprises : programmation, langues, déclenchement manuel, sommeil et redémarrage ; tags pynab existants et nouvellement écrits.
- [ ] Boule magique au clic puis maintien ; récupération administrateur uniquement après deux clics puis maintien de 10 secondes, sans extinction accidentelle.
- [ ] Livres : toutes les voix, navigation par oreille, interruption et exclusivité avec les annonces ; radio MP3 continue, arrêt au bouton et coupure réseau sans croissance mémoire.
- [ ] Webhooks, IFTTT et WAQI avec les comptes réels ; associations UID et secrets conservés, désactivation et erreurs visibles.
- [ ] Mastodon : OAuth, proposition/acceptation/refus/séparation et oreilles entre nabos et pynab ; coupure réseau, reconnexion et absence de doublons.
- [ ] Sur Zero 2 W : LVA activé au bouton, API périphériques accessible uniquement en boucle locale ; désactivation possible ; consommation mémoire et stabilité prolongée mesurées.
- [ ] Les constructions et tests des artefacts exactement téléchargés réussissent pour les deux cibles ; manifestes `SHA256SUMS-<cible>`, verrous `flake-<cible>.lock` et révisions conservés avec les preuves. Le bundle respecte la limite d’import de 2 Gio ; taille des autres artefacts, durée totale et pic disque de chaque job relevés.
- [ ] Rapports cold/warm/version/nixpkgs conservés sous `dist/measurements/` avec entrées et scopes ; union Cachix dédupliquée mesurée après publication, objets manquants/évincés signalés. Aucune suffisance de 5 Go conclue sans ces mesures.

Les assets sont ceux du [guide de fabrication](build.md#construire-et-vérifier), tous nommés par cible : image de premier flash, bundle RAUC, certificat public, verrou Nix, script de démarrage, racines de cache et manifestes. Le certificat de confiance est amorcé dans `/data/rauc/ca.cert.pem` ; la clé privée n’est jamais une entrée Nix ni un artefact. Firmware Raspberry Pi, U-Boot et leur configuration sont communs aux racines A/B, mais les deux copies FAT sont mises à jour par RAUC. Un changement du partitionnement nécessite un nouveau flash.

## Qualification de l’extraction device-core

- [ ] Pin externe final : dépôt indépendant, commit, archive immuable et SHA-256 ; fabrication depuis cette archive pour ARMv6 et ARM64, distincte de la révision NabOS. Aucune valeur provisoire publiée.
- [ ] Entrées Nix et verrous Cargo distincts de nab-hardware/device-core : pins et hashes corrects, cible ARMv6 ou ARM64 vérifiée ; refus d’une source ou dépendance substituée. Le verrou livré correspond à celui utilisé pour construire.
- [ ] LED sur Zero W et Zero 2 W : couleurs, pulses, animations, Clear et maintenance, SIGTERM et SIGKILL/ExecStopPost, racine en lecture seule et audio simultané.
- [ ] Séparation des privilèges : nab-hardware ne reçoit aucune capability et accède aux GPIO et aux cinq LED sysfs ; nabos reçoit CAP_NET_BIND_SERVICE et /data/nabos ; aucun paquet/configuration/unité/binaire de Mosquitto ou ancien service livré.
- [ ] Closure runtime livrée : exécutables, ressources et notices de licence nécessaires, dont LICENSE/NOTICE device-core ; aucun checkout, outil Go/Cargo, cache ou vendor de fabrication. Publier dans Cachix les racines sélectionnées dans `cache-roots-<cible>`, outils de compilation compris ; images et signatures restent hors du cache.
- [ ] Racine réellement en lecture seule, montages persistants de l’initrd NixOS et restrictions systemd effectives : premier démarrage puis redémarrage ; `/data/device-core/settings.json` et `/data/nabos/application.json` séparés. Activer SSH et LVA dès ce premier démarrage, vérifier home/cache/préférences sous `/var/lib/nabos/lva`.
- [ ] device-core sans capability matérielle, PipeWire partagé ; droits NetworkManager/power/time/SSH/NTP/LVA accordés uniquement à son unité. Refus depuis nab-hardware, nabos et une session du même compte ; présence D-Bus admise uniquement depuis nab-hardware.
- [ ] HTTP device-core désactivé : contrôle de santé dépendant des deux Ready D-Bus, de nabos et des unités actives ; daemon arrêté ou non prêt ne doit pas confirmer le slot.
- [ ] Garder un FD de réseau ouvert, redémarrer device-core, vérifier inode inchangé et mutations toujours bloquées ; fermer le FD, vérifier reprise. Vérifier aussi nouveau generation/ancien generation rejeté.
- [ ] Audio interrompu, restart de nabos et disparition du client : aucun ancien ID ne stoppe une nouvelle lecture ; chorégraphies et calibration matérielle inchangées.
- [ ] Réseau sans Go ; clients Go/nab-hardware connectés directement au même bus device-core. RAUC reste accessible à root pour la santé ; documenter la portée par compte partagé de sa policy D-Bus.

## Qualification Wi-Fi

- [ ] Sur ARMv6 et ARM64 : scan explicite pendant le hotspot, saisie manuelle, connexion avec mot de passe erroné et absence de DHCP, annulation et retour à la connexion précédente ou au hotspot.
- [ ] Confirmer la réussite sur un LAN sans Internet ; conserver les profils fonctionnels précédents.
- [ ] Premier démarrage et persistance après coupure de courant, redémarrage du cœur et de NetworkManager pendant une tentative, racine réellement en lecture seule et restrictions systemd effectives.
- [ ] Wi-Fi device-core sans Go ; matériel disponible malgré NetworkManager indisponible.
- [ ] Formulaire avant administration accessible uniquement par le hotspot réel avec confirmation physique liée à la réservation ; après administration, connexion obligatoire y compris sur le hotspot.
- [ ] Vérifier DNS captif et accès HTTP sur 80 ; `/healthz` distant reste inaccessible.
