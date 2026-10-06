{
  description = "NabOS NixOS appliance (RAUC A/B)";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    nixos-hardware.url = "github:NixOS/nixos-hardware/31cc5f4d9b9ba601071e8b8504601b9b176e2756";
    nixos-hardware.inputs.nixpkgs.follows = "nixpkgs";
  };

  outputs = { self, nixpkgs, nixos-hardware }:
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
            inherit target version;
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
          inherit pkgs;
          target = "zero2-arm64";
          kernelPackages = pkgs.linuxPackages;
        });
    in {
      lib = { inherit mkImage; };
      packages = forSystems (buildSystem:
        nixpkgs.lib.genAttrs targets (target:
          (mkImage { inherit buildSystem target; }).payload) // {
            device-core-native = nativePackages.${buildSystem}.device-core;
            nab-hardware-native = nativePackages.${buildSystem}.nab-hardware;
            nabos-native = nativePackages.${buildSystem}.nabos;
            uboot-sandbox = nativePackages.${buildSystem}.uboot-sandbox;
          });
      devShells = forSystems (buildSystem:
        let pkgs = nixpkgs.legacyPackages.${buildSystem};
        in { default = pkgs.mkShellNoCC {
          packages = with pkgs; [ nix cachix rauc genimage e2fsprogs dosfstools
            mtools ubootTools openssl xz zstd jq python3 fakeroot shellcheck dtc
            go_1_27 rustc cargo pkg-config dbus systemd util-linux ];
        }; });
    };
}
