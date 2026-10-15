{ pkgs, nativePkgs, kernel, packages }:
let
  tools = with pkgs; [ bash coreutils util-linux e2fsprogs ubootTools kmod gnugrep gawk busybox ];
  commands = with pkgs;
    [ { source = runtimeShell; target = "/bin/sh"; } ]
    ++ map (name: { source = "${coreutils}/bin/${name}"; target = "/bin/${name}"; })
      [ "cat" "mkdir" "mv" "sleep" "stat" "sync" "timeout" ]
    ++ map (name: { source = "${util-linux}/bin/${name}"; target = "/bin/${name}"; })
      [ "blkid" "mount" "umount" "sfdisk" ]
    ++ map (name: { source = "${e2fsprogs}/bin/${name}"; target = "/bin/${name}"; })
      [ "mkfs.ext4" "e2fsck" ]
    ++ map (name: { source = "${ubootTools}/bin/${name}"; target = "/bin/${name}"; })
      [ "fw_printenv" "fw_setenv" ]
    ++ [
      { source = "${kmod}/bin/modprobe"; target = "/bin/modprobe"; }
      { source = "${gawk}/bin/awk"; target = "/bin/awk"; }
      { source = "${gnugrep}/bin/grep"; target = "/bin/grep"; }
      { source = "${busybox}/bin/reboot"; target = "/bin/reboot"; }
    ];
  modules = pkgs.makeModulesClosure {
    kernel = pkgs.aggregateModules [ (pkgs.lib.getOutput "modules" kernel) packages.led ];
    firmware = nativePkgs.runCommand "nabos-recovery-no-firmware" { } "mkdir -p $out/lib/firmware";
    rootModules = [ "mmc_block" "ext4" "bcm2835-ws2812" ];
    allowMissing = false;
  };
in
nativePkgs.makeInitrdNG {
  name = "nabos-recovery-initrd";
  compressor = "zstd";
  contents = [
    { source = ./runtime/recovery-init.sh; target = "/init"; }
    { source = ./runtime/recovery.sh; target = "/lib/nabos-recovery.sh"; }
    { source = ../image/layout.sh; target = "/lib/nabos-layout.sh"; }
    { source = ./runtime/recovery-led.sh; target = "/bin/nabos-recovery-led"; }
    { source = modules + "/lib/modules"; target = "/lib/modules"; }
    { source = ../image/rootfs/etc/fw_env.config; target = "/etc/fw_env.config"; }
  ] ++ commands ++ map (package: { source = pkgs.lib.getBin package; }) tools;
}
