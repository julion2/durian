//! Compiles the WebKit rasterizer helper (helpers/mailrender) next to the build output.
//! The app spawns it at runtime; its path is baked in as MAILRENDER_BIN.

use std::path::PathBuf;
use std::process::Command;

fn main() {
    let src = "helpers/mailrender/main.swift";
    println!("cargo:rerun-if-changed={src}");
    if std::env::var("CARGO_CFG_TARGET_OS").as_deref() != Ok("macos") {
        return;
    }
    let out = PathBuf::from(std::env::var("OUT_DIR").unwrap()).join("mailrender");
    let status = Command::new("xcrun")
        .args(["swiftc", "-O", "-framework", "AppKit", "-framework", "WebKit", src, "-o"])
        .arg(&out)
        .status()
        .expect("failed to run swiftc (Xcode command line tools required)");
    assert!(status.success(), "swiftc failed for {src}");
    println!("cargo:rustc-env=MAILRENDER_BIN={}", out.display());
}
