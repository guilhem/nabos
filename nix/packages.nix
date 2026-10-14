{ pkgs, target, kernelPackages, device-core, wm8960, bcm2835-ws2812 }:
let
  inherit (pkgs) lib;
  lock = builtins.fromJSON (builtins.readFile ../image/sources.lock.json);
  board = lock.targets.${target};
  source = name: pkgs.fetchurl {
    name = "${name}-${sourceVersion name}.tar.gz";
    inherit (lock.sources.${name}) url sha256;
  };
  sourceVersion = name: builtins.substring 0 12 lock.sources.${name}.commit;
  native = pkgs.stdenv.buildPlatform.canExecute pkgs.stdenv.hostPlatform;

  # Fixture buses need the session configuration shipped by Nixpkgs, since the
  # build sandbox has no NixOS /etc/dbus-1. This wrapper is only a check input.
  dbusForTests = pkgs.buildPackages.writeShellScriptBin "dbus-daemon" ''
    args=()
    for arg in "$@"; do
      if [ "$arg" = --session ]; then
        args+=(--config-file=${pkgs.buildPackages.dbus}/share/dbus-1/session.conf)
      else
        args+=("$arg")
      fi
    done
    exec ${pkgs.buildPackages.dbus}/bin/dbus-daemon "''${args[@]}"
  '';

  deviceCore = pkgs.callPackage (device-core + "/nix/package.nix") {
    src = device-core;
  };

  assets = pkgs.buildPackages.stdenvNoCC.mkDerivation {
    name = "nabos-assets";
    src = ../assets;
    dontBuild = true;
    installPhase = ''
      mkdir -p "$out/share/nabos"
      cp -a sounds choreographies "$out/share/nabos/"
    '';
  };
in
assert builtins.elem target [ "zero-armv6" "zero2-arm64" ];
{
  inherit assets;
  nabos = pkgs.buildGo127Module {
    name = "nabos";
    src = ../services;
    vendorHash = "sha256-pi4qI/OI42QGgtkE36szhSl9rjChYptShmBcFbmGvJQ=";
    subPackages = [ "cmd/nabos" ];
    doCheck = native;
    env = { CGO_ENABLED = 0; GOARM = board.goarm; };
    ldflags = [ "-s" "-w" ];
    nativeCheckInputs = [ dbusForTests ];
    preCheck = ''
      # NewApp reads /etc/machine-id directly, which the Nix sandbox lacks.
      # Go's source overlay changes only the test build; the installed binary
      # was already built from the unmodified source in buildPhase.
      printf '%s\n' 0123456789abcdef0123456789abcdef > "$TMPDIR/machine-id"
      cp cmd/nabos/app.go "$TMPDIR/app-test.go"
      substituteInPlace "$TMPDIR/app-test.go" \
        --replace-fail 'os.ReadFile("/etc/machine-id")' "os.ReadFile(\"$TMPDIR/machine-id\")"
      printf '{"Replace":{"%s":"%s"}}\n' \
        "$PWD/cmd/nabos/app.go" "$TMPDIR/app-test.go" > "$TMPDIR/go-test-overlay.json"
      export GOFLAGS="$GOFLAGS -overlay=$TMPDIR/go-test-overlay.json"
    '';
    postInstall = ''
      install -Dm644 ${../LICENSE} "$out/share/doc/nabos/LICENSE"
      install -m644 ${../NOTICE} "$out/share/doc/nabos/NOTICE"
    '';
  };

  nab-hardware = pkgs.rustPlatform.buildRustPackage {
    pname = "nab-hardware";
    version = (builtins.fromTOML (builtins.readFile ../core/Cargo.toml)).package.version;
    src = ../core;
    cargoLock = {
      lockFile = ../core/Cargo.lock;
      outputHashes = {
        "cr14-0.1.0" = "sha256-AbFVP7h/uxx6cUiMYZSlupUp9c1Z0Uu2S+4w4nn1EQs=";
        "st25r391x-0.1.0" = "sha256-C0b4+6GjfQs7te9eCFWlDItVrefve3lzVkhNDvWxcoM=";
      };
    };
    doCheck = native;
    nativeCheckInputs = [ dbusForTests pkgs.buildPackages.coreutils ];
    postInstall = ''
      install -Dm644 ${../LICENSE} "$out/share/doc/nab-hardware/LICENSE"
      install -m644 ${../NOTICE} "$out/share/doc/nab-hardware/NOTICE"
    '';
  };

  device-core = pkgs.symlinkJoin {
    name = "device-core-nabos";
    inherit (deviceCore) meta;
    paths = [ deviceCore ];
    nativeBuildInputs = [ pkgs.makeWrapper ];
    postBuild = ''
      wrapProgram "$out/bin/device-core" \
        --prefix PATH : ${lib.makeBinPath [ pkgs.alsa-utils pkgs.mpg123 pkgs.wireplumber pkgs.openssh pkgs.rauc ]} \
        --set-default DEVICE_CORE_AUDIO_ROOTS "${assets}/share/nabos/sounds:/data/nabos/media/sounds"
    '';
  };

  sound = pkgs.callPackage (wm8960 + "/nix/package.nix") {
    src = wm8960;
    inherit kernelPackages;
  };
  led = pkgs.callPackage (bcm2835-ws2812 + "/nix/package.nix") {
    src = bcm2835-ws2812;
    inherit kernelPackages;
  };

  uboot = pkgs.buildUBoot {
    version = lock.tools.uboot;
    src = source "uboot";
    defconfig = board.uboot_defconfig;
    extraConfig = builtins.readFile ../image/boot/uboot.config
      + lib.optionalString (target == "zero-armv6") "\nCONFIG_USE_PRIVATE_LIBGCC=y\n";
    filesToInstall = [ "u-boot.bin" ];
    postConfigure = ''
      make olddefconfig
      while IFS= read -r option; do
        case "$option" in
          CONFIG_*|'# CONFIG_'*) grep -qxF "$option" .config ;;
        esac
      done < ${../image/boot/uboot.config}
      ${lib.optionalString (target == "zero-armv6") ''
        grep -qxF CONFIG_CPU_ARM1176=y .config
        grep -qxF CONFIG_USE_PRIVATE_LIBGCC=y .config
      ''}
    '';
    postBuild = ''
      $READELF -h -A -l -d u-boot > u-boot.elf.txt
      if grep -Eq 'INTERP|\(NEEDED\)' u-boot.elf.txt; then
        echo 'U-Boot must be freestanding' >&2; exit 1
      fi
      ${lib.optionalString (target == "zero-armv6") ''
        grep -Eq 'Tag_CPU_arch: v[4-6]([^0-9]|$)' u-boot.elf.txt
        if grep -Eq 'Tag_CPU_arch: v([7-9]|[1-9][0-9])|Tag_THUMB_ISA_use: Thumb-2' u-boot.elf.txt; then
          echo 'U-Boot requires instructions newer than ARMv6' >&2; exit 1
        fi
        if grep -Eq 'libgcc\.a|libc\.a' u-boot.map; then exit 1; fi
      ''}
    '';
    postInstall = ''
      install -Dm644 .config "$out/share/nabos/uboot.config"
      install -m644 u-boot.elf.txt "$out/share/nabos/uboot.elf.txt"
    '';
  };

  # Host-native hush execution tests use the same locked U-Boot source.
  uboot-sandbox = pkgs.buildPackages.buildUBoot {
    version = lock.tools.uboot;
    src = source "uboot";
    defconfig = "sandbox_defconfig";
    extraConfig = ''
      # CONFIG_SANDBOX_SDL is not set
      # CONFIG_TOOLS_MKEFICAPSULE is not set
      # CONFIG_UNIT_TEST is not set
      # CONFIG_EFI_CAPSULE_AUTHENTICATE is not set
      # CONFIG_EFI_CAPSULE_ON_DISK is not set
      # CONFIG_CMD_UPL is not set
      # CONFIG_UPL is not set
    '';
    postConfigure = "make olddefconfig";
    extraMakeFlags = [ "CONFIG_PYLIBFDT=" ];
    buildPhase = ''
      runHook preBuild
      make "''${makeFlags[@]}" "''${makeFlagsArray[@]}" -j"$NIX_BUILD_CORES" u-boot tools
      runHook postBuild
    '';
    filesToInstall = [ "u-boot" "tools/mkimage" "tools/mkenvimage" ];
    installDir = "$out/bin";
    postInstall = ''
      ln -s ${pkgs.buildPackages.dtc}/bin/dtc "$out/bin/dtc"
    '';
    # Hash the final binaries after stripping and ELF fixups.
    postFixup = ''
      cd "$out"
      sha256sum bin/u-boot bin/dtc bin/mkimage bin/mkenvimage > SHA256SUMS
    '';
  };

  lva = if target == "zero-armv6" then null else (pkgs.callPackage ./lva.nix {
    src = source "lva";
    version = lock.tools.lva;
  }).overrideAttrs (old: {
    preFixup = (old.preFixup or "") + ''
      # QEMU can lose the Python wrapper's argv0-based dependency path.
      export PYTHONPATH="${pkgs.buildPackages.python3Packages.pyelftools}/${pkgs.buildPackages.python3.sitePackages}''${PYTHONPATH:+:$PYTHONPATH}"
    '';
  });
}
