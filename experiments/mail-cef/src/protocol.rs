//! Private parent/helper protocol. Pixels are length-checked BGRA, never HTML
//! interpreted by GPUI. Only the helper process can write this pipe.
use serde::{Deserialize, Serialize};
use std::collections::HashMap;

pub const MAX_FRAME_BYTES: usize = 64 * 1024 * 1024;

#[derive(Debug, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum Command {
    Open {
        html: String,
        dark: bool,
        images: HashMap<String, String>,
    },
    Viewport {
        width: u32,
        height: u32,
        scale: f32,
        y: f64,
    },
    Mouse {
        x: i32,
        y: i32,
        button: bool,
        down: Option<bool>,
        count: i32,
    },
    Key {
        code: i32,
        shift: bool,
    },
    SelectAll,
    Close,
}

#[derive(Debug, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case")]
pub enum Event {
    /// This header is immediately followed by width*height*4 raw BGRA bytes.
    Frame {
        width: u32,
        height: u32,
        scale: f32,
        y: f64,
    },
    Height {
        height: f64,
    },
    Link {
        url: String,
    },
    Selection {
        text: String,
    },
    Error {
        message: String,
    },
}

pub fn frame_len(width: u32, height: u32) -> Option<usize> {
    let bytes = (width as usize)
        .checked_mul(height as usize)?
        .checked_mul(4)?;
    (width > 0 && height > 0 && width <= 8192 && height <= 8192 && bytes <= MAX_FRAME_BYTES)
        .then_some(bytes)
}

#[cfg(test)]
mod tests {
    use super::*;
    #[test]
    fn pipe_frame_limits_reject_overflow_and_oversize_before_allocating() {
        assert_eq!(frame_len(123, 47), Some(23124));
        assert_eq!(frame_len(4096, 4096), Some(MAX_FRAME_BYTES));
        assert_eq!(frame_len(4096, 4097), None);
        assert_eq!(frame_len(u32::MAX, u32::MAX), None);
        assert_eq!(frame_len(0, 100), None);
        assert_eq!(frame_len(8193, 1), None);
    }
}
