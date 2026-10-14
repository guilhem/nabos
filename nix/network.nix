{
  networking.wireless = {
    # NetworkManager owns persistent profiles; WPA must not write to immutable /etc.
    allowAuxiliaryImperativeNetworks = false;
    extraConfig = "update_config=0";
  };
  services.avahi = { enable = true; nssmdns4 = true; publish = { enable = true; addresses = true; }; };
  # Precreate the static service directory before systemd starts on immutable /etc.
  environment.etc."avahi/services/.keep".text = "";
}
