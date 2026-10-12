"""Package the build.rs-produced helper beside the native app executable."""

def _mailrender_impl(ctx):
    out_dir = ctx.file.out_dir
    helper = ctx.actions.declare_file(ctx.label.name)
    files = [helper]
    arguments = [out_dir.path, helper.path]
    command = 'cp "$1/mailrender" "$2" && chmod +x "$2"'
    if ctx.attr.linux:
        extension = ctx.actions.declare_directory("mailrender-extensions")
        files.append(extension)
        arguments.append(extension.path)
        # Build-script inputs may be sandbox symlinks. Ship the actual library,
        # not a link outside the directory exposed to WebKit's process sandbox.
        command += ' && mkdir -p "$3" && cp -RL "$1/mailrender-extensions/." "$3/"'
    ctx.actions.run_shell(
        inputs = [out_dir],
        outputs = files,
        arguments = arguments,
        command = command,
        mnemonic = "PackageMailrender",
        progress_message = "Packaging the WebKit mail renderer",
    )
    return [DefaultInfo(files = depset(files), runfiles = ctx.runfiles(files = files))]

mailrender = rule(
    implementation = _mailrender_impl,
    attrs = {
        "out_dir": attr.label(mandatory = True, allow_single_file = True),
        "linux": attr.bool(default = False),
    },
)
