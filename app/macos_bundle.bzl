"""Bundle an already-linked Rust executable; rules_apple's application rule links its own main."""

def _macos_bundle_impl(ctx):
    minimum_os = ctx.configuration.default_shell_env.get("MACOSX_DEPLOYMENT_TARGET")
    if not minimum_os:
        fail("Missing MACOSX_DEPLOYMENT_TARGET; enable the repository's macos Bazel configuration.")
    bundle = ctx.actions.declare_directory("Durian Preview.app")
    inputs = [ctx.file.gui, ctx.file.cli, ctx.file.renderer, ctx.file.icon, ctx.file.license, ctx.file.version, ctx.file.plist, ctx.file._packager]
    ctx.actions.run_shell(
        inputs = inputs,
        outputs = [bundle],
        arguments = [ctx.file._packager.path] + [f.path for f in inputs[:-1]] + [bundle.path, minimum_os],
        command = """
            set -euo pipefail
            bash "$@"
            bundle="${@: -2:1}"
            minimum="${@: -1}"
            /usr/bin/plutil -lint "$bundle/Contents/Info.plist"
            # A valid signature/plist cannot prove OS compatibility. Reject a
            # binary requiring a newer OS than the bundle promises, including
            # the helpers (Go may use the older LC_VERSION_MIN_MACOSX command).
            for binary in durian durian-cli mailrender; do
                versions=$(/usr/bin/xcrun vtool -show-build "$bundle/Contents/MacOS/$binary")
                if ! printf '%s\\n' "$versions" | awk -v maximum="$minimum" '
                    function newer(version, actual, limit, i) {
                        split(version, actual, /[.]/); split(maximum, limit, /[.]/)
                        for (i=1; i<=3; i++) {
                            if (actual[i]+0 != limit[i]+0) return actual[i]+0 > limit[i]+0
                        }
                        return 0
                    }
                    $1 == "cmd" { mac = legacy = ($2 == "LC_VERSION_MIN_MACOSX") }
                    $1 == "platform" { mac = ($2 == "MACOS") }
                    $1 == "minos" || (legacy && $1 == "version") {
                        seen++
                        if (!mac || $2 !~ /^[0-9]+[.][0-9]+([.][0-9]+)?$/ || newer($2)) bad=1
                    }
                    END { exit (!seen || bad) }
                '; then
                    printf '%s requires an unsupported platform or macOS newer than %s:\\n%s\\n' "$binary" "$minimum" "$versions" >&2
                    exit 1
                fi
            done
            # Sign only copies inside this output, never dependency outputs in bazel-bin.
            for binary in durian-cli mailrender; do
                /usr/bin/codesign --force --sign - --timestamp=none "$bundle/Contents/MacOS/$binary"
            done
            /usr/bin/codesign --force --sign - --timestamp=none "$bundle"
            /usr/bin/codesign --verify --deep --strict "$bundle"
        """,
        mnemonic = "PackageMacApp",
        progress_message = "Packaging and ad-hoc signing Durian Preview.app",
    )
    return [DefaultInfo(files = depset([bundle]))]

macos_bundle = rule(
    implementation = _macos_bundle_impl,
    attrs = {
        "gui": attr.label(mandatory = True, allow_single_file = True),
        "cli": attr.label(mandatory = True, allow_single_file = True),
        "renderer": attr.label(mandatory = True, allow_single_file = True),
        "icon": attr.label(mandatory = True, allow_single_file = True),
        "license": attr.label(mandatory = True, allow_single_file = True),
        "version": attr.label(mandatory = True, allow_single_file = True),
        "plist": attr.label(mandatory = True, allow_single_file = True),
        "_packager": attr.label(default = ":package_macos.sh", allow_single_file = True),
    },
)
