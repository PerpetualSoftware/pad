{
  lib,
  stdenv,
  buildGoModule,
  go_1_26,
  nodejs_24,
  importNpmLock,
}:

let
  version = "0.13.0";

  src = lib.fileset.toSource {
    root = ../.;
    fileset = lib.fileset.gitTracked ../.;
  };

  # SvelteKit static build (web/build) that gets embedded into the Go
  # binary via `//go:embed all:web/build` in embed.go. Built separately
  # so the Go derivation only needs a file copy, not a Node toolchain.
  # Uses importNpmLock (per-package fetchurl against web/package-lock.json's
  # own integrity hashes) rather than buildNpmPackage's npmDepsHash, so npm
  # dependency updates never require discovering/updating a separate hash.
  webUI = stdenv.mkDerivation {
    pname = "pad-web";
    inherit version src;
    sourceRoot = "source/web";

    nativeBuildInputs = [
      nodejs_24
      importNpmLock.hooks.linkNodeModulesHook
    ];

    npmDeps = importNpmLock.buildNodeModules {
      npmRoot = ../web;
      nodejs = nodejs_24;
    };

    buildPhase = ''
      runHook preBuild
      # Without sandboxing, npm would otherwise write its cache to the
      # real $HOME Nix sets for purity-checking (which must stay absent
      # between derivations); keep it inside this build's own tmpdir.
      export HOME="$TMPDIR"
      npm run build
      runHook postBuild
    '';

    installPhase = ''
      runHook preInstall
      cp -r build $out
      runHook postInstall
    '';
  };
in
buildGoModule {
  pname = "pad";
  inherit version src;

  go = go_1_26;

  # Update alongside go.sum. Regenerate via:
  #   nix build .#default 2>&1 | grep -A2 'got:'
  vendorHash = "sha256-sxEvWbpdJIk5OJJFGPrewI1mgzNkpPr1tgM+16RRiwk=";

  subPackages = [ "cmd/pad" ];

  # subPackages also narrows buildGoModule's default checkPhase to just
  # cmd/pad; override it to run the full `go test ./...` (matching CI),
  # since cmd/loadtest-collab is an unrelated dev tool we don't ship.
  checkPhase = ''
    runHook preCheck
    # Match buildGoModule's default checkPhase: don't trim source paths
    # for tests, since some (e.g. invocation_framing_test.go) locate the
    # repo root via runtime.Caller.
    export GOFLAGS=''${GOFLAGS//-trimpath/}
    # ValidateWebhookURL does a real net.LookupIP as an SSRF guard;
    # these four subtests exercise that path against example.com, which
    # needs DNS/network the Nix build sandbox deliberately doesn't have.
    # Everything else in the package (invalid schemes, private-IP
    # rejection, etc.) needs no network and still runs.
    # -timeout is explicit for the same reason as CI and the Makefile: the
    # 10m default is a budget nobody chose (TASK-2545). This runs in the
    # Nix sandbox via .github/workflows/nix.yml.
    go test -timeout=45m -skip 'TestValidateWebhookURL/valid_(https|http|with_port|with_path)$' ./...
    runHook postCheck
  '';

  # Populate web/build with the real SvelteKit output before `go build`
  # runs, so `//go:embed all:web/build` in embed.go has real files to
  # embed. postPatch runs after patchPhase, before configure/build.
  postPatch = ''
    rm -rf web/build
    cp -r ${webUI} web/build
  '';

  env.CGO_ENABLED = 0;

  ldflags = [
    "-s"
    "-w"
    "-X main.version=${version}"
  ];

  doCheck = true;

  meta = {
    description = "Local-first project management for developers and AI agents";
    homepage = "https://github.com/PerpetualSoftware/pad";
    changelog = "https://github.com/PerpetualSoftware/pad/releases/tag/v${version}";
    license = lib.licenses.asl20;
    mainProgram = "pad";
    platforms = lib.platforms.unix;
    maintainers = [ ];
  };
}
