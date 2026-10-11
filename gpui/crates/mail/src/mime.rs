//! Outgoing mail as an RFC 5322 message with a multipart/alternative body:
//!   text/plain – the Markdown source (Markdown is readable plain text by design)
//!   text/html  – the same Markdown rendered with inline styles
//! Every client picks the richest part it can show; terminal clients get readable text.

use base64::Engine;

use ui::markdown;

pub struct Address {
    pub name: String,
    pub email: Option<String>,
}

impl Address {
    fn header(&self) -> String {
        match &self.email {
            Some(email) if self.name.is_empty() || self.name == *email => email.clone(),
            Some(email) => format!("{} <{email}>", encode_word(&self.name)),
            None => encode_word(&self.name),
        }
    }
}

pub struct Outgoing<'a> {
    pub from: Address,
    pub to: Vec<Address>,
    pub cc: Vec<Address>,
    /// Never written as a header – Bcc goes to the envelope only. Read by whoever delivers
    /// (durian's outbox takes it as `bcc[]`); the local-only prototype has no envelope yet.
    #[allow(dead_code)]
    pub bcc: Vec<Address>,
    pub subject: &'a str,
    pub in_reply_to: Option<&'a str>,
    pub markdown: &'a str,
}

/// RFC 2047 encoded-word for non-ASCII header text.
fn encode_word(text: &str) -> String {
    if text.is_ascii() && !text.contains(['"', ',', '<', '>']) {
        text.to_string()
    } else if text.is_ascii() {
        format!("\"{}\"", text.replace('"', "\\\""))
    } else {
        format!("=?UTF-8?B?{}?=", base64::engine::general_purpose::STANDARD.encode(text))
    }
}

/// Quoted-printable (RFC 2045): ASCII stays readable, lines stay under 76 chars.
fn quoted_printable(text: &str) -> String {
    let mut out = String::new();
    for (n, line) in text.replace("\r\n", "\n").split('\n').enumerate() {
        if n > 0 {
            out.push_str("\r\n");
        }
        let mut width = 0;
        let bytes = line.as_bytes();
        for (i, &b) in bytes.iter().enumerate() {
            let trailing_space = (b == b' ' || b == b'\t') && i == bytes.len() - 1;
            let literal = b == b'\t' || (32..=126).contains(&b);
            let piece = if b == b'=' || !literal || trailing_space {
                format!("={b:02X}")
            } else {
                (b as char).to_string()
            };
            if width + piece.len() > 75 {
                out.push_str("=\r\n"); // soft line break
                width = 0;
            }
            width += piece.len();
            out.push_str(&piece);
        }
    }
    out
}

pub fn build(mail: &Outgoing) -> String {
    let domain = mail.from.email.as_deref().and_then(|e| e.split('@').nth(1)).unwrap_or("localhost");
    let message_id = format!("<{}@{domain}>", uuid::Uuid::new_v4());
    let boundary = format!("=_alt_{}", uuid::Uuid::new_v4().simple());
    let list = |addrs: &[Address]| addrs.iter().map(Address::header).collect::<Vec<_>>().join(", ");

    let mut headers = vec![
        format!("From: {}", mail.from.header()),
        format!("To: {}", list(&mail.to)),
    ];
    if !mail.cc.is_empty() {
        headers.push(format!("Cc: {}", list(&mail.cc)));
    }
    headers.push(format!("Subject: {}", encode_word(mail.subject)));
    headers.push(format!("Date: {}", chrono::Local::now().to_rfc2822()));
    headers.push(format!("Message-ID: {message_id}"));
    if let Some(parent) = mail.in_reply_to {
        headers.push(format!("In-Reply-To: {parent}"));
        headers.push(format!("References: {parent}"));
    }
    headers.push("MIME-Version: 1.0".into());
    headers.push(format!("Content-Type: multipart/alternative; boundary=\"{boundary}\""));
    headers.push("X-Mailer: habric mail (GPUI prototype)".into());

    let part = |ctype: &str, body: &str| {
        format!(
            "--{boundary}\r\nContent-Type: {ctype}; charset=utf-8\r\nContent-Transfer-Encoding: quoted-printable\r\n\r\n{}\r\n",
            quoted_printable(body)
        )
    };
    format!(
        "{}\r\n\r\nThis is a multi-part message in MIME format.\r\n\r\n{}{}--{boundary}--\r\n",
        headers.join("\r\n"),
        part("text/plain", mail.markdown),
        part("text/html", &markdown::to_html(mail.markdown)),
    )
}

#[cfg(test)]
mod tests {
    use super::*;

    fn addr(name: &str, email: &str) -> Address {
        Address { name: name.into(), email: Some(email.into()) }
    }

    #[test]
    fn multipart_alternative_with_both_parts_and_no_bcc_header() {
        let raw = build(&Outgoing {
            from: addr("Julian Schenker", "julian@habric.com"),
            to: vec![addr("Mara Lindqvist", "mara@habric.com")],
            cc: vec![],
            bcc: vec![addr("Secret", "s@x.dev")],
            subject: "Grüße zum Release",
            in_reply_to: None,
            markdown: "Hi **Mara**,\n\n- shipped\n- tested",
        });
        assert!(raw.contains("Content-Type: multipart/alternative; boundary="));
        assert!(raw.contains("Content-Type: text/plain; charset=utf-8"));
        assert!(raw.contains("Content-Type: text/html; charset=utf-8"));
        assert!(raw.contains("Subject: =?UTF-8?B?"));
        assert!(raw.contains("Hi **Mara**,"), "plain part keeps the Markdown");
        assert!(raw.contains("<strong>Mara</strong>"));
        assert!(!raw.contains("s@x.dev"), "Bcc never appears in headers");
        assert!(raw.lines().all(|l| l.len() <= 998));
    }

    #[test]
    fn qp_soft_breaks_long_lines() {
        let qp = quoted_printable(&"x".repeat(200));
        assert!(qp.split("\r\n").all(|l| l.len() <= 76));
    }
}
