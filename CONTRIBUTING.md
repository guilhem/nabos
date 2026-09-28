# Contribuer à NabOS

Le système courant est décrit dans le [README](README.md), son transport dans le [contrat MQTT v1](docs/protocol-v1.md), et ses images dans le [guide de build](docs/build.md).

## Boucle locale

Utiliser Go 1.27.1, Rust 1.98.1 et Mosquitto avec ses clients. Le cœur possède un mode `--simulate` pour vérifier le protocole sur un ordinateur sans matériel Nabaztag. Python reste nécessaire aux composants externes et à la fabrication d'U-Boot ; les outils et tests propres au dépôt sont en Go.

```sh
cargo fmt --manifest-path core/Cargo.toml --check
cargo clippy --locked --manifest-path core/Cargo.toml --all-targets -- -D warnings
cargo test --locked --manifest-path core/Cargo.toml
(cd services && go vet ./... && go test -race ./...)
(cd services && NABOS_INTEGRATION=1 go test -count=1 ./tests/integration)
```

Les réglages doivent rester lisibles par la version précédente après rollback. Les commandes MQTT sont identifiées, expirables et non retenues. Ne pas déplacer la temporisation des mouvements hors du cœur local.

## Modifications système

Les modules externes sont construits contre les en-têtes et symboles du noyau installé dans l'image. Vérifier les deux architectures. Une compilation ARMv7 ne valide pas le Zero ARMv6. Ne jamais installer ou compiler les dépendances sur l'appareil lors d'une mise à jour.

Pour modifier une source externe, mettre à jour sa révision et son SHA-256 dans `image/sources.lock.json`. Conserver les licences et les archives nécessaires à la reconstruction. Les dépendances Cargo et Go doivent avoir leurs fichiers de verrouillage à jour.

Les changements aux pilotes, au démarrage, au son ou au rollback requièrent aussi les essais de la [fiche matérielle](docs/release-checklist.md). Indiquer clairement dans la PR ce qui a été réellement essayé et ce qui attend le matériel.

Les images sur tags deviennent des brouillons de GitHub Releases. Leur publication suit la qualification matérielle ; l'approbation et le merge des PR restent des décisions distinctes.
