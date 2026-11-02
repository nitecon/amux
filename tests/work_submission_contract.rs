//! `amux submit-work` Git behavior against temporary checkouts sharing a local
//! bare remote. Ownership is confirmed by the caller before this layer runs.
//! Fixtures set identity, signing and hooks in repository config so the tests
//! never depend on the machine's global Git configuration.

use amux::work_submission::{Mode, Outcome, Preferences, PullRequest, Request, submit};
use std::cell::RefCell;
use std::path::{Path, PathBuf};
use std::process::Command;
use uuid::Uuid;

const TASK: &str = "01a19a44-e705-743e-86f7-126db8e7b5eb";
const MESSAGE: &str = "Fix the parser\n\nHandles empty input.\n\nCo-Authored-By: Claude <noreply@anthropic.com>\n🤖 Generated with [Claude Code](https://claude.com/claude-code)";

fn git(directory: &Path, arguments: &[&str]) -> String {
    let output = Command::new("git")
        .arg("-C")
        .arg(directory)
        .args(arguments)
        .env("GIT_CONFIG_NOSYSTEM", "1")
        .env("GIT_CONFIG_GLOBAL", directory.join("no-global-config"))
        .output()
        .unwrap();
    assert!(
        output.status.success(),
        "git {arguments:?}: {}",
        String::from_utf8_lossy(&output.stderr)
    );
    String::from_utf8(output.stdout).unwrap().trim().to_owned()
}

struct Fixture {
    _temp: tempfile::TempDir,
    remote: PathBuf,
    work: PathBuf,
    hooks: PathBuf,
}

impl Fixture {
    /// A pushed checkout tracking `origin/main` with a.txt, b.txt and gone.txt.
    fn new() -> Self {
        let temp = tempfile::tempdir().unwrap();
        let root = amux::workspace_launch::external_path(temp.path().canonicalize().unwrap());
        let (remote, work, hooks) = (
            root.join("remote.git"),
            root.join("work"),
            root.join("hooks"),
        );
        std::fs::create_dir(&hooks).unwrap();
        git(
            &root,
            &["init", "--bare", "-b", "main", remote.to_str().unwrap()],
        );
        git(&root, &["init", "-b", "main", work.to_str().unwrap()]);
        let fixture = Self {
            _temp: temp,
            remote,
            work,
            hooks,
        };
        fixture.configure(&fixture.work);
        for (name, text) in [("a.txt", "a\n"), ("b.txt", "b\n"), ("gone.txt", "gone\n")] {
            fixture.write(name, text);
        }
        git(&fixture.work, &["add", "."]);
        git(&fixture.work, &["commit", "-m", "initial"]);
        git(
            &fixture.work,
            &["remote", "add", "origin", fixture.remote.to_str().unwrap()],
        );
        git(&fixture.work, &["push", "-u", "origin", "main"]);
        fixture
    }

    fn configure(&self, checkout: &Path) {
        for (key, value) in [
            ("user.name", "Fixture Agent"),
            ("user.email", "agent@example.invalid"),
            ("commit.gpgsign", "false"),
            ("core.autocrlf", "false"),
            ("core.hooksPath", self.hooks.to_str().unwrap()),
        ] {
            git(checkout, &["config", key, value]);
        }
    }

    fn write(&self, name: &str, text: &str) {
        std::fs::write(self.work.join(name), text).unwrap();
    }

    /// Another checkout pushes a commit writing `name`.
    fn push_elsewhere(&self, name: &str, text: &str) {
        let other = self.work.with_file_name("other");
        if !other.exists() {
            git(
                self.work.parent().unwrap(),
                &["clone", self.remote.to_str().unwrap(), "other"],
            );
            self.configure(&other);
        }
        git(&other, &["pull", "--ff-only"]);
        std::fs::write(other.join(name), text).unwrap();
        git(&other, &["add", name]);
        git(&other, &["commit", "-m", &format!("other {name}")]);
        git(&other, &["push"]);
    }

    /// Branch, HEAD, index entries, staged diff and status: what submission
    /// must leave alone when it rejects a request or works in a temporary index.
    fn snapshot(&self) -> Vec<String> {
        [
            &["symbolic-ref", "HEAD"][..],
            &["rev-parse", "HEAD"],
            &["ls-files", "--stage"],
            &["diff", "--cached"],
            &["status", "--porcelain", "--untracked-files=all"],
        ]
        .iter()
        .map(|arguments| git(&self.work, arguments))
        .collect()
    }

    fn remote_main(&self) -> String {
        git(&self.remote, &["rev-parse", "main"])
    }

    fn submit(
        &self,
        preferences: &Preferences,
        message: &str,
        paths: &[&str],
    ) -> anyhow::Result<Outcome> {
        let paths: Vec<PathBuf> = paths.iter().map(PathBuf::from).collect();
        let request = Request {
            root: &self.work,
            task_id: Uuid::parse_str(TASK).unwrap(),
            message,
            paths: &paths,
        };
        submit(&request, preferences, |_| {
            panic!("commit mode opens no pull request")
        })
    }
}

fn changed(checkout: &Path, commit: &str) -> String {
    git(checkout, &["show", "--name-status", "--format=", commit])
}

fn assert_clean_message(checkout: &Path, commit: &str) {
    let message = git(checkout, &["log", "-1", "--format=%B", commit]);
    assert_eq!(
        message,
        format!("Fix the parser\n\nHandles empty input.\n\nAmuxID: {TASK}")
    );
    assert_eq!(
        git(
            checkout,
            &[
                "log",
                "-1",
                "--format=%(trailers:key=AmuxID,valueonly)",
                commit
            ]
        ),
        TASK
    );
    assert!(!message.to_lowercase().contains("co-authored-by"));
}

#[test]
fn commit_and_push_commits_only_listed_paths_including_new_and_deleted_files() {
    let fixture = Fixture::new();
    fixture.write("a.txt", "a changed\n");
    fixture.write("new file.txt", "new\n");
    std::fs::remove_file(fixture.work.join("gone.txt")).unwrap();
    fixture.write("b.txt", "b staged by another agent\n");
    git(&fixture.work, &["add", "b.txt"]);
    fixture.write("untracked.txt", "another agent's draft\n");
    let absolute = fixture.work.join("a.txt");
    let outcome = fixture
        .submit(
            &Preferences::default(),
            MESSAGE,
            &[
                absolute.to_str().unwrap(),
                "new file.txt",
                "./gone.txt",
                "a.txt",
            ],
        )
        .unwrap();
    assert_eq!(outcome.files, ["a.txt", "new file.txt", "gone.txt"]);
    assert_eq!(
        outcome.reference,
        git(&fixture.work, &["rev-parse", "HEAD"])
    );
    assert_eq!(fixture.remote_main(), outcome.reference);
    assert_eq!(
        changed(&fixture.work, "HEAD"),
        "M\ta.txt\nD\tgone.txt\nA\tnew file.txt"
    );
    assert_clean_message(&fixture.work, "HEAD");
    assert_eq!(
        git(&fixture.work, &["diff", "--cached", "--name-only"]),
        "b.txt"
    );
    assert_eq!(
        git(
            &fixture.work,
            &["status", "--porcelain", "--untracked-files=all"]
        ),
        "M  b.txt\n?? untracked.txt"
    );
    assert!(
        outcome
            .comment()
            .starts_with(&format!("Work submitted: {}", outcome.reference))
    );
    assert!(
        outcome
            .comment()
            .ends_with("\n- a.txt\n- new file.txt\n- gone.txt")
    );
}

#[test]
fn rejected_requests_leave_the_repository_untouched() {
    let fixture = Fixture::new();
    fixture.write("a.txt", "a changed\n");
    fixture.write("b.txt", "b staged\n");
    git(&fixture.work, &["add", "b.txt"]);
    std::fs::write(fixture.work.with_file_name("outside.txt"), "outside\n").unwrap();
    let before = fixture.snapshot();
    let remote = fixture.remote_main();
    let outside = fixture.work.with_file_name("outside.txt");
    let cases: [(&str, &[&str], &str); 7] = [
        (MESSAGE, &[], "at least one changed path"),
        (
            MESSAGE,
            &[outside.to_str().unwrap()],
            "not inside the project",
        ),
        (
            MESSAGE,
            &["a.txt", "../outside.txt"],
            "not inside the project",
        ),
        (MESSAGE, &["."], "not inside the project"),
        (MESSAGE, &["a.txt", "gone.txt"], "gone.txt has no changes"),
        (MESSAGE, &[".git/config"], "inside the Git directory"),
        (
            "Co-Authored-By: Claude <noreply@anthropic.com>",
            &["a.txt"],
            "message is empty",
        ),
    ];
    for (message, paths, expected) in cases {
        let error = fixture
            .submit(&Preferences::default(), message, paths)
            .expect_err(expected);
        assert!(
            format!("{error:#}").contains(expected),
            "{paths:?}: {error:#}"
        );
        assert_eq!(fixture.snapshot(), before, "{paths:?}");
        assert_eq!(fixture.remote_main(), remote);
    }
}

#[test]
fn a_remote_that_moved_ahead_is_rebased_onto_and_pushed() {
    let fixture = Fixture::new();
    fixture.push_elsewhere("c.txt", "from another machine\n");
    fixture.write("a.txt", "a changed\n");
    fixture.write("b.txt", "uncommitted work\n");
    let outcome = fixture
        .submit(&Preferences::default(), MESSAGE, &["a.txt"])
        .unwrap();
    assert_eq!(fixture.remote_main(), outcome.reference);
    assert_eq!(
        git(&fixture.work, &["rev-parse", "HEAD"]),
        outcome.reference
    );
    assert_eq!(
        git(&fixture.work, &["log", "-1", "--format=%s", "HEAD~1"]),
        "other c.txt"
    );
    assert_eq!(changed(&fixture.work, "HEAD"), "M\ta.txt");
    assert_clean_message(&fixture.work, "HEAD");
    assert_eq!(
        std::fs::read_to_string(fixture.work.join("b.txt")).unwrap(),
        "uncommitted work\n"
    );
}

#[test]
fn a_rebase_conflict_keeps_the_commit_local_without_losing_work() {
    let fixture = Fixture::new();
    fixture.push_elsewhere("a.txt", "theirs\n");
    let remote = fixture.remote_main();
    fixture.write("a.txt", "ours\n");
    fixture.write("b.txt", "uncommitted work\n");
    let error = fixture
        .submit(&Preferences::default(), MESSAGE, &["a.txt"])
        .unwrap_err();
    let error = format!("{error:#}");
    assert!(
        error.contains("is kept locally and was not pushed"),
        "{error}"
    );
    assert!(error.contains("rebase was aborted"), "{error}");
    assert_eq!(fixture.remote_main(), remote);
    assert!(!fixture.work.join(".git/rebase-merge").exists());
    assert!(!fixture.work.join(".git/rebase-apply").exists());
    assert_eq!(
        git(&fixture.work, &["symbolic-ref", "--short", "HEAD"]),
        "main"
    );
    assert_clean_message(&fixture.work, "HEAD");
    assert_eq!(git(&fixture.work, &["show", "HEAD:a.txt"]), "ours");
    assert_eq!(
        std::fs::read_to_string(fixture.work.join("a.txt")).unwrap(),
        "ours\n"
    );
    assert_eq!(
        std::fs::read_to_string(fixture.work.join("b.txt")).unwrap(),
        "uncommitted work\n"
    );
}

#[test]
fn pull_request_mode_pushes_a_task_branch_without_changing_the_checkout() {
    let fixture = Fixture::new();
    fixture.push_elsewhere("c.txt", "from another machine\n");
    let upstream = fixture.remote_main();
    fixture.write("a.txt", "a changed\n");
    fixture.write("new.txt", "new\n");
    std::fs::remove_file(fixture.work.join("gone.txt")).unwrap();
    fixture.write("b.txt", "b staged by another agent\n");
    git(&fixture.work, &["add", "b.txt"]);
    let before = fixture.snapshot();
    let opened = RefCell::new(None);
    let paths = [
        PathBuf::from("a.txt"),
        PathBuf::from("new.txt"),
        PathBuf::from("gone.txt"),
    ];
    let preferences = Preferences {
        mode: Mode::PullRequest,
        branch_prefix: "amux/".into(),
    };
    let request = Request {
        root: &fixture.work,
        task_id: Uuid::parse_str(TASK).unwrap(),
        message: MESSAGE,
        paths: &paths,
    };
    let outcome = submit(&request, &preferences, |pull: &PullRequest| {
        *opened.borrow_mut() = Some(pull.clone());
        Ok("https://example.invalid/pull/1".into())
    })
    .unwrap();
    assert_eq!(outcome.reference, "https://example.invalid/pull/1");
    assert_eq!(outcome.files, ["a.txt", "new.txt", "gone.txt"]);
    assert_eq!(fixture.snapshot(), before);
    assert_eq!(fixture.remote_main(), upstream);
    let branch = git(
        &fixture.remote,
        &["rev-parse", &format!("refs/heads/amux/{TASK}")],
    );
    assert_eq!(
        git(&fixture.remote, &["rev-parse", &format!("{branch}^")]),
        upstream
    );
    assert_eq!(
        changed(&fixture.remote, &branch),
        "M\ta.txt\nD\tgone.txt\nA\tnew.txt"
    );
    assert_eq!(
        git(&fixture.remote, &["show", &format!("{branch}:a.txt")]),
        "a changed"
    );
    assert_clean_message(&fixture.remote, &branch);
    let pull = opened.into_inner().unwrap();
    assert_eq!(pull.head, format!("amux/{TASK}"));
    assert_eq!(pull.base, "main");
    assert_eq!(pull.title, "Fix the parser");
    assert!(pull.body.ends_with(&format!("\n\nAmuxID: {TASK}")));
    assert!(!pull.body.to_lowercase().contains("co-authored-by"));
}

#[test]
fn pull_request_mode_rejects_upstream_changes_to_selected_files_without_losing_work() {
    for (upstream_text, local_text) in [
        (
            "new header\nmiddle\nold tail\n",
            "old header\nmiddle\nlocal tail\n",
        ),
        ("upstream replacement\n", "local replacement\n"),
    ] {
        let fixture = Fixture::new();
        fixture.write("a.txt", "old header\nmiddle\nold tail\n");
        git(&fixture.work, &["add", "a.txt"]);
        git(&fixture.work, &["commit", "-m", "base text"]);
        git(&fixture.work, &["push"]);
        fixture.push_elsewhere("a.txt", upstream_text);
        let upstream = fixture.remote_main();
        fixture.write("a.txt", local_text);
        fixture.write("b.txt", "another agent's staged work\n");
        git(&fixture.work, &["add", "b.txt"]);
        let before = fixture.snapshot();
        let preferences = Preferences {
            mode: Mode::PullRequest,
            branch_prefix: "amux/".into(),
        };
        let error = fixture
            .submit(&preferences, MESSAGE, &["a.txt"])
            .unwrap_err();
        let error = format!("{error:#}");
        assert!(error.contains("selected paths differ"), "{error}");
        assert!(error.contains("a.txt"), "{error}");
        assert!(error.contains("Reconcile"), "{error}");
        assert_eq!(fixture.snapshot(), before);
        assert_eq!(fixture.remote_main(), upstream);
        assert_eq!(
            git(&fixture.remote, &["show", "main:a.txt"]),
            upstream_text.trim()
        );
        assert_eq!(
            std::fs::read_to_string(fixture.work.join("a.txt")).unwrap(),
            local_text
        );
        assert!(git(&fixture.remote, &["for-each-ref", "refs/heads/amux/"]).is_empty());
    }
}

#[test]
fn pull_request_mode_rejects_an_invalid_branch_prefix_before_pushing() {
    let fixture = Fixture::new();
    fixture.write("a.txt", "a changed\n");
    let before = fixture.snapshot();
    let preferences = Preferences {
        mode: Mode::PullRequest,
        branch_prefix: "bad prefix..".into(),
    };
    let error = fixture
        .submit(&preferences, MESSAGE, &["a.txt"])
        .unwrap_err();
    assert!(
        format!("{error:#}").contains("not a valid branch name"),
        "{error:#}"
    );
    assert_eq!(fixture.snapshot(), before);
    assert!(
        git(&fixture.remote, &["branch", "--list"])
            .lines()
            .all(|b| b.trim_start_matches("* ") == "main")
    );
}
