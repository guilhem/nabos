{ pkgs, ... }:
let
  rootfs = ../image/rootfs;
in
{
  users.users.nab-audio = {
    uid = 1004;
    home = "/var/lib/nabos/lva";
    isSystemUser = true;
    createHome = false;
    group = "nab-audio";
    extraGroups = [ "audio" ];
    hashedPassword = "!";
    linger = true;
  };
  users.groups.nab-audio.gid = 1004;
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
  systemd.services."user@1004" = {
    overrideStrategy = "asDropin";
    after = [ "systemd-tmpfiles-setup.service" ];
    serviceConfig.NoNewPrivileges = true;
  };
}
