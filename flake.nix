{
  description = "Durian — email client (Go CLI backend + Swift macOS GUI) dev toolchain";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
  };

  outputs = { self, nixpkgs }:
    let
      systems = [ "aarch64-darwin" "x86_64-darwin" "aarch64-linux" "x86_64-linux" ];
      forAllSystems = f:
        nixpkgs.lib.genAttrs systems (system: f (import nixpkgs { inherit system; }));
    in
    {
      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          name = "durian";

          # The CLI and GUI are built through Bazel (bazelisk reads .bazelversion);
          # never `go build` directly. `go` here is for tooling and to let Hugo
          # mount the docs theme module. `hugo` builds the docs/ site.
          packages = with pkgs; [
            bazelisk # build/test: bazel //cli/... //macos/... //integration/...
            pkl # config runtime — durian evaluates *.pkl via `pkl eval`
            go # tooling + Hugo module fetch for the docs theme
            hugo # docs site (docs/, Hextra theme via Hugo modules)
            buildifier # BUILD.bazel formatter — CI gate (CLI job exit 4)
            gofumpt # stricter gofmt; CI fails on any unformatted ./cli file
          ]
          ++ lib.optionals stdenv.hostPlatform.isLinux [
            libsecret # `secret-tool` for keychain credential storage on Linux
          ];

          shellHook = ''
            export BAZEL_USE_CPP_ONLY_TOOLCHAIN=1
            echo "durian dev shell: bazelisk, pkl, hugo, go"
            echo "  docs:  cd docs && hugo server -D"
            echo "  build: bazel build //cli/cmd/durian"
          '';
        };
      });
    };
}
