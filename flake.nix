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
      vendorHash = "sha256-EWRII7pvpWlTFYF1bihu7IObx7pvBxbGVhQ5PpzhYE8=";
    };

    devShells.x86_64-linux.default = pkgs.mkShell {
      buildInputs = [ pkgs.go ];
    };
  };
}
