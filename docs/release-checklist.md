Image NabOS pour un premier flash et des mises à jour signées. Raspberry Pi OS Lite Trixie, RAUC A/B, PipeWire et MQTT 5 ; carte microSD de 16 Go minimum. Choisir `zero-armv6` pour le Zero original, `zero2-arm64` pour le Zero 2.

Cette release est créée en **brouillon**. La réussite de la CI valide la fabrication ; elle ne valide pas le matériel. Ne publier qu'après avoir enregistré les résultats ci-dessous pour les **deux** cibles, avec version, révision de carte, carte SD et journaux.

- [ ] Premier démarrage, Wi-Fi depuis un téléphone, administration authentifiée, SSH fermé.
- [ ] Démarrage autonome de PipeWire et des applications, sans connexion utilisateur.
- [ ] Oreilles, calibration, cinq LED, bouton, capture et lecture simultanées ; lecteurs CR14 et NFC ST25 testés séparément sur leurs cartes.
- [ ] Animations pendant le son ; arrêt et reprise après redémarrage de PipeWire.
- [ ] Reconnexion MQTT ; aucune commande expirée, retenue ou déjà exécutée ne se rejoue.
- [ ] Mise à jour A → B, puis B → A ; noyau, DTB, overlays et modules proviennent du même slot.
- [ ] Refus de signatures incorrectes et de la mauvaise architecture ; interruption du téléchargement.
- [ ] Coupure d'alimentation pendant écriture ; échec du nouveau démarrage ; rollback après épuisement des tentatives, sans Internet.
- [ ] Watchdog : première alimentation par Linux moins de 16 secondes après U-Boot, y compris à froid sur Zero ; blocage avant systemd et racine absente provoquent un redémarrage (délai de reprise de 300 secondes), puis un rollback.
- [ ] Réseau, identité, authentification, réglages et calibration conservés après mise à jour et rollback.
- [ ] Horloge, sommeil, lecture et RFID utilisables sans Internet ni Home Assistant.
- [ ] Sur Zero 2 : LVA activé au bouton, API périphériques accessible uniquement en boucle locale ; désactivation possible ; consommation mémoire et stabilité prolongée mesurées.
- [ ] Chaque artefact est inférieur à 2 Gio ; pic disque de chaque job relevé dans `disk-usage-*.txt`.

Les assets comprennent l'image de premier flash, le bundle RAUC signé, les manifestes des versions et sommes SHA-256, et les dépendances archivées. Le certificat de confiance est embarqué dans l'image ; la clé privée n'y figure jamais. Firmware Raspberry Pi et U-Boot restent communs aux slots : leur mise à niveau nécessite un nouveau flash.
