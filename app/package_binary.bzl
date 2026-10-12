"""Package a native executable beside the GPUI app."""

def _package_binary_impl(ctx):
    binary = ctx.actions.declare_file(ctx.label.name)
    ctx.actions.run_shell(
        inputs = [ctx.executable.src],
        outputs = [binary],
        arguments = [ctx.executable.src.path, binary.path],
        command = 'cp "$1" "$2" && chmod +x "$2"',
        mnemonic = "PackageBinary",
        progress_message = "Packaging %s beside the GPUI app" % ctx.label.name,
    )
    return [DefaultInfo(executable = binary, runfiles = ctx.runfiles(files = [binary]))]

package_binary = rule(
    implementation = _package_binary_impl,
    executable = True,
    attrs = {
        "src": attr.label(mandatory = True, executable = True, cfg = "target"),
    },
)
