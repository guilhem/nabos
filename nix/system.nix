{ config, lib, pkgs, modulesPath, nabosTarget, nabosVersion, nabosPackages, ... }:
let
  inherit (lib) mkForce mkIf;
  rootfs = ../image/rootfs;
  packages = nabosPackages;
  hasVoice = nabosTarget == "zero2-arm64" && packages.lva != null;
  lock = builtins.fromJSON (builtins.readFile ../image/sources.lock.json);
  compatible = lock.targets.${nabosTarget}.compatible;
  persistLibrary = pkgs.writeText "nabos-persist-library" (builtins.readFile ./runtime/persist.sh);
  tools = with pkgs; [ coreutils util-linux e2fsprogs gnugrep gawk systemd curl
    ubootTools rauc openssh alsa-utils mpg123 wireplumber pulseaudio ];
  stateSeeds = pkgs.runCommand "nabos-persistent-defaults" { } ''
    mkdir -p $out/etc/NetworkManager/system-connections $out/var/lib/{NetworkManager,systemd/timesync,systemd/linger,tagtagtag-sound,nabos/lva}
    cp ${rootfs}/var/lib/NetworkManager/NetworkManager.state $out/var/lib/NetworkManager/
    cp ${packages.sound}/share/tagtagtag-sound/mixer.conf.default $out/var/lib/tagtagtag-sound/mixer.conf.default
    cp $out/var/lib/tagtagtag-sound/mixer.conf.default $out/var/lib/tagtagtag-sound/mixer.conf
    touch $out/var/lib/systemd/linger/nab-audio
    chmod 0700 $out/etc/NetworkManager/system-connections $out/var/lib/NetworkManager
    chmod 0711 $out/var/lib/nabos
  '';
  persist = pkgs.writeShellScript "nabos-initrd-persist" ''
    export PATH=${lib.makeBinPath [ pkgs.coreutils pkgs.util-linux pkgs.e2fsprogs pkgs.gnugrep pkgs.findutils ]}
    export NABOS_PERSIST_LIB=${persistLibrary}
    export NABOS_STATE_SEEDS=${stateSeeds}
    ${builtins.readFile ./runtime/initrd-persist.sh}
  '';
  health = pkgs.writeShellScript "nabos-health" ''
    export PATH=${lib.makeBinPath tools}
    ${lib.replaceStrings
      [ "/usr/lib/nabos/persist.sh" ] [ "${persistLibrary}" ]
      (builtins.readFile (rootfs + "/usr/lib/nabos/health"))}
  '';
  manual = pkgs.writeShellScript "nabos-rauc-manual" ''
    export PATH=${lib.makeBinPath (tools ++ [ pkgs.openssl ])}
    ${builtins.readFile (rootfs + "/usr/lib/nabos/rauc-manual")}
  '';
  postInstall = pkgs.writeShellScript "nabos-rauc-post-install" ''
    export PATH=${lib.makeBinPath [ pkgs.coreutils ]}
    ${builtins.readFile (rootfs + "/usr/lib/nabos/rauc-post-install")}
  '';
  # Reuse the complete source units, including their confinement and dependencies.
  runtimeUnits = pkgs.runCommand "nabos-runtime-units" { } ''
    mkdir -p $out/lib/systemd/system
    cp ${rootfs}/usr/lib/systemd/system/*.service $out/lib/systemd/system/
    substituteInPlace $out/lib/systemd/system/*.service \
      --replace /usr/bin/nabos ${packages.nabos}/bin/nabos \
      --replace /usr/bin/nab-hardware ${packages.nab-hardware}/bin/nab-hardware \
      --replace /usr/bin/device-core ${packages.device-core}/bin/device-core \
      --replace /usr/bin/grep ${pkgs.gnugrep}/bin/grep \
      --replace /bin/sh ${pkgs.runtimeShell} \
      --replace /usr/lib/nabos/health ${health} \
      --replace /usr/lib/nabos/rauc-manual ${manual}
    ${if hasVoice then ''
      substituteInPlace $out/lib/systemd/system/linux-voice-assistant.service \
        --replace-fail /opt/linux-voice-assistant/.venv/bin/python ${packages.lva}/bin/linux-voice-assistant \
        --replace-fail /opt/linux-voice-assistant ${packages.lva}/share/linux-voice-assistant \
        --replace-fail ' -m linux_voice_assistant' ""
    '' else ''
      rm $out/lib/systemd/system/linux-voice-assistant.service
    ''}
  '';
  dbusPolicies = pkgs.runCommand "nabos-dbus-policies" { } ''
    mkdir -p $out/share/dbus-1/system.d
    cp ${rootfs}/etc/dbus-1/system.d/io.github.guilhem.*.conf $out/share/dbus-1/system.d/
    cp ${rootfs}/etc/dbus-1/system.d/nabos-rauc.conf $out/share/dbus-1/system.d/zz-nabos-rauc.conf
  '';
  raucSettings = {
    system = {
      inherit compatible;
      bootloader = "uboot";
      boot-attempts = 3;
      boot-attempts-primary = 3;
      data-directory = "/data/rauc";
      mountprefix = "/run/rauc/mnt";
      bundle-formats = "-plain +verity";
    };
    keyring = { path = "/data/rauc/ca.cert.pem"; directory = "/run/nabos-rauc-trust"; };
    handlers.post-install = "${postInstall}";
    "slot.rootfs.0" = { device = "/dev/mmcblk0p2"; type = "ext4"; bootname = "A"; };
    "slot.rootfs.1" = { device = "/dev/mmcblk0p3"; type = "ext4"; bootname = "B"; };
    # The native slot type enum does not include boot-mbr-switch.
    "slot.bootloader.0" = {
      device = "/dev/mmcblk0";
      type = "boot-mbr-switch";
      region-start = "4M";
      region-size = "512M";
      install-same = false;
    };
  };
  fixedUser = uid: home: groups: {
    inherit uid home;
    isSystemUser = true;
    createHome = false;
    group = (builtins.elemAt [ "nab-app" "nab-hardware" "device-core" "nab-audio" ] (uid - 1001));
    extraGroups = groups;
    hashedPassword = "!";
  };
in
{
  imports = [
    "${modulesPath}/profiles/image-based-appliance.nix"
    {
      # buildEnv otherwise requests meta.outputsToInstall (including manuals),
      # even though the appliance never links their directories into its profile.
      options.system.path = lib.mkOption {
        apply = path: path.override {
          paths = map (package: package // {
            meta = (package.meta or { }) // {
              outputsToInstall = builtins.filter
                (output: !(builtins.elem output [ "man" "info" "doc" "devman" "devinfo" "devdoc" ]))
                (package.meta.outputsToInstall or [ (package.outputName or "out") ]);
            };
          }) config.environment.systemPackages;
        };
      };
    }
  ];
  disabledModules = [ "profiles/base.nix" ];
  assertions = [
    { assertion = builtins.elem nabosTarget [ "zero-armv6" "zero2-arm64" ]; message = "Unknown NabOS target"; }
    { assertion = nabosTarget != "zero-armv6" || packages.lva == null; message = "LVA is ARM64-only"; }
  ];
  nixpkgs.overlays = [ (_final: previous: {
    # Audio consumers use the C API; avoid building a Fortran toolchain.
    fftw = (previous.fftw.override { withDoc = false; }).overrideAttrs (old: {
      nativeBuildInputs = [ ];
      configureFlags = old.configureFlags ++ [ "--disable-fortran" ];
    });
  } // lib.optionalAttrs (nabosTarget == "zero-armv6") {
    # Omit unused RDMA capture and documentation that fail ARMv6 cross builds.
    libpcap = previous.libpcap.override { withRdma = false; };
    orc = previous.orc.override { buildDevDoc = false; };
  }) ];
  system.stateVersion = "26.05";
  system.nixos.label = "nabos-${nabosVersion}";
  system.nixos-init.enable = true;
  system.activatable = false;
  # The initrd discards references; also ship defaults in the root closure.
  system.extraDependencies = [ stateSeeds ];
  # Image assembly precreates these symlinks on the read-only root.
  environment.binsh = null;
  environment.usrbinenv = null;
  system.etc.overlay = { enable = true; mutable = false; };
  services.userborn = { enable = true; static = true; };
  # Only suppress the build-time lockout assertion: SSH keys are managed by
  # device-core, while every account password remains locked.
  users.allowNoPasswordLogin = true;
  systemd.sysusers.enable = false;
  users.users = {
    root.hashedPassword = "!";
    nabos = {
      uid = 1000; group = "nabos"; isNormalUser = true;
      home = "/var/lib/nabos"; createHome = false; shell = pkgs.bashInteractive;
      hashedPassword = "!"; autoSubUidGidRange = false;
    };
    nab-app = fixedUser 1001 "/data/nabos" [ "nab-media" ];
    nab-hardware = fixedUser 1002 "/run/nab-hardware" [ "gpio" ];
    device-core = fixedUser 1003 "/data/device-core" [ "nab-media" "nab-audio" ];
    nab-audio = fixedUser 1004 "/var/lib/nabos/lva" [ "audio" ];
  };
  users.groups = {
    nabos.gid = 1000; nab-app.gid = 1001; nab-hardware.gid = 1002;
    device-core.gid = 1003; nab-audio.gid = 1004; nab-media.gid = 1005; gpio.gid = 1006;
  };
  security.sudo.extraRules = [{ users = [ "nabos" ]; commands = [{ command = "ALL"; options = [ "NOPASSWD" ]; }]; }];

  boot.loader.grub.enable = false;
  boot.loader.systemd-boot.enable = false;
  boot.loader.external = { enable = true; installHook = pkgs.writeShellScript "nabos-image-bootloader" "exit 0"; };
  boot.initrd.systemd.enable = true;
  # Neither Zero board has a TPM; the RPi kernel lacks tpm-crb.
  boot.initrd.systemd.tpm2.enable = false;
  # SD controllers are built in; omit default PC storage/USB drivers.
  boot.initrd.includeDefaultModules = false;
  boot.initrd.availableKernelModules = [ "mmc_block" ];
  # U-Boot supplies root= for the selected slot, never root=fstab or a fixed p2.
  boot.initrd.systemd.root = null;
  boot.initrd.checkJournalingFS = false;
  boot.kernelParams = [ "ro" "rootwait" "rootfstype=ext4" "fsck.mode=skip" "watchdog.open_timeout=300" "panic=10" ];
  boot.kernelPatches = [{
    name = "nabos-appliance";
    patch = null;
    structuredExtraConfig = with lib.kernel; {
      WATCHDOG = yes; BCM2835_WDT = yes; WATCHDOG_HANDLE_BOOT_ENABLED = yes;
      GPIO_CDEV = yes; KEYBOARD_GPIO = module; INPUT_EVDEV = module;
      I2C = yes; I2C_CHARDEV = module; I2C_BCM2835 = module;
      SND = module; SND_SOC = module; SND_BCM2835_SOC_I2S = module;
      SND_SOC_WM8960 = no;
      LEDS_CLASS_MULTICOLOR = module; DMA_BCM2835 = yes;
      OVERLAY_FS_METACOPY = yes; OVERLAY_FS_REDIRECT_DIR = yes;
      SQUASHFS = module; SQUASHFS_ZSTD = yes; EROFS_FS = module; OVERLAY_FS = module;
    } // lib.optionalAttrs (nabosTarget == "zero-armv6") {
      # These drivers use 64-bit division helpers unavailable in the ARMv6 kernel.
      # The Zero W uses BCM2835 I2C and has no RP1 peripheral controller.
      PWM_RP1 = no; VIDEO_RP1_CFE_DOWNSTREAM = no; I2C_DESIGNWARE_CORE = no;
    };
  }];
  boot.extraModulePackages = [ packages.sound packages.led ];
  boot.kernelModules = [ "i2c-dev" "bcm2835-ws2812" ];
  boot.blacklistedKernelModules = [ "snd_bcm2835" ];
  fileSystems = {
    "/" = { device = "/dev/root"; fsType = "ext4"; options = [ "ro" ]; noCheck = true; };
    "/var" = { fsType = "tmpfs"; options = [ "mode=0755" "size=64M" "nosuid" "nodev" ]; neededForBoot = true; };
    "/tmp" = { fsType = "tmpfs"; options = [ "mode=1777" "size=64M" "nosuid" "nodev" ]; };
  };
  hardware.deviceTree = {
    enable = true;
    filter = if nabosTarget == "zero2-arm64" then "*bcm2710-rpi-zero-2-w.dtb" else "*bcm2708-rpi-zero-w.dtb";
    overlays = [
      { name = "bcm2835-ws2812"; dtboFile = "${packages.led}/overlays/bcm2835-ws2812.dtbo"; }
      { name = "nabos-profile"; dtsFile = ../image/nabos-overlay.dts; }
    ];
  };
  swapDevices = [ ];
  boot.initrd.systemd.services.nabos-persist = {
    description = "Check/grow data p4 and bind persistent appliance state";
    requiredBy = [ "initrd-switch-root.service" ];
    before = [ "initrd-switch-root.service" ];
    after = [ "sysroot.mount" "sysroot-etc.mount" "sysroot-var.mount" ];
    requires = [ "sysroot.mount" "sysroot-etc.mount" "sysroot-var.mount" ];
    unitConfig.DefaultDependencies = false;
    serviceConfig = { Type = "oneshot"; RemainAfterExit = true; ExecStart = "${persist}"; };
  };
  boot.initrd.systemd.storePaths = [ persist stateSeeds ];
  boot.initrd.systemd.settings.Manager = { RuntimeWatchdogSec = "14s"; RebootWatchdogSec = "2min"; };
  systemd.settings.Manager = { RuntimeWatchdogSec = "14s"; RebootWatchdogSec = "2min"; };

  networking.hostName = "nabaztag";
  networking.usePredictableInterfaceNames = false;
  networking.useNetworkd = false;
  networking.useDHCP = false;
  networking.resolvconf.enable = false;
  networking.networkmanager = {
    enable = true;
    settings.main = { rc-manager = "unmanaged"; firewall-backend = "nftables"; };
    connectionConfig."wifi.powersave" = 2;
  };
  networking.firewall.allowedTCPPorts = [ 80 ] ++ lib.optional hasVoice 6053;
  # NetworkManager installs forwarding/NAT rules, not host INPUT allowances.
  networking.firewall.interfaces.wlan0 = {
    allowedUDPPorts = [ 53 67 ];
    allowedTCPPorts = [ 53 ];
  };
  services.avahi = { enable = true; nssmdns4 = true; publish = { enable = true; addresses = true; }; };
  services.timesyncd.enable = true;
  services.journald.settings.Journal = { Storage = "volatile"; RuntimeMaxUse = "16M"; };
  hardware.bluetooth.enable = false;
  hardware.firmware = [ pkgs.raspberrypiWirelessFirmware ];
  nixpkgs.config.allowUnfreePredicate = package:
    lib.getName package == "raspberrypi-wireless-firmware";
  services.dbus = { enable = true; packages = [ dbusPolicies ]; };
  security.polkit.enable = true;
  services.udev = {
    packages = [ packages.sound ];
    extraRules = lib.replaceStrings [ "/usr/bin/chgrp" "/usr/bin/chmod" ]
      [ "${pkgs.coreutils}/bin/chgrp" "${pkgs.coreutils}/bin/chmod" ]
      (builtins.readFile (rootfs + "/etc/udev/rules.d/60-nabos.rules"));
  };
  services.pipewire = {
    enable = true; alsa.enable = true; pulse.enable = true;
    configPackages = [ (pkgs.writeTextDir "share/pipewire/pipewire.conf.d/30-nabos-audio.conf"
      (builtins.readFile (rootfs + "/etc/pipewire/pipewire.conf.d/30-nabos-audio.conf"))) ];
    wireplumber.configPackages = [ (pkgs.writeTextDir "share/wireplumber/wireplumber.conf.d/51-nabos-audio.conf"
      (builtins.readFile (rootfs + "/etc/wireplumber/wireplumber.conf.d/51-nabos-audio.conf"))) ];
  };
  systemd.user.services.pipewire = { wantedBy = [ "default.target" ]; unitConfig.ConditionUser = [ "" "nab-audio" ]; };
  systemd.user.services.pipewire-pulse = { wantedBy = [ "default.target" ]; unitConfig.ConditionUser = [ "" "nab-audio" ]; };
  systemd.user.services.wireplumber.unitConfig.ConditionUser = [ "" "nab-audio" ];
  systemd.user.sockets.pipewire.unitConfig.ConditionUser = [ "" "nab-audio" ];
  systemd.user.sockets.pipewire-pulse.unitConfig.ConditionUser = [ "" "nab-audio" ];

  services.rauc = {
    enable = true; mark-good.enable = false; inherit compatible;
    bootloader = "uboot"; dataDir = "/data/rauc"; settings = raucSettings;
  };
  services.openssh = {
    enable = true;
    authorizedKeysFiles = mkForce [ "none" ];
    authorizedKeysCommand = "${pkgs.coreutils}/bin/cat /data/device-core/ssh/authorized_keys";
    authorizedKeysCommandUser = "device-core";
    hostKeys = map (type: { inherit type; path = "/data/system/ssh/etc/ssh/ssh_host_${type}_key"; }) [ "ed25519" "rsa" "ecdsa" ];
    settings = {
      AllowUsers = [ "nabos" ]; PermitRootLogin = "no";
      AuthenticationMethods = "publickey"; PasswordAuthentication = false;
      KbdInteractiveAuthentication = false; UsePAM = true;
    };
  };
  system.build = { nabosRuntimeUnits = runtimeUnits; nabosHealth = health; nabosPersist = persist; };
  systemd.packages = [ runtimeUnits packages.sound ];
  systemd.services = {
    nabos = {
      wantedBy = [ "multi-user.target" ];
      environment = {
        NABOS_SOUNDS_DIRS = "${packages.assets}/share/nabos/sounds:/data/nabos/media/sounds";
        NABOS_CHOREOGRAPHIES_DIRS = "${packages.assets}/share/nabos/choreographies:/data/nabos/media/choreographies";
      };
    };
    nab-hardware.wantedBy = [ "multi-user.target" ];
    device-core = {
      wantedBy = [ "multi-user.target" ]; path = tools;
      environment = {
        DEVICE_CORE_AUDIO_ROOTS = "${packages.assets}/share/nabos/sounds:/data/nabos/media/sounds";
        DEVICE_CORE_SSH_UNIT = "ssh.service";
      };
    };
    nabos-health.wantedBy = [ "multi-user.target" ];
    tagtagtag-mixerd = {
      wantedBy = [ "multi-user.target" ];
      serviceConfig = {
        # Foreground mode lets systemd own the process; no PID file is needed.
        Type = "simple";
        ExecStart = [ "" "${packages.sound}/bin/tagtagtag-mixerd" ];
        PIDFile = "";
        WorkingDirectory = "/var/lib/tagtagtag-sound";
        ProtectSystem = "strict";
        ReadWritePaths = [ "/var/lib/tagtagtag-sound" ];
      };
    };
    "user@1004" = { after = [ "systemd-tmpfiles-setup.service" ]; serviceConfig.NoNewPrivileges = true; };
    linux-voice-assistant = mkIf hasVoice {
      wantedBy = [ "multi-user.target" ];
      # LVA loads libmpv directly; the CLI wrapper pulls in unused yt-dlp/Deno.
      path = [ pkgs.coreutils pkgs.alsa-utils pkgs.pipewire ];
      environment = {
        PYTHONDONTWRITEBYTECODE = "1";
        XDG_CACHE_HOME = "/var/lib/nabos/lva/cache";
        XDG_CONFIG_HOME = "/var/lib/nabos/lva/config";
        XDG_DATA_HOME = "/var/lib/nabos/lva/data";
      };
    };
    rauc = {
      path = [ pkgs.ubootTools pkgs.coreutils pkgs.util-linux ];
      unitConfig.ConditionFileNotEmpty = "/data/rauc/ca.cert.pem";
      serviceConfig = {
        ExecStart = mkForce "${lib.getExe config.services.rauc.package} --conf=/etc/rauc/system.conf --mount=/run/rauc/mnt service";
        StateDirectory = mkForce [ ];
      };
    };
    sshd = {
      # Canonical unit identity matters: systemd authorizes the resolved unit,
      # so an ssh.service alias for sshd.service would fail the existing rule.
      name = mkForce "ssh.service";
      unitConfig = { ConditionFileNotEmpty = "/data/device-core/ssh/authorized_keys"; ConditionPathExists = "!/data/.volatile"; };
    };
    sshd-keygen = {
      unitConfig = { ConditionFileNotEmpty = mkForce "/data/device-core/ssh/authorized_keys"; ConditionPathExists = "!/data/.volatile"; };
      postStart = "${pkgs.coreutils}/bin/sync -f /data/system/ssh/etc/ssh";
    };
  };
  systemd.tmpfiles.rules = lib.splitString "\n" (builtins.readFile (rootfs + "/usr/lib/tmpfiles.d/nabos.conf"));
  environment.systemPackages = [ packages.nabos packages.nab-hardware packages.device-core packages.assets ] ++ tools;
  environment.etc = {
    "NetworkManager/system-connections/.keep" = { text = ""; mode = "0600"; };
    "NetworkManager/dnsmasq-shared.d/nabos.conf".source = rootfs + "/etc/NetworkManager/dnsmasq-shared.d/nabos.conf";
    "resolv.conf" = { source = "/run/NetworkManager/resolv.conf"; mode = "direct-symlink"; };
    "polkit-1/rules.d/40-nabos-network.rules".source = rootfs + "/etc/polkit-1/rules.d/40-nabos-network.rules";
    "polkit-1/rules.d/50-nabos.rules".source = rootfs + "/etc/polkit-1/rules.d/50-nabos.rules";
    "fw_env.config".source = rootfs + "/etc/fw_env.config";
    "rauc/system.conf".source = (pkgs.formats.ini { }).generate "nabos-rauc.conf" raucSettings;
    "nabos/release".text = "${nabosVersion}\n";
    "nabos/release.env".text = ''
      NABOS_VERSION=${nabosVersion}
      DEVICE_CORE_IMAGE_VERSION=${nabosVersion}
      DEVICE_CORE_UPDATE_REPO=guilhem/nabos
      DEVICE_CORE_UPDATE_ASSET=nabos-${nabosTarget}.raucb
      DEVICE_CORE_LVA_UNIT=${lib.optionalString hasVoice "linux-voice-assistant.service"}
    '';
  };
}
