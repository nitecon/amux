//! Observational work reports. No task mutations, repository writes, or stored snapshots.
use anyhow::{Context, Result, ensure};
use chrono::{DateTime, NaiveDate, Utc};
use serde::Serialize;
use serde_json::Value;
use std::{
    collections::{BTreeMap, BTreeSet},
    fmt::Write,
    path::Path,
    process::Stdio,
    time::Duration,
};
use tokio::io::AsyncWriteExt;
use uuid::Uuid;

#[derive(Debug, Clone, clap::Args)]
pub struct Args {
    /// Git range: FROM..TO (tags or refs, FROM excluded, TO included).
    #[arg(required_unless_present = "since", conflicts_with_all = ["since", "until"])]
    pub range: Option<String>,
    /// Start inclusive: YYYY-MM-DD (UTC) or RFC3339. Searches history reachable from HEAD.
    #[arg(long, requires = "until")]
    pub since: Option<String>,
    /// End exclusive: YYYY-MM-DD (UTC) or RFC3339.
    #[arg(long, requires = "since")]
    pub until: Option<String>,
    /// Skip gateway and GitHub reads; emit local facts and explicit partial markers.
    #[arg(long)]
    pub offline: bool,
}

#[derive(Debug, Serialize)]
pub struct Commit {
    pub sha: String,
    pub committed_at: i64,
    pub subject: String,
    pub task_ids: Vec<Uuid>,
}

#[derive(Debug, Serialize)]
pub struct TaskWork {
    pub id: Uuid,
    pub title: Option<String>,
    pub status: Option<String>,
    pub commits: Vec<String>,
    pub latest_review: Option<Value>,
}

#[derive(Debug, Serialize)]
pub struct Report {
    pub as_of: String,
    pub selection: String,
    pub from_sha: Option<String>,
    pub to_sha: String,
    pub since_ms: i64,
    pub until_ms: i64,
    pub commits: Vec<Commit>,
    pub files: Vec<String>,
    pub tasks: BTreeMap<Uuid, TaskWork>,
    pub untrailered_commits: Vec<String>,
    pub completed_not_released: Vec<TaskWork>,
    pub ci_runs: Vec<Value>,
    pub partial: Vec<String>,
}

fn date(value: &str) -> Result<i64> {
    if let Ok(date) = NaiveDate::parse_from_str(value, "%Y-%m-%d") {
        return Ok(date
            .and_hms_opt(0, 0, 0)
            .unwrap()
            .and_utc()
            .timestamp_millis());
    }
    Ok(DateTime::parse_from_rfc3339(value)
        .context("dates must be YYYY-MM-DD or RFC3339")?
        .timestamp_millis())
}

async fn output(mut command: std::process::Command, input: Option<Vec<u8>>) -> Result<Vec<u8>> {
    command.stdout(Stdio::piped()).stderr(Stdio::piped());
    let mut command = tokio::process::Command::from(command);
    command.kill_on_drop(true);
    command.stdin(if input.is_some() {
        Stdio::piped()
    } else {
        Stdio::null()
    });
    let mut child = command
        .spawn()
        .context("cannot start read-only report command")?;
    let stdin = child.stdin.take();
    let result = tokio::time::timeout(Duration::from_secs(60), async move {
        let writer = async move {
            if let (Some(mut stdin), Some(input)) = (stdin, input) {
                stdin.write_all(&input).await?;
            }
            Ok::<_, std::io::Error>(())
        };
        let (written, output) = tokio::join!(writer, child.wait_with_output());
        written?;
        output
    })
    .await
    .context("read-only report command timed out")??;
    ensure!(
        result.status.success(),
        "read-only report command failed ({}): {}",
        result.status,
        String::from_utf8_lossy(&result.stderr).trim()
    );
    Ok(result.stdout)
}

async fn git(root: &Path, args: &[&str], input: Option<Vec<u8>>) -> Result<String> {
    let mut command = crate::git::command(&[]);
    command
        .args([
            "--no-optional-locks",
            "-c",
            "core.hooksPath=/dev/null",
            "-C",
        ])
        .arg(root)
        .args(args);
    String::from_utf8(output(command, input).await?).context("Git report output is not UTF-8")
}

async fn resolve(root: &Path, reference: &str) -> Result<(String, i64)> {
    ensure!(!reference.is_empty(), "range needs two nonempty refs");
    let sha = git(
        root,
        &[
            "rev-parse",
            "--verify",
            "--end-of-options",
            &format!("{reference}^{{commit}}"),
        ],
        None,
    )
    .await
    .with_context(|| format!("cannot resolve ref {reference:?} to a commit"))?
    .trim()
    .to_owned();
    let time: i64 = git(root, &["show", "-s", "--format=%ct", &sha, "--"], None)
        .await?
        .trim()
        .parse()?;
    Ok((sha, time * 1000))
}

pub async fn local(root: &Path, args: &Args) -> Result<Report> {
    let (from_sha, to_sha, since_ms, until_ms, selection, revisions) =
        if let Some(range) = &args.range {
            let (from, to) = range.split_once("..").context("use FROM..TO")?;
            ensure!(
                !to.starts_with('.') && !to.contains(".."),
                "use a two-dot FROM..TO range"
            );
            let (from_sha, since) = resolve(root, from).await?;
            let (to_sha, until) = resolve(root, to).await?;
            ensure!(since <= until, "range endpoint commit dates are reversed");
            let revisions = vec![format!("{from_sha}..{to_sha}")];
            (
                Some(from_sha),
                to_sha,
                since,
                until,
                range.clone(),
                revisions,
            )
        } else {
            let since = date(
                args.since
                    .as_deref()
                    .context("provide FROM..TO or --since and --until")?,
            )?;
            let until = date(args.until.as_deref().context("provide --until")?)?;
            ensure!(since < until, "--since must precede --until");
            let (head, _) = resolve(root, "HEAD").await?;
            let revisions = vec![
                head.clone(),
                format!("--since-as-filter=@{}", since.div_euclid(1000)),
                format!("--until=@{}", until.div_euclid(1000)),
            ];
            (
                None,
                head,
                since,
                until,
                format!(
                    "{} <= commit time < {} (HEAD)",
                    args.since.as_deref().unwrap(),
                    args.until.as_deref().unwrap()
                ),
                revisions,
            )
        };
    let mut arguments = vec![
        "log",
        "-z",
        "--reverse",
        "--format=%H%x00%ct%x00%s%x00%(trailers:key=AmuxID,valueonly,separator=%x1f)",
    ];
    arguments.extend(revisions.iter().map(String::as_str));
    arguments.push("--");
    let log = git(root, &arguments, None).await?;
    let mut fields: Vec<_> = log.split('\0').collect();
    if fields.last() == Some(&"") {
        fields.pop();
    }
    ensure!(fields.len().is_multiple_of(4), "invalid Git commit records");
    let mut report = Report {
        as_of: Utc::now().to_rfc3339(),
        selection,
        from_sha,
        to_sha,
        since_ms,
        until_ms,
        commits: Vec::new(),
        files: Vec::new(),
        tasks: BTreeMap::new(),
        untrailered_commits: Vec::new(),
        completed_not_released: Vec::new(),
        ci_runs: Vec::new(),
        partial: Vec::new(),
    };
    for record in fields.as_chunks::<4>().0 {
        let committed_at = record[1].parse::<i64>()? * 1000;
        if report.from_sha.is_none() && !(since_ms..until_ms).contains(&committed_at) {
            continue;
        }
        let mut ids = BTreeSet::new();
        for trailer in record[3]
            .split(['\x1f', '\n'])
            .map(str::trim)
            .filter(|s| !s.is_empty())
        {
            if let Ok(id) = Uuid::parse_str(trailer)
                && !id.is_nil()
            {
                ids.insert(id);
            } else {
                report
                    .partial
                    .push(format!("Invalid AmuxID trailer in {}", record[0]));
            }
        }
        let commit = Commit {
            sha: record[0].into(),
            committed_at,
            subject: record[2].into(),
            task_ids: ids.into_iter().collect(),
        };
        if commit.task_ids.is_empty() {
            report.untrailered_commits.push(commit.sha.clone());
        }
        for id in &commit.task_ids {
            report
                .tasks
                .entry(*id)
                .or_insert_with(|| TaskWork {
                    id: *id,
                    title: None,
                    status: None,
                    commits: Vec::new(),
                    latest_review: None,
                })
                .commits
                .push(commit.sha.clone());
        }
        report.commits.push(commit);
    }
    if !report.commits.is_empty() {
        let input = report
            .commits
            .iter()
            .map(|c| format!("{}\n", c.sha))
            .collect::<String>();
        // Union of files touched by selected commits, including merge-parent
        // diffs and files changed then reverted. NULs preserve unusual filenames.
        let files = git(
            root,
            &[
                "diff-tree",
                "--stdin",
                "--root",
                "-m",
                "-r",
                "--no-commit-id",
                "--no-renames",
                "--name-only",
                "-z",
            ],
            Some(input.into_bytes()),
        )
        .await?;
        report.files = files
            .split('\0')
            .filter(|s| !s.is_empty())
            .map(str::to_owned)
            .collect::<BTreeSet<_>>()
            .into_iter()
            .collect();
    }
    let shallow = git(root, &["rev-parse", "--is-shallow-repository"], None).await?;
    if shallow.trim() == "true" {
        report
            .partial
            .push("Shallow checkout: local history may be incomplete".into());
    }
    Ok(report)
}

impl Report {
    pub async fn enrich_tasks(
        &mut self,
        gateway: &crate::gateway::Gateway,
        project: &str,
    ) -> Result<()> {
        let rows = gateway
            .list_filtered(
                project,
                Some("todo,in_progress,user_review,done,cancelled"),
                true,
            )
            .await?;
        let rows = rows
            .as_array()
            .context("gateway task list is not an array")?;
        let mut ids: BTreeSet<_> = self.tasks.keys().copied().collect();
        for row in rows {
            let id = row["id"]
                .as_str()
                .and_then(|s| Uuid::parse_str(s).ok())
                .context("invalid task ID in report list")?;
            // A completion lies between creation and last update. Summaries
            // omit done_at; reject only provably out-of-window rows here and
            // read details for the remaining candidates, including stale work.
            let possible_completion = row["created_at"]
                .as_i64()
                .is_none_or(|created| created <= self.until_ms)
                && row["updated_at"]
                    .as_i64()
                    .is_none_or(|updated| updated >= self.since_ms);
            if row["status"] == "done" && possible_completion {
                ids.insert(id);
            }
        }
        let mut reviews = BTreeSet::new();
        for row in rows {
            let related = row["labels"].as_array().is_some_and(|labels| {
                labels.iter().any(|label| label == "task-review")
                    && labels
                        .iter()
                        .filter_map(|label| {
                            label
                                .as_str()?
                                .strip_prefix("review-of:")?
                                .parse::<Uuid>()
                                .ok()
                        })
                        .any(|source| ids.contains(&source))
            });
            if related {
                reviews.insert(
                    row["id"]
                        .as_str()
                        .context("invalid review ID")?
                        .parse::<Uuid>()?,
                );
            }
        }
        ids.extend(reviews.iter());
        let mut details = BTreeMap::new();
        for id in ids {
            match gateway
                .task(project, id, if reviews.contains(&id) { 100 } else { 0 })
                .await
            {
                Ok(task) => {
                    details.insert(id, task);
                }
                Err(_) => self
                    .partial
                    .push(format!("Task {id}: gateway detail unavailable")),
            }
        }
        self.apply_tasks(&details);
        Ok(())
    }

    /// Apply current gateway facts, never infer historic task status from a commit.
    pub fn apply_tasks(&mut self, details: &BTreeMap<Uuid, Value>) {
        for (id, task) in details {
            // Review tasks never produce commits; they only supply latest_review.
            let review = task["labels"]
                .as_array()
                .is_some_and(|labels| labels.iter().any(|l| l == "task-review"));
            if task["status"] == "done" && !review {
                match task["done_at"].as_i64() {
                    Some(done)
                        if (if self.from_sha.is_some() {
                            done > self.since_ms && done <= self.until_ms
                        } else {
                            (self.since_ms..self.until_ms).contains(&done)
                        }) && !self.tasks.contains_key(id) =>
                    {
                        self.completed_not_released.push(TaskWork {
                            id: *id,
                            title: task["title"].as_str().map(str::to_owned),
                            status: Some("done".into()),
                            commits: Vec::new(),
                            latest_review: None,
                        });
                    }
                    None => self
                        .partial
                        .push(format!("Task {id}: completion time unavailable")),
                    _ => {}
                }
            }
        }
        for work in self
            .tasks
            .values_mut()
            .chain(self.completed_not_released.iter_mut())
        {
            if let Some(task) = details.get(&work.id) {
                work.title = task["title"].as_str().map(str::to_owned);
                work.status = task["status"].as_str().map(str::to_owned);
            }
            if work.title.is_none() || work.status.is_none() {
                self.partial
                    .push(format!("Task {}: title/status unavailable", work.id));
            }
            let label = format!("review-of:{}", work.id);
            work.latest_review = details.values().filter(|task| {
                task["labels"].as_array().is_some_and(|labels| labels.iter().any(|l| l == &label) && labels.iter().any(|l| l == "task-review"))
            }).max_by_key(|task| task["created_at"].as_i64()).map(|review| {
                let comment = review["comments"].as_array().and_then(|comments| comments.iter().filter(|c| c["author_type"] != "system").max_by_key(|c| c["created_at"].as_i64()));
                serde_json::json!({"task_id": review["id"], "status": review["status"], "latest_comment": comment})
            });
            if let Some(review) = &work.latest_review
                && review["latest_comment"].is_null()
            {
                self.partial.push(format!(
                    "Review task {}: result comment unavailable in newest 100 comments",
                    review["task_id"]
                ));
            }
        }
    }

    pub async fn enrich_ci(&mut self, root: &Path) -> Result<()> {
        let mut repo = std::process::Command::new("gh");
        repo.current_dir(root)
            .args(["repo", "view", "--json", "nameWithOwner"]);
        let repo: Value = serde_json::from_slice(&output(repo, None).await?)?;
        let name = repo["nameWithOwner"]
            .as_str()
            .context("GitHub repository unavailable")?;
        let mut command = std::process::Command::new("gh");
        command.current_dir(root).args([
            "api",
            "--paginate",
            &format!(
                "repos/{name}/actions/runs?head_sha={}&per_page=100",
                self.to_sha
            ),
        ]);
        self.apply_ci(&output(command, None).await?)
    }

    pub fn apply_ci(&mut self, pages: &[u8]) -> Result<()> {
        // Older gh versions emit consecutive JSON documents with --paginate.
        for page in serde_json::Deserializer::from_slice(pages).into_iter::<Value>() {
            let page = page?;
            for run in page["workflow_runs"]
                .as_array()
                .context("invalid GitHub CI response")?
            {
                ensure!(
                    run["head_sha"] == self.to_sha,
                    "GitHub returned CI for a different SHA"
                );
                ensure!(
                    run["id"].as_u64().is_some_and(|id| id > 0)
                        && run["run_attempt"]
                            .as_u64()
                            .is_some_and(|attempt| attempt > 0)
                        && run["status"].as_str().is_some()
                        && (run["status"] != "completed" || run["conclusion"].as_str().is_some()),
                    "GitHub CI run is missing its ID, attempt, status or conclusion"
                );
                self.ci_runs.push(serde_json::json!({"id": run["id"], "attempt": run["run_attempt"], "name": run["name"], "event": run["event"], "head_sha": run["head_sha"], "status": run["status"], "conclusion": run["conclusion"], "url": run["html_url"]}));
            }
        }
        ensure!(
            !self.ci_runs.is_empty(),
            "no CI runs found for the exact endpoint SHA"
        );
        Ok(())
    }

    pub fn render(&self) -> String {
        let mut out = format!(
            "# Work report: {}\n\nAs of: {}\nReport: {}\n",
            self.selection,
            self.as_of,
            if self.partial.is_empty() {
                "facts retrieved (not a release approval)"
            } else {
                "PARTIAL"
            }
        );
        if let Some(from) = &self.from_sha {
            let _ = writeln!(out, "From: {from}");
        }
        let _ = writeln!(
            out,
            "To / CI SHA: {}\nCommits: {}\nFiles touched: {}\n",
            self.to_sha,
            self.commits.len(),
            self.files.len()
        );
        for gap in &self.partial {
            let _ = writeln!(out, "- Missing/incomplete: {gap}");
        }
        let (start, end) = if self.from_sha.is_some() {
            ('(', ']')
        } else {
            ('[', ')')
        };
        let _ = writeln!(
            out,
            "\nTask status and review comments are current at retrieval. Completion window: {start}{}, {}{end} Unix milliseconds.\n",
            self.since_ms, self.until_ms
        );
        for work in self.tasks.values() {
            let _ = writeln!(
                out,
                "## {} — {} [{}]\n",
                work.id,
                work.title.as_deref().unwrap_or("title unavailable"),
                work.status.as_deref().unwrap_or("status unavailable")
            );
            self.render_commits(&mut out, &work.commits);
            render_review(&mut out, work);
        }
        let _ = writeln!(out, "\n## Commits without a valid AmuxID\n");
        self.render_commits(&mut out, &self.untrailered_commits);
        let _ = writeln!(
            out,
            "\n## Completed, not released\n\nNo matching AmuxID commit in this range; untrailered commits may contain this work.\n"
        );
        for work in &self.completed_not_released {
            let _ = writeln!(
                out,
                "- {} — {} [done]",
                work.id,
                work.title.as_deref().unwrap_or("title unavailable")
            );
            render_review(&mut out, work);
        }
        let _ = writeln!(out, "\n## CI for {}\n", self.to_sha);
        for run in &self.ci_runs {
            let _ = writeln!(
                out,
                "- {}: run {} attempt {}, {} / {} ({})",
                run["name"].as_str().unwrap_or("unknown workflow"),
                run["id"],
                run["attempt"],
                run["status"].as_str().unwrap_or("unknown"),
                run["conclusion"].as_str().unwrap_or("pending"),
                run["url"].as_str().unwrap_or("URL unavailable")
            );
        }
        let _ = writeln!(out, "\n## Files touched\n");
        for file in &self.files {
            let _ = writeln!(out, "- {file:?}");
        }
        out
    }

    fn render_commits(&self, out: &mut String, shas: &[String]) {
        for sha in shas {
            if let Some(commit) = self.commits.iter().find(|c| &c.sha == sha) {
                let _ = writeln!(out, "- {} {}", sha, commit.subject);
            }
        }
    }
}

fn render_review(out: &mut String, work: &TaskWork) {
    if let Some(review) = &work.latest_review {
        let _ = writeln!(
            out,
            "\nReview task {} [{}]; latest non-system comment (verbatim):\n\n{}\n",
            review["task_id"].as_str().unwrap_or("unknown"),
            review["status"].as_str().unwrap_or("unknown"),
            review["latest_comment"]["content"]
                .as_str()
                .unwrap_or("No result comment available")
        );
    } else {
        let _ = writeln!(
            out,
            "\nReview result: unavailable (no linked review retrieved).\n"
        );
    }
}
