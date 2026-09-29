Image NabOS pour un premier flash et des mises à jour signées. Raspberry Pi OS Lite Trixie, RAUC A/B, PipeWire et MQTT 5 ; carte microSD de 16 Go minimum. Choisir `zero-armv6` pour le Zero original, `zero2-arm64` pour le Zero 2.

Créer la release avec son changelog et le statut choisi ; la CI y ajoute les artefacts selon la [procédure de release](build.md#créer-une-release). La réussite de la CI valide la fabrication ; elle ne valide pas le matériel. Garder le statut **prérelease** ou **brouillon** jusqu'à avoir enregistré les résultats ci-dessous pour les **deux** cibles, avec version, révision de carte, carte SD et journaux.

- [ ] Premier démarrage, Wi-Fi depuis un téléphone, administration authentifiée, SSH fermé.
- [ ] Racine en lecture seule : premier démarrage avec `/data` vierge puis redémarrage avec état conservé ; point d'accès Comitup puis connexion Wi-Fi/DNS fonctionnels. Vérifier que `swapon --show` ne présente que `/dev/zram0`, que Bluetooth et Cloud-init restent désactivés. Relever `systemctl --failed` et les erreurs d'écriture du journal avant extinction (journaux volatils).
- [ ] Clés SSH depuis les réglages : ajout, remplacement, refus d'une clé inconnue/des mots de passe/de root, retrait de toutes les clés ; `sudo -n id -u` renvoie `0` sous `nabos`. Clés autorisées et empreinte hôte conservées après redémarrage et bascule A/B.
- [ ] Démarrage autonome de PipeWire et des applications, sans connexion utilisateur.
- [ ] Oreilles, calibration, cinq LED, bouton, capture et lecture simultanées ; lecteurs CR14 et NFC ST25 testés séparément sur leurs cartes.
- [ ] Animations pendant le son ; arrêt et reprise après redémarrage de PipeWire.
- [ ] Reconnexion MQTT ; aucune commande expirée, retenue ou déjà exécutée ne se rejoue.
- [ ] Mise à jour A → B, puis B → A ; noyau, DTB, overlays et modules proviennent du même slot.
- [ ] Refus de signatures incorrectes et de la mauvaise architecture ; interruption du téléchargement.
- [ ] Coupure d'alimentation pendant écriture ; échec du nouveau démarrage ; rollback après épuisement des tentatives, sans Internet.
- [ ] Watchdog : première alimentation par Linux moins de 16 secondes après U-Boot, y compris à froid sur Zero ; blocage avant systemd et racine absente provoquent un redémarrage (délai de reprise de 300 secondes), puis un rollback.
- [ ] Réseau, identité, authentification, réglages et calibration conservés après mise à jour et rollback.
- [ ] Interface de mise à jour : choix d'une version, canaux Stable/Test, réglages conservés et automatique désactivé lors de la migration.
- [ ] Automatique nocturne : heure fiable, fin des lectures/radio/voix, créneau traversant minuit, désactivation avant redémarrage et une seule tentative par créneau.
- [ ] Reprise après redémarrage de nab-service et coupure en fin d'installation : aucune seconde écriture ni redémarrage intempestif ; échec ou rollback visible dans l'interface, version exclue de l'automatique, réessai manuel possible.
- [ ] Horloge, sommeil, lecture et RFID utilisables sans Internet ni Home Assistant.
- [ ] Tai-chi et surprises : programmation, langues, déclenchement manuel, sommeil et redémarrage ; tags pynab existants et nouvellement écrits.
- [ ] Boule magique au clic puis maintien ; récupération administrateur uniquement après deux clics puis maintien de 10 secondes, sans extinction accidentelle.
- [ ] Livres : toutes les voix, navigation par oreille, interruption et exclusivité avec les annonces ; radio MP3 continue, arrêt au bouton et coupure réseau sans croissance mémoire.
- [ ] Webhooks, IFTTT et WAQI avec les comptes réels ; associations UID et secrets conservés, désactivation et erreurs visibles.
- [ ] Mastodon : OAuth, proposition/acceptation/refus/séparation et oreilles entre nabos et pynab ; coupure réseau, reconnexion et absence de doublons.
- [ ] Sur Zero 2 : LVA activé au bouton, API périphériques accessible uniquement en boucle locale ; désactivation possible ; consommation mémoire et stabilité prolongée mesurées.
- [ ] Chaque artefact est inférieur à 2 Gio ; pic disque de chaque job relevé dans `disk-usage-*.txt`.

Les assets comprennent l'image de premier flash, le bundle RAUC signé, les manifestes des versions et sommes SHA-256, et les dépendances archivées. Le certificat de confiance est embarqué dans l'image ; la clé privée n'y figure jamais. Firmware Raspberry Pi et U-Boot restent communs aux slots : leur mise à niveau nécessite un nouveau flash.
