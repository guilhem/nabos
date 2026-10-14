{ pkgs }:
pkgs.testers.runNixOSTest {
  name = "nabos-network-boot";
  globalTimeout = 180;
  nodes.machine = { lib, modulesPath, ... }: {
    imports = [ ../network.nix "${modulesPath}/profiles/image-based-appliance.nix" ];
    system.stateVersion = "26.05";
    system.nixos-init.enable = true;
    system.activatable = false;
    system.etc.overlay = { enable = true; mutable = false; };
    services.userborn = { enable = true; static = true; };
    systemd.sysusers.enable = false;
    boot.postBootCommands = lib.mkForce "";
    environment.binsh = null;
    environment.usrbinenv = null;
    boot.tmp.useTmpfs = true;
    boot.kernelModules = [ "rfkill" ];
    virtualisation = {
      diskImage = null;
      writableStore = false;
      memorySize = 1024;
      emptyDiskImages = [{
        size = 128;
        driveConfig.deviceExtraOpts.serial = "network-state";
      }];
      fileSystems = {
        "/var" = {
          device = "tmpfs"; fsType = "tmpfs"; neededForBoot = true;
          options = [ "mode=0755" "size=64M" "nosuid" "nodev" ];
        };
        "/data" = {
          device = "/dev/disk/by-id/virtio-network-state";
          fsType = "ext4"; autoFormat = true; neededForBoot = true;
        };
      };
    };
    networking.useNetworkd = false;
    networking.resolvconf.enable = false;
    networking.modemmanager.enable = false;
    networking.networkmanager.enable = true;
    # The VM module disables wireless even when NetworkManager enables it.
    networking.wireless.enable = lib.mkOverride 0 true;
    environment.etc."NetworkManager/system-connections/.keep" = { text = ""; mode = "0600"; };
    systemd.services.systemd-remount-fs.enable = false;
    # Exercise D-Bus activation rather than starting WPA with multi-user.target.
    systemd.services.wpa_supplicant.wantedBy = lib.mkForce [ ];
    boot.initrd.systemd.services.network-readonly-root = {
      requiredBy = [ "initrd-switch-root.service" ];
      before = [ "initrd-switch-root.service" ];
      after = [ "initrd-fs.target" ];
      unitConfig.DefaultDependencies = false;
      serviceConfig.Type = "oneshot";
      path = [ pkgs.coreutils pkgs.util-linuxMinimal ];
      script = ''
        mkdir -p /sysroot/bin /sysroot/usr/bin /sysroot/dev /sysroot/proc /sysroot/sys
        ln -s ${pkgs.runtimeShell} /sysroot/bin/sh
        ln -s ${pkgs.coreutils}/bin/env /sysroot/usr/bin/env
        mkdir -p /sysroot/var/lib/NetworkManager
        for path in /etc/NetworkManager/system-connections /var/lib/NetworkManager; do
          mkdir -p "/sysroot/data/system$path"
          chmod 0700 "/sysroot/data/system$path"
          mount --bind "/sysroot/data/system$path" "/sysroot$path"
        done
        mount -o remount,ro /sysroot
      '';
    };
  };
  testScript = ''
    machine.start()

    def check_network():
        machine.wait_for_unit("multi-user.target")
        machine.wait_for_unit("NetworkManager.service")
        for path in ("/", "/etc", "/nix/store"):
            assert "ro" in machine.succeed(f"findmnt -n -o OPTIONS --mountpoint {path}").strip().split(",")
            machine.fail(f"touch {path.rstrip('/')}/.network-readonly-probe")
        machine.wait_for_unit("avahi-daemon.service")
        assert machine.succeed("busctl call org.freedesktop.DBus /org/freedesktop/DBus org.freedesktop.DBus NameHasOwner s org.freedesktop.Avahi").strip() == "b true"
        machine.wait_until_succeeds("test \"$(busctl call org.freedesktop.Avahi / org.freedesktop.Avahi.Server GetState)\" = 'i 2'")
        machine.succeed("test -d /etc/avahi/services")
        machine.fail("touch /etc/avahi/services/.network-readonly-probe")
        for name, expected in {
            "ConfigurationDirectory": "avahi/services", "ProtectSystem": "strict",
            "NoNewPrivileges": "yes", "PrivateDevices": "yes",
        }.items():
            assert machine.succeed(f"systemctl show avahi-daemon -p {name} --value").strip() == expected
        machine.succeed("busctl call org.freedesktop.DBus /org/freedesktop/DBus org.freedesktop.DBus StartServiceByName su fi.w1.wpa_supplicant1 0")
        machine.wait_for_unit("wpa_supplicant.service")
        assert machine.succeed("busctl call org.freedesktop.DBus /org/freedesktop/DBus org.freedesktop.DBus NameHasOwner s fi.w1.wpa_supplicant1").strip() == "b true"
        machine.succeed("busctl get-property fi.w1.wpa_supplicant1 /fi/w1/wpa_supplicant1 fi.w1.wpa_supplicant1 Interfaces")
        assert machine.succeed("systemctl show wpa_supplicant -p ExecStartPre --value").strip() == ""
        machine.succeed("grep -qx update_config=0 /etc/wpa_supplicant/nixos.conf && test ! -e /etc/wpa_supplicant/imperative.conf")
        for name, expected in {
            "User": "wpa_supplicant", "Group": "wpa_supplicant",
            "RootDirectory": "/run/wpa_supplicant", "ProtectSystem": "strict",
            "NoNewPrivileges": "yes", "PrivateDevices": "yes", "PrivateTmp": "yes",
            "ProtectKernelTunables": "yes", "ProtectKernelModules": "yes",
            "ProtectControlGroups": "yes", "MemoryDenyWriteExecute": "yes",
        }.items():
            assert machine.succeed(f"systemctl show wpa_supplicant -p {name} --value").strip() == expected
        assert set(machine.succeed("systemctl show wpa_supplicant -p CapabilityBoundingSet --value").split()) == {"cap_net_admin", "cap_net_raw"}
        machine.succeed("grep -Eq '^NoNewPrivs:[[:space:]]+1$' /proc/$(systemctl show wpa_supplicant -p MainPID --value)/status")

    check_network()
    # Saving a profile needs no Wi-Fi device and must use the persistent keyfile store.
    machine.succeed("nmcli connection add type wifi con-name persistent-wifi ifname wlan0 ssid boot-test connection.autoconnect no")
    machine.succeed("test -f /data/system/etc/NetworkManager/system-connections/persistent-wifi.nmconnection")
    uuid = machine.succeed("nmcli -g connection.uuid connection show persistent-wifi").strip()
    boot_id = machine.succeed("cat /proc/sys/kernel/random/boot_id").strip()
    machine.succeed("sync")
    machine.crash()
    machine.start()
    check_network()
    assert machine.succeed("cat /proc/sys/kernel/random/boot_id").strip() != boot_id
    assert machine.succeed("nmcli -g connection.uuid connection show persistent-wifi").strip() == uuid
    machine.succeed("test -f /data/system/etc/NetworkManager/system-connections/persistent-wifi.nmconnection")
  '';
}
