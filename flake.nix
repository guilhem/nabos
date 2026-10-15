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
      targets = (builtins.fromJSON (builtins.readFile ./image/sources.lock.json)).targets;
      forSystems = nixpkgs.lib.genAttrs systems;
      mkImage = { buildSystem, target, version ? "dev-local" }:
        let
          board = targets.${target};
          system = nixpkgs.lib.nixosSystem {
            specialArgs = {
              inherit nixos-hardware;
              nabosTarget = target;
              nabosBoard = board;
              nabosVersion = version;
              nabosPackages = packages;
            };
            modules = [
              ./nix/system.nix
              {
                nixpkgs.buildPlatform = buildSystem;
                nixpkgs.hostPlatform = if board.arm64 then "aarch64-linux"
                  else nixpkgs.lib.systems.examples.raspberryPi;
              }
            ];
          };
          packages = import ./nix/packages.nix {
            pkgs = system.pkgs;
            inherit board device-core wm8960 bcm2835-ws2812;
            kernelPackages = system.config.boot.kernelPackages;
          };
          payload = import ./nix/image.nix {
            pkgs = system.pkgs.buildPackages;
            inherit system packages target board version;
          };
        in { inherit system packages payload; };
      nativePackages = forSystems (buildSystem:
        let pkgs = nixpkgs.legacyPackages.${buildSystem};
        in import ./nix/packages.nix {
          inherit pkgs device-core wm8960 bcm2835-ws2812;
          board = targets.zero2-arm64;
          kernelPackages = pkgs.linuxPackages;
        });
    in {
      lib = { inherit mkImage; };
      checks.x86_64-linux.hardware-targets = import ./nix/tests/hardware-targets.nix {
        pkgs = nixpkgs.legacyPackages.x86_64-linux;
        inherit mkImage;
      };
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
        nixpkgs.lib.genAttrs (builtins.attrNames targets) (target:
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
