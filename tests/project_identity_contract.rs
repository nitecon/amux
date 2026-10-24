use amux::project_identity::{from_parts, resolve};
use std::{path::Path, process::Command};
fn fixture() -> tempfile::TempDir {
    let path = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
    std::fs::create_dir_all(&path).unwrap();
    tempfile::tempdir_in(path).unwrap()
}
fn git(directory: &Path, args: &[&str]) {
    let status = Command::new("git")
        .arg("-C")
        .arg(directory)
        .args(args)
        .status()
        .unwrap();
    assert!(status.success());
}
#[test]
fn upstream_and_checkout_directory_define_identity() {
    let first = fixture();
    let second = fixture();
    let ssh = from_parts(first.path(), Some("git@github.com:Nitecon/AMUX.git")).unwrap();
    let https = from_parts(first.path(), Some("https://github.com/nitecon/amux")).unwrap();
    assert_eq!(ssh, https);
    assert_eq!(ssh.gateway_project.as_deref(), Some("amux"));
    assert_ne!(
        ssh.id,
        from_parts(second.path(), Some("https://github.com/nitecon/amux"))
            .unwrap()
            .id
    );
    assert_ne!(
        ssh.id,
        from_parts(first.path(), Some("https://github.com/nitecon/SRE.git"))
            .unwrap()
            .id
    );
    assert_eq!(
        from_parts(first.path(), Some("https://github.com/nitecon/SRE.git"))
            .unwrap()
            .gateway_project
            .as_deref(),
        Some("sre")
    );
    assert_eq!(ssh.session_id("codex-1"), https.session_id("codex-1"));
    assert_ne!(ssh.session_id("codex-1"), ssh.session_id("codex-2"));
    assert_ne!(ssh.session_id("codex-1"), ssh.session_id("claude-1"));
}
#[test]
fn git_subdirectory_resolves_to_same_checkout() {
    let dir = fixture();
    git(dir.path(), &["init", "--quiet"]);
    git(
        dir.path(),
        &["remote", "add", "origin", "git@github.com:nitecon/amux.git"],
    );
    let child = dir.path().join("subdir");
    std::fs::create_dir(&child).unwrap();
    let root = resolve(dir.path()).unwrap();
    assert_eq!(resolve(&child).unwrap(), root);
    assert_eq!(root.gateway_project.as_deref(), Some("amux"));
}
#[test]
fn directory_without_origin_has_stable_local_identity() {
    let dir = fixture();
    // A nested fixture repo prevents accidentally inheriting amux's own origin.
    git(dir.path(), &["init", "--quiet"]);
    let first = resolve(dir.path()).unwrap();
    assert_eq!(first, resolve(dir.path()).unwrap());
    assert!(first.upstream.is_none());
    assert!(first.gateway_project.is_none());
    assert!(!first.id.is_nil());
}
#[cfg(unix)]
#[test]
fn symlink_alias_does_not_create_a_second_project() {
    let dir = fixture();
    let links = fixture();
    let alias = links.path().join("alias");
    std::os::unix::fs::symlink(dir.path(), &alias).unwrap();
    assert_eq!(
        from_parts(dir.path(), Some("git@github.com:nitecon/amux.git")).unwrap(),
        from_parts(&alias, Some("https://github.com/nitecon/amux")).unwrap()
    );
}
