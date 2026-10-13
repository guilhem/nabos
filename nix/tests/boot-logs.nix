{ pkgs }:
let
  seeds = pkgs.runCommand "boot-log-state-seeds" { } ''
    mkdir -p $out/etc/NetworkManager/system-connections $out/var/lib/{NetworkManager,systemd/timesync,tagtagtag-sound,nabos}
  '';
  library = pkgs.writeText "boot-log-persist-library" (builtins.readFile ../runtime/persist.sh);
  logHelper = pkgs.writeText "boot-log-helper" (builtins.readFile ../runtime/initrd-log.sh);
  persist = pkgs.writeShellScript "boot-log-persist" ''
    export PATH=/bin:/sbin
    export NABOS_DISK=/dev/vda
    export NABOS_SYSFS=/run/boot-log-sysfs
    export NABOS_PERSIST_LIB=${library}
    export NABOS_STATE_SEEDS=${seeds}
    ${builtins.readFile ../runtime/initrd-persist.sh}
  '';
  marker = pkgs.writeShellScript "boot-log-marker" ''
    export PATH=/bin:/sbin
    echo initrd-marker
    if [ -e /sysroot/data/system/fail-next ]; then
      rm /sysroot/data/system/fail-next
      echo initrd-failure-marker
      # Give the stream time to record the failure before reboot-force.
      sleep 1
      cp /sysroot/data/system/boot-logs/slot-A.initrd.log /sysroot/data/system/failed-attempt.log
      exit 1
    fi
    sleep 1
  '';
in pkgs.testers.runNixOSTest {
  name = "nabos-boot-logs";
  globalTimeout = 240;
  nodes.machine = { config, lib, modulesPath, ... }: {
    imports = [ ../diagnostics.nix "${modulesPath}/profiles/image-based-appliance.nix" ];
    system.stateVersion = "26.05";
    system.nixos-init.enable = true;
    system.activatable = false;
    system.etc.overlay = { enable = true; mutable = false; };
    services.userborn = { enable = true; static = true; };
    systemd.sysusers.enable = false;
    boot.postBootCommands = lib.mkForce "";
    environment.binsh = null;
    environment.usrbinenv = null;
    environment.etc."NetworkManager/system-connections/.keep".text = "";
    boot.tmp.useTmpfs = true;
    boot.kernelParams = [ "rauc.slot=A" ];
    boot.initrd.kernelModules = [ "virtio_blk" "ext4" ];
    virtualisation = {
      diskImage = null;
      writableStore = false;
      memorySize = 1024;
      emptyDiskImages = [{ size = 192; driveConfig.deviceExtraOpts.serial = "boot-state"; }];
      fileSystems."/var" = {
        device = "tmpfs"; fsType = "tmpfs"; neededForBoot = true;
        options = [ "mode=0755" "size=64M" "nosuid" "nodev" ];
      };
    };
    systemd.services.systemd-remount-fs.enable = false;
    systemd.services.nabos-health = {
      wantedBy = [ "multi-user.target" ];
      serviceConfig = { Type = "oneshot"; RemainAfterExit = true; ExecStart = "${pkgs.coreutils}/bin/true"; };
    };
    boot.initrd.systemd = {
      storePaths = [ persist library seeds marker ];
      initrdBin = [ pkgs.gnugrep pkgs.findutils ];
      extraBin = {
        mountpoint = "${pkgs.util-linux}/bin/mountpoint";
        sfdisk = "${pkgs.util-linux}/bin/sfdisk";
        partx = "${pkgs.util-linux}/bin/partx";
        e2fsck = "${pkgs.e2fsprogs}/bin/e2fsck";
        resize2fs = "${pkgs.e2fsprogs}/bin/resize2fs";
        "mkfs.ext4" = "${pkgs.e2fsprogs}/bin/mkfs.ext4";
      };
      services.nabos-initrd-log.environment.NABOS_DISK = "/dev/vda";
      services.boot-log-root = {
        requiredBy = [ "nabos-persist.service" ];
        before = [ "nabos-persist.service" ];
        after = [ "initrd-fs.target" ];
        unitConfig.DefaultDependencies = false;
        serviceConfig = { Type = "oneshot"; TimeoutStartSec = "30s"; };
        script = ''
          while [ ! -b /dev/vda ]; do sleep 0.1; done
          if [ ! -b /dev/vda4 ]; then
            printf ',1M,L\n,1M,L\n,1M,L\n,,L\n' | sfdisk /dev/vda
            partx -u /dev/vda
            while [ ! -b /dev/vda4 ]; do sleep 0.1; done
            mkfs.ext4 -F /dev/vda4
          fi
          ln -s /dev/vda4 /dev/vdap4
          mkdir -p /run/boot-log-sysfs/class/block
          ln -s /sys/class/block/vda /run/boot-log-sysfs/class/block/vda
          ln -s /sys/class/block/vda4 /run/boot-log-sysfs/class/block/vdap4
          mkdir -p /sysroot/bin /sysroot/usr/bin /sysroot/dev /sysroot/proc /sysroot/sys /sysroot/data /sysroot/sysroot
          ln -s ${pkgs.runtimeShell} /sysroot/bin/sh
          ln -s ${pkgs.coreutils}/bin/env /sysroot/usr/bin/env
          mount -o remount,ro /sysroot
        '';
      };
      services.nabos-persist = {
        requiredBy = [ "initrd-switch-root.service" ];
        before = [ "initrd-switch-root.service" ];
        after = [ "sysroot.mount" "sysroot-etc.mount" "sysroot-var.mount" ];
        requires = [ "sysroot.mount" "sysroot-etc.mount" "sysroot-var.mount" ];
        unitConfig = { DefaultDependencies = false; FailureAction = "reboot-force"; };
        serviceConfig = {
          Type = "oneshot";
          RemainAfterExit = true;
          ExecStart = "${persist}";
          ExecStartPost = "${marker}";
        };
      };
    };
    # Run the same collector under the same limits inside isolated mount namespaces.
    systemd.services.boot-log-probe = {
      environment.NABOS_DISK = "/dev/vda";
      serviceConfig = config.boot.initrd.systemd.services.nabos-initrd-log.serviceConfig // {
        ExecStartPre = lib.mkForce [ ];
        ExecStart = lib.mkForce "${pkgs.bash}/bin/bash /run/boot-log-probe";
        PrivateMounts = true;
      };
      path = [ pkgs.coreutils pkgs.util-linux pkgs.systemd pkgs.e2fsprogs ];
    };
  };
  testScript = ''
    machine.start(allow_reboot=True)

    def healthy():
        machine.wait_for_unit("multi-user.target")
        machine.wait_for_unit("nabos-health.service")
        for path in ("/", "/etc", "/nix/store"):
            assert "ro" in machine.succeed(f"findmnt -n -o OPTIONS --mountpoint {path}").strip().split(",")
            machine.fail(f"touch {path.rstrip('/')}/.boot-log-readonly-probe")
        machine.succeed("grep -q initrd-marker /data/system/boot-logs/slot-A.initrd.log")
        assert machine.succeed("stat -c '%u:%g:%a' /data/system/boot-logs").strip() == "0:0:700"
        assert machine.succeed("stat -c '%u:%g:%a' /data/system/boot-logs/slot-A.initrd.log").strip() == "0:0:600"
        machine.fail("pgrep -f '^journalctl --boot=0 --follow'")

    healthy()
    boot_id = machine.succeed("cat /proc/sys/kernel/random/boot_id").strip()
    journal_boot_id = boot_id.replace("-", "")
    machine.succeed("cp /data/system/boot-logs/slot-A.initrd.log /data/system/first-attempt.log")
    machine.succeed("echo runtime-marker | systemd-cat -t boot-log-test; journalctl --sync; sync -f /data")
    machine.crash()
    machine.start(allow_reboot=True)
    healthy()
    machine.succeed(f"grep -q boot_id={boot_id} /data/system/first-attempt.log; grep -q initrd-marker /data/system/first-attempt.log")
    machine.succeed(f"journalctl --boot={journal_boot_id} --no-pager | grep -q runtime-marker")
    machine.succeed("touch /data/system/fail-next; sync -f /data")
    machine.crash()
    machine.start(allow_reboot=True)
    healthy()
    machine.succeed("grep -q initrd-failure-marker /data/system/failed-attempt.log")

    health_id = machine.succeed("systemctl show nabos-health -p InvocationID --value").strip()

    def probe(setup, expected_success=False):
        script = "set -eu\nmount -t tmpfs tmpfs /sysroot\nmkdir -p /sysroot/data\n" + setup + "\nexec ${pkgs.runtimeShell} ${logHelper} collect\n"
        machine.succeed("cat > /run/boot-log-probe <<'PROBE'\n" + script + "PROBE\n")
        machine.succeed("systemctl reset-failed boot-log-probe || true")
        if expected_success:
            machine.succeed("systemctl start boot-log-probe")
        else:
            machine.succeed("systemctl start boot-log-probe || true")
            machine.wait_until_succeeds("test $(systemctl show boot-log-probe -p ActiveState --value) = failed")
        assert machine.succeed("systemctl show boot-log-probe -p NRestarts --value").strip() == "0"
        machine.wait_for_unit("nabos-health.service")
        assert machine.succeed("systemctl show nabos-health -p InvocationID --value").strip() == health_id

    probe(":")  # No data mount.
    probe("mount -t tmpfs tmpfs /sysroot/data")  # Wrong filesystem/device.
    probe("mount --bind /data /sysroot/data; mount -o remount,bind,ro /sysroot/data")
    machine.succeed("touch /data/.volatile")
    probe("mount --bind /data /sysroot/data")
    machine.succeed("rm /data/.volatile; fallocate -l 128M /data/full-probe")
    probe("mount --bind /data /sysroot/data")
    machine.succeed("rm /data/full-probe")

    probe("mount --bind /data /sysroot/data", expected_success=True)
    machine.succeed("head -c 5000000 /dev/zero | tr '\\0' x | fold -w 4096 | systemd-cat -t cap-probe")
    machine.wait_until_succeeds("test $(systemctl show boot-log-probe -p ActiveState --value) = failed")
    assert int(machine.succeed("stat -c %s /data/system/boot-logs/slot-A.initrd.log")) <= 4194304
    assert machine.succeed("systemctl show boot-log-probe -p LimitFSIZE --value").strip() == "4194304"
    assert machine.succeed("systemctl show boot-log-probe -p LimitCORE --value").strip() == "0"
    assert machine.succeed("systemctl show boot-log-probe -p NRestarts --value").strip() == "0"
    machine.wait_for_unit("nabos-health.service")
    assert machine.succeed("systemctl show nabos-health -p InvocationID --value").strip() == health_id
  '';
}
