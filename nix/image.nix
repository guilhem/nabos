{ pkgs, system, packages, target, version }:
let
  inherit (pkgs) lib;
  config = system.config;
  arm64 = target == "zero2-arm64";
  dtb = if arm64 then "bcm2710-rpi-zero-2-w.dtb" else "bcm2708-rpi-zero-w.dtb";
  kernel = config.boot.kernelPackages.kernel;
  # Runtime references omit headers and native helpers needed on a fresh builder.
  cacheRoots = [ config.system.build.toplevel packages.uboot kernel.dev
    pkgs.gtk4.dev pkgs.gobject-introspection.dev ];
  bootScript = pkgs.writeText "nabos-boot.cmd" (builtins.readFile ../image/boot/boot.cmd);
  rootfs = pkgs.callPackage (pkgs.path + "/nixos/lib/make-ext4-fs.nix") {
    storePaths = [ config.system.build.toplevel ];
    volumeLabel = "nabos-root";
    populateImageCommands = ''
      mkdir -p files/{boot/dtb,boot/overlays,etc/nixos,data,var,tmp,run,proc,sys,dev,bin,usr/bin}
      ln -s ${config.system.build.toplevel}/init files/init
      ln -s ${system.pkgs.bash}/bin/sh files/bin/sh
      ln -s ${system.pkgs.coreutils}/bin/env files/usr/bin/env
      cp ${kernel}/${kernel.target} files/boot/kernel
      cp ${config.system.build.initialRamdisk}/initrd files/boot/initrd
      dtbfile=$(find ${config.hardware.deviceTree.package} -name '${dtb}' -print)
      test -f "$dtbfile"
      # Nixpkgs skips overlays without a matching compatible property.
      node=$(${pkgs.dtc}/bin/fdtget "$dtbfile" /__symbols__ i2c1)
      test "$(${pkgs.dtc}/bin/fdtget "$dtbfile" "$node" status)" = okay
      for symbol in uart0 fb vchiq usb cam1_reg cam_dummy_reg; do
        node=$(${pkgs.dtc}/bin/fdtget "$dtbfile" /__symbols__ "$symbol")
        test "$(${pkgs.dtc}/bin/fdtget "$dtbfile" "$node" status)" = disabled
      done
      cp "$dtbfile" files/boot/dtb/${dtb}
      cp ${packages.sound}/overlays/tagtagtag-sound.dtbo files/boot/overlays/
      printf 'nabos_init=%s\nnabos_kernel_params=%s\n' \
        '${config.system.build.toplevel}/init' '${lib.concatStringsSep " " config.boot.kernelParams}' > files/boot/init
      chmod 1777 files/tmp
    '';
  };
in
pkgs.runCommand "nabos-nixos-${target}-${version}" {
  nativeBuildInputs = with pkgs; [ e2fsprogs dosfstools mtools ubootTools jq ];
  passthru = { inherit rootfs bootScript cacheRoots; systemClosure = config.system.build.toplevel; };
} ''
  mkdir -p $out boot
  cp --reflink=auto --sparse=always ${rootfs} $out/rootfs.ext4
  chmod u+w $out/rootfs.ext4
  test $(stat -c %s $out/rootfs.ext4) -le $((6 * 1024 * 1024 * 1024))
  truncate -s 6G $out/rootfs.ext4
  resize2fs $out/rootfs.ext4
  cp ${pkgs.raspberrypifw}/share/raspberrypi/boot/{bootcode.bin,start*.elf,fixup*.dat,LICENCE.broadcom} boot/
  find ${kernel}/dtbs -name '*.dtb' -exec cp '{}' boot/ \;
  cp ${packages.uboot}/u-boot.bin boot/
  cp ${../image/boot/config.txt} boot/config.txt
  sed -e 's/@TARGET@/${target}/g' -e 's/@DTB@/${dtb}/g' ${../image/boot/boot.env.in} > boot/boot.env
  mkimage -A arm -T script -C none -n 'NabOS NixOS RAUC A/B' -d ${bootScript} boot/boot.scr
  truncate -s 256M $out/boot.vfat
  mkfs.vfat -F32 -n NABOSBOOT $out/boot.vfat
  mcopy -i $out/boot.vfat -s boot/* ::
  mkenvimage -r -s 0x10000 -o $out/uboot.env ${../image/boot/uboot.env}
  cp ${bootScript} $out/boot.cmd
  cp ${config.system.build.toplevel}/kernel-params $out/kernel-params
  printf '%s\n' ${lib.escapeShellArgs cacheRoots} > $out/cache-roots
  jq -n --arg target '${target}' --arg version '${version}' --arg kernel '${kernel.modDirVersion}' \
    --arg system '${config.system.build.toplevel}' --arg initrd '${config.system.build.initialRamdisk}' \
    '{target:$target,version:$version,kernel:$kernel,system:$system,initrd:$initrd,hardware_validated:false}' > $out/build.json
''
