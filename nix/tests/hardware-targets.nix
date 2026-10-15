{ pkgs, mkImage }:
let
  inherit (pkgs) lib;
  # Expectations are independent of the target table and module conditions.
  targets = {
    zero-armv6 = {
      system = "armv6l-linux";
      config = "armv6l-unknown-linux-gnueabihf";
      gcc = { arch = "armv6kz"; fpu = "vfpv2"; };
      dtb = "bcm2708-rpi-zero-w.dtb";
      kernelDefconfig = "bcmrpi_defconfig";
      kernelTarget = "zImage";
      ubootDefconfig = "rpi_0_w_defconfig";
      compatible = "nabos-native-rauc-zero-armv6";
      goarm = "6";
      voice = false;
      disabledDrivers = [ "PWM_RP1" "VIDEO_RP1_CFE_DOWNSTREAM" "I2C_DESIGNWARE_CORE" ];
      ubootGuards = [ "CONFIG_CPU_ARM1176=y" "CONFIG_USE_PRIVATE_LIBGCC=y" ];
    };
    zero2-arm64 = {
      system = "aarch64-linux";
      config = "aarch64-unknown-linux-gnu";
      dtb = "bcm2710-rpi-zero-2-w.dtb";
      kernelDefconfig = "bcm2711_defconfig";
      kernelTarget = "Image";
      ubootDefconfig = "rpi_arm64_defconfig";
      compatible = "nabos-native-rauc-zero2-arm64";
      goarm = "";
      voice = true;
      disabledDrivers = [ ];
      ubootGuards = [ ];
    };
  };
  kernelSettings = {
    WATCHDOG = "y"; BCM2835_WDT = "y"; WATCHDOG_HANDLE_BOOT_ENABLED = "y";
    GPIO_CDEV = "y"; KEYBOARD_GPIO = "m"; INPUT_EVDEV = "m";
    I2C = "y"; I2C_CHARDEV = "m"; I2C_BCM2835 = "m";
    SND = "m"; SND_SOC = "m"; SND_BCM2835_SOC_I2S = "m";
    SND_SOC_WM8960 = "n"; SND_SOC_MAX9759 = "n";
    LEDS_CLASS_MULTICOLOR = "m"; DMA_BCM2835 = "y";
    OVERLAY_FS_METACOPY = "y"; OVERLAY_FS_REDIRECT_DIR = "y";
    SQUASHFS = "m"; SQUASHFS_ZSTD = "y"; EROFS_FS = "m"; OVERLAY_FS = "m";
  };
  checkTarget = target: expected:
    let
      image = mkImage { buildSystem = "x86_64-linux"; inherit target; version = "hardware-test"; };
      inherit (image) packages;
      inherit (image.system) config;
      platform = image.system.pkgs.stdenv.hostPlatform;
      kernel = config.boot.kernelPackages.kernel;
      settings = kernel.configfile.moduleStructuredConfig.settings;
      requiredSettings = kernelSettings // lib.genAttrs expected.disabledDrivers (_: "n");
      release = lib.splitString "\n" config.environment.etc."nabos/release.env".text;
      check = message: condition: lib.assertMsg condition "${target}: ${message}";
    in
    assert check "host platform or ABI changed"
      (platform.system == expected.system && platform.config == expected.config
        && (!(expected ? gcc) || platform.gcc == expected.gcc));
    assert check "vendor kernel or board defconfig changed"
      (kernel.pname == "linux-rpi" && kernel.target == expected.kernelTarget
        && lib.hasInfix expected.kernelDefconfig kernel.configfile.buildPhase);
    assert check "kernel helper patches were dropped"
      (lib.all (name: lib.elem name (map (patch: patch.name) kernel.kernelPatches))
        [ "bridge-stp-helper" "request-key-helper" "nabos-appliance" ]);
    assert check "kernel driver settings or build-time guardrails changed"
      (lib.all (name:
        (settings.${name}.tristate or null) == requiredSettings.${name}
        && lib.hasInfix (if requiredSettings.${name} == "n"
          then "# CONFIG_${name} is not set" else "CONFIG_${name}=${requiredSettings.${name}}")
          kernel.postConfigure) (builtins.attrNames requiredSettings));
    assert check "initrd hardware module policy changed"
      (!config.boot.initrd.includeDefaultModules && !config.boot.initrd.systemd.tpm2.enable
        && lib.elem "mmc_block" config.boot.initrd.availableKernelModules
        && !lib.any (name: lib.elem name config.boot.initrd.availableKernelModules)
          [ "ahci" "sd_mod" "xhci_pci" "tpm-crb" ]);
    assert check "hardware drivers changed"
      (lib.all (name: lib.elem name config.boot.kernelModules) [ "i2c-dev" "bcm2835-ws2812" ]
        && lib.elem "snd_bcm2835" config.boot.blacklistedKernelModules
        && lib.all (package: lib.elem package config.boot.extraModulePackages) [ packages.sound packages.led ]);
    assert check "Device Tree selection or overlays changed"
      (config.hardware.deviceTree.enable && config.hardware.deviceTree.filter == "*${expected.dtb}"
        && map (overlay: overlay.name) config.hardware.deviceTree.overlays == [ "bcm2835-ws2812" "nabos-profile" ]);
    assert check "U-Boot board or ARMv6 safeguards changed"
      (packages.uboot.defconfig == expected.ubootDefconfig
        && lib.all (guard: lib.hasInfix guard packages.uboot.postConfigure) expected.ubootGuards
        && (expected.ubootGuards == [ ] ||
          (lib.hasInfix "CONFIG_USE_PRIVATE_LIBGCC=y" packages.uboot.extraConfig
            && lib.hasInfix "Tag_CPU_arch: v[4-6]" packages.uboot.postBuild
            && lib.hasInfix "Tag_THUMB_ISA_use: Thumb-2" packages.uboot.postBuild
            && lib.hasInfix "libgcc\\.a|libc\\.a" packages.uboot.postBuild)));
    assert check "Go ARM level changed" (packages.nabos.GOARM == expected.goarm);
    assert check "RAUC compatibility changed" (config.services.rauc.settings.system.compatible == expected.compatible);
    assert check "LVA package, service, firewall and release signaling disagree"
      ((packages.lva != null) == expected.voice
        && (config.systemd.services ? linux-voice-assistant) == expected.voice
        && lib.elem 6053 config.networking.firewall.allowedTCPPorts == expected.voice
        && lib.elem ("DEVICE_CORE_LVA_UNIT=" + (if expected.voice then "linux-voice-assistant.service" else "")) release
        && lib.elem "DEVICE_CORE_UPDATE_ASSET=nabos-${target}.raucb" release);
    true;
in
assert lib.all (result: result) (lib.mapAttrsToList checkTarget targets);
pkgs.runCommand "nabos-hardware-targets" { } ''
  touch "$out"
''
