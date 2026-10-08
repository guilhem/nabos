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
    mkdir -p $out/etc/NetworkManager/system-connections $out/var/lib/{NetworkManager,systemd/timesync,tagtagtag-sound,nabos/lva}
    cp ${rootfs}/var/lib/NetworkManager/NetworkManager.state $out/var/lib/NetworkManager/
    cp ${packages.sound}/share/tagtagtag-sound/mixer.conf.default $out/var/lib/tagtagtag-sound/mixer.conf.default
    cp $out/var/lib/tagtagtag-sound/mixer.conf.default $out/var/lib/tagtagtag-sound/mixer.conf
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
  ledDevices = map (index: "sys-class-leds-multi:indicator\\x2d${toString index}.device") (lib.range 0 4);
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
    ./audio.nix
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
  # Apply appliance settings only to ARM; retain upstream native builder packages.
  nixpkgs.overlays = [ (_final: previous: lib.optionalAttrs previous.stdenv.hostPlatform.isAarch {
    # Audio consumers use the C API; avoid building a Fortran toolchain.
    fftw = (previous.fftw.override { withDoc = false; }).overrideAttrs (old: {
      nativeBuildInputs = [ ];
      configureFlags = old.configureFlags ++ [ "--disable-fortran" ];
    });
    # Orc's documentation generators cannot run during cross compilation.
    orc = previous.orc.override { buildDevDoc = false; };
  } // lib.optionalAttrs previous.stdenv.hostPlatform.isAarch32 {
    # Omit unused RDMA capture that fails ARMv6 cross builds.
    libpcap = previous.libpcap.override { withRdma = false; };
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
      home = "/var/lib/nabos/admin"; createHome = false; shell = pkgs.bashInteractive;
      hashedPassword = "!"; autoSubUidGidRange = false;
    };
    nab-app = fixedUser 1001 "/data/nabos" [ "nab-media" ];
    nab-hardware = fixedUser 1002 "/run/nab-hardware" [ "gpio" ];
    device-core = fixedUser 1003 "/data/device-core" [ "nab-media" "nab-audio" ];
  };
  users.groups = {
    nabos.gid = 1000; nab-app.gid = 1001; nab-hardware.gid = 1002;
    device-core.gid = 1003; nab-media.gid = 1005; gpio.gid = 1006;
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
  networking.modemmanager.enable = false;
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
  system.build = { nabosHealth = health; nabosPersist = persist; };
  systemd.services = {
    nabos = {
      description = "Nabaztag application (states, media, choreography, HTTP :80)";
      wantedBy = [ "multi-user.target" ];
      wants = [ "device-core.service" "nab-hardware.service" ];
      after = [ "systemd-tmpfiles-setup.service" "device-core.service" "nab-hardware.service" ];
      environment = {
        XDG_RUNTIME_DIR = "/run/nabos";
        HOME = "/data/nabos";
        NABOS_DATA_DIR = "/data/nabos";
        NABOS_HTTP_ADDR = ":80";
        NABOS_SOUNDS_DIRS = "${packages.assets}/share/nabos/sounds:/data/nabos/media/sounds";
        NABOS_CHOREOGRAPHIES_DIRS = "${packages.assets}/share/nabos/choreographies:/data/nabos/media/choreographies";
      };
      serviceConfig = {
        Type = "exec"; User = "nab-app"; Group = "nab-app";
        EnvironmentFile = "/etc/nabos/release.env";
        RuntimeDirectory = "nabos"; WorkingDirectory = "/run/nabos";
        ExecStart = "${packages.nabos}/bin/nabos";
        Restart = "always"; RestartSec = 2; UMask = "0027";
        AmbientCapabilities = "CAP_NET_BIND_SERVICE";
        CapabilityBoundingSet = "CAP_NET_BIND_SERVICE";
        NoNewPrivileges = true; PrivateDevices = true;
        ProtectSystem = "strict"; ReadWritePaths = [ "/data/nabos" ];
      };
    };
    nab-hardware = {
      description = "Nabaztag hardware (D-Bus)";
      wantedBy = [ "multi-user.target" ];
      requires = [ "dbus.socket" "dev-i2c\\x2d1.device" ] ++ ledDevices;
      wants = [ "device-core.service" ];
      after = ledDevices ++ [ "systemd-tmpfiles-setup.service" "tagtagtag-mixerd.service" "device-core.service" "dbus.socket" "dev-i2c\\x2d1.device" ];
      environment.HOME = "/run/nab-hardware";
      serviceConfig = {
        Type = "notify"; NotifyAccess = "main";
        User = "nab-hardware"; Group = "nab-hardware"; SupplementaryGroups = [ "gpio" ];
        CapabilityBoundingSet = "";
        RuntimeDirectory = "nab-hardware"; WorkingDirectory = "/run/nab-hardware";
        ExecStart = "${packages.nab-hardware}/bin/nab-hardware";
        ExecStopPost = "${packages.nab-hardware}/bin/nab-hardware --stop-hardware";
        WatchdogSec = "1s"; WatchdogSignal = "SIGKILL"; KillMode = "control-group";
        TimeoutStopSec = "6s"; Restart = "always"; RestartSec = 2; UMask = "0027";
        NoNewPrivileges = true; ProtectSystem = "strict";
        # Missing LEDs must not prevent ExecStopPost from cutting the motors.
        ReadWritePaths = lib.concatMap (index: map (attr:
          "-/sys/class/leds/multi:indicator-${toString index}/${attr}")
          [ "brightness" "multi_intensity" ]) (lib.range 0 4)
          ++ [ "-/sys/class/leds/multi:indicator-0/sync" ];
      };
    };
    device-core = {
      description = "Linux device services (D-Bus)";
      wantedBy = [ "multi-user.target" ]; path = tools;
      wants = [ "user@1004.service" "NetworkManager.service" "rauc.service" ];
      after = [ "systemd-tmpfiles-setup.service" "user@1004.service" "NetworkManager.service" "rauc.service" ];
      environment = {
        HOME = "/data/device-core";
        XDG_RUNTIME_DIR = "/run/device-core";
        PIPEWIRE_REMOTE = "/run/nabos-audio/pipewire-0";
        DEVICE_CORE_DATA_DIR = "/data/device-core";
        DEVICE_CORE_NETWORK_GUARD = "/run/lock/device-core/network";
        DEVICE_CORE_PRESENCE_USER = "nab-hardware";
        DEVICE_CORE_MAINTENANCE_USERS = "nab-app:nab-hardware";
        DEVICE_CORE_HOTSPOT_PREFIX = "Nabaztag-";
        DEVICE_CORE_HOTSPOT_UUID = "4e61624f-5300-4000-8000-000000000001";
        DEVICE_CORE_BOOT_HEALTH = "/run/nabos-boot-health";
        DEVICE_CORE_UPDATE_PREPARE_UNIT = "nabos-rauc-manual.service";
        DEVICE_CORE_UPDATE_PREPARED_BUNDLE = "/data/nabos-rauc-manual/bundle.raucb";
        DEVICE_CORE_AUDIO_ROOTS = "${packages.assets}/share/nabos/sounds:/data/nabos/media/sounds";
        DEVICE_CORE_SSH_UNIT = "ssh.service";
      };
      serviceConfig = {
        Type = "exec"; User = "device-core"; Group = "device-core";
        SupplementaryGroups = [ "nab-media" "nab-audio" ];
        EnvironmentFile = "/etc/nabos/release.env";
        RuntimeDirectory = "device-core"; WorkingDirectory = "/run/device-core";
        ExecStart = "${packages.device-core}/bin/device-core";
        Restart = "always"; RestartSec = 2; UMask = "0077";
        NoNewPrivileges = true; CapabilityBoundingSet = ""; PrivateDevices = true;
        ProtectSystem = "strict";
        ReadWritePaths = [ "/data/device-core" "/run/device-core" "/run/lock/device-core" ];
      };
    };
    nabos-health = {
      description = "Confirm the booted RAUC slot once local services are healthy";
      wantedBy = [ "multi-user.target" ];
      after = [ "device-core.service" "nab-hardware.service" "nabos.service" "user@1004.service" ];
      unitConfig.OnSuccess = "nabos-board-led-off.service";
      serviceConfig = { Type = "exec"; ExecStart = "${health}"; };
    };
    nabos-board-led-off = {
      description = "Turn off the Raspberry Pi ACT LED after a healthy boot";
      unitConfig.ConditionPathExists = [ "/sys/class/leds/ACT/brightness" "/run/nabos-boot-health" ];
      serviceConfig = {
        Type = "oneshot";
        ExecCondition = "${pkgs.gnugrep}/bin/grep -qx \"good [AB]\" /run/nabos-boot-health";
        ExecStart = "${pkgs.runtimeShell} -c 'echo 0 > /sys/class/leds/ACT/brightness'";
        NoNewPrivileges = true; CapabilityBoundingSet = ""; ProtectSystem = "strict";
      };
    };
    nabos-rauc-manual = {
      description = "Prepare an explicitly trusted local NabOS update";
      after = [ "rauc.service" ];
      environment.HOME = "/run/nabos-rauc-manual";
      serviceConfig = {
        Type = "oneshot"; RemainAfterExit = true;
        RuntimeDirectory = [ "nabos-rauc-manual" "nabos-rauc-trust" ]; RuntimeDirectoryMode = "0755";
        WorkingDirectory = "/run/nabos-rauc-manual";
        ExecStartPre = "${manual} cleanup"; ExecStart = "${manual}"; ExecStopPost = "${manual} cleanup";
        TimeoutStartSec = "15min"; UMask = "0077"; NoNewPrivileges = true;
        CapabilityBoundingSet = [ "CAP_CHOWN" "CAP_FOWNER" "CAP_DAC_READ_SEARCH" ];
        PrivateDevices = true; PrivateNetwork = true; ProtectSystem = "strict"; ProtectHome = true;
        ReadWritePaths = [ "/data/nabos-rauc-manual" "/run/nabos-rauc-manual" "/run/nabos-rauc-trust" ];
      };
    };
    tagtagtag-mixerd = {
      description = "Tagtagtag Sound Mixer Daemon";
      wantedBy = [ "multi-user.target" ];
      bindsTo = [ "dev-input-tagtagtag\\x2dvolume.device" ];
      after = [ "dev-input-tagtagtag\\x2dvolume.device" ];
      unitConfig.StartLimitIntervalSec = 0;
      serviceConfig = {
        # Foreground mode lets systemd own the process; no PID file is needed.
        Type = "simple";
        ExecStart = "${packages.sound}/bin/tagtagtag-mixerd";
        User = "root"; Restart = "always"; RestartSec = 1;
        WorkingDirectory = "/var/lib/tagtagtag-sound";
        ProtectSystem = "strict";
        ReadWritePaths = [ "/var/lib/tagtagtag-sound" ];
      };
    };
    linux-voice-assistant = mkIf hasVoice {
      description = "Linux Voice Assistant (Home Assistant voice satellite)";
      wantedBy = [ "multi-user.target" ];
      unitConfig.ConditionPathExists = [ "/data/device-core/voice-enabled" "${packages.lva}/bin/linux-voice-assistant" ];
      wants = [ "user@1004.service" "network-online.target" ];
      after = [ "systemd-tmpfiles-setup.service" "user@1004.service" "network-online.target" ];
      # LVA loads libmpv directly; the CLI wrapper pulls in unused yt-dlp/Deno.
      path = [ pkgs.coreutils pkgs.alsa-utils pkgs.pipewire ];
      environment = {
        HOME = "/var/lib/nabos/lva";
        XDG_RUNTIME_DIR = "/run/user/1004";
        PYTHONDONTWRITEBYTECODE = "1";
        XDG_CACHE_HOME = "/var/lib/nabos/lva/cache";
        XDG_CONFIG_HOME = "/var/lib/nabos/lva/config";
        XDG_DATA_HOME = "/var/lib/nabos/lva/data";
      };
      serviceConfig = {
        Type = "exec"; User = "nab-audio"; Group = "nab-audio";
        WorkingDirectory = "${packages.lva}/share/linux-voice-assistant";
        ExecStart = "${packages.lva}/bin/linux-voice-assistant --name Nabaztag --port 6053 --peripheral-host 127.0.0.1 --peripheral-port 6055 --preferences-file /var/lib/nabos/lva/preferences.json --download-dir /var/lib/nabos/lva/wakewords";
        Restart = "on-failure"; RestartSec = 5;
        NoNewPrivileges = true; PrivateDevices = true; ProtectSystem = "strict";
        ReadWritePaths = [ "/var/lib/nabos/lva" ];
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
