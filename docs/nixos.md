# Prototype NixOS + Cachix

Cette voie construit un prototype NabOS pour `zero-armv6` et `zero2-arm64`.
Le constructeur Raspberry Pi OS décrit dans [build.md](build.md) reste disponible.
Le passage au prototype demande un **nouveau flash de la carte SD** ; aucune
migration depuis le système existant n’est prévue. Sauvegarder les données avant
de reflasher.

## Construire et vérifier

Installer Nix sur un hôte Linux jetable disposant de suffisamment d’espace pour
les closures, la racine de 6 Gio, le disque et la compression. Utiliser x86-64
pour la compilation croisée ARMv6 et ARM64 natif pour Zero 2. Les scripts activent
explicitement `nix-command flakes` et utilisent `build/nix-tmp` pour les temporaires.

```sh
# Sur x86-64 Linux :
bash image/nix-build.sh zero-armv6 dev-nixos --development
# Sur ARM64 Linux :
bash image/nix-build.sh zero2-arm64 dev-nixos --development
```

`flake.lock` verrouille Nixpkgs à
`151fa4e8ddfdd8dd25d945ad94ed54a13de9f6e4` et nixos-hardware à
`31cc5f4d9b9ba601071e8b8504601b9b176e2756` pour ce premier prototype.
Le fichier livré avec chaque construction est la référence pour ses entrées.
Nix produit le payload `rootfs.ext4`, `boot.vfat`, `uboot.env`, `boot.cmd`,
`build.json` et `cache-roots`. L’assembleur signe le bundle **hors du store Nix**,
prépare la partition de données puis génère et compresse le disque SD.

Dans `dist/nixos/<cible>/` :

| Fichier | Usage |
|---|---|
| `nabos-<cible>.img.xz` | Premier flash SD |
| `nabos-<cible>.raucb` | Mise à jour RAUC du prototype |
| `ca.cert.pem` | Certificat public de signature utilisé |
| `flake.lock`, `boot.cmd` | Entrées verrouillées et script de démarrage livré |
| `cache-roots` | Toplevel système puis U-Boot, un chemin store par ligne |
| `build-<cible>.json` | Version, cible, révisions et temps du payload Nix |
| `SHA256SUMS-<cible>` | Empreintes des artefacts livrés |

```sh
cd dist/nixos/zero-armv6
sha256sum -c SHA256SUMS-zero-armv6
```

La construction exécute `image/nix-test.sh` avant compression. Ces contrôles
vérifient le partitionnement MBR, le contenu de la racine et du démarrage,
l’initrd, les propriétaires, les checksums du bundle et sa signature, sans
modifier les originaux. `--defer-tests` réserve les contrôles à une étape
ultérieure explicite ; il ne constitue pas une validation.
Pour les fichiers non compressés conservés dans l’espace de travail affiché :

```sh
# Remplacer WORK par le chemin affiché par l’assembleur.
bash image/nix-test.sh zero-armv6 \
  WORK/images/sdcard.img WORK/images/rootfs.ext4 WORK/images/boot.vfat \
  dist/nixos/zero-armv6/nabos-zero-armv6.raucb \
  dist/nixos/zero-armv6/ca.cert.pem
```

## Deux chaînes de confiance

La clé publique Cachix permet à **l’hôte constructeur** de vérifier les binaires
Nix téléchargés. Elle ne donne pas au lapin l’autorisation d’installer un bundle.
La confiance RAUC repose sur le certificat dans `/data/rauc/ca.cert.pem`, amorcé
dans la partition de données du premier disque SD. Les mises à jour normales
remplacent les slots système/démarrage et préservent cette partition et son CA.

`--development` crée une autorité temporaire valable sept jours. Le lapin ainsi
flashé rejette les bundles de production signés par une autre autorité. Pour une
chaîne durable, conserver la même autorité RAUC et fournir les fichiers PEM :

```sh
RAUC_KEY=/chemin/prive/key.pem RAUC_CERT=/chemin/public/cert.pem \
  bash image/nix-build.sh zero-armv6 v2.0.0
```

La clé privée ne doit jamais devenir une entrée de dérivation ni un artefact.
Le workflow prototype utilise uniquement des signatures de développement ;
il ne publie pas de release et n’utilise pas les secrets de signature RAUC.

## Lire et alimenter Cachix

La lecture de `https://nabos.cachix.org` est publique et anonyme. Ajouter sur
l’hôte constructeur les lignes suivantes à la configuration Nix appropriée
(`nix.conf`, avec un utilisateur autorisé à définir les substituters) :

```ini
extra-substituters = https://nabos.cachix.org
extra-trusted-public-keys = nabos.cachix.org-1:jLoce+DvPr6ejhFfvmEKXznQLVKxZ6zCP5N7dirR/JQ=
```

La CI utilise `cachix-action` en lecture seule (`skipPush: true`). Après succès,
une étape distincte transmet à `cachix push nabos` les seuls chemins de
`cache-roots`. Le client publie leurs dépendances d’exécution et exclut les
chemins disponibles dans le cache officiel. Le payload, les images SD, les
bundles, les certificats et le dev shell ne sont pas des racines de publication.
Ni `watch-store`, ni publication globale de `/nix/store` n’est utilisée.
Voir les références du [client Cachix](https://docs.cachix.org/pushing) et de
[cachix-action](https://github.com/cachix/cachix-action).

Les écritures sont réservées aux exécutions manuelles : mode `build` sur `main`,
ou mode `benchmark` sur la branche choisie. Ce dernier publie ses closures pour
mesurer la reprise avant merge, avec la variable GitHub `CACHIX_CACHE=nabos` et
le secret `CACHIX_AUTH_TOKEN`.
Le token est transmis uniquement à l’étape de publication. Aucun job de PR,
y compris les forks, ne reçoit ce secret. L’absence du token ou une valeur de
cache incorrecte fait échouer la publication et empêche de lancer la reprise.

## Mesures bornées

Le workflow `.github/workflows/nixos.yml` effectue sur les pushes/PR uniquement
la vérification du parseur, la syntaxe des scripts et le parse/eval des deux
configurations, avec les imports dépendant d’une construction interdits.
Les constructions complètes se lancent manuellement sur toute branche du dépôt, mode `build`,
avec `ubuntu-24.04` pour ARMv6 et `ubuntu-24.04-arm` pour ARM64.
Une branche de prototype peut donc produire ses artefacts avant merge ; hors
`main`, le mode `build` ignore la publication Cachix et ne reçoit aucun token.

Le mode manuel `benchmark`, qui amorce explicitement le cache, construit
**seulement les closures système/U-Boot** :

| Phase | Entrées et environnement |
|---|---|
| `cold` | Runner neuf ; cache NabOS désactivé, cache officiel NixOS autorisé |
| `warm` | Même commit/version/pin sur un nouveau runner, après publication vérifiée de `cold` |
| `version` | Runner neuf, même pin, version applicative suffixée `.next` |
| `nixpkgs` | Facultatif : runner neuf, version initiale, commit Nixpkgs explicite fourni dans `nixpkgs_rev` |

Chaque cible possède un job `cold`. Les jobs suivants attendent le succès de
la publication et la présence de toutes les dépendances dans au moins un cache
public. Ils ne restaurent aucun store Nix via `actions/cache` et ne téléchargent
pas les artefacts du job précédent. Aucune image de 6 Gio n’est réassemblée
pour ces phases. Sans nouveau pin explicite, la phase `nixpkgs` n’est pas lancée.
Le `.next` est un changement de version, pas une simulation de changement de
logique métier ou de dépendance Go/Rust.

L’outil local permet de répéter une phase sur un hôte jetable :

```sh
bash image/nix-benchmark.sh cold zero-armv6 dev-nixos
# Sur un AUTRE hôte neuf, après publication du cold :
bash image/nix-benchmark.sh warm zero-armv6 dev-nixos
```

Chaque phase produit sous `dist/nixos/measurements/<cible>/<phase>/`
`build-<cible>.json`, `flake.lock`, `outputs.json`, `build.log` et
`cache-roots.txt`. `build_seconds` y mesure la réalisation des deux closures.
Dans le rapport de l’image complète, `build_seconds` mesure le **payload Nix**,
qui inclut la génération ext4/FAT. Ces deux scopes ne sont pas comparables
directement ; signature, assemblage SD, tests et compression sont hors de ces
chronomètres. Le temps des étapes GitHub complète la mesure de bout en bout.

```sh
python3 image/nix-cache-report.py --self-test
python3 image/nix-cache-report.py dist/nixos/zero-armv6/cache-roots \
  --output dist/nixos/zero-armv6/cache-report.json
# Après publication explicite des racines :
python3 image/nix-cache-report.py dist/nixos/zero-armv6/cache-roots \
  --require-published --output dist/nixos/zero-armv6/cache-after-push.json
```

Le rapport interroge la closure locale avec `nix path-info --recursive --json`,
puis les métadonnées publiques `.narinfo` des caches officiel et NabOS :

- `closure_nar_bytes` additionne les `narSize` des chemins store **distincts**.
  Cette taille NAR n’est ni la taille ext4, ni une taille compressée Cachix.
- `upstream_missing_path_count` compte les chemins absents du cache officiel.
- `cachix_compressed_bytes` additionne les `FileSize` effectivement publiés,
  en dédupliquant par URL de fichier NAR. Les fichiers absents ne sont jamais
  estimés à partir de `narSize` ; `unpublished_paths` et `publication_complete`
  indiquent si la closure est disponible dans les caches publics.

La CI conserve les rapports avant/après publication. Une erreur réseau ou des
métadonnées incohérentes fait échouer la mesure ; seul un HTTP 404 représente
une absence. Pour additionner les deux cibles ou plusieurs versions, fusionner
leurs rapports **après publication**, sans additionner naïvement leurs totaux.
La fusion réinterroge les caches : `previously_cached_missing_paths` révèle les
objets qui ont disparu depuis les rapports précédents (éviction ou suppression),
y compris ceux de la première cible après publication de la seconde :

```sh
python3 image/nix-cache-report.py --merge \
  dist/nixos/zero-armv6/cache-after-push.json \
  dist/nixos/zero2-arm64/cache-after-push.json \
  --output dist/nixos/cache-union.json
```

Cette union mesure les fichiers référencés par ces closures à l’instant de la
mesure, pas tout le compte Cachix ni sa facturation. La rétention des anciennes
versions augmente l’occupation ; les toolchains de compilation croisée ne sont
pas publiées par défaut. Leur ajout doit répondre à des recompilations coûteuses
observées dans `build.log`, avec une mesure de leur coût compressé. Aucune
compatibilité avec une enveloppe de 5 Go n’est annoncée avant ces mesures réelles.
Le job final `report` refait cette mesure après toutes les publications et livre
`cache-union.json`, même si des objets nécessaires ont été évincés.

## Limites de qualification

Les pins, checksums et entrées verrouillées rendent les entrées inspectables ;
aucune reproductibilité bit à bit des images ou signatures n’est promise.
Les contrôles d’artefacts ne démarrent pas le lapin et ne prouvent pas le
fonctionnement avec 512 Mio de RAM. Les deux cibles doivent être qualifiées sur
matériel, racine réellement en lecture seule, montages persistants et restrictions
systemd effectives : premier démarrage, Wi-Fi, audio, LED, oreilles/NFC, fonctions
optionnelles dont la voix sur ARM64, consommation mémoire, installation RAUC,
confirmation de slot, redémarrage et repli A/B. Recueillir les journaux volatils
avant arrêt ou prévoir une collecte sur `/data`.

Aucune construction CI complète, durée, occupation Cachix ou qualification
matérielle n’est attestée par ce document. Les preuves doivent citer le commit,
le runner, les rapports et, pour le matériel, la carte et le protocole d’essai.
