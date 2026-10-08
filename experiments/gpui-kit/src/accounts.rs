//! Same config.pkl source as the Swift/Qt clients, projected inside Pkl so
//! neither transport configuration nor credentials enter the GPUI process.
use serde::Deserialize;
use std::ffi::OsStr;
use std::io::Read;
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};
use std::time::{Duration, Instant};

const PROJECTION: &str = r#"(new JsonRenderer {}).renderValue(accounts.toList().map((account) -> new Dynamic {
  name = account.name
  email = account.email
}))"#;
const MAX_OUTPUT: u64 = 64 * 1024;

#[derive(Clone, Debug, PartialEq, Eq, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Account {
    pub name: String,
    pub email: String,
}

fn config_path(xdg: Option<&OsStr>, home: Option<&OsStr>) -> Result<PathBuf, &'static str> {
    let base = if let Some(xdg) = xdg.filter(|value| !value.is_empty()) {
        PathBuf::from(xdg)
    } else {
        PathBuf::from(
            home.filter(|value| !value.is_empty())
                .ok_or("Home directory is unavailable.")?,
        )
        .join(".config")
    };
    Ok(base.join("durian/config.pkl"))
}

/// Blocking; call on the background executor and only in live mode. Preserve
/// config order, matching the Swift composer, rather than inventing identities.
pub fn load() -> Result<Vec<Account>, &'static str> {
    let config = config_path(
        std::env::var_os("XDG_CONFIG_HOME").as_deref(),
        std::env::var_os("HOME").as_deref(),
    )?;
    let executable = std::env::var_os("PKL_EXEC")
        .filter(|value| !value.is_empty())
        .map(PathBuf::from)
        .or_else(|| {
            std::env::var_os("PATH").and_then(|path| {
                std::env::split_paths(&path)
                    .map(|dir| dir.join("pkl"))
                    .find(|candidate| candidate.is_file())
            })
        })
        .or_else(|| {
            ["/opt/homebrew/bin/pkl", "/usr/local/bin/pkl"]
                .into_iter()
                .map(PathBuf::from)
                .find(|candidate| candidate.is_file())
        })
        .ok_or("Install Pkl or set PKL_EXEC to load configured senders.")?;
    load_from(&config, &executable)
}

fn load_from(config: &Path, executable: &Path) -> Result<Vec<Account>, &'static str> {
    let config = config
        .canonicalize()
        .map_err(|_| "Couldn’t find config.pkl. Configure a mail account first.")?;
    let schema = tempfile::tempdir().map_err(|_| "Couldn’t prepare the account schema.")?;
    std::fs::write(
        schema.path().join("Config.pkl"),
        include_str!("../../../schema/Config.pkl"),
    )
    .map_err(|_| "Couldn’t prepare the account schema.")?;
    // A private, bounded output file avoids blocking on a full stdout pipe.
    // Never capture stderr: Pkl diagnostics may quote secret-bearing source.
    let output = tempfile::tempfile().map_err(|_| "Couldn’t prepare the account reader.")?;
    let mut child = Command::new(executable)
        .args([
            "eval",
            "--settings",
            "pkl:settings",
            "--allowed-modules",
            "pkl:,file:,modulepath:,repl:",
            "--timeout",
            "9",
            "--module-path",
        ])
        .arg(schema.path())
        .args(["--expression", PROJECTION])
        .arg(config)
        .stdin(Stdio::null())
        .stdout(
            output
                .try_clone()
                .map_err(|_| "Couldn’t prepare the account reader.")?,
        )
        .stderr(Stdio::null())
        .spawn()
        .map_err(|_| "Couldn’t start Pkl. Check PKL_EXEC and the Pkl installation.")?;
    let start = Instant::now();
    let result = loop {
        if output
            .metadata()
            .map(|meta| meta.len() > MAX_OUTPUT)
            .unwrap_or(true)
        {
            break Err("Account list exceeds the 64 KiB limit.");
        }
        if start.elapsed() >= Duration::from_secs(10) {
            break Err("Reading configured senders timed out.");
        }
        match child.try_wait() {
            Ok(Some(status)) if status.success() => break Ok(()),
            Ok(Some(_)) => break Err("Couldn’t evaluate configured senders. Check config.pkl."),
            Ok(None) => std::thread::sleep(Duration::from_millis(20)),
            Err(_) => break Err("Couldn’t read configured senders."),
        }
    };
    if result.is_err() {
        let _ = child.kill();
        let _ = child.wait();
    }
    result?;
    use std::io::{Seek, SeekFrom};
    let mut output = output;
    output
        .seek(SeekFrom::Start(0))
        .map_err(|_| "Couldn’t read configured senders.")?;
    let mut bytes = Vec::new();
    output
        .take(MAX_OUTPUT + 1)
        .read_to_end(&mut bytes)
        .map_err(|_| "Couldn’t read configured senders.")?;
    if bytes.len() as u64 > MAX_OUTPUT {
        return Err("Account list exceeds the 64 KiB limit.");
    }
    serde_json::from_slice(&bytes).map_err(|_| "Pkl returned an invalid sender list.")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn config_uses_the_same_xdg_and_home_paths_as_existing_clients() {
        assert_eq!(
            config_path(
                Some(OsStr::new("/tmp/settings")),
                Some(OsStr::new("/home/demo"))
            )
            .unwrap(),
            Path::new("/tmp/settings/durian/config.pkl")
        );
        assert_eq!(
            config_path(Some(OsStr::new("")), Some(OsStr::new("/home/demo"))).unwrap(),
            Path::new("/home/demo/.config/durian/config.pkl")
        );
        assert!(config_path(None, None).is_err());
    }

    #[test]
    #[ignore = "requires Pkl; set PKL_EXEC and run with --include-ignored"]
    fn pkl_projects_only_identity_fields_without_evaluating_secrets() {
        let dir = tempfile::tempdir().unwrap();
        let config = dir.path().join("config.pkl");
        std::fs::write(
            &config,
            r#"
import "modulepath:/Config.pkl" as C
accounts: Listing<C.AccountConfig> = new {
  (C.gmail) {
    name = "R&D – private"
    email = "sender@example.test"
    alias = "private"
    oauth { client_secret = throw("Secret must not be evaluated") }
  }
  (C.microsoft365) {
    name = "Work"
    email = "work@example.test"
    alias = "work"
    default = true
  }
}
output { text = throw("Full config must not be rendered") }
"#,
        )
        .unwrap();
        let executable = std::env::var_os("PKL_EXEC").unwrap_or_else(|| "pkl".into());
        let accounts = load_from(&config, Path::new(&executable)).unwrap();
        assert_eq!(
            accounts,
            vec![
                Account {
                    name: "R&D – private".into(),
                    email: "sender@example.test".into()
                },
                Account {
                    name: "Work".into(),
                    email: "work@example.test".into()
                },
            ]
        );
        std::fs::write(&config, "accounts = new Listing {}\n").unwrap();
        assert!(
            load_from(&config, Path::new(&executable))
                .unwrap()
                .is_empty()
        );
        std::fs::write(&config, "accounts = throw(\"PRIVATE_CANARY\")\n").unwrap();
        let error = load_from(&config, Path::new(&executable)).unwrap_err();
        assert!(!error.contains("PRIVATE_CANARY"));
    }
}
