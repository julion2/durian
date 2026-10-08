//! CEF subprocess entry point for the bundled macOS helper applications.

use cef::{args::Args, *};

fn main() {
    let args = Args::new();

    #[cfg(target_os = "macos")]
    let _sandbox = {
        // CEF requires the sandbox to initialize before its framework is loaded.
        let mut sandbox = cef::sandbox::Sandbox::new();
        sandbox.initialize(args.as_main_args());
        sandbox
    };

    #[cfg(target_os = "macos")]
    let _loader = {
        let loader = library_loader::LibraryLoader::new(
            &std::env::current_exe().expect("CEF helper executable path"),
            true,
        );
        assert!(loader.load(), "Could not load the bundled CEF framework");
        loader
    };

    let _ = api_hash(sys::CEF_API_VERSION_LAST, 0);
    let exit_code = execute_process(
        Some(args.as_main_args()),
        None::<&mut App>,
        std::ptr::null_mut(),
    );
    assert!(
        exit_code >= 0,
        "CEF helper was launched as a browser process"
    );
    // Return normally so the sandbox context and framework loader are dropped.
}
