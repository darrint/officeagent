{
  description = "officeagent - AI-powered office productivity agent";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-25.11";
    beads.url = "github:steveyegge/beads/v0.59.0";
  };

  outputs =
    {
      self,
      nixpkgs,
      beads,
    }:
    let
      systems = [
        "aarch64-darwin"
        "aarch64-linux"
        "x86_64-darwin"
        "x86_64-linux"
      ];
      forAllSystems =
        f:
        nixpkgs.lib.genAttrs systems (
          system:
          f {
            pkgs = import nixpkgs { inherit system; };
            inherit system self;
          }
        );
    in
    {
      devShells = forAllSystems (
        { pkgs, ... }:
        let
          bd = pkgs.buildGoModule {
            pname = "bd";
            version = "0.59.0";
            src = beads;
            subPackages = [ "cmd/bd" ];
            doCheck = false;
            vendorHash = "sha256-ygZPi56fVEHaEShGVGpObFkrLs1DHrM8i2Y4BktMmpA=";
            postPatch = ''
              goVer="$(go env GOVERSION | sed 's/^go//')"
              sed -i "s/^go .*/go $goVer/" go.mod
            '';
            env.GOTOOLCHAIN = "auto";
          };
        in
        {
          default = pkgs.mkShell {
            buildInputs = with pkgs; [
              go
              git
              gopls
              gotools
              golangci-lint
              sqlite
              dolt
              bd
              gh
            ];
            shellHook = ''
              echo "officeagent development shell"
              echo "Go version: $(go version)"
            '';
          };
        }
      );
    };
}
