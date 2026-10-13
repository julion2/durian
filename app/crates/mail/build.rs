//! Compiles the WebKit rasterizer helper (helpers/mailrender) next to the build output.
//! The app spawns it at runtime; its path is baked in as MAILRENDER_BIN.

use std::path::PathBuf;
use std::process::Command;

fn main() {
    if std::env::var("CARGO_CFG_TARGET_OS").as_deref() == Ok("linux") {
        build_linux();
        return;
    }
    let src = "helpers/mailrender/main.swift";
    println!("cargo:rerun-if-changed={src}");
    if std::env::var("CARGO_CFG_TARGET_OS").as_deref() != Ok("macos") {
        return;
    }
    // Swift does not use MACOSX_DEPLOYMENT_TARGET as its default target. Pass
    // the same minimum explicitly, preserving the artifact's (not host's) arch.
    println!("cargo:rerun-if-env-changed=MACOSX_DEPLOYMENT_TARGET");
    let minimum = std::env::var("MACOSX_DEPLOYMENT_TARGET").unwrap_or_else(|_| "26.0".into());
    let arch = std::env::var("CARGO_CFG_TARGET_ARCH").expect("target architecture");
    let arch = match arch.as_str() {
        "aarch64" => "arm64",
        "x86_64" => "x86_64",
        other => panic!("unsupported macOS renderer architecture: {other}"),
    };
    let target = format!("{arch}-apple-macosx{minimum}");
    let out = PathBuf::from(std::env::var("OUT_DIR").unwrap()).join("mailrender");
    let status = Command::new("xcrun")
        .args(["swiftc", "-target", &target, "-O", "-framework", "AppKit", "-framework", "WebKit", src, "-o"])
        .arg(&out)
        .status()
        .expect("failed to run swiftc (Xcode command line tools required)");
    assert!(status.success(), "swiftc failed for {src}");
    println!("cargo:rustc-env=MAILRENDER_BIN={}", out.display());
}

fn build_linux() {
    let out = PathBuf::from(std::env::var("OUT_DIR").unwrap());
    std::fs::create_dir_all(out.join("mailrender-extensions")).expect("create WebKit extension directory");
    println!("cargo:rerun-if-env-changed=CXX");
    println!("cargo:rerun-if-env-changed=PKG_CONFIG_PATH");
    for (source, output, package, shared) in [
        ("main.cc", "mailrender", "webkit2gtk-4.1", false),
        ("request-filter.cc", "mailrender-extensions/libdurian-policy.so", "webkit2gtk-web-extension-4.1", true),
    ] {
        let src = format!("helpers/mailrender-linux/{source}");
        println!("cargo:rerun-if-changed={src}");
        let flags = Command::new("pkg-config")
            .args(["--cflags", "--libs", &format!("{package} >= 2.40")])
            .output()
            .expect("pkg-config required for WebKitGTK helper");
        assert!(flags.status.success(), "WebKitGTK 4.1 >= 2.40 development packages required: {}", String::from_utf8_lossy(&flags.stderr));
        let mut compiler = Command::new(std::env::var("CXX").unwrap_or_else(|_| "c++".into()));
        compiler.args(["-std=c++17", "-O2", "-Wall", "-Wextra", "-Werror"]);
        if shared {
            compiler.args(["-fPIC", "-shared"]);
        }
        let status = compiler
            .arg(&src)
            .arg("-o")
            .arg(out.join(output))
            .args(String::from_utf8(flags.stdout).unwrap().split_whitespace())
            // Bazel may supply gcc as CXX; .cc selects C++, but its driver does
            // not link the C++ standard library automatically.
            .arg("-lstdc++")
            .status()
            .expect("C++ compiler required for WebKitGTK helper");
        assert!(status.success(), "WebKitGTK helper compilation failed for {src}");
    }
    println!("cargo:rustc-env=MAILRENDER_BIN={}", out.join("mailrender").display());
}
