{
  description = "mweave mounts and serves storage from the command line";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { nixpkgs, ... }:
    let
      system = "x86_64-linux";
      pkgs = nixpkgs.legacyPackages.${system};
      mweave = pkgs.stdenvNoCC.mkDerivation rec {
        pname = "mweave";
        version = "0.2.0";

        src = pkgs.fetchurl {
          url = "https://github.com/nostalume/mountweave-cli/releases/download/v${version}/mweave_v${version}_linux_amd64.tar.gz";
          hash = "sha256-7BfWXCfjigM3Jpn0yMyFJMsPFJSPchAbXtgst2YpNkI=";
        };

        sourceRoot = ".";
        dontBuild = true;
        dontFixup = true;

        installPhase = ''
          runHook preInstall
          install -Dm755 mweave "$out/bin/mweave"
          install -Dm644 LICENSE "$out/share/licenses/mweave/LICENSE"
          cp -R licenses "$out/share/licenses/mweave/"
          runHook postInstall
        '';

        meta = {
          description = "Mount and serve local and remote storage";
          homepage = "https://github.com/nostalume/mountweave-cli";
          license = pkgs.lib.licenses.mit;
          mainProgram = "mweave";
          platforms = [ system ];
        };
      };
    in {
      packages.${system} = {
        inherit mweave;
        default = mweave;
      };
    };
}
