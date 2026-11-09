//! Real local Git history and GET-only gateway fixtures; no GitHub/network credentials.
use amux::work_report::{Args, local};
use serde_json::json;
use std::{collections::BTreeMap, path::Path, process::Command};
use uuid::Uuid;

const TASK: &str = "01a19a45-0eae-75e3-9aa9-dad8a2320139";
const OTHER: &str = "01a1986d-8c70-778a-ad46-316ec310c1b6";

fn git(root: &Path, args: &[&str]) -> String {
    let output = Command::new("git")
        .arg("-C")
        .arg(root)
        .args([
            "-c",
            "commit.gpgsign=false",
            "-c",
            "core.hooksPath=/dev/null",
        ])
        .args(args)
        .env("GIT_CONFIG_NOSYSTEM", "1")
        .env("GIT_CONFIG_GLOBAL", root.join("no-global"))
        .env("GIT_AUTHOR_DATE", "2024-01-02T00:00:00Z")
        .env("GIT_COMMITTER_DATE", "2024-01-02T00:00:00Z")
        .output()
        .unwrap();
    assert!(
        output.status.success(),
        "{args:?}: {}",
        String::from_utf8_lossy(&output.stderr)
    );
    String::from_utf8(output.stdout).unwrap().trim().into()
}

fn fixture() -> tempfile::TempDir {
    let temp = tempfile::tempdir().unwrap();
    git(temp.path(), &["init", "-b", "main"]);
    git(temp.path(), &["config", "user.name", "Fixture"]);
    git(
        temp.path(),
        &["config", "user.email", "fixture@example.invalid"],
    );
    git(temp.path(), &["commit", "--allow-empty", "-m", "base"]);
    git(temp.path(), &["tag", "v0.0.58"]);
    temp
}

fn args(range: &str) -> Args {
    Args {
        range: Some(range.into()),
        since: None,
        until: None,
        offline: true,
    }
}

#[tokio::test]
async fn enumerates_large_ranges_groups_trailers_and_keeps_untrailered_commits() {
    let temp = fixture();
    let root = temp.path();
    for i in 0..310 {
        std::fs::write(root.join(format!("file {i}")), "one\n").unwrap();
    }
    git(root, &["add", "."]);
    git(root, &["commit", "-m", &format!("bulk\n\nAmuxID: {TASK}")]);
    // Exceeds the GitHub compare commit cap as well as its file cap above.
    for i in 0..251 {
        git(
            root,
            &["commit", "--allow-empty", "-m", &format!("extra {i}")],
        );
    }
    std::fs::write(root.join("file 0"), "two\n").unwrap();
    git(
        root,
        &[
            "commit",
            "-am",
            &format!("two tasks\n\nAmuxID: {TASK}\nAmuxID: {OTHER}"),
        ],
    );
    git(
        root,
        &[
            "commit",
            "--allow-empty",
            "-m",
            "invalid\n\nAmuxID: not-an-id",
        ],
    );
    git(root, &["tag", "v0.0.59"]);
    let before = git(root, &["status", "--porcelain=v1"]);
    let report = local(root, &args("v0.0.58..v0.0.59")).await.unwrap();
    assert_eq!(report.commits.len(), 254);
    assert_eq!(report.files.len(), 310);
    assert_eq!(
        report.tasks[&Uuid::parse_str(TASK).unwrap()].commits.len(),
        2
    );
    assert_eq!(
        report.tasks[&Uuid::parse_str(OTHER).unwrap()].commits.len(),
        1
    );
    assert_eq!(report.untrailered_commits.len(), 252);
    assert!(
        report
            .partial
            .iter()
            .any(|gap| gap.contains("Invalid AmuxID"))
    );
    assert_eq!(report.to_sha, git(root, &["rev-parse", "v0.0.59"]));
    assert_eq!(before, git(root, &["status", "--porcelain=v1"]));
}

#[tokio::test]
async fn completed_work_and_review_comments_are_observations_not_release_approval() {
    let temp = fixture();
    git(
        temp.path(),
        &[
            "commit",
            "--allow-empty",
            "-m",
            &format!("work\n\nAmuxID: {TASK}"),
        ],
    );
    let mut report = local(temp.path(), &args("v0.0.58..HEAD")).await.unwrap();
    // Use a nonempty completion window independent of commit fixture timestamps.
    report.since_ms = 0;
    report.until_ms = 300;
    let task = Uuid::parse_str(TASK).unwrap();
    let other = Uuid::parse_str(OTHER).unwrap();
    let review = Uuid::new_v4();
    let older_review = Uuid::new_v4();
    let details = BTreeMap::from([
        (
            task,
            json!({"id":TASK,"title":"Feature","status":"done","done_at":100}),
        ),
        (
            other,
            json!({"id":OTHER,"title":"Local only","status":"done","done_at":200}),
        ),
        (
            older_review,
            json!({"id":older_review,"status":"done","done_at":50,"created_at":40,"labels":["task-review",format!("review-of:{TASK}")],"comments":[{"author_type":"agent","content":"Old result","created_at":55}]}),
        ),
        (
            review,
            json!({"id":review,"status":"done","done_at":250,"created_at":100,"labels":["task-review",format!("review-of:{TASK}")],"comments":[{"author_type":"agent","content":"No findings","created_at":240},{"author_type":"system","content":"Reminder","created_at":260}]}),
        ),
    ]);
    report.apply_tasks(&details);
    assert_eq!(report.tasks[&task].title.as_deref(), Some("Feature"));
    let result = report.tasks[&task].latest_review.as_ref().unwrap();
    assert_eq!(result["task_id"], review.to_string());
    assert_eq!(result["latest_comment"]["content"], "No findings");
    assert!(
        report
            .completed_not_released
            .iter()
            .any(|work| work.id == other)
    );
    assert!(
        !report
            .completed_not_released
            .iter()
            .any(|work| work.id == task || work.id == review)
    );
    assert!(
        report
            .render()
            .contains("untrailered commits may contain this work")
    );
    assert_eq!(details[&task]["status"], "done");
}

#[tokio::test]
async fn date_window_and_offline_cli_need_no_actor_or_gateway_and_write_no_state() {
    let temp = fixture();
    git(temp.path(), &["commit", "--allow-empty", "-m", "unlinked"]);
    let selected = Args {
        range: None,
        since: Some("2024-01-02".into()),
        until: Some("2024-01-03T00:00:00Z".into()),
        offline: true,
    };
    assert_eq!(
        local(temp.path(), &selected).await.unwrap().commits.len(),
        2
    );
    let excluded = Args {
        since: Some("2024-01-01".into()),
        until: Some("2024-01-02".into()),
        ..selected.clone()
    };
    assert!(
        local(temp.path(), &excluded)
            .await
            .unwrap()
            .commits
            .is_empty()
    );
    let invalid = Args {
        since: Some("yesterday-ish-invalid".into()),
        ..selected
    };
    assert!(local(temp.path(), &invalid).await.is_err());
    assert!(
        local(temp.path(), &args("--not-a-ref..HEAD"))
            .await
            .is_err()
    );
    let state = temp.path().join("absent-state");
    let before = git(temp.path(), &["status", "--porcelain=v1"]);
    let output = Command::new(env!("CARGO_BIN_EXE_amux"))
        .current_dir(temp.path())
        .args(["tasks", "--json", "report", "v0.0.58..HEAD", "--offline"])
        .env("AMUX_STATE_DIR", &state)
        .env_remove("AMUX_AGENT_ID")
        .output()
        .unwrap();
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    let report: serde_json::Value = serde_json::from_slice(&output.stdout).unwrap();
    assert_eq!(report["commits"].as_array().unwrap().len(), 1);
    assert!(report["partial"][0].as_str().unwrap().contains("Offline"));
    assert!(
        !state.exists(),
        "a read-only report must not create preferences/server/actor state"
    );
    assert_eq!(before, git(temp.path(), &["status", "--porcelain=v1"]));
    // No enrollment is also a partial report, not a hard failure.
    let output = Command::new(env!("CARGO_BIN_EXE_amux"))
        .current_dir(temp.path())
        .args(["tasks", "--json", "report", "v0.0.58..HEAD"])
        .env("AMUX_STATE_DIR", &state)
        .output()
        .unwrap();
    assert!(output.status.success());
    let report: serde_json::Value = serde_json::from_slice(&output.stdout).unwrap();
    assert!(
        report["partial"]
            .as_array()
            .unwrap()
            .iter()
            .any(|gap| gap.as_str().unwrap().contains("Gateway"))
    );
    assert!(
        report["partial"]
            .as_array()
            .unwrap()
            .iter()
            .any(|gap| gap.as_str().unwrap().contains("GitHub"))
    );
    assert!(!state.exists());
}

#[tokio::test]
async fn exact_sha_ci_uses_all_pages_and_preserves_attempt_and_conclusion() {
    let temp = fixture();
    let mut report = local(temp.path(), &args("v0.0.58..HEAD")).await.unwrap();
    let run = |id, attempt, conclusion| json!({"id":id,"run_attempt":attempt,"head_sha":report.to_sha,"name":"Native","event":"push","status":"completed","conclusion":conclusion,"html_url":"https://example.invalid/run"});
    let pages = vec![
        json!({"workflow_runs":[run(1,2,"failure")]}),
        json!({"workflow_runs":[run(2,1,"success")]}),
    ];
    report.apply_ci(&pages).unwrap();
    assert_eq!(report.ci_runs.len(), 2);
    assert_eq!(report.ci_runs[0]["attempt"], 2);
    assert_eq!(report.ci_runs[0]["conclusion"], "failure");
    assert!(
        report
            .apply_ci(&[json!({"workflow_runs":[{"head_sha":"different"}]})])
            .is_err()
    );
    let mut empty = local(temp.path(), &args("v0.0.58..HEAD")).await.unwrap();
    assert!(empty.apply_ci(&[json!({"workflow_runs":[]})]).is_err());
}

#[tokio::test]
async fn gateway_enrichment_uses_only_get_and_requests_stale_completed_tasks() {
    use axum::{
        Json, Router,
        extract::{Path as RoutePath, Query},
        routing::get,
    };
    let temp = fixture();
    git(
        temp.path(),
        &[
            "commit",
            "--allow-empty",
            "-m",
            &format!("work\n\nAmuxID: {TASK}"),
        ],
    );
    let mut report = local(temp.path(), &args("v0.0.58..HEAD")).await.unwrap();
    report.since_ms = 0;
    report.until_ms = 500;
    let review = Uuid::new_v4().to_string();
    let review_id = review.clone();
    let app = Router::new().route("/v1/projects/demo/tasks", get(move |Query(query): Query<BTreeMap<String,String>>| {
        let review = review.clone();
        async move {
            assert_eq!(query["include_stale"], "true");
            assert!(query["status"].contains("done"));
            Json(json!([
                {"id":TASK,"status":"done"},
                {"id":OTHER,"status":"done"},
                {"id":review,"status":"done","labels":["task-review",format!("review-of:{TASK}")]},
                {"id":"01a19a44-e705-743e-86f7-126db8e7b5eb","status":"done","created_at":600,"updated_at":700}
            ]))
        }
    })).route("/v1/projects/demo/tasks/{id}", get(move |RoutePath(id): RoutePath<String>, Query(query): Query<BTreeMap<String,String>>| {
        let review = review_id.clone();
        async move {
            assert!(id == TASK || id == OTHER || id == review, "out-of-window task must not need a detail read");
            assert_eq!(query["comments"], if id == review { "100" } else { "0" });
            if id == review { Json(json!({"id":id,"project_ident":"demo","title":"Review","status":"done","done_at":300,"created_at":200,"labels":["task-review",format!("review-of:{TASK}")],"comments":[{"content":"No findings","author_type":"agent","created_at":300}]})) }
            else { Json(json!({"id":id,"project_ident":"demo","title":"Work","status":"done","done_at":100,"comments":[]})) }
        }
    }));
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let gateway = amux::gateway::Gateway::new(
        &format!("http://{}", listener.local_addr().unwrap()),
        "fixture-key".into(),
    )
    .unwrap();
    let server = tokio::spawn(async move {
        axum::serve(listener, app).await.unwrap();
    });
    report.enrich_tasks(&gateway, "demo").await.unwrap();
    assert_eq!(
        report.tasks[&Uuid::parse_str(TASK).unwrap()]
            .status
            .as_deref(),
        Some("done")
    );
    assert!(
        report
            .completed_not_released
            .iter()
            .any(|work| work.id.to_string() == OTHER)
    );
    assert_eq!(
        report.tasks[&Uuid::parse_str(TASK).unwrap()]
            .latest_review
            .as_ref()
            .unwrap()["latest_comment"]["content"],
        "No findings"
    );
    server.abort();
}
