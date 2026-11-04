//! Clone reuse must not mistake partial Git metadata for a completed checkout.
use amux::workspace_launch::clone_repository;
use std::{path::Path, process::Command};

fn fixture() -> tempfile::TempDir {
    let root = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
    std::fs::create_dir_all(&root).unwrap();
    tempfile::tempdir_in(root).unwrap()
}

#[tokio::test]
async fn unfinished_legacy_checkout_is_rejected_without_changing_its_files() {
    let fixture = fixture();
    let checkout = fixture.path().join("project");
    assert!(
        Command::new("git")
            .arg("init")
            .arg(&checkout)
            .status()
            .unwrap()
            .success()
    );
    assert!(
        Command::new("git")
            .arg("-C")
            .arg(&checkout)
            .args([
                "remote",
                "add",
                "origin",
                "https://example.org/team/project.git"
            ])
            .status()
            .unwrap()
            .success()
    );
    std::fs::write(checkout.join("keep"), "partial data").unwrap();
    let error = clone_repository(
        fixture.path().into(),
        "https://example.org/team/project.git".into(),
    )
    .await
    .unwrap_err()
    .to_string();
    assert!(error.contains("incomplete"), "{error}");
    assert_eq!(
        std::fs::read_to_string(checkout.join("keep")).unwrap(),
        "partial data"
    );
}

#[tokio::test]
async fn normal_clone_failure_remains_an_error_on_retry_and_preserves_partial_folder() {
    let fixture = fixture();
    let parent = fixture.path().join("clones");
    std::fs::create_dir(&parent).unwrap();
    let missing = fixture.path().join("missing.git").display().to_string();
    assert!(
        clone_repository(parent.clone(), missing.clone())
            .await
            .is_err()
    );
    let destination = parent.join("missing");
    assert!(destination.is_dir());
    assert!(!destination.join(".git").exists());
    std::fs::write(destination.join("keep"), "preserved").unwrap();
    assert!(clone_repository(parent, missing).await.is_err());
    assert_eq!(
        std::fs::read_to_string(destination.join("keep")).unwrap(),
        "preserved"
    );
}

#[tokio::test]
async fn deleting_a_failed_clone_folder_allows_a_fresh_retry() {
    let fixture = fixture();
    let parent = fixture.path().join("clones");
    std::fs::create_dir(&parent).unwrap();
    let source = fixture.path().join("missing.git");
    let repository = source.display().to_string();
    assert!(
        clone_repository(parent.clone(), repository.clone())
            .await
            .is_err()
    );
    std::fs::remove_dir_all(parent.join("missing")).unwrap();
    assert!(
        Command::new("git")
            .args(["init", "--bare"])
            .arg(&source)
            .status()
            .unwrap()
            .success()
    );
    let checkout = clone_repository(parent.clone(), repository).await.unwrap();
    assert!(checkout.ends_with("missing") && checkout.join(".git").is_dir());
}

#[cfg(target_os = "linux")]
mod live_clone {
    use super::*;
    use std::process::{Child, Stdio};

    #[test]
    #[ignore = "internal clone subprocess, run only by the contract test"]
    fn clone_fixture() {
        let Ok(parent) = std::env::var("AMUX_CLONE_PARENT") else {
            return;
        };
        tokio::runtime::Runtime::new().unwrap().block_on(async {
            clone_repository(parent.into(), std::env::var("AMUX_CLONE_SOURCE").unwrap())
                .await
                .unwrap();
        });
    }

    struct RunningClone(Child);
    impl RunningClone {
        fn kill(&mut self) {
            if self.0.try_wait().unwrap().is_none() {
                assert!(
                    Command::new("kill")
                        .args(["-KILL", "--", &format!("-{}", self.0.id())])
                        .status()
                        .unwrap()
                        .success()
                );
                self.0.wait().unwrap();
            }
        }
    }
    impl Drop for RunningClone {
        fn drop(&mut self) {
            self.kill();
        }
    }

    #[tokio::test]
    async fn concurrent_and_killed_clones_are_rejected_even_after_head_is_written() {
        use std::os::unix::process::CommandExt;
        for complete in [false, true] {
            let fixture = fixture();
            let source = fixture.path().join("source.git");
            let parent = fixture.path().join("clones");
            std::fs::create_dir(&parent).unwrap();
            assert!(
                Command::new("git")
                    .arg("init")
                    .arg(&source)
                    .status()
                    .unwrap()
                    .success()
            );
            std::fs::write(
                source.join(".gitattributes"),
                "payload filter=amux-clone-gate\n",
            )
            .unwrap();
            std::fs::write(source.join("payload"), "committed payload\n").unwrap();
            for args in [
                vec!["add", "."],
                vec![
                    "-c",
                    "user.name=Fixture",
                    "-c",
                    "user.email=fixture@example.invalid",
                    "commit",
                    "-m",
                    "fixture",
                ],
            ] {
                assert!(
                    Command::new("git")
                        .arg("-C")
                        .arg(&source)
                        .args(args)
                        .status()
                        .unwrap()
                        .success()
                );
            }
            let ready = fixture.path().join("ready");
            let release = fixture.path().join("release");
            let mut command = Command::new(std::env::current_exe().unwrap());
            command.args(["--ignored", "--exact", "live_clone::clone_fixture"])
                .env("AMUX_CLONE_PARENT", &parent).env("AMUX_CLONE_SOURCE", &source)
                .env("AMUX_CLONE_READY", &ready).env("AMUX_CLONE_RELEASE", &release)
                .env("GIT_CONFIG_COUNT", "2")
                .env("GIT_CONFIG_KEY_0", "filter.amux-clone-gate.smudge")
                .env("GIT_CONFIG_VALUE_0", "touch \"$AMUX_CLONE_READY\"; while [ ! -f \"$AMUX_CLONE_RELEASE\" ]; do sleep 0.01; done; cat")
                .env("GIT_CONFIG_KEY_1", "filter.amux-clone-gate.required")
                .env("GIT_CONFIG_VALUE_1", "true")
                .stdout(Stdio::null()).process_group(0);
            let mut running = RunningClone(command.spawn().unwrap());
            tokio::time::timeout(std::time::Duration::from_secs(15), async {
                while !ready.exists() {
                    assert!(
                        running.0.try_wait().unwrap().is_none(),
                        "clone exited before checkout gate"
                    );
                    tokio::time::sleep(std::time::Duration::from_millis(10)).await;
                }
            })
            .await
            .expect("clone did not reach checkout gate");
            let destination = parent.join("source");
            assert!(
                Command::new("git")
                    .arg("-C")
                    .arg(&destination)
                    .args(["rev-parse", "--verify", "HEAD^{commit}"])
                    .stdout(Stdio::null())
                    .status()
                    .unwrap()
                    .success()
            );
            let repository = source.display().to_string();
            assert!(
                clone_repository(parent.clone(), repository.clone())
                    .await
                    .unwrap_err()
                    .to_string()
                    .contains("incomplete")
            );
            if complete {
                std::fs::write(release, "continue").unwrap();
                assert!(running.0.wait().unwrap().success());
                std::fs::write(destination.join("keep"), "local edit").unwrap();
                assert_eq!(
                    clone_repository(parent, repository).await.unwrap(),
                    destination
                );
                assert_eq!(
                    std::fs::read_to_string(destination.join("payload")).unwrap(),
                    "committed payload\n"
                );
                assert_eq!(
                    std::fs::read_to_string(destination.join("keep")).unwrap(),
                    "local edit"
                );
            } else {
                running.kill();
                assert!(
                    clone_repository(parent, repository)
                        .await
                        .unwrap_err()
                        .to_string()
                        .contains("incomplete")
                );
                assert!(destination.join(".git").is_dir());
                assert!(!destination.join(".git/amux-clone-complete").exists());
            }
        }
    }
}
