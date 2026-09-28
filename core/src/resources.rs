//! Locale-aware media lookup:
//! <root>/<locale>/<res> first, then <root>/<res>; "*" or "*.ext" as last
//! component picks a random file; "a;b" tries alternatives.

use crate::protocol::valid_resource;
use std::path::{Path, PathBuf};
use std::sync::Mutex;

#[derive(Clone, Copy, PartialEq, Debug)]
pub enum Kind {
    Sound,
    Choreography,
}

pub struct Resources {
    sounds: Vec<PathBuf>,
    chors: Vec<PathBuf>,
    locale: Mutex<String>,
}

impl Resources {
    pub fn new(sounds: Vec<PathBuf>, chors: Vec<PathBuf>) -> Resources {
        Resources {
            sounds,
            chors,
            locale: Mutex::new("fr_FR".into()),
        }
    }

    pub fn set_locale(&self, locale: &str) {
        *self.locale.lock().unwrap() = locale.to_string();
    }

    fn roots(&self, kind: Kind) -> &[PathBuf] {
        match kind {
            Kind::Sound => &self.sounds,
            Kind::Choreography => &self.chors,
        }
    }

    fn allowed(kind: Kind, p: &Path) -> bool {
        let ext = p
            .extension()
            .and_then(|e| e.to_str())
            .unwrap_or("")
            .to_ascii_lowercase();
        match kind {
            Kind::Sound => ext == "mp3" || ext == "wav",
            Kind::Choreography => ext == "chor",
        }
    }

    /// Canonical path if it is a regular file that stays inside root (no symlink escape).
    fn contained(root: &Path, p: &Path) -> Option<PathBuf> {
        let c = p.canonicalize().ok()?;
        let r = root.canonicalize().ok()?;
        (c.starts_with(&r) && c.is_file()).then_some(c)
    }

    pub fn find(&self, kind: Kind, spec: &str) -> Option<PathBuf> {
        if !valid_resource(spec) {
            return None;
        }
        let locale = self.locale.lock().unwrap().clone();
        for part in spec.split(';') {
            let rel = Path::new(part);
            let name = rel.file_name()?.to_str()?;
            let found = if let Some(suffix) = name.strip_prefix('*') {
                let parent = rel.parent().unwrap_or(Path::new(""));
                let mut files = Vec::new();
                for root in self.roots(kind) {
                    for dir in [root.join(&locale).join(parent), root.join(parent)] {
                        let Ok(rd) = std::fs::read_dir(&dir) else {
                            continue;
                        };
                        for e in rd.flatten() {
                            let p = e.path();
                            let n = e.file_name().to_string_lossy().to_string();
                            if n.ends_with(suffix) && !n.starts_with('.') && Self::allowed(kind, &p)
                            {
                                if let Some(c) = Self::contained(root, &p) {
                                    files.push(c);
                                }
                            }
                        }
                    }
                }
                files.sort();
                files.dedup();
                (!files.is_empty()).then(|| files.swap_remove(fastrand::usize(..files.len())))
            } else {
                self.roots(kind).iter().find_map(|root| {
                    [root.join(&locale).join(rel), root.join(rel)]
                        .iter()
                        .filter(|p| Self::allowed(kind, p))
                        .find_map(|p| Self::contained(root, p))
                })
            };
            if found.is_some() {
                return found;
            }
        }
        None
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn lookup_rules() {
        let tmp = std::env::temp_dir().join(format!("nabres-{}", std::process::id()));
        let _ = std::fs::remove_dir_all(&tmp);
        let root = tmp.join("sounds");
        std::fs::create_dir_all(root.join("fr_FR/clock/7")).unwrap();
        std::fs::create_dir_all(root.join("system")).unwrap();
        std::fs::write(root.join("fr_FR/clock/7/a.mp3"), b"x").unwrap();
        std::fs::write(root.join("system/abort.wav"), b"x").unwrap();
        std::fs::write(root.join("system/notes.txt"), b"x").unwrap();
        std::fs::write(tmp.join("secret.mp3"), b"x").unwrap();
        std::os::unix::fs::symlink(tmp.join("secret.mp3"), root.join("system/link.mp3")).unwrap();
        let r = Resources::new(vec![root.clone()], vec![]);
        assert!(r
            .find(Kind::Sound, "clock/7/*.mp3")
            .unwrap()
            .ends_with("fr_FR/clock/7/a.mp3"));
        assert!(r
            .find(Kind::Sound, "missing.mp3;system/abort.wav")
            .unwrap()
            .ends_with("system/abort.wav"));
        assert!(r.find(Kind::Sound, "system/notes.txt").is_none());
        assert!(
            r.find(Kind::Sound, "system/link.mp3").is_none(),
            "symlink escape"
        );
        assert!(r.find(Kind::Sound, "../secret.mp3").is_none());
        r.set_locale("en_US");
        assert!(r.find(Kind::Sound, "clock/7/*.mp3").is_none());
        let _ = std::fs::remove_dir_all(&tmp);
    }
}
