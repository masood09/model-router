{
  description = "model-router: an OpenAI-compatible proxy that switches local LLM models on demand";

  inputs = {
    nixpkgs = {
      url = "github:NixOS/nixpkgs/nixos-26.05";
    };
  };

  outputs = {
    self,
    nixpkgs,
  }: let
    systems = ["x86_64-linux" "aarch64-linux" "x86_64-darwin" "aarch64-darwin"];
    forAll = f: nixpkgs.lib.genAttrs systems (system: f nixpkgs.legacyPackages.${system});
    version = "0.1.0-${self.shortRev or self.dirtyShortRev or "dev"}";
    src = nixpkgs.lib.fileset.toSource {
      root = ./.;
      fileset = nixpkgs.lib.fileset.unions [./go.mod ./cmd ./internal];
    };
  in {
    packages = forAll (pkgs: {
      default = pkgs.callPackage ./nix/package.nix {
        inherit version src;
        # Go 1.27: the default Go of a stable channel can lag behind
        # standard-library security fixes (GO-2026-6859 is fixed in 1.26.11+
        # and 1.27; nixos-26.05 still ships 1.26.10 as its default).
        buildGoModule = pkgs.buildGo127Module or pkgs.buildGoLatestModule;
      };
    });

    overlays = {
      default = final: _prev: {
        model-router = final.callPackage ./nix/package.nix {
          inherit version src;
          buildGoModule = final.buildGo127Module or final.buildGoLatestModule;
        };
      };
    };

    nixosModules = {
      default = import ./nix/module.nix self;
    };

    checks = forAll (pkgs:
      {
        package = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
        gofmt = pkgs.runCommand "gofmt-check" {nativeBuildInputs = [pkgs.go];} ''
          cd ${src}
          unformatted="$(gofmt -l .)"
          if [ -n "$unformatted" ]; then echo "gofmt needed:"; echo "$unformatted"; exit 1; fi
          touch $out
        '';
      }
      // nixpkgs.lib.optionalAttrs pkgs.stdenv.hostPlatform.isLinux {
        vm = import ./nix/vm-test.nix {inherit self pkgs;};
      });

    devShells = forAll (pkgs: {
      default = pkgs.mkShell {
        packages = [(pkgs.go_1_27 or pkgs.go) pkgs.gopls pkgs.go-tools pkgs.govulncheck pkgs.alejandra];
      };
    });

    formatter = forAll (pkgs: pkgs.alejandra);
  };
}
