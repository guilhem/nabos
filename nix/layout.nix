let
  layout = builtins.fromJSON (builtins.readFile ../image/layout.json);
  device = partition: "${layout.disk}p${toString partition}";
in
layout // {
  rauc = {
    system = {
      boot-attempts = layout.boot_attempts;
      boot-attempts-primary = layout.boot_attempts;
    };
    "slot.rootfs.0" = { device = device layout.root_a_partition; type = "ext4"; bootname = "A"; };
    "slot.rootfs.1" = { device = device layout.root_b_partition; type = "ext4"; bootname = "B"; };
    "slot.bootloader.0" = {
      device = layout.disk;
      type = "boot-mbr-switch";
      region-start = toString layout.boot_region_start;
      region-size = toString layout.boot_region_size;
      install-same = false;
    };
  };
}
