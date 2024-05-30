{
  description = "A very basic flake";

  inputs = {
    nixpkgs.url = "github:nixos/nixpkgs?ref=nixos-unstable";
  };

  outputs = { self, nixpkgs }: let
    pkgs = nixpkgs.legacyPackages.x86_64-linux;
  in
  {
    packages.x86_64-linux.sipxel = pkgs.buildGoModule rec {
      pname = "sipxel";
      version = "0.0.1";

      src = ./.;
      vendorHash = "sha256-+oIK3EwfGm7N35bI9//ZJXv1KQgBF+BxjVE+6TRPsvA=";
    };

    devShells.x86_64-linux.default = pkgs.mkShell {
      buildInputs = [ pkgs.go ];
    };
  };
}
