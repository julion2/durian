//! Separate native HTML window. Deliberately NOT an embedded GPUI WebView.
mod document;

use base64::Engine as _;
use std::{borrow::Cow, collections::HashMap, io::Read, time::Duration};
use tao::{
    event::{Event, WindowEvent},
    event_loop::{ControlFlow, EventLoopBuilder},
    window::WindowBuilder,
};
use wry::{NewWindowResponse, PermissionResponse, WebViewBuilder, http::Response};

#[derive(Debug)]
enum ViewerEvent {
    Ipc(String),
    Timeout,
}

fn main() -> Result<(), Box<dyn std::error::Error>> {
    let args: Vec<_> = std::env::args().collect();
    let verify = args.iter().any(|arg| arg == "--self-test");
    let demo = verify || args.iter().any(|arg| arg == "--demo");
    let narrow = args.iter().any(|arg| arg == "--narrow");
    let mail: document::Mail = if demo {
        document::Mail {
            html: include_str!("../fixtures/browser.html").into(),
            dark: args.iter().any(|arg| arg == "--dark"),
            images: HashMap::from([(
                "review-image".into(),
                base64::engine::general_purpose::STANDARD
                    .encode(include_bytes!("../../gpui-kit/fixtures/review.png")),
            )]),
        }
    } else {
        let mut input = Vec::new();
        std::io::stdin()
            .take(32 * 1024 * 1024 + 1)
            .read_to_end(&mut input)?;
        if input.len() > 32 * 1024 * 1024 {
            return Err("Mail preview input exceeds 32 MiB".into());
        }
        serde_json::from_slice(&input)?
    };
    let document = document::prepare(&mail)?;
    let event_loop = EventLoopBuilder::<ViewerEvent>::with_user_event().build();
    let window = WindowBuilder::new()
        .with_title("HTML preview · remote content blocked — Durian")
        .with_inner_size(tao::dpi::LogicalSize::new(
            if narrow { 520.0 } else { 920.0 },
            760.0,
        ))
        .build(&event_loop)?;
    let proxy = event_loop.create_proxy();
    let dark_transform = document::dark_script();
    let dark = mail.dark;
    let host = format!(
        r#"addEventListener('DOMContentLoaded', () => {{
            if ({dark}) {{
                {dark_transform}
                document.documentElement.style.setProperty('background', '#2a2a2c', 'important');
                document.body.style.setProperty('background-color', '#2a2a2c', 'important');
                document.body.style.setProperty('color', '#e5e5e5', 'important');
                document.documentElement.style.colorScheme = 'dark';
            }}
        }}, {{once:true}});
        addEventListener('keydown', event => {{
            if (event.key === 'Escape' || ((event.metaKey || event.ctrlKey) && event.key === 'w')) {{
                event.preventDefault(); window.ipc.postMessage('close');
            }}
        }});
        // No external navigation or automatic browser launch from email content.
        addEventListener('click', event => {{
            const link = event.target.closest('a');
            if (link && !link.getAttribute('href')?.startsWith('#')) {{
                event.preventDefault();
                window.ipc.postMessage('link:' + (link.getAttribute('href') || ''));
            }}
        }}, true);"#
    );
    let mut builder = WebViewBuilder::new()
        .with_html(document.html)
        .with_initialization_script(&host)
        .with_custom_protocol("durianmail".into(), move |_, request| {
            match document.images.get(&request.uri().to_string()) {
                Some(png) => Response::builder()
                    .header("Content-Type", "image/png")
                    .header("Cache-Control", "no-store")
                    .body(Cow::Owned(png.clone()))
                    .unwrap(),
                None => Response::builder()
                    .status(404)
                    .body(Cow::Borrowed(&[][..]))
                    .unwrap(),
            }
        })
        .with_navigation_handler(|url| url == "about:blank" || url.starts_with("about:blank#"))
        .with_new_window_req_handler(|_, _| NewWindowResponse::Deny)
        .with_download_started_handler(|_, _| false)
        .with_permission_handler(|_| PermissionResponse::Deny)
        .with_ipc_handler(move |request| {
            let _ = proxy.send_event(ViewerEvent::Ipc(request.body().clone()));
        })
        .with_incognito(true)
        .with_devtools(false);
    if verify {
        builder = builder.with_initialization_script(include_str!("verify.js"));
        let timeout = event_loop.create_proxy();
        std::thread::spawn(move || {
            std::thread::sleep(Duration::from_secs(20));
            let _ = timeout.send_event(ViewerEvent::Timeout);
        });
    }
    #[cfg(target_os = "macos")]
    let webview = {
        use wry::WebViewBuilderExtDarwin;
        builder.with_allow_link_preview(false).build(&window)?
    };
    #[cfg(target_os = "linux")]
    let webview = {
        use tao::platform::unix::WindowExtUnix;
        use wry::WebViewBuilderExtUnix;
        builder.build_gtk(window.default_vbox().ok_or("No GTK container")?)?
    };
    event_loop.run(move |event, _, control_flow| {
        let _keep_alive = (&window, &webview);
        *control_flow = ControlFlow::Wait;
        match event {
            Event::WindowEvent {
                event: WindowEvent::CloseRequested,
                ..
            } => {
                *control_flow = ControlFlow::Exit;
            }
            Event::UserEvent(ViewerEvent::Ipc(ref message)) if message == "close" => {
                *control_flow = ControlFlow::Exit;
            }
            Event::UserEvent(ViewerEvent::Ipc(message)) => {
                if let Some(url) = message.strip_prefix("link:") {
                    if document::external_link(url) {
                        // Leave confirmation/opening to GPUI, after this window
                        // closes and focus returns to the native mail reader.
                        println!("{}", serde_json::json!({"external_link":url}));
                        *control_flow = ControlFlow::Exit;
                    }
                } else if verify && message.starts_with("verify:") {
                    let result: serde_json::Value = serde_json::from_str(&message[7..]).unwrap();
                    let valid = result["dark"] == dark
                        && result["narrow"] == narrow
                        && result["checks"].as_object().is_some_and(|checks| {
                            !checks.is_empty() && checks.values().all(|v| v == true)
                        });
                    println!("{} {result}", if valid { "PASS" } else { "FAIL" });
                    *control_flow = ControlFlow::ExitWithCode(if valid { 0 } else { 1 });
                }
            }
            Event::UserEvent(ViewerEvent::Timeout) => {
                eprintln!("FAIL: WebKit verification timed out");
                *control_flow = ControlFlow::ExitWithCode(1);
            }
            _ => {}
        }
    });
}
