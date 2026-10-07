{ pkgs, target, version ? "dev-local", kernelPackages }:
let
  inherit (pkgs) lib;
  lock = builtins.fromJSON (builtins.readFile ../image/sources.lock.json);
  board = lock.targets.${target};
  kernel = kernelPackages.kernel;
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

  # The compiler and prepared headers are those of the actual system kernel.
  driver = name: modules: overlay: kernel.stdenv.mkDerivation {
    pname = "tagtagtag-${name}";
    version = sourceVersion name;
    src = source name;
    patches = lib.optional (name == "sound") ./tagtagtag-mixer.patch;
    nativeBuildInputs = kernel.moduleBuildDependencies ++ [
      pkgs.buildPackages.dtc pkgs.buildPackages.kmod
    ];
    buildInputs = lib.optionals (name == "sound") [ pkgs.alsa-lib ];
    hardeningDisable = [ "pic" "format" ];
    enableParallelBuilding = true;
    buildPhase = ''
      runHook preBuild
      make -C ${kernel.dev}/lib/modules/${kernel.modDirVersion}/build \
        ${lib.concatStringsSep " " kernelPackages.kernelModuleMakeFlags} \
        M="$PWD" -j"$NIX_BUILD_CORES" modules
      dtc -@ -I dts -O dtb -o ${overlay}.dtbo ${overlay}-overlay.dts
      ${lib.optionalString (name == "sound") ''
        $CC -Wall -Werror tagtagtag-mixerd.c -lasound -lpthread -o tagtagtag-mixerd
      ''}
      runHook postBuild
    '';
    doCheck = native;
    checkPhase = if name == "sound" then ''
      $CC -Wall -Werror -DTEST tagtagtag-mixerd.c -lasound -lpthread -o tagtagtag-mixerd-test
      ./tagtagtag-mixerd-test
    '' else ''
      make check HOSTCC="$CC"
    '';
    installPhase = ''
      runHook preInstall
      for module in ${lib.concatStringsSep " " modules}; do
        # A successful compile alone is insufficient if a different kernel was used.
        test "$(modinfo -F vermagic "$module.ko" | cut -d ' ' -f1)" = ${lib.escapeShellArg kernel.modDirVersion}
        install -Dm644 "$module.ko" "$out/lib/modules/${kernel.modDirVersion}/extra/$module.ko"
      done
      install -Dm644 ${overlay}.dtbo "$out/overlays/${overlay}.dtbo"
      install -m644 ${overlay}-overlay.dts "$out/overlays/${overlay}-overlay.dts"
      ${lib.optionalString (name == "sound") ''
        install -Dm755 tagtagtag-mixerd "$out/bin/tagtagtag-mixerd"
        install -Dm644 mixer.conf.default "$out/share/tagtagtag-sound/mixer.conf.default"
        install -Dm644 60-tagtagtag-volume.rules "$out/lib/udev/rules.d/60-tagtagtag-volume.rules"
        install -Dm644 tagtagtag-mixerd.service "$out/lib/systemd/system/tagtagtag-mixerd.service"
        substituteInPlace "$out/lib/systemd/system/tagtagtag-mixerd.service" \
          --replace-fail /usr/local/sbin/tagtagtag-mixerd "$out/bin/tagtagtag-mixerd"
      ''}
      runHook postInstall
    '';
    passthru = { inherit kernel; kernelVersion = kernel.modDirVersion; };
    meta.platforms = lib.platforms.linux;
  };

  assets = pkgs.stdenvNoCC.mkDerivation {
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
    pname = "nabos";
    inherit version;
    src = ../services;
    vendorHash = "sha256-pi4qI/OI42QGgtkE36szhSl9rjChYptShmBcFbmGvJQ=";
    subPackages = [ "cmd/nabos" ];
    doCheck = native;
    env = { CGO_ENABLED = 0; GOARM = board.goarm; };
    ldflags = [ "-s" "-w" "-X main.version=${version}" ];
    nativeBuildInputs = [ pkgs.makeWrapper ];
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
      wrapProgram "$out/bin/nabos" \
        --set-default NABOS_SOUNDS_DIRS "${assets}/share/nabos/sounds:/data/nabos/media/sounds" \
        --set-default NABOS_CHOREOGRAPHIES_DIRS "${assets}/share/nabos/choreographies:/data/nabos/media/choreographies"
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

  device-core = pkgs.rustPlatform.buildRustPackage {
    pname = "device-core";
    version = sourceVersion "device_core";
    src = source "device_core";
    # Observe asynchronous fixture completion before checking its effects.
    patches = [ ./device-core-tests.patch ];
    postPatch = ''
      # The decoder fixture deliberately clears PATH; its interpreter must be absolute.
      substituteInPlace tests/audio.rs --replace-fail '#!/usr/bin/python3' \
        '#!${pkgs.buildPackages.python3}/bin/python3'
    '';
    cargoLock.lockFile = ./device-core.Cargo.lock;
    doCheck = native;
    # Test harnesses do not need the release profile's full LTO under QEMU.
    checkType = "debug";
    nativeBuildInputs = [ pkgs.makeWrapper ];
    nativeCheckInputs = [ dbusForTests ] ++ (with pkgs.buildPackages; [
      coreutils python3 openssh openssl rauc squashfsTools
    ]);
    postInstall = ''
      install -Dm644 LICENSE "$out/share/doc/device-core/LICENSE"
      install -m644 NOTICE "$out/share/doc/device-core/NOTICE"
      wrapProgram "$out/bin/device-core" \
        --prefix PATH : ${lib.makeBinPath [ pkgs.alsa-utils pkgs.mpg123 pkgs.wireplumber pkgs.openssh pkgs.rauc ]} \
        --set-default DEVICE_CORE_AUDIO_ROOTS "${assets}/share/nabos/sounds:/data/nabos/media/sounds"
    '';
  };

  sound = driver "sound" [ "snd-soc-wm8960" "snd-soc-max9759" ] "tagtagtag-sound";
  led = driver "led" [ "bcm2835-ws2812" ] "bcm2835-ws2812";

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
