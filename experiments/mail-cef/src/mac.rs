//! Cocoa bootstrap required by CEF before the browser process initializes.

use cef::application_mac::{CefAppProtocol, CrAppControlProtocol, CrAppProtocol};
use objc2::{
    ClassType, DefinedClass, MainThreadMarker, define_class, extern_methods, msg_send,
    rc::Retained,
    runtime::{Bool, NSObjectProtocol},
};
use objc2_app_kit::{NSApp, NSApplication, NSEvent};
use std::{cell::Cell, ffi::c_void, time::Duration};

#[link(name = "CoreFoundation", kind = "framework")]
unsafe extern "C" {
    static kCFRunLoopDefaultMode: *const c_void;
    fn CFRunLoopRunInMode(mode: *const c_void, seconds: f64, return_after_source: u8) -> i32;
}

#[derive(Default)]
pub struct DurianCefApplicationIvars {
    handling_send_event: Cell<Bool>,
}

define_class!(
    /// CEF requires the process-wide NSApplication to implement CefAppProtocol.
    #[unsafe(super(NSApplication))]
    #[ivars = DurianCefApplicationIvars]
    pub struct DurianCefApplication;

    impl DurianCefApplication {
        #[unsafe(method(sendEvent:))]
        unsafe fn send_event(&self, event: &NSEvent) {
            let was_sending_event = self.is_handling_send_event();
            if !was_sending_event {
                self.set_handling_send_event(true);
            }

            let _: () = msg_send![super(self), sendEvent: event];

            if !was_sending_event {
                self.set_handling_send_event(false);
            }
        }
    }

    unsafe impl CrAppControlProtocol for DurianCefApplication {
        #[unsafe(method(setHandlingSendEvent:))]
        unsafe fn _set_handling_send_event(&self, handling_send_event: Bool) {
            self.ivars().handling_send_event.set(handling_send_event);
        }
    }

    unsafe impl CrAppProtocol for DurianCefApplication {
        #[unsafe(method(isHandlingSendEvent))]
        unsafe fn _is_handling_send_event(&self) -> Bool {
            self.ivars().handling_send_event.get()
        }
    }

    unsafe impl CefAppProtocol for DurianCefApplication {}
);

impl DurianCefApplication {
    extern_methods! {
        #[unsafe(method(sharedApplication))]
        fn shared_application() -> Retained<Self>;

        #[unsafe(method(setHandlingSendEvent:))]
        fn set_handling_send_event(&self, handling_send_event: bool);

        #[unsafe(method(isHandlingSendEvent))]
        fn is_handling_send_event(&self) -> bool;
    }
}

/// Installs CEF's required NSApplication subclass before anything else reads NSApp.
pub fn setup_application() {
    let _ = DurianCefApplication::shared_application();
    let main_thread =
        MainThreadMarker::new().expect("CEF must initialize on the macOS main thread");
    assert!(NSApp(main_thread).isKindOfClass(DurianCefApplication::class()));
}

/// Lets Cocoa and CEF finish subprocess teardown before cef::shutdown().
pub fn drain_shutdown() {
    for _ in 0..10 {
        // Matches CEF's macOS external-message-pump shutdown drain.
        unsafe {
            CFRunLoopRunInMode(kCFRunLoopDefaultMode, 0.001, 1);
        }
        cef::do_message_loop_work();
        std::thread::sleep(Duration::from_millis(50));
    }
}
