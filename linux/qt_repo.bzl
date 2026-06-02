def _qt_local_repository_impl(ctx):
    qt_root = ctx.os.environ.get("QTDIR") or ctx.os.environ.get("QT_HOME")
    if not qt_root:
        fail("Qt6 not found. Set QTDIR or QT_HOME to your Qt6 install root.")

    qt_root_path = ctx.path(qt_root)
    if not qt_root_path.exists:
        fail("Qt root does not exist: %s" % qt_root)

    # Find lib dir — handles /usr/lib, /usr/lib64, /usr/lib64/qt6/lib
    lib_dir = None
    for candidate in [
        qt_root + "/lib",
        qt_root + "/lib64",
        qt_root + "/lib/x86_64-linux-gnu",  # Ubuntu/Debian x86_64
        qt_root + "/lib/aarch64-linux-gnu",  # Ubuntu/Debian arm64
        qt_root + "/../",  # QTDIR=/usr/lib64/qt6 → libs in /usr/lib64
    ]:
        p = ctx.path(candidate)
        if p.exists and ctx.path(candidate + "/libQt6Core.so").exists:
            lib_dir = candidate
            break

        # macOS frameworks
        if p.exists and ctx.path(candidate + "/QtCore.framework").exists:
            lib_dir = candidate
            break
    if not lib_dir:
        # Fallback: just pick first existing
        for candidate in [qt_root + "/lib", qt_root + "/lib64"]:
            if ctx.path(candidate).exists:
                lib_dir = candidate
                break
    if not lib_dir:
        fail("Qt lib dir not found under: %s" % qt_root)

    ctx.symlink(lib_dir, "lib")

    # Create wrapper scripts for rcc and moc (symlinks break in Bazel sandbox)
    for tool in ["rcc", "moc"]:
        tool_path = None
        for candidate in [
            qt_root + "/libexec/" + tool,
            qt_root + "/bin/" + tool,
            qt_root + "/share/qt/libexec/" + tool,
            # Fedora: QTDIR=/usr, tools in /usr/lib64/qt6/libexec/
            qt_root + "/lib64/qt6/libexec/" + tool,
            # Ubuntu/Debian: QTDIR=/usr, tools in /usr/lib/qt6/libexec/
            qt_root + "/lib/qt6/libexec/" + tool,
            qt_root + "/lib/x86_64-linux-gnu/qt6/libexec/" + tool,
            qt_root + "/lib/aarch64-linux-gnu/qt6/libexec/" + tool,
        ]:
            if ctx.path(candidate).exists:
                tool_path = candidate
                break
        if not tool_path:
            fail("Qt tool '%s' not found under %s" % (tool, qt_root))
        ctx.file(tool, "#!/bin/bash\nexec %s \"$@\"\n" % tool_path, executable = True)

    # Find include dir — handles /usr/include/qt6 (Fedora) and /usr/include (standard)
    include_dir = None
    for candidate in [
        qt_root + "/include/qt6",
        qt_root + "/include/x86_64-linux-gnu/qt6",  # Ubuntu/Debian x86_64
        qt_root + "/include/aarch64-linux-gnu/qt6",  # Ubuntu/Debian arm64
        qt_root + "/include",
    ]:
        if ctx.path(candidate).exists and ctx.path(candidate + "/QtCore").exists:
            include_dir = candidate
            break
    if not include_dir:
        for candidate in [qt_root + "/include/qt6", qt_root + "/include"]:
            if ctx.path(candidate).exists:
                include_dir = candidate
                break
    has_include = include_dir and ctx.path(include_dir).exists
    has_frameworks = ctx.path(lib_dir + "/QtWidgets.framework").exists

    if not has_include and not has_frameworks:
        fail("Qt headers not found. Expected include dir with QtCore or Qt*.framework under %s" % qt_root)

    if has_include:
        ctx.symlink(include_dir, "include")

    # Header-only modules (e.g. QtQmlIntegration) may live in a sibling package
    # on Homebrew. Check for qtdeclarative include dir.
    qt_decl_include = None
    for candidate in [
        qt_root + "/../qtdeclarative/include",  # Homebrew symlink
    ]:
        if ctx.path(candidate).exists:
            qt_decl_include = candidate
            break

    if qt_decl_include:
        ctx.symlink(qt_decl_include, "include_decl")

    if has_frameworks:
        qt_includes = "qt_includes"
        ctx.symlink(lib_dir + "/QtCore.framework/Headers", qt_includes + "/QtCore")
        ctx.symlink(lib_dir + "/QtGui.framework/Headers", qt_includes + "/QtGui")
        ctx.symlink(lib_dir + "/QtWidgets.framework/Headers", qt_includes + "/QtWidgets")
        ctx.symlink(lib_dir + "/QtNetwork.framework/Headers", qt_includes + "/QtNetwork")
        ctx.symlink(lib_dir + "/QtQuick.framework/Headers", qt_includes + "/QtQuick")
        ctx.symlink(lib_dir + "/QtQml.framework/Headers", qt_includes + "/QtQml")
        ctx.symlink(lib_dir + "/QtQuickControls2.framework/Headers", qt_includes + "/QtQuickControls2")
        ctx.symlink(lib_dir + "/QtWebEngineCore.framework/Headers", qt_includes + "/QtWebEngineCore")
        ctx.symlink(lib_dir + "/QtWebEngineQuick.framework/Headers", qt_includes + "/QtWebEngineQuick")
        ctx.symlink(lib_dir + "/QtTest.framework/Headers", qt_includes + "/QtTest")

        includes = [
            "lib/QtCore.framework/Headers",
            "lib/QtCore.framework/Headers/QtCore",
            "lib/QtGui.framework/Headers",
            "lib/QtGui.framework/Headers/QtGui",
            "lib/QtWidgets.framework/Headers",
            "lib/QtWidgets.framework/Headers/QtWidgets",
            "lib/QtNetwork.framework/Headers",
            "lib/QtNetwork.framework/Headers/QtNetwork",
            "lib/QtQuick.framework/Headers",
            "lib/QtQuick.framework/Headers/QtQuick",
            "lib/QtQml.framework/Headers",
            "lib/QtQml.framework/Headers/QtQml",
            "lib/QtQuickControls2.framework/Headers",
            "lib/QtQuickControls2.framework/Headers/QtQuickControls2",
            "lib/QtWebEngineCore.framework/Headers",
            "lib/QtWebEngineCore.framework/Headers/QtWebEngineCore",
            "lib/QtWebEngineQuick.framework/Headers",
            "lib/QtWebEngineQuick.framework/Headers/QtWebEngineQuick",
            "lib/QtTest.framework/Headers",
            "lib/QtTest.framework/Headers/QtTest",
            "qt_includes",
        ] + (["include_decl", "include_decl/QtQmlIntegration"] if qt_decl_include else [])
        linkopts = [
            "-F" + lib_dir,
            "-Wl,-rpath," + lib_dir,
            "-framework",
            "QtWidgets",
            "-framework",
            "QtGui",
            "-framework",
            "QtCore",
            "-framework",
            "QtNetwork",
            "-framework",
            "QtQuick",
            "-framework",
            "QtQml",
            "-framework",
            "QtQuickControls2",
            "-framework",
            "QtWebEngineCore",
            "-framework",
            "QtWebEngineQuick",
            "-framework",
            "QtTest",
        ]
    else:
        includes = [
            "include",
            "include/QtCore",
            "include/QtGui",
            "include/QtWidgets",
            "include/QtNetwork",
            "include/QtQuick",
            "include/QtQml",
            "include/QtQmlIntegration",
            "include/QtQuickControls2",
            "include/QtWebEngineCore",
            "include/QtWebEngineQuick",
            "include/QtTest",
        ]
        linkopts = [
            "-L" + lib_dir,
            "-Wl,-rpath," + lib_dir,
            "-lQt6Widgets",
            "-lQt6Gui",
            "-lQt6Core",
            "-lQt6Network",
            "-lQt6Quick",
            "-lQt6Qml",
            "-lQt6QuickControls2",
            "-lQt6WebEngineCore",
            "-lQt6WebEngineQuick",
            "-lQt6Test",
        ]

    build = """
exports_files(["rcc", "moc"])

cc_library(
    name = "qt6",
    hdrs = glob(["include/**", "include_decl/**", "lib/**/Headers/**", "qt_includes/**"], allow_empty = True),
    includes = {includes},
    defines = [
        "QT_WIDGETS_LIB",
        "QT_GUI_LIB",
        "QT_CORE_LIB",
        "QT_QUICK_LIB",
        "QT_QML_LIB",
    ],
    linkopts = {linkopts},
    visibility = ["//visibility:public"],
)
""".format(includes = includes, linkopts = linkopts)

    ctx.file("BUILD.bazel", build)

qt_local_repository = repository_rule(
    implementation = _qt_local_repository_impl,
    local = True,
    environ = ["QTDIR", "QT_HOME"],
)
