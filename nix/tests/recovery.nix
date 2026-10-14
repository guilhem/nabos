{ pkgs }:
let
  # BRD provides a disposable VM disk; only its major differs from the Pi SD.
  library = pkgs.writeText "recovery-vm-library"
    (pkgs.lib.replaceStrings [ "179:" ] [ "1:" ]
      (builtins.readFile ../runtime/recovery.sh));
  environment = pkgs.writeText "recovery-vm-environment" ''
    BOOT_ORDER=B A
    BOOT_A_LEFT=0
    BOOT_B_LEFT=1
    NABOS_RESET_SLOT=A
  '';
  reset = pkgs.writeShellScriptBin "test-recovery-reset" ''
    set -eu
    . ${library}
    RECOVERY_SYS=/run/recovery-sys
    RECOVERY_WORK=/run/recovery-work
    RECOVERY_ENV_CONFIG=/run/fw_env.config
    mkdir -p "$RECOVERY_WORK"
    recovery_reset
  '';
  setup = pkgs.writeShellScript "recovery-vm-setup" ''
    set -eu
    modprobe brd rd_nr=1 rd_size=14159872 max_part=8
    mknod /dev/mmcblk0 b 1 0
    sfdisk /dev/mmcblk0 <<'TABLE'
    label: dos
    unit: sectors
    /dev/mmcblk0p1 : start=8192, size=524288, type=c, bootable
    /dev/mmcblk0p2 : start=1056768, size=12582912, type=83
    /dev/mmcblk0p3 : start=13639680, size=12582912, type=83
    /dev/mmcblk0p4 : start=26222592, size=2097152, type=83
    TABLE
    udevadm settle
    for n in 1 2 3 4; do mknod /dev/mmcblk0p$n b 1 $n; done
    mkdir -p /run/recovery-sys/mmcblk0/queue
    for attribute in size dev; do cp /sys/class/block/ram0/$attribute /run/recovery-sys/mmcblk0/; done
    cp /sys/class/block/ram0/queue/logical_block_size /run/recovery-sys/mmcblk0/queue/
    for n in 1 2 3 4; do
      mkdir -p /run/recovery-sys/mmcblk0p$n/holders
      for attribute in start size dev partition; do
        cp /sys/class/block/ram0p$n/$attribute /run/recovery-sys/mmcblk0p$n/
      done
    done
    printf '/dev/mmcblk0 0x100000 0x10000\n/dev/mmcblk0 0x200000 0x10000\n' > /run/fw_env.config
    mkenvimage -r -s 0x10000 -o /run/environment.bin ${environment}
    for offset in 1 2; do dd if=/run/environment.bin of=/dev/mmcblk0 bs=1M seek=$offset conv=notrunc status=none; done
    for offset in 4 260 516 6660; do
      printf 'immutable boot/root/public certificate sentinel' | dd of=/dev/mmcblk0 bs=1M seek=$offset conv=notrunc status=none
    done
    mkfs.ext4 -q -F /dev/mmcblk0p4
    mkdir /run/data
    mount /dev/mmcblk0p4 /run/data
    mkdir -p /run/data/{rauc,system,device-core,nabos/media/sounds}
    for file in .hidden rauc/ca.cert.pem system/machine-id device-core/settings.json nabos/media/sounds/private.wav; do
      printf 'old user data' > /run/data/$file
    done
    sync
    umount /run/data
  '';
in pkgs.testers.runNixOSTest {
  name = "nabos-factory-reset";
  globalTimeout = 600;
  nodes.machine = { lib, modulesPath, ... }: {
    imports = [ "${modulesPath}/profiles/image-based-appliance.nix" ];
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
    virtualisation.diskImage = null;
    virtualisation.memorySize = 1536;
    virtualisation.cores = 2;
    virtualisation.writableStore = false;
    virtualisation.fileSystems."/var" = {
      device = "tmpfs"; fsType = "tmpfs"; neededForBoot = true;
      options = [ "mode=0755" "size=64M" "nosuid" "nodev" ];
    };
    systemd.services.systemd-remount-fs.enable = false;
    boot.initrd.systemd.services.recovery-readonly-root = {
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
    environment.systemPackages = with pkgs; [ reset coreutils util-linux e2fsprogs ubootTools kmod gawk gnugrep ];
  };
  testScript = ''
    machine.start()
    machine.wait_for_unit("multi-user.target")
    machine.succeed("${setup}")
    # The native sparse-disk test hashes all protected bytes; sample boundaries here to keep TCG bounded.
    prefix = "{ dd if=/dev/mmcblk0 bs=512 count=1 status=none; for offset in 4 259 260 515 516 6659 6660 12803; do dd if=/dev/mmcblk0 bs=1M skip=$offset count=1 status=none; done; } | sha256sum"
    before = machine.succeed(prefix)
    assert "ro" in machine.succeed("findmnt -n -o OPTIONS --mountpoint /").strip().split(",")
    machine.fail("touch /.recovery-readonly-probe")
    machine.succeed("test-recovery-reset")
    assert machine.succeed(prefix) == before
    machine.succeed("test $(fw_printenv -c /run/fw_env.config -n BOOT_A_LEFT) = 3")
    machine.succeed("test \"$(fw_printenv -c /run/fw_env.config -n BOOT_ORDER)\" = 'A B'")
    machine.fail("fw_printenv -c /run/fw_env.config NABOS_RESET_SLOT")
    machine.succeed("mount /dev/mmcblk0p4 /run/data; test $(find /run/data -mindepth 1 -maxdepth 1 | wc -l) = 1; test -d /run/data/lost+found; umount /run/data")
    # A cut during formatting leaves authorization; an unreadable p4 can retry.
    machine.succeed("fw_setenv -c /run/fw_env.config NABOS_RESET_SLOT B; dd if=/dev/zero of=/dev/mmcblk0p4 bs=1M count=1 status=none")
    machine.succeed("test-recovery-reset")
    assert machine.succeed(prefix) == before
    machine.succeed("test $(fw_printenv -c /run/fw_env.config -n BOOT_B_LEFT) = 3")
    machine.succeed("test \"$(fw_printenv -c /run/fw_env.config -n BOOT_ORDER)\" = 'B A'")
    machine.fail("fw_printenv -c /run/fw_env.config NABOS_RESET_SLOT")
  '';
}
