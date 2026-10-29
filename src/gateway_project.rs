//! CMUX-compatible project resolution: a workspace's Git origin selects exactly one
//! gateway project by its canonical remote. Identities are never guessed from names;
//! a Git checkout without any origin uses only an explicit persisted binding.
use anyhow::{Context, Result, bail};
use serde_json::Value;
use std::{
    collections::{BTreeMap, HashSet},
    io::Write,
    path::Path,
};

/// Normalize a Git remote to `host[:port]/path` as CMUX does (model.rs `repository`).
/// Default SSH/Git ports are dropped; custom ports are kept. Ambiguous Windows
/// drive-letter origins are local paths, never repository hosts to register.
pub fn repository(value: &str) -> Option<String> {
    amux_gateway_backend::projects::canonical_remote(value)
}

/// Read the repository-local `origin` URL of a workspace (any subdirectory or worktree)
/// with a bounded Git call that ignores inherited Git overrides, prompts and hooks.
/// Errors never echo the remote, which may embed credentials.
pub fn origin(directory: &Path) -> Result<String> {
    configured_origin(directory)?.context("workspace is not a Git repository with an origin remote")
}

/// Like [`origin`], but `None` only when a Git checkout has no `origin` configured
/// at all. Unsupported origins, non-repositories and Git failures stay errors.
pub fn configured_origin(directory: &Path) -> Result<Option<String>> {
    let (status, text) = crate::git::observe(
        directory,
        &["config", "--local", "--get", "remote.origin.url"],
        "workspace origin",
    )?;
    // `git config --get` exits 1 for a missing key; outside a repository it exits 128.
    if status.code() == Some(1) {
        return Ok(None);
    }
    if !status.success() {
        bail!("workspace is not a Git repository with an origin remote");
    }
    // repository applies its 4096-byte limit to the value, excluding Git's LF.
    repository(text.trim_end_matches(['\r', '\n']))
        .context("workspace origin is not a supported Git remote URL")
        .map(Some)
}

/// The raw `origin` URL another machine can clone from, without any HTTP(S)
/// credentials. `None` when the checkout has no usable origin.
pub fn clone_origin(directory: &Path) -> Option<String> {
    let (status, text) = crate::git::observe(
        directory,
        &["config", "--local", "--get", "remote.origin.url"],
        "workspace origin",
    )
    .ok()?;
    let url = text.trim();
    if !status.success() || url.is_empty() {
        return None;
    }
    Some(strip_credentials(url))
}

fn strip_credentials(url: &str) -> String {
    for scheme in ["https://", "http://"] {
        if let Some(rest) = url.strip_prefix(scheme) {
            let authority = rest.split('/').next().unwrap_or_default();
            if let Some((_, host)) = authority.rsplit_once('@') {
                return format!("{scheme}{host}{}", &rest[authority.len()..]);
            }
        }
    }
    url.to_owned()
}

/// True when `ident` is listed as an active ad-hoc project without a repository,
/// the only kind of project a local-only checkout may be bound to.
pub fn local_only(ident: &str, projects: &Value) -> Result<bool> {
    let entries = projects
        .as_array()
        .filter(|entries| entries.len() <= 4096)
        .context("gateway returned an invalid project list")?;
    let mut found = entries.iter().filter(|entry| entry["ident"] == ident);
    let Some(entry) = found.next() else {
        return Ok(false);
    };
    if found.next().is_some() {
        bail!("gateway project list contains a duplicate project identity");
    }
    Ok(entry["kind"] == "adhoc"
        && entry["canonical_remote"].is_null()
        && entry["archived_at"].is_null())
}

/// The gateway project explicitly bound to a local-only checkout root, if any.
/// Bindings are keyed by the exact canonical root, never by folder name.
pub fn bound_project_at(state: &Path, root: &Path) -> Result<Option<String>> {
    Ok(bindings_at(state)?.remove(&root.to_string_lossy().into_owned()))
}

pub fn bind_project_at(state: &Path, root: &Path, ident: &str) -> Result<()> {
    crate::gateway::validate_project(ident)?;
    let mut bindings = bindings_at(state)?;
    bindings.insert(root.to_string_lossy().into_owned(), ident.to_string());
    std::fs::create_dir_all(state)?;
    let mut file = tempfile::NamedTempFile::new_in(state)?;
    file.write_all(&serde_json::to_vec_pretty(&bindings)?)?;
    file.as_file().sync_all()?;
    file.persist(state.join(BINDINGS))
        .map_err(|error| error.error)?;
    Ok(())
}

const BINDINGS: &str = "project-bindings.json";

fn bindings_at(state: &Path) -> Result<BTreeMap<String, String>> {
    match std::fs::read(state.join(BINDINGS)) {
        Ok(bytes) => serde_json::from_slice(&bytes).context("invalid project bindings file"),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(BTreeMap::new()),
        Err(error) => Err(error.into()),
    }
}

/// Select the single project whose canonical remote equals `remote` from a
/// `/v1/projects` listing. Malformed listings and ambiguous mappings are errors.
pub fn matching(remote: &str, projects: &Value) -> Result<Option<String>> {
    let entries = projects
        .as_array()
        .filter(|entries| entries.len() <= 4096)
        .context("gateway returned an invalid project list")?;
    let mut idents = HashSet::new();
    let mut found = None;
    for entry in entries {
        let ident = entry["ident"]
            .as_str()
            .filter(|ident| {
                !ident.trim().is_empty()
                    && ident.len() <= 512
                    && !ident.chars().any(char::is_control)
            })
            .context("gateway project list contains an invalid project identity")?;
        if !idents.insert(ident) {
            bail!("gateway project list contains a duplicate project identity");
        }
        let canonical = match &entry["canonical_remote"] {
            Value::Null => continue,
            Value::String(text) => text,
            _ => bail!("gateway project list contains a malformed canonical remote"),
        };
        // Like CMUX, a remote that cannot be normalized maps nothing.
        if repository(canonical).as_deref() != Some(remote) {
            continue;
        }
        if found.replace(ident).is_some() {
            bail!(
                "several gateway projects map this repository; resolve the ambiguity on the gateway"
            );
        }
    }
    let Some(ident) = found else {
        return Ok(None);
    };
    crate::gateway::validate_project(ident)
        .context("gateway project mapped to this repository is not a usable identity")?;
    Ok(Some(ident.to_string()))
}

#[cfg(test)]
mod tests {
    #[test]
    fn clone_origins_drop_http_credentials_but_keep_ssh_users() {
        for (raw, shared) in [
            (
                "https://user:token@github.com/nitecon/amux.git",
                "https://github.com/nitecon/amux.git",
            ),
            (
                "http://token@git.example/team/repo",
                "http://git.example/team/repo",
            ),
            (
                "https://github.com/nitecon/amux",
                "https://github.com/nitecon/amux",
            ),
            (
                "git@github.com:nitecon/amux.git",
                "git@github.com:nitecon/amux.git",
            ),
            (
                "ssh://git@host:2222/team/repo.git",
                "ssh://git@host:2222/team/repo.git",
            ),
        ] {
            assert_eq!(super::strip_credentials(raw), shared);
        }
    }
}
