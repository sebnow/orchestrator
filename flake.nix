{
  description = "Orchestrator for unattended Claude Code agents";

  inputs = {
    flake-parts.url = "github:hercules-ci/flake-parts";
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs = inputs @ {
    flake-parts,
    nixpkgs,
    ...
  }:
    flake-parts.lib.mkFlake {inherit inputs;} {
      systems = nixpkgs.lib.systems.flakeExposed;
      perSystem = {
        pkgs,
        system,
        ...
      }: {
        _module.args.pkgs = import nixpkgs {
          inherit system;
          config.allowUnfreePredicate = pkg: nixpkgs.lib.getName pkg == "google-chrome";
        };

        formatter = pkgs.alejandra;

        devShells.default = pkgs.mkShell {
          packages =
            [
              pkgs.git
              pkgs.go
              pkgs.gopls
              pkgs.golangci-lint
            ]
            ++ pkgs.lib.optionals pkgs.stdenv.hostPlatform.isLinux [pkgs.chromium]
            ++ pkgs.lib.optionals pkgs.stdenv.hostPlatform.isDarwin [pkgs.google-chrome];
        };
      };
    };
}
