{ pkgs, nativePkgs, kernel, packages }:
let
  tools = with pkgs; [ bash coreutils util-linux e2fsprogs ubootTools kmod gnugrep gawk busybox ];
  led = pkgs.writeShellScript "nabos-recovery-led" ''
    ${pkgs.lib.replaceStrings [ "PATH=/bin:/sbin" ]
      [ "PATH=${pkgs.lib.makeBinPath tools}" ]
      (builtins.readFile ./runtime/recovery-led.sh)}
  '';
  library = pkgs.writeText "nabos-recovery-library"
    (pkgs.lib.replaceStrings [ "/bin/nabos-recovery-led" ] [ "${led}" ]
      (builtins.readFile ./runtime/recovery.sh));
  init = pkgs.writeShellScript "nabos-recovery-init" ''
    export PATH=${pkgs.lib.makeBinPath tools}
    ${pkgs.lib.replaceStrings
      [ "PATH=/bin:/sbin" ". /lib/nabos-recovery.sh" "/bin/nabos-recovery-led" ]
      [ "PATH=${pkgs.lib.makeBinPath tools}" ". ${library}" "${led}" ]
      (builtins.readFile ./runtime/recovery-init.sh)}
  '';
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
    { source = init; target = "/init"; }
    { source = library; }
    { source = led; }
    { source = pkgs.runtimeShell; target = "/bin/sh"; }
    { source = modules + "/lib"; target = "/lib"; }
    { source = ../image/rootfs/etc/fw_env.config; target = "/etc/fw_env.config"; }
  ] ++ map (package: { source = pkgs.lib.getBin package; }) tools;
}
