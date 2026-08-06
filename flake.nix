{
  description = "Pad — local-first project management for developers and AI agents";

  inputs.nixpkgs.url = "https://channels.nixos.org/nixos-26.05/nixexprs.tar.xz";

  outputs =
    { self, nixpkgs }:
    let
      systems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
      ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    in
    {
      packages = forAllSystems (pkgs: {
        default = pkgs.callPackage ./nix/package.nix { };
        pad = pkgs.callPackage ./nix/package.nix { };
      });

      apps = forAllSystems (pkgs: {
        default = {
          type = "app";
          program = "${self.packages.${pkgs.system}.default}/bin/pad";
        };
      });

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = with pkgs; [
            go_1_26
            nodejs_24
            golangci-lint
            sqlite
            gopls
          ];

          shellHook = ''
            echo "pad dev shell: $(go version), node $(node --version)"
          '';
        };
      });

      checks = forAllSystems (pkgs: {
        default = self.packages.${pkgs.system}.default;
        version-smoke = pkgs.runCommand "pad-version-smoke" { } ''
          ${self.packages.${pkgs.system}.default}/bin/pad --version
          touch $out
        '';
      });
    };
}
