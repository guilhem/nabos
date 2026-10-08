# Contrainte de fonctionnement : racine en lecture seule

NabOS démarre avec le système racine **en lecture seule**. Cette contrainte
concerne les applications, les services déclarés dans NixOS et toutes
leurs dépendances, y compris les écritures faites à l'import ou au premier usage.
Les écritures autorisées pendant la fabrication de l'image ne prouvent pas
qu'un logiciel fonctionnera sur le lapin.

Avant de modifier un logiciel exécuté sur l'appareil, lire
`nix/runtime/initrd-persist.sh`, `nix/runtime/persist.sh`, `nix/system.nix` et les
unités systemd du logiciel concerné, y compris celles de ses paquets et leurs overrides.

- Conserver les données applicatives dans `/data/nabos` ou réutiliser les chemins
  persistants de l’initrd, notamment `/var/lib/nabos` pour le home et les
  préférences de LVA. Ces montages viennent de `/data/system` ; `/var/lib` et
  `/etc` ne sont pas globalement inscriptibles. Le mode de secours
  `/data/.volatile` ne garantit aucune persistance après redémarrage.
- Placer les fichiers temporaires, sockets, FIFO, PID et verrous dans un espace
  volatile adapté, généralement `/run` via `RuntimeDirectory=`. Définir aussi
  `WorkingDirectory=` si une dépendance écrit dans son répertoire courant : un
  service système démarre sinon dans `/`.
- Vérifier les chemins par défaut de `HOME`, XDG, caches, modèles téléchargés,
  journaux et sous-processus, avec l'utilisateur et les droits réels du service.
  Sous `ProtectSystem=strict`, un montage temporaire accessible sur le système
  peut être en lecture seule dans le service. `ReadWritePaths=` et les droits
  root ne rendent pas un système de fichiers monté en lecture seule inscriptible.
- Corriger les chemins et les montages nécessaires, sans rendre la racine active
  inscriptible ni retirer globalement le confinement. Les écritures RAUC dans
  le slot inactif et l'environnement U-Boot sont un mécanisme distinct.
- À chaque changement de l'image de base ou des paquets, revoir également les
  services, timers et générateurs hérités : swap, redimensionnement, initialisation,
  caches et maintenance peuvent introduire de nouvelles écritures.

Avant de déclarer un chemin compatible, l'exercer avec une racine réellement en
lecture seule, les montages prévus et les restrictions systemd effectives.
Couvrir le premier démarrage et l'activation des fonctions optionnelles ; un
simple import, une analyse syntaxique d'unité ou un test sur l'hôte ne valide pas
tout un service. Si cet essai est indisponible, poursuivre les vérifications
possibles et signaler la validation manquante. Distinguer inspection et exécution
sur le matériel.
Les journaux sont volatils : les recueillir avant l'arrêt, ou préparer leur
collecte sur `/data` pour un diagnostic par carte SD.
