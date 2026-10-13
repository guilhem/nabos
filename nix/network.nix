{
  networking.wireless = {
    # NetworkManager owns persistent profiles; WPA must not write to immutable /etc.
    allowAuxiliaryImperativeNetworks = false;
    extraConfig = "update_config=0";
  };
}
