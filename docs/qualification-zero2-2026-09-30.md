# Qualification partielle Zero 2 W — 30 septembre 2026

Ces essais ont été exécutés sur le matériel avec `v0.0.4-rc.1`, la racine
réellement en lecture seule et les restrictions systemd installées.
**Ils ne valident pas le passage en stable.** La [fiche de qualification](release-checklist.md)
reste à compléter sur les deux modèles.

| Élément | Appareil testé |
|---|---|
| Carte | Raspberry Pi Zero 2 W Rev 1.0, révision U-Boot `0x902120` |
| Cible du bundle | `nabos-zero2-arm64` |
| Linux | `6.18.50+rpt-rpi-v8`, aarch64 |
| RAUC | 1.13 |
| microSD | 31 266 439 168 octets ; nom sysfs `NCard`, manfid `0x000089`, oemid `0x0303`, date `08/2024` ; marque commerciale non confirmée |

## Résultats observés

| Contrôle | Résultat |
|---|---|
| Géométrie | Deux FAT de 256 Mio à 4 et 260 Mio ; racines de 6 Gio à 516 et 6660 Mio ; données à 12804 Mio, étendues à 17 840 472 064 octets. |
| Lecture seule et persistance | Création d'un fichier à la racine refusée avec `EROFS` ; FAT non montée ; `/data` sur p4, sans mode volatile, avec les bind mounts persistants. Zram utilise `/data/swap` via un périphérique loop. |
| Images et environnements | Les deux FAT correspondent au manifeste signé. L'image complète de B après installation correspond à `rootfs.ext4`. CRC valides pour les environnements de 64 Kio à 1 et 2 Mio ; empreintes des zones réservées et géométrie des partitions 2/3/4 conservées. |
| Bundle | SHA-256 et signature vérifiés avec le certificat installé ; format `verity`, images `rootfs` puis `bootloader`. Signature altérée et cible Zero W refusées avant installation ; MBR, environnements, en-têtes FAT/racines et réglages contrôlés conservés. |
| A → B → A | Le vrai daemon RAUC écrit la racine inactive puis la FAT inactive, bascule p1 de 4 à 260 Mio puis inversement, et termine son handler de synchronisation avec succès. Les deux racines démarrent et reçoivent `good A` / `good B`. |
| Données et SSH | Configuration NabOS, machine-id, clés hôte et clés autorisées conservés après mises à jour et rollback. Profil Wi-Fi client intact. Le profil AP conserve ses réglages ; seul son UUID, recréé par Comitup, est exclu de la comparaison. Clé inconnue, mot de passe et connexion SSH root refusés. |
| Audio et MQTT | Capture et lecture silencieuse simultanées via PipeWire/ALSA, environ 2,07 s à 48 kHz. Commande réelle vers nab-core exécutée avec `aplay` ; doublon, commande expirée et commande retained refusés. |
| LVA | Activation du service avec `User=nabos`, `ProtectSystem=strict` et état sous `/var/lib/nabos/lva` ; API WebSocket locale sur 6055, changement de volume et persistance après redémarrage du service. Préférences originales restaurées et activation temporaire retirée. |
| HTTP et état final | `/healthz` OK ; mutations sans session ou avec origine incorrecte refusées. Aucune unité système ou utilisateur en échec ; `ProtectSystem=strict` effectif pour nab-core, nab-service et LVA. |

Les deux installations utilisent **le même bundle signé**. Pour exercer la seconde
écriture FAT malgré `install-same=false`, le daemon RAUC a reçu temporairement
`install-same=true` via une configuration et un drop-in dans `/run`. Son démarrage
d'origine a été rétabli avant le reboot sur A. Cet essai ne qualifie pas une
transition entre versions différentes.

## Rollback Linux contrôlé

Après confirmation de A sain, nab-core a été masqué uniquement dans `/run` et les
tentatives A mises à zéro par RAUC. `mark-bad` retire aussi A de `BOOT_ORDER` :
`fw_setenv` a rétabli l'ordre `A B`, avec A à zéro et B à trois tentatives, pour
exercer le saut d'A épuisé par U-Boot. Les trois échecs successifs ne sont pas testés.

Le service de santé conserve son code et son délai normal de 300 secondes. Le
suiveur du journal n'ayant pas conservé la décision lors du premier essai, celui-ci
a été répété avec seulement les sorties du service redirigées vers `/data` par un
drop-in runtime. Le descripteur effectif et les lignes persistantes ont été vérifiés :

```text
slot A unhealthy after 300s: nab-core.service is not active
rebooting: slot A has 0 attempts left, slot B 3
```

Après le redémarrage automatique : boot-id différent, racine p3 en lecture seule,
`rauc.slot=B`, `good B`, core actif et `/healthz` OK. Les masques et drop-ins de test
ont disparu. A a été remis bon et B prioritaire : `BOOT_ORDER=B A`, trois tentatives
chacun. Les contrôles finaux confirment à nouveau la conservation des réglages,
des identités, des zones réservées et de la géométrie.

## Incidents et qualification restante

Pendant le premier téléchargement et une lecture complète simultanée de la racine,
des erreurs Wi-Fi SDIO `mmc1/brcmfmac` (`Controller never released inhibit bit(s)`,
`CMD53 ... -5`, `sdio error`) ont précédé une perte SSH. RAUC n'installait rien.
Un redémarrage physique a rétabli l'accès ; la cause reste indéterminée. La reprise
du téléchargement était limitée à 1 Mio/s : le téléchargeur applicatif n'est pas
qualifié par cet essai. Aucun nouveau message SDIO de ce type n'a été relevé dans
les boots contrôlés après la reprise. Des requêtes SSH retardées pendant B → A ont
repris sans intervention.

La racine rc.1 contient `WirelessEnabled=false` dans l'état usine NetworkManager,
observé avec `debugfs`. Les données persistantes fournies avaient déjà été corrigées
avec Wi-Fi actif. La correction de fabrication fusionnée dans la PR #14, commit
`a890621`, n'est pas présente dans cette image : **le premier démarrage avec données
vierges et point d'accès reste à qualifier sur une image corrigée**.

Les journaux noyau conservent aussi des messages de probe UART auxiliaire
`-EINVAL`/`-22` et un avertissement d'overlay concernant une suppression éventuelle.

Restent notamment le Zero W, les coupures réelles autour des écritures et du MBR,
les versions différentes, le watchdog et trois tentatives réellement épuisées,
l'administration Web authentifiée et le journal de l'updater, le parcours vocal
Home Assistant/modèles, les essais physiques oreilles/LED/bouton/RFID et les autres
fonctions optionnelles. Une FAT trop grande et une erreur de synchronisation n'ont
pas été injectées sur cette carte réelle. La capture audio ne juge pas sa qualité
auditive ; l'activation LVA au bouton et la reconnexion MQTT ne sont pas couvertes.

## Preuves conservées

Les snapshots, manifestes, sommes, journaux d'installation, tests négatifs et
contrôles finaux sont conservés sur l'appareil sous
`/data/nabos/qualification/20260930-zero2-rc1/`. `rollback-health-direct.log` conserve
la décision ci-dessus ; `rollback-result.json`, `final-cleanup.json` et
`final-boot-b/summary.json` documentent le retour et l'état restauré. Une archive
privée des diagnostics a été récupérée et contrôlée, sans les bundles téléchargés.
Les journaux bruts et sauvegardes réseau/identité restent privés.
