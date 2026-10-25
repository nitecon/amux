use amux::{
    backlog,
    delegation::{Origin, Outcome, Target},
    delegation_queue::Queue,
    identity::Provider,
};
use serde_json::json;
use std::time::Duration;
use uuid::Uuid;

fn target() -> Target {
    let session = Uuid::new_v4();
    Target {
        workspace: Uuid::new_v4(),
        terminal: 1,
        session,
        agent_id: Some("Peer - Codex".into()),
        pid: 5,
        generation: "fixture".into(),
        directory: std::env::current_dir().unwrap(),
        project: "amux".into(),
        origin: Some(Origin {
            session_id: session,
            instance_id: Uuid::new_v4(),
            provider: Provider::Codex,
            os: "linux".into(),
        }),
        goal: None,
    }
}

#[test]
fn review_tasks_never_become_eligible_and_clear_old_handoffs_before_user_requeue() {
    let root = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
    let dir = tempfile::tempdir_in(root).unwrap();
    let mut queue = Queue::open(dir.path().join("review.json"), "fixture").unwrap();
    let target = target();
    let id = Uuid::new_v4();
    queue
        .begin_backlog(backlog::Receipt {
            task: id,
            revision: 10,
            target: target.clone(),
            outcome: Outcome::Submitting,
        })
        .unwrap();
    queue.finish_backlog(id, 10, Outcome::Uncertain).unwrap();
    let mut task = json!({"id":id,"project_ident":"amux","status":"user_review","updated_at":11});
    for _ in 0..3 {
        assert!(!backlog::eligible(&task, &target));
        queue.reconcile_backlog("amux", &[task.clone()]).unwrap();
        assert!(queue.journal.backlog.is_empty());
    }
    task["status"] = json!("todo");
    task["updated_at"] = json!(12);
    assert!(backlog::eligible(&task, &target));
}

#[test]
fn workspace_activity_and_backlog_share_the_exact_thirty_second_boundary() {
    assert!(amux::session::output_active(Some(Duration::from_millis(
        29_999
    ))));
    assert!(!amux::session::output_active(Some(Duration::from_secs(30))));
    assert!(!amux::session::output_active(Some(Duration::from_secs(31))));
    assert!(!amux::session::output_active(None));
    assert_eq!(amux::session::IDLE_AFTER, Duration::from_secs(30));
}

#[test]
fn queue_only_work_is_eligible_but_other_owners_goals_and_targets_are_not() {
    let target = target();
    let mut task = json!({"id":Uuid::new_v4(), "project_ident":"amux", "status":"todo", "routing":{"mode":"unassigned"}});
    assert!(backlog::eligible(&task, &target));
    task["owner_agent_id"] = json!("Other peer");
    assert!(!backlog::eligible(&task, &target));
    task["owner_agent_id"] = json!("Peer - Codex");
    assert!(backlog::eligible(&task, &target));
    let mut other = target.origin.clone().unwrap();
    other.session_id = Uuid::new_v4();
    task["owner_origin"] = json!(other);
    assert!(
        !backlog::eligible(&task, &target),
        "exact owner beats matching legacy name"
    );
    task["owner_origin"] = json!(target.origin);
    assert!(backlog::eligible(&task, &target));
    task["status"] = json!("in_progress");
    assert!(!backlog::eligible(&task, &target));
    task["status"] = json!("todo");
    task["goal"] = json!("different goal");
    assert!(!backlog::eligible(&task, &target));
    task["goal"] = json!(null);
    task["routing"] = json!({"mode":"session", "target":{"origin":other,"generation":target.executor_generation()}});
    assert!(!backlog::eligible(&task, &target));
    task["routing"] = json!({"mode":"unassigned"});
    task["project_ident"] = json!("other-repository");
    assert!(!backlog::eligible(&task, &target));
}

#[test]
fn durable_backlog_attempts_survive_restart_without_reclaim_or_input_replay() {
    let root = std::path::Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
    std::fs::create_dir_all(&root).unwrap();
    let dir = tempfile::tempdir_in(root).unwrap();
    let path = dir.path().join("backlog.json");
    let mut queue = Queue::open(path.clone(), "fixture").unwrap();
    let receipt = backlog::Receipt {
        task: Uuid::new_v4(),
        revision: 10,
        target: target(),
        outcome: Outcome::Submitting,
    };
    queue.begin_backlog(receipt.clone()).unwrap();
    assert!(queue.begin_backlog(receipt.clone()).is_err());
    let mut restored = Queue::open(path, "fixture").unwrap();
    assert_eq!(restored.journal.backlog[0].outcome, Outcome::Uncertain);
    restored
        .reconcile_backlog(
            "amux",
            &[json!({"id":receipt.task,"status":"todo","updated_at":11})],
        )
        .unwrap();
    assert_eq!(
        restored.journal.backlog.len(),
        1,
        "a later list snapshot cannot authorize retrying an unknown claim or input"
    );
    queue
        .finish_backlog(receipt.task, receipt.revision, Outcome::Queued)
        .unwrap();
    queue
        .reconcile_backlog(
            "amux",
            &[json!({"id":receipt.task,"status":"todo","updated_at":11})],
        )
        .unwrap();
    assert!(
        queue.journal.backlog.is_empty(),
        "explicit newer requeue allows a new review"
    );
    queue.begin_backlog(receipt.clone()).unwrap();
    queue
        .reconcile_backlog("amux", &[json!({"id":receipt.task,"status":"done"})])
        .unwrap();
    assert_eq!(
        queue.journal.backlog.len(),
        1,
        "a callback still owns the pending claim fence"
    );
    queue
        .finish_backlog(receipt.task, receipt.revision, Outcome::Submitted)
        .unwrap();
    queue
        .reconcile_backlog("amux", &[json!({"id":receipt.task,"status":"done"})])
        .unwrap();
    assert!(queue.journal.backlog.is_empty());
}
