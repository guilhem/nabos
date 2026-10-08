{ pkgs }:
pkgs.testers.runNixOSTest {
  name = "nabos-audio-boot";
  globalTimeout = 180;
  nodes.machine = { lib, modulesPath, ... }: {
    imports = [ ../audio.nix "${modulesPath}/profiles/image-based-appliance.nix" ];
    system.stateVersion = "26.05";
    system.nixos-init.enable = true;
    system.activatable = false;
    system.etc.overlay = { enable = true; mutable = false; };
    services.userborn = { enable = true; static = true; };
    systemd.sysusers.enable = false;
    boot.postBootCommands = lib.mkForce "";
    environment.binsh = null;
    environment.usrbinenv = null;
    virtualisation = { diskImage = null; writableStore = false; memorySize = 1024; };
    virtualisation.fileSystems."/var" = {
      device = "tmpfs"; fsType = "tmpfs"; neededForBoot = true;
      options = [ "mode=0755" "size=64M" "nosuid" "nodev" ];
    };
    systemd.services.systemd-remount-fs.enable = false;
    # Freeze the test root before guest PID 1 or the audio session can write it.
    boot.initrd.systemd.services.audio-readonly-root = {
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
        mount -o remount,ro /sysroot
      '';
    };
    systemd.tmpfiles.rules = lib.filter (line:
      lib.hasInfix " /var/lib/nabos " line || lib.hasInfix " /var/lib/nabos/lva " line ||
      lib.hasInfix " /run/nabos-audio " line)
      (lib.splitString "\n" (builtins.readFile ../../image/rootfs/usr/lib/tmpfiles.d/nabos.conf));
  };
  testScript = ''
    machine.start()
    machine.wait_for_unit("multi-user.target")
    # These waits neither start the user manager nor create a login session.
    machine.wait_for_unit("user@1004.service")
    machine.wait_until_succeeds("pgrep -u 1004 -x pipewire && pgrep -u 1004 -x pipewire-pulse && pgrep -u 1004 -x wireplumber")
    machine.succeed("test -S /run/user/1004/pipewire-0 && test -S /run/user/1004/pulse/native && test -S /run/nabos-audio/pipewire-0")
    assert machine.succeed("cat /proc/1/comm").strip() == "systemd"
    assert machine.succeed("loginctl show-user nab-audio -p Linger --value").strip() == "yes"
    machine.succeed("test -f /var/lib/systemd/linger/nab-audio")
    for path in ("/", "/etc"):
        assert "ro" in machine.succeed(f"findmnt -n -o OPTIONS --mountpoint {path}").strip().split(",")
        machine.fail(f"touch {path.rstrip('/')}/.audio-readonly-probe")
    assert machine.succeed("findmnt -n -o FSTYPE --mountpoint /var").strip() == "tmpfs"
    assert machine.succeed("stat -c '%u:%g:%a' /var/lib/nabos/lva").strip() == "1004:1004:700"
    assert machine.succeed("stat -c '%u:%g:%a' /run/nabos-audio/pipewire-0").strip() == "1004:1004:660"
    machine.succeed("runuser -u nab-audio -- touch /var/lib/nabos/lva/.first-use")
    for name in ("pipewire", "pipewire-pulse", "wireplumber"):
        machine.succeed(f"grep -Eq '^NoNewPrivs:[[:space:]]+1$' /proc/$(pgrep -u 1004 -x {name})/status")
    machine.succeed("grep -Eq '^NoNewPrivs:[[:space:]]+1$' /proc/$(systemctl show user@1004.service -p MainPID --value)/status")
  '';
}
