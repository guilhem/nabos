{
  description = "NabOS NixOS appliance (RAUC A/B)";

  inputs = {
    device-core = { url = "github:guilhem/device-core"; flake = false; };
    wm8960 = { url = "github:guilhem/wm8960/nabos-gpio-keys"; flake = false; };
    bcm2835-ws2812 = { url = "github:guilhem/bcm2835-ws2812"; flake = false; };
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    nixos-hardware.url = "github:NixOS/nixos-hardware";
    nixos-hardware.inputs.nixpkgs.follows = "nixpkgs";
  };

  outputs = { self, nixpkgs, nixos-hardware, device-core, wm8960, bcm2835-ws2812 }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];
      targets = [ "zero-armv6" "zero2-arm64" ];
      forSystems = nixpkgs.lib.genAttrs systems;
      mkImage = { buildSystem, target, version ? "dev-local" }:
        let
          arm64 = target == "zero2-arm64";
          hostPlatform = if arm64 then "aarch64-linux" else nixpkgs.lib.systems.examples.raspberryPi;
          system = nixpkgs.lib.nixosSystem {
            specialArgs = {
              nabosTarget = target;
              nabosVersion = version;
              nabosPackages = packages;
            };
            modules = [
              ./nix/system.nix
              ({ config, pkgs, ... }: {
                nixpkgs.buildPlatform = buildSystem;
                nixpkgs.hostPlatform = hostPlatform;
                boot.kernelPackages = pkgs.linuxPackagesFor ((pkgs.callPackage
                  (nixos-hardware + "/raspberry-pi/common/kernel.nix") {
                    rpiVersion = if arm64 then 3 else 1;
                    # The vendor definition replaces kernelPatches; append the
                    # appliance settings after its defaults through argsOverride.
                    argsOverride.kernelPatches = with pkgs.kernelPatches; [
                      bridge_stp_helper request_key_helper
                    ] ++ config.boot.kernelPatches;
                  }).overrideAttrs (old: {
                    postConfigure = (old.postConfigure or "") + "\n" +
                      nixpkgs.lib.concatMapStringsSep "\n" (patch:
                        nixpkgs.lib.concatStringsSep "\n" (nixpkgs.lib.mapAttrsToList
                          (name: value: "grep -qxF " + nixpkgs.lib.escapeShellArg
                            (if value.tristate == "n" then "# CONFIG_${name} is not set"
                             else "CONFIG_${name}=${value.tristate}") + " $buildRoot/.config")
                          (patch.structuredExtraConfig or {})))
                      (builtins.filter (patch: patch.name == "nabos-appliance") config.boot.kernelPatches);
                  }));
              })
            ];
          };
          packages = import ./nix/packages.nix {
            pkgs = system.pkgs;
            inherit target device-core wm8960 bcm2835-ws2812;
            kernelPackages = system.config.boot.kernelPackages;
          };
          payload = import ./nix/image.nix {
            pkgs = system.pkgs.buildPackages;
            inherit system packages target version;
          };
        in { inherit system packages payload; };
      nativePackages = forSystems (buildSystem:
        let pkgs = nixpkgs.legacyPackages.${buildSystem};
        in import ./nix/packages.nix {
          inherit pkgs device-core wm8960 bcm2835-ws2812;
          target = "zero2-arm64";
          kernelPackages = pkgs.linuxPackages;
        });
    in {
      lib = { inherit mkImage; };
      checks.x86_64-linux.audio-boot = import ./nix/tests/audio-boot.nix {
        pkgs = nixpkgs.legacyPackages.x86_64-linux;
      };
      checks.x86_64-linux.network-boot = import ./nix/tests/network-boot.nix {
        pkgs = nixpkgs.legacyPackages.x86_64-linux;
      };
      checks.x86_64-linux.boot-logs = import ./nix/tests/boot-logs.nix {
        pkgs = nixpkgs.legacyPackages.x86_64-linux;
      };
      checks.x86_64-linux.factory-reset = import ./nix/tests/recovery.nix {
        pkgs = nixpkgs.legacyPackages.x86_64-linux;
      };
      packages = forSystems (buildSystem:
        nixpkgs.lib.genAttrs targets (target:
          (mkImage { inherit buildSystem target; }).payload) // {
            device-core-native = nativePackages.${buildSystem}.device-core;
            nab-hardware-native = nativePackages.${buildSystem}.nab-hardware;
            nabos-native = nativePackages.${buildSystem}.nabos;
            uboot-sandbox = nativePackages.${buildSystem}.uboot-sandbox;
          });
      devShells = forSystems (buildSystem:
        let
          pkgs = nixpkgs.legacyPackages.${buildSystem};
          image = pkgs.mkShellNoCC {
            packages = with pkgs; [ nix cachix rauc genimage e2fsprogs dosfstools
              mtools ubootTools openssl xz zstd cpio jq python3 fakeroot dtc squashfsTools
              go_1_27 dbus systemd util-linux ];
          };
        in {
          inherit image;
          default = pkgs.mkShellNoCC {
            inputsFrom = [ image ];
            packages = with pkgs; [ rustc cargo pkg-config shellcheck ];
          };
        });
    };
}
