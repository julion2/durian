//! Test driver for the public document boundary; the native helper receives its output.
use std::collections::HashMap;
use std::io::{self, BufRead};

#[derive(serde::Deserialize)]
struct Input {
    html: String,
    #[serde(default)]
    images: HashMap<String, String>,
    #[serde(default)]
    remote_images: bool,
}

fn main() {
    for line in io::stdin().lock().lines() {
        let input: Input = serde_json::from_str(&line.unwrap()).unwrap();
        let html =
            mail::html_doc::prepare(&input.html, &input.images, mail::html_doc::Policy { remote_images: input.remote_images }).unwrap();
        println!("{}", serde_json::to_string(&html).unwrap());
    }
}
