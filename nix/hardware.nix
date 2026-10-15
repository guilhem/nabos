{ config, lib, pkgs, nixos-hardware, nabosBoard, nabosPackages, ... }:
let
  packages = nabosPackages;
in
{
  boot.kernelPackages = pkgs.linuxPackagesFor ((pkgs.callPackage
    (nixos-hardware + "/raspberry-pi/common/kernel.nix") {
      rpiVersion = nabosBoard.rpi_version;
      # The vendor definition replaces kernelPatches; append the
      # appliance settings after its defaults through argsOverride.
      argsOverride.kernelPatches = with pkgs.kernelPatches; [
        bridge_stp_helper request_key_helper
      ] ++ config.boot.kernelPatches;
    }).overrideAttrs (old: {
      postConfigure = (old.postConfigure or "") + "\n" +
        lib.concatMapStringsSep "\n" (patch:
          lib.concatStringsSep "\n" (lib.mapAttrsToList
            (name: value: "grep -qxF " + lib.escapeShellArg
              (if value.tristate == "n" then "# CONFIG_${name} is not set"
               else "CONFIG_${name}=${value.tristate}") + " $buildRoot/.config")
            (patch.structuredExtraConfig or {})))
        (builtins.filter (patch: patch.name == "nabos-appliance") config.boot.kernelPatches);
    }));

  # Neither Zero board has a TPM; the RPi kernel lacks tpm-crb.
  boot.initrd.systemd.tpm2.enable = false;
  # SD controllers are built in; omit default PC storage/USB drivers.
  boot.initrd.includeDefaultModules = false;
  boot.initrd.availableKernelModules = [ "mmc_block" ];
  boot.kernelPatches = [{
    name = "nabos-appliance";
    patch = null;
    structuredExtraConfig = with lib.kernel; {
      WATCHDOG = yes; BCM2835_WDT = yes; WATCHDOG_HANDLE_BOOT_ENABLED = yes;
      GPIO_CDEV = yes; KEYBOARD_GPIO = module; INPUT_EVDEV = module;
      I2C = yes; I2C_CHARDEV = module; I2C_BCM2835 = module;
      SND = module; SND_SOC = module; SND_BCM2835_SOC_I2S = module;
      SND_SOC_WM8960 = no; SND_SOC_MAX9759 = no;
      LEDS_CLASS_MULTICOLOR = module; DMA_BCM2835 = yes;
      OVERLAY_FS_METACOPY = yes; OVERLAY_FS_REDIRECT_DIR = yes;
      SQUASHFS = module; SQUASHFS_ZSTD = yes; EROFS_FS = module; OVERLAY_FS = module;
    } // lib.optionalAttrs (!nabosBoard.arm64) {
      # These drivers use 64-bit division helpers unavailable in the ARMv6 kernel.
      # The Zero W uses BCM2835 I2C and has no RP1 peripheral controller.
      PWM_RP1 = no; VIDEO_RP1_CFE_DOWNSTREAM = no; I2C_DESIGNWARE_CORE = no;
    };
  }];
  boot.extraModulePackages = [ packages.sound packages.led ];
  boot.kernelModules = [ "i2c-dev" "bcm2835-ws2812" ];
  boot.blacklistedKernelModules = [ "snd_bcm2835" ];
  hardware.deviceTree = {
    enable = true;
    filter = "*${nabosBoard.dtb}";
    overlays = [
      { name = "bcm2835-ws2812"; dtboFile = "${packages.led}/overlays/bcm2835-ws2812.dtbo"; }
      { name = "nabos-profile"; dtsFile = ../image/nabos-overlay.dts; }
    ];
  };
  hardware.bluetooth.enable = false;
  hardware.firmware = [ pkgs.raspberrypiWirelessFirmware ];
  nixpkgs.config.allowUnfreePredicate = package:
    lib.getName package == "raspberrypi-wireless-firmware";
  services.udev = {
    packages = [ packages.sound ];
    extraRules = lib.replaceStrings [ "/usr/bin/chgrp" "/usr/bin/chmod" ]
      [ "${pkgs.coreutils}/bin/chgrp" "${pkgs.coreutils}/bin/chmod" ]
      (builtins.readFile ../image/rootfs/etc/udev/rules.d/60-nabos.rules);
  };
}
