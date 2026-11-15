{
  pkgs ? import (builtins.fetchTarball {
    url = "https://github.com/NixOS/nixpkgs/archive/d4f25a3e562008648d45bcf2d70bcc7422cad84e.tar.gz";
    sha256 = "sha256-x9sADJvoVG0L6RejZB+/BB4YiGZFBZrfTlhF4uBq1tQ=";
  }) { },
}:

let
  clnrest = pkgs.stdenvNoCC.mkDerivation {
    pname = "clnrest";
    version = "26.06.10";
    src = pkgs.fetchurl {
      url = "https://github.com/ElementsProject/lightning/releases/download/v26.06.10/clightning-v26.06.10-Ubuntu-22.04-amd64.tar.xz";
      sha256 = "e1e0a7e89b55060e2470d8568ba535a3124d6823c628def548cadeef4279c491";
    };
    dontUnpack = true;
    nativeBuildInputs = [ pkgs.autoPatchelfHook ];
    buildInputs = [ pkgs.stdenv.cc.cc.lib ];
    installPhase = ''
      tar -xJf "$src" ./usr/libexec/c-lightning/plugins/clnrest
      install -Dm755 usr/libexec/c-lightning/plugins/clnrest "$out/bin/clnrest"
    '';
  };
in
assert pkgs.stdenv.hostPlatform.system == "x86_64-linux";
pkgs.mkShell {
  packages = with pkgs; [ go gcc bitcoin clightning lnd ];
  LNURL_CLNREST_PLUGIN = "${clnrest}/bin/clnrest";
}
