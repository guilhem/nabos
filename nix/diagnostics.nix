{ config, pkgs, ... }:
let
  collector = pkgs.writeShellScript "nabos-initrd-log" ''
    export PATH=/bin:/sbin
    ${builtins.readFile ./runtime/initrd-log.sh}
  '';
in {
  services.journald.settings.Journal = {
    Storage = "persistent";
    SplitMode = "none";
    SystemMaxUse = "32M";
    SystemMaxFileSize = "4M";
    SystemKeepFree = "64M";
    RuntimeMaxUse = "16M";
    SyncIntervalSec = "5s";
  };
  systemd.services.systemd-journal-flush.serviceConfig = {
    TimeoutStartSec = "3s";
    TimeoutStopSec = "2s";
  };
  boot.initrd.systemd = {
    storePaths = [ collector "${config.boot.initrd.systemd.package}/bin/journalctl" ];
    extraBin = {
      nabos-initrd-log = collector;
      journalctl = "${config.boot.initrd.systemd.package}/bin/journalctl";
      findmnt = "${pkgs.util-linux}/bin/findmnt";
    };
    services.nabos-initrd-log = {
      description = "Save optional bounded initrd diagnostics on the data partition";
      after = [ "systemd-journald.service" ];
      conflicts = [ "shutdown.target" ];
      before = [ "shutdown.target" ];
      unitConfig.DefaultDependencies = false;
      serviceConfig = {
        Type = "exec";
        ExecStartPre = "/bin/timeout --signal=KILL 3s ${collector} check";
        ExecStart = "${collector} collect";
        TimeoutStartSec = "3s";
        TimeoutStopSec = "2s";
        Restart = "no";
        UMask = "0077";
        LimitFSIZE = 4194304;
        LimitCORE = 0;
      };
    };
  };
}
