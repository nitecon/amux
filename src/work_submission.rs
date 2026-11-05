//! Submit an owned task's finished work from the shared project checkout.
//!
//! Agents name the files they changed; this module commits exactly those files
//! and publishes them in the mode chosen in Settings: directly to the current
//! branch's upstream, or as a pull request built without touching the shared
//! checkout's branch, index or working tree. Everything is validated before
//! the repository changes. Git runs with hooks and signing as configured, never
//! with `--author`, `--no-verify` or a force push, and attribution trailers such
//! as `Co-Authored-By` are removed from the message.
use anyhow::{Context, Result, bail};
use serde::{Deserialize, Serialize};
use std::collections::HashSet;
use std::io::Write;
use std::path::{Component, Path, PathBuf};
use uuid::Uuid;

/// Inherited push transport settings; every other `GIT_*` variable is dropped.
const TRANSPORT_VARIABLES: &[&str] = &[
    "GIT_SSH",
    "GIT_SSH_COMMAND",
    "GIT_SSH_VARIANT",
    "GIT_ASKPASS",
];
/// Message trailer keys removed before committing (compared case-insensitively).
/// `AmuxID` is removed too: the submission appends the authoritative one.
const STRIPPED_TRAILERS: &[&str] = &[
    "co-authored-by",
    "co-developed-by",
    "assisted-by",
    "generated-by",
    "amuxid",
];
/// Agents whose "Generated with …" attribution lines are removed.
const ATTRIBUTED_AGENTS: &[&str] = &["claude", "codex", "copilot", "gemini"];

#[derive(Debug, Clone, Copy, Default, PartialEq, Eq, Deserialize, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum Mode {
    /// Commit to the current branch and push it to its upstream.
    #[default]
    CommitAndPush,
    /// Push a task branch based on the upstream and open a pull request.
    PullRequest,
}

/// Work submission settings, stored with the desktop preferences.
#[derive(Debug, Clone, PartialEq, Eq, Deserialize, Serialize)]
#[serde(default)]
pub struct Preferences {
    pub mode: Mode,
    /// Pull request branches are named `<prefix><full task UUID>`.
    pub branch_prefix: String,
}

impl Default for Preferences {
    fn default() -> Self {
        Self {
            mode: Mode::default(),
            branch_prefix: "amux/".into(),
        }
    }
}

/// One submission, after the caller has confirmed it owns the task.
pub struct Request<'a> {
    /// The project's checkout root; every path must be inside it.
    pub root: &'a Path,
    pub task_id: Uuid,
    pub message: &'a str,
    /// Absolute paths, or paths relative to `root`.
    pub paths: &'a [PathBuf],
}

/// The mode-independent result: a commit SHA or pull request URL.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct Outcome {
    pub reference: String,
    /// Submitted paths, relative to the project root with `/` separators.
    pub files: Vec<String>,
}

impl Outcome {
    /// Task comment recording the submission.
    pub fn comment(&self) -> String {
        let files: String = self.files.iter().map(|f| format!("\n- {f}")).collect();
        format!("Work submitted: {}\n\nFiles:{files}", self.reference)
    }
}

/// A pull request to open for a pushed task branch.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct PullRequest {
    pub root: PathBuf,
    /// Task branch name, without `refs/heads/`.
    pub head: String,
    /// Upstream branch name, without `refs/heads/`.
    pub base: String,
    pub title: String,
    pub body: String,
}

/// Validate and submit `request`. `open` opens the pull request in
/// [`Mode::PullRequest`] and returns its URL; production passes
/// [`open_pull_request`]. Validation failures leave the repository untouched.
pub fn submit(
    request: &Request<'_>,
    preferences: &Preferences,
    open: impl FnOnce(&PullRequest) -> Result<String>,
) -> Result<Outcome> {
    let repo = Repo::new(request.root)?;
    let message = commit_message(request.message, request.task_id)?;
    let files = repo.changed_pathspecs(request.paths)?;
    let upstream = repo.upstream()?;
    let reference = match preferences.mode {
        Mode::CommitAndPush => repo.commit_and_push(&upstream, &files, &message)?,
        Mode::PullRequest => {
            let branch = branch_name(&preferences.branch_prefix, request.task_id);
            repo.ensure_branch_name(&branch)?;
            let pull = repo.push_task_branch(&upstream, &files, &message, &branch)?;
            open(&pull).with_context(|| {
                format!("branch {branch} was pushed, but its pull request could not be opened")
            })?
        }
    };
    Ok(Outcome { reference, files })
}

/// Open a pull request with the GitHub CLI and return its URL.
pub fn open_pull_request(request: &PullRequest) -> Result<String> {
    let output = std::process::Command::new("gh")
        .args(["pr", "create"])
        .arg(format!("--head={}", request.head))
        .arg(format!("--base={}", request.base))
        .arg(format!("--title={}", request.title))
        .arg(format!("--body={}", request.body))
        .current_dir(&request.root)
        .env("GH_PROMPT_DISABLED", "1")
        .stdin(std::process::Stdio::null())
        .output()
        .context("cannot run gh; install the GitHub CLI and sign in with gh auth login")?;
    if !output.status.success() {
        bail!(
            "gh pr create failed: {}",
            String::from_utf8_lossy(&output.stderr).trim()
        );
    }
    String::from_utf8_lossy(&output.stdout)
        .lines()
        .map(str::trim)
        .rfind(|line| !line.is_empty())
        .map(str::to_owned)
        .context("gh pr create did not print the pull request URL")
}

/// The message without attribution lines, surplus blank lines or trailing
/// whitespace. Removed: trailers named in [`STRIPPED_TRAILERS`] and agent
/// "Generated with …" lines.
pub fn clean_message(message: &str) -> String {
    let mut lines: Vec<&str> = Vec::new();
    for line in message.lines().filter(|line| !is_attribution(line)) {
        let line = line.trim_end();
        if line.is_empty() && lines.last().is_none_or(|last| last.is_empty()) {
            continue;
        }
        lines.push(line);
    }
    while lines.last().is_some_and(|last| last.is_empty()) {
        lines.pop();
    }
    lines.join("\n")
}

fn is_attribution(line: &str) -> bool {
    let line = line.trim().to_lowercase();
    if let Some((key, _)) = line.split_once(':')
        && STRIPPED_TRAILERS.contains(&key.trim_end())
    {
        return true;
    }
    let text = line.trim_start_matches(|c: char| !c.is_alphanumeric());
    text.starts_with("generated with") && ATTRIBUTED_AGENTS.iter().any(|a| text.contains(a))
}

/// The cleaned message followed by its `AmuxID` trailer.
fn commit_message(message: &str, task_id: Uuid) -> Result<String> {
    let message = clean_message(message);
    if message.is_empty() {
        bail!("the commit message is empty once attribution lines are removed");
    }
    Ok(format!("{message}\n\nAmuxID: {task_id}"))
}

/// `<prefix><full task UUID>`. Task IDs are UUIDv7, whose leading characters
/// are a timestamp shared by tasks created together, so no prefix of the ID is
/// unique enough.
fn branch_name(prefix: &str, task_id: Uuid) -> String {
    format!("{}{task_id}", prefix.trim())
}

/// The branch a checkout tracks: `remote` and its `refs/heads/…` name.
struct Upstream {
    remote: String,
    merge: String,
}

impl Upstream {
    fn branch(&self) -> &str {
        self.merge.trim_start_matches("refs/heads/")
    }
}

struct Repo {
    root: PathBuf,
    /// Temporary index replacing the checkout's own, when set.
    index: Option<PathBuf>,
}

impl Repo {
    fn new(root: &Path) -> Result<Self> {
        let root = root
            .canonicalize()
            .map(crate::workspace_launch::external_path)
            .with_context(|| format!("cannot open project root {}", root.display()))?;
        Ok(Self { root, index: None })
    }

    /// Run git in the root with literal pathspecs; return trimmed stdout.
    fn git(&self, arguments: &[&str]) -> Result<String> {
        Ok(self.git_raw(arguments)?.trim().to_owned())
    }

    /// Preserve NUL-delimited output, including leading spaces in status records.
    fn git_raw(&self, arguments: &[&str]) -> Result<String> {
        let mut command = crate::git::command(TRANSPORT_VARIABLES);
        command
            .arg("-C")
            .arg(&self.root)
            .args(arguments)
            .env("GIT_LITERAL_PATHSPECS", "1");
        if let Some(index) = &self.index {
            command.env("GIT_INDEX_FILE", index);
        }
        let name = arguments.first().copied().unwrap_or_default();
        let output = command
            .output()
            .with_context(|| format!("cannot run git {name}"))?;
        if !output.status.success() {
            bail!(
                "git {name} failed: {}",
                String::from_utf8_lossy(&output.stderr).trim()
            );
        }
        Ok(String::from_utf8_lossy(&output.stdout).into_owned())
    }

    /// Keep exact add/commit path lists off the OS command line.
    fn git_paths(&self, arguments: &[&str], files: &[String]) -> Result<String> {
        let mut paths =
            tempfile::NamedTempFile::new().context("cannot create Git pathspec file")?;
        for file in files {
            paths.write_all(file.as_bytes())?;
            paths.write_all(&[0])?;
        }
        paths.flush()?;
        let file = paths
            .path()
            .to_str()
            .context("Git pathspec file is not UTF-8")?;
        let option = format!("--pathspec-from-file={file}");
        let mut all = arguments.to_vec();
        all.extend([option.as_str(), "--pathspec-file-nul"]);
        self.git(&all)
    }

    /// Root-relative pathspecs for `paths`, each of which must be inside the
    /// root, outside `.git`, and modified, added or deleted.
    fn changed_pathspecs(&self, paths: &[PathBuf]) -> Result<Vec<String>> {
        if paths.is_empty() {
            bail!("list at least one changed path to submit");
        }
        let status = self.git_raw(&[
            "status",
            "--porcelain=v1",
            "-z",
            "--no-renames",
            "--untracked-files=all",
        ])?;
        // Porcelain paths are repository-relative; the project root may be a
        // subdirectory of the repository.
        let prefix = self.git(&["rev-parse", "--show-prefix"])?;
        let changed: HashSet<_> = status
            .split('\0')
            .filter_map(|record| record.get(3..)?.strip_prefix(prefix.as_str()))
            .collect();
        let mut files = Vec::new();
        let mut seen = HashSet::new();
        for path in paths {
            let file = self.pathspec(path)?;
            if !changed.contains(file.as_str()) {
                bail!("{file} has no changes to submit");
            }
            if seen.insert(file.clone()) {
                files.push(file);
            }
        }
        Ok(files)
    }

    fn pathspec(&self, path: &Path) -> Result<String> {
        let shown = path.display();
        let mut normal = PathBuf::new();
        for component in self.root.join(path).components() {
            match component {
                Component::CurDir => {}
                Component::ParentDir => {
                    normal.pop();
                }
                other => normal.push(other),
            }
        }
        // Resolve links in the parent folders only: the path itself may be a
        // deleted file or a committed symbolic link.
        let name = normal
            .file_name()
            .with_context(|| format!("{shown} is not a file path"))?;
        let mut parent = normal.parent().context("path has no parent")?;
        let mut missing = Vec::new();
        let resolved = loop {
            match parent.canonicalize() {
                Ok(resolved) => break crate::workspace_launch::external_path(resolved),
                Err(_) => {
                    missing.push(parent.file_name().context("path has no existing folder")?);
                    parent = parent.parent().context("path has no existing folder")?;
                }
            }
        };
        let resolved = missing
            .iter()
            .rev()
            .fold(resolved, |p, c| p.join(c))
            .join(name);
        let relative = resolved
            .strip_prefix(&self.root)
            .ok()
            .filter(|relative| !relative.as_os_str().is_empty())
            .with_context(|| {
                format!(
                    "{shown} is not inside the project at {}",
                    self.root.display()
                )
            })?;
        let parts = relative
            .components()
            .map(|part| part.as_os_str().to_str())
            .collect::<Option<Vec<_>>>()
            .with_context(|| format!("{shown} is not valid UTF-8"))?;
        if parts[0].eq_ignore_ascii_case(".git") {
            bail!("{shown} is inside the Git directory");
        }
        let file = parts.join("/");
        if file.contains('\0') {
            bail!("file paths must not contain NUL characters");
        }
        Ok(file)
    }

    fn upstream(&self) -> Result<Upstream> {
        let branch = self
            .git(&["symbolic-ref", "--quiet", "--short", "HEAD"])
            .context(
                "HEAD is detached; check out a branch with an upstream before submitting work",
            )?;
        let setting = |key| self.git(&["config", "--get", &format!("branch.{branch}.{key}")]);
        match (setting("remote"), setting("merge")) {
            (Ok(remote), Ok(merge)) if !remote.is_empty() && merge.starts_with("refs/heads/") => {
                Ok(Upstream { remote, merge })
            }
            _ => bail!(
                "branch {branch} has no upstream; set one with git push -u before submitting work"
            ),
        }
    }

    fn ensure_branch_name(&self, branch: &str) -> Result<()> {
        self.git(&["check-ref-format", &format!("refs/heads/{branch}")])
            .map(drop)
            .with_context(|| format!("{branch:?} is not a valid branch name; change the pull request branch prefix in Settings"))
    }

    /// Commit only `files` (other staged changes stay staged), then push. A
    /// rejected push is retried once after rebasing onto the fetched upstream.
    fn commit_and_push(
        &self,
        upstream: &Upstream,
        files: &[String],
        message: &str,
    ) -> Result<String> {
        self.git_paths(&["add", "--all"], files)?;
        self.git_paths(&["commit", "--only", "-m", message], files)?;
        let commit = self.git(&["rev-parse", "HEAD"])?;
        let kept = |commit: &str| format!("commit {commit} is kept locally and was not pushed");
        let target = format!("HEAD:{}", upstream.merge);
        let push = ["push", upstream.remote.as_str(), target.as_str()];
        let Err(rejected) = self.git(&push) else {
            return Ok(commit);
        };
        self.git(&["fetch", &upstream.remote])
            .with_context(|| kept(&commit))?;
        // Only a remote that moved ahead is retried; other failures are reported.
        if self
            .git(&["merge-base", "--is-ancestor", "@{upstream}", "HEAD"])
            .is_ok()
        {
            return Err(rejected.context(kept(&commit)));
        }
        self.rebase_preserving_index(upstream.branch())
            .with_context(|| kept(&self.git(&["rev-parse", "HEAD"]).unwrap_or(commit)))?;
        let commit = self.git(&["rev-parse", "HEAD"])?;
        self.git(&push).with_context(|| kept(&commit))?;
        Ok(commit)
    }

    /// Rebase autostash does not restore staged snapshots. Save tracked work
    /// explicitly and apply it with --index after either success or abort.
    fn rebase_preserving_index(&self, branch: &str) -> Result<()> {
        let git_dir = PathBuf::from(self.git(&["rev-parse", "--absolute-git-dir"])?);
        let stash = if self
            .git(&["status", "--porcelain", "--untracked-files=no"])?
            .is_empty()
        {
            None
        } else {
            self.git(&["stash", "push", "--message", "amux submit-work rebase"])?;
            Some(self.git(&["rev-parse", "refs/stash"])?)
        };
        let rebased = self.git(&["rebase", "--no-autostash", "@{upstream}"]);
        if rebased.is_err()
            && (git_dir.join("rebase-merge").exists() || git_dir.join("rebase-apply").exists())
        {
            self.git(&["rebase", "--abort"]).with_context(|| {
                format!(
                    "could not abort rebase; saved work remains in stash {}",
                    stash.as_deref().unwrap_or("(none)")
                )
            })?;
        }
        if let Some(stash) = stash {
            self.git(&["stash", "apply", "--index", &stash])
                .with_context(|| format!(
                    "could not restore staged and unstaged work; it remains saved in stash {stash}. Resolve the checkout before retrying; recover with git stash apply --index {stash}"
                ))?;
            // Never drop another caller's newer stash.
            if self.git(&["rev-parse", "refs/stash"])? == stash {
                self.git(&["stash", "drop", "stash@{0}"])?;
            }
        }
        rebased.with_context(|| format!(
            "{branch} moved and rebasing onto it failed, so any active rebase was aborted and staged and unstaged work restored. If you have unrelated drafts, save them with git stash push --include-untracked and record that stash's ID with git rev-parse refs/stash. Run git pull --rebase; resolve conflicts, git add the resolved files, and git -c core.editor=true rebase --continue until finished, then git push. Restore saved drafts with git stash apply --index <saved-stash-id>; verify the staged and unstaged drafts before dropping only that saved stash"
        ))?;
        Ok(())
    }

    /// Build the commit on the fetched upstream in a temporary index, push it
    /// to `branch` and describe its pull request. The checkout's branch, index
    /// and working tree are not changed.
    fn push_task_branch(
        &self,
        upstream: &Upstream,
        files: &[String],
        message: &str,
        branch: &str,
    ) -> Result<PullRequest> {
        self.git(&["fetch", &upstream.remote])?;
        let base = self.git(&["rev-parse", "--verify", "@{upstream}^{commit}"])?;
        // Whole-file staging is safe only when upstream still has the same
        // selected files as our checkout base; otherwise it could revert edits.
        let selected: HashSet<_> = files.iter().map(String::as_str).collect();
        let diff = self.git_raw(&[
            "diff",
            "--name-only",
            "--relative",
            "--no-renames",
            "-z",
            "HEAD",
            &base,
        ])?;
        let changed = diff
            .split('\0')
            .filter(|path| selected.contains(path))
            .collect::<Vec<_>>()
            .join("\n");
        if !changed.is_empty() {
            bail!(
                "selected paths differ between HEAD and {}: {changed}. Reconcile these paths with upstream before submitting; no task branch was pushed and your checkout was not changed",
                upstream.branch()
            );
        }
        let scratch = tempfile::tempdir().context("cannot create a temporary index")?;
        let staging = Repo {
            root: self.root.clone(),
            index: Some(scratch.path().join("index")),
        };
        staging.git(&["read-tree", &base])?;
        staging.git_paths(&["add", "--all"], files)?;
        let tree = staging.git(&["write-tree"])?;
        if tree == self.git(&["rev-parse", &format!("{base}^{{tree}}")])? {
            bail!(
                "the listed paths already match {}; nothing to submit",
                upstream.branch()
            );
        }
        let commit = self.git(&["commit-tree", &tree, "-p", &base, "-m", message])?;
        self.git(&[
            "push",
            &upstream.remote,
            &format!("{commit}:refs/heads/{branch}"),
        ])?;
        Ok(PullRequest {
            root: self.root.clone(),
            head: branch.to_owned(),
            base: upstream.branch().to_owned(),
            title: message.lines().next().unwrap_or_default().to_owned(),
            body: message.to_owned(),
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn attribution_lines_are_removed_and_blank_lines_collapsed() {
        let message = "\nFix the parser\n\n\nHandles empty input.\n\nCo-Authored-By: Claude <noreply@anthropic.com>\nco-authored-by : x <y>\nAmuxID: 123\n🤖 Generated with [Claude Code](https://claude.com/claude-code)\nRefs: #4\n";
        assert_eq!(
            clean_message(message),
            "Fix the parser\n\nHandles empty input.\n\nRefs: #4"
        );
        assert_eq!(
            commit_message("Subject\nCo-authored-by: a <b>", Uuid::nil()).unwrap(),
            format!("Subject\n\nAmuxID: {}", Uuid::nil())
        );
        assert!(commit_message("CO-AUTHORED-BY: a <b>", Uuid::nil()).is_err());
        assert_eq!(clean_message("Generated with care"), "Generated with care");
    }

    #[test]
    fn preferences_default_to_commit_and_push_with_the_amux_prefix() {
        let preferences: Preferences = serde_json::from_str("{}").unwrap();
        assert_eq!(preferences, Preferences::default());
        assert_eq!(preferences.mode, Mode::CommitAndPush);
        assert_eq!(preferences.branch_prefix, "amux/");
        // Two UUIDv7 IDs from the same millisecond share their timestamp prefix.
        let first = Uuid::parse_str("01a19a44-e705-743e-86f7-126db8e7b5eb").unwrap();
        let second = Uuid::parse_str("01a19a44-e705-7c01-9a2b-3c4d5e6f7081").unwrap();
        assert_eq!(
            branch_name(" amux/ ", first),
            "amux/01a19a44-e705-743e-86f7-126db8e7b5eb"
        );
        assert_ne!(branch_name("amux/", first), branch_name("amux/", second));
    }
}
