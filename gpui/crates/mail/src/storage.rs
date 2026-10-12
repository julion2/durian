//! Private, process-scoped files for decrypted mail. Persistent caches contain only settings.

use std::fs::{self, File, OpenOptions};
use std::io;
use std::os::fd::AsRawFd;
use std::os::unix::fs::{DirBuilderExt, MetadataExt, OpenOptionsExt};
use std::path::{Component, Path, PathBuf};
use std::sync::OnceLock;

static SESSION: OnceLock<Session> = OnceLock::new();

pub fn default_cache_root() -> PathBuf {
    let home = std::env::var_os("HOME").map(PathBuf::from).unwrap_or_else(std::env::temp_dir);
    #[cfg(target_os = "macos")]
    let base = home.join("Library/Caches");
    #[cfg(not(target_os = "macos"))]
    let base = std::env::var_os("XDG_CACHE_HOME").map(PathBuf::from).filter(|p| p.is_absolute()).unwrap_or_else(|| home.join(".cache"));
    base.join("durian-gpui")
}

/// Fail closed before opening a live mailbox if its temporary directory is not private.
pub fn init_session() -> io::Result<()> {
    if SESSION.get().is_none() {
        let parent = std::env::temp_dir().join(format!("durian-gpui-{}", unsafe { libc::geteuid() }));
        let session = Session::new(&parent)?;
        // If two callers initialized concurrently, Drop cleans up the unused session.
        let _ = SESSION.set(session);
    }
    Ok(())
}

pub fn session_dir(sub: &str) -> PathBuf {
    init_session().expect("cannot create private mail session");
    SESSION.get().expect("session initialized").directory(sub).expect("cannot create private session directory")
}

pub fn clear_session() {
    // Quitting without having opened live mail must not create a session just to remove it.
    if let Some(session) = SESSION.get() {
        let _ = fs::remove_dir_all(&session.path);
    }
}

struct Session {
    path: PathBuf,
    _lock: File,
}

fn private_directory(path: &Path) -> io::Result<()> {
    match fs::DirBuilder::new().mode(0o700).create(path) {
        Ok(()) => {}
        Err(e) if e.kind() == io::ErrorKind::AlreadyExists => {}
        Err(e) => return Err(e),
    }
    let meta = fs::symlink_metadata(path)?;
    if !meta.is_dir() || meta.uid() != unsafe { libc::geteuid() } || meta.mode() & 0o077 != 0 {
        return Err(io::Error::new(io::ErrorKind::PermissionDenied, "mail session directory is not private"));
    }
    Ok(())
}

fn lock(file: &File) -> io::Result<()> {
    if unsafe { libc::flock(file.as_raw_fd(), libc::LOCK_EX | libc::LOCK_NB) } == 0 {
        Ok(())
    } else {
        Err(io::Error::last_os_error())
    }
}

impl Session {
    fn new(parent: &Path) -> io::Result<Self> {
        private_directory(parent)?;
        // Serialize discovery and creation: another startup must not collect a session
        // between creation of its lock file and acquisition of its lifetime lock.
        let initialization = OpenOptions::new().read(true).write(true).create(true).truncate(false)
            .mode(0o600).custom_flags(libc::O_NOFOLLOW).open(parent.join(".initialization"))?;
        if unsafe { libc::flock(initialization.as_raw_fd(), libc::LOCK_EX) } != 0 {
            return Err(io::Error::last_os_error());
        }
        // Only our UUID-named, unlocked directories are ours to clean. No PID reuse races,
        // no following symlinks, and a second app instance keeps its decrypted files.
        for entry in fs::read_dir(parent)?.flatten() {
            let name = entry.file_name();
            let Some(id) = name.to_str().and_then(|n| n.strip_prefix("session-")) else { continue };
            if uuid::Uuid::parse_str(id).is_err() || private_directory(&entry.path()).is_err() {
                continue;
            }
            let Ok(file) = OpenOptions::new().read(true).custom_flags(libc::O_NOFOLLOW).open(entry.path().join(".lock")) else { continue };
            if lock(&file).is_ok() {
                let _ = fs::remove_dir_all(entry.path());
            }
        }
        let path = parent.join(format!("session-{}", uuid::Uuid::new_v4()));
        fs::DirBuilder::new().mode(0o700).create(&path)?;
        let result = (|| {
            let file = OpenOptions::new().write(true).create_new(true).mode(0o600).open(path.join(".lock"))?;
            lock(&file)?;
            Ok(Self { path: path.clone(), _lock: file })
        })();
        if result.is_err() {
            let _ = fs::remove_dir_all(path);
        }
        result
    }

    fn directory(&self, sub: &str) -> io::Result<PathBuf> {
        let mut path = self.path.clone();
        for component in Path::new(sub).components() {
            let Component::Normal(name) = component else {
                return Err(io::Error::new(io::ErrorKind::InvalidInput, "invalid session subdirectory"));
            };
            path.push(name);
            private_directory(&path)?;
        }
        Ok(path)
    }
}

impl Drop for Session {
    fn drop(&mut self) {
        let _ = fs::remove_dir_all(&self.path);
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::os::unix::fs::{PermissionsExt, symlink};

    #[test]
    fn sessions_are_private_independent_and_clean_up_only_abandoned_files() {
        let parent = std::env::temp_dir().join(format!("durian-storage-test-{}", uuid::Uuid::new_v4()));
        let first = Session::new(&parent).unwrap();
        let attachment = first.directory("attachments/part-1").unwrap().join("message.pdf");
        fs::write(&attachment, b"private").unwrap();
        let stale = parent.join(format!("session-{}", uuid::Uuid::new_v4()));
        private_directory(&stale).unwrap();
        fs::write(stale.join(".lock"), b"").unwrap();
        let unrelated = parent.join("leave-me");
        fs::create_dir(&unrelated).unwrap();
        let second = Session::new(&parent).unwrap();
        assert!(attachment.exists(), "a running instance retains its files");
        assert!(!stale.exists());
        assert!(unrelated.exists());
        assert_ne!(first.path, second.path);
        assert_eq!(fs::metadata(attachment.parent().unwrap()).unwrap().permissions().mode() & 0o777, 0o700);
        assert!(first.directory("../outside").is_err());
        assert!(first.directory("/outside").is_err());
        symlink(&unrelated, first.path.join("redirect")).unwrap();
        assert!(first.directory("redirect/mail").is_err());
        let first_path = first.path.clone();
        drop(first);
        assert!(!first_path.exists());
        assert!(second.path.exists());
        drop(second);
        fs::remove_dir_all(parent).unwrap();
    }

    #[test]
    fn refuses_public_or_symlink_session_roots() {
        let root = std::env::temp_dir().join(format!("durian-storage-test-{}", uuid::Uuid::new_v4()));
        fs::create_dir(&root).unwrap();
        let public = root.join("public");
        fs::create_dir(&public).unwrap();
        fs::set_permissions(&public, fs::Permissions::from_mode(0o755)).unwrap();
        assert!(Session::new(&public).is_err());
        let alias = root.join("alias");
        symlink(&public, &alias).unwrap();
        assert!(Session::new(&alias).is_err());
        fs::remove_dir_all(root).unwrap();
    }
}
