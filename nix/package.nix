{
  lib,
  buildGo126Module,
}:
buildGo126Module {
  pname = "regate";
  version = "1.0.0-dev";

  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.unions [
      ../cmd
      ../internal
      ../skills
      ../go.mod
      ../go.sum
    ];
  };
  vendorHash = "sha256-LuUKWmM31ycJUJrxw30WIIgB4KmAugt1qwJZTRhM9sE=";
  subPackages = [ "cmd/regate" ];
  env.CGO_ENABLED = "0";
  # The server integration suite runs separately with loopback fixtures.
  doCheck = false;

  postInstall = ''
    mkdir -p "$out/share/regate"
    cp -r skills "$out/share/regate/skills"
  '';

  meta.mainProgram = "regate";
}
