# Contribuer à NabOS

Le système courant est décrit dans le [README](README.md), ses [rôles et contrats D-Bus](docs/protocol-v1.md), et ses images dans le [guide de build](docs/build.md).

## Boucle locale

Utiliser Go 1.27.1, Rust 1.98.1 ; Mosquitto et ses clients servent aux fixtures Home Assistant. `nab-hardware` et le binaire externe `device-core` possèdent un mode `--simulate`. Fournir un bus privé explicite (`NABOS_DEVICE_BUS_ADDRESS` pour nab-hardware et nabos, `DEVICE_CORE_BUS_ADDRESS` pour device-core) ; ils ne doivent pas modifier les services de l’hôte. Python reste nécessaire aux composants externes et à la fabrication d'U-Boot ; les outils et tests propres au dépôt sont en Go.

```sh
cargo fmt --manifest-path core/Cargo.toml --check
cargo clippy --locked --manifest-path core/Cargo.toml --all-targets -- -D warnings
cargo test --locked --manifest-path core/Cargo.toml
(cd services && go vet ./... && go test -race ./...)
(cd services && NABOS_INTEGRATION=1 go test -race -count=1 -skip '^TestEndToEnd$' ./tests/integration ./cmd/nabos)
```

Les réglages Linux appartiennent à device-core (`/data/device-core/settings.json`) ; Go ne conserve que les applications (`/data/nabos/application.json`). Cette extraction ne fournit ni migration ni rétrocompatibilité des anciens formats ou API. Go possède les états, médias et chorégraphies ; Rust conserve uniquement le matériel et la temporisation des pilotes. Respecter le [contrat hardware D-Bus](docs/hardware-dbus.md).

## Modifications système

Les modules externes sont construits contre les en-têtes et symboles du noyau installé dans l'image. Vérifier les deux architectures. Une compilation ARMv7 ne valide pas le Zero ARMv6. Ne jamais installer ou compiler les dépendances sur l'appareil lors d'une mise à jour.

Nix utilise exclusivement l’archive device-core du commit verrouillé dans `image/sources.lock.json`. `nix build .#device-core-native` construit le paquet natif pour les tests ; `make image TARGET=... VERSION=... DEVELOPMENT=1` construit une image de développement avec ses composants.

Pour modifier une source externe, mettre à jour sa révision et son SHA-256 dans `image/sources.lock.json`. Conserver les licences et les archives nécessaires à la reconstruction. Les dépendances Cargo et Go doivent avoir leurs fichiers de verrouillage à jour.

La racine de l’appareil est en lecture seule. Lire `nix/runtime/initrd-persist.sh`, `nix/runtime/persist.sh`, `nix/system.nix`, les unités des paquets et leurs overrides avant toute modification système. Les données vont dans `/data`, le home LVA dans `/var/lib/nabos/lva`, le home SSH dans `/var/lib/nabos/admin`, les temporaires dans `/run` ; tester les restrictions effectives et le premier démarrage, puis signaler tout essai matériel manquant. Ne jamais remplacer ou supprimer le verrou `/run/lock/device-core/network` lors d’un redémarrage du daemon.

Les changements aux pilotes, au démarrage, au son ou au rollback requièrent aussi les essais de la [fiche matérielle](docs/release-checklist.md). Indiquer clairement dans la PR ce qui a été réellement essayé et ce qui attend le matériel.

Le mainteneur crée la GitHub Release avec son statut et son changelog ; sa publication lance la CI, qui y ajoute les images signées sans modifier ces informations. La [procédure de release](docs/build.md#créer-une-release) décrit aussi le cas des brouillons. Le passage en stable suit la qualification matérielle ; l'approbation et le merge des PR restent des décisions distinctes.

Les tests natifs de device-core nécessitent les credentials D-Bus `ProcessFD`
et systemd `GetUnitByPIDFD` (systemd 255 ou plus récent) ; utiliser les paquets
Nix épinglés, comme la CI. Sans descripteur, l’autorisation est refusée ; aucun repli sur les PID.
