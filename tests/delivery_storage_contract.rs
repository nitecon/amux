//! Storage failure checks only; serialized fixtures do not prove live recipient identity.
use amux::{
    delivery::{DeliveryCause, DeliveryLedger},
    identity::{AgentIdentity, Provider},
    recipient::RecipientAddress,
};
use serde_json::json;
use std::{fs, path::Path};
use uuid::Uuid;

fn fixture() -> tempfile::TempDir {
    let target = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
    fs::create_dir_all(&target).unwrap();
    tempfile::tempdir_in(target).unwrap()
}

fn address(root: &Path) -> RecipientAddress {
    serde_json::from_value(json!({
        "terminal_id":1,
        "identity":AgentIdentity::new(Provider::Codex, root).unwrap(),
        "launcher":{"pid":1,"generation":"storage fixture launcher"},
        "agent":{"pid":2,"generation":"storage fixture agent"}
    }))
    .unwrap()
}

#[test]
fn damaged_or_oversized_ledger_is_preserved_and_never_reset() {
    let root = fixture();
    let path = root.path().join("delivery.json");
    for bytes in [
        b"broken JSON".to_vec(),
        br#"{"version":2,"records":[]}"#.to_vec(),
        vec![b' '; 2 * 1024 * 1024 + 1],
    ] {
        fs::write(&path, &bytes).unwrap();
        assert!(DeliveryLedger::open(&path).is_err());
        assert_eq!(
            fs::read(&path).unwrap(),
            bytes,
            "deduplication history must not be silently replaced"
        );
    }
}

#[test]
fn failed_persistence_disables_further_delivery_mutations() {
    let root = fixture();
    let path = root.path().join("delivery.json");
    let recipient = address(root.path());
    let mut ledger = DeliveryLedger::open(&path).unwrap();
    ledger
        .queue(
            Uuid::new_v4(),
            recipient.clone(),
            DeliveryCause::RecoveryQuery,
            100,
            100,
        )
        .unwrap();
    let retained = root.path().join("retained.json");
    fs::rename(&path, &retained).unwrap();
    let original = fs::read(&retained).unwrap();
    fs::create_dir(&path).unwrap();
    assert!(
        ledger
            .queue(
                Uuid::new_v4(),
                recipient.clone(),
                DeliveryCause::RecoveryQuery,
                100,
                100
            )
            .is_err()
    );
    fs::remove_dir(&path).unwrap();
    let error = ledger
        .queue(
            Uuid::new_v4(),
            recipient,
            DeliveryCause::RecoveryQuery,
            100,
            100,
        )
        .unwrap_err();
    assert!(
        error.to_string().contains("disabled for this invocation"),
        "storage recovery alone must not reenable writes after uncertain persistence"
    );
    assert_eq!(fs::read(retained).unwrap(), original);
    assert!(
        !path.exists(),
        "poisoned ledger must not manufacture replacement history"
    );
}

/// An address whose fences are this test process (live) unless overridden.
#[cfg(any(target_os = "linux", windows))]
fn fenced(root: &Path, launcher: (u32, &str), agent: (u32, &str)) -> serde_json::Value {
    json!({
        "terminal_id":1,
        "identity":AgentIdentity::new(Provider::Codex, root).unwrap(),
        "launcher":{"pid":launcher.0,"generation":launcher.1},
        "agent":{"pid":agent.0,"generation":agent.1}
    })
}

#[cfg(any(target_os = "linux", windows))]
fn stored(recipient: &serde_json::Value, state: &str) -> serde_json::Value {
    json!({
        "id":Uuid::new_v4(),
        "task_id":Uuid::new_v4(),
        "recipient":recipient,
        "cause":"RecoveryQuery",
        "observed_at_ms":100,
        "state":state
    })
}

/// No process has this PID on Linux or Windows.
#[cfg(any(target_os = "linux", windows))]
const ABSENT: u32 = 0x7fff_fff0;

#[cfg(any(target_os = "linux", windows))]
#[test]
fn a_full_ledger_reclaims_only_provably_gone_recipient_generations() {
    let root = fixture();
    let path = root.path().join("delivery.json");
    let pid = std::process::id();
    let current = amux::identity::process_generation(pid).unwrap();
    let live = fenced(root.path(), (pid, &current), (pid, &current));
    let mut records: Vec<_> = ["Pending", "Uncertain", "Enqueued", "Retired"]
        .iter()
        .map(|state| stored(&live, state))
        .collect();
    // A well-formed native generation that is not this process's: an earlier start.
    let earlier = format!("{}:1", current.rsplit_once(':').unwrap().0);
    // Absent agent PID, or this PID now carrying another generation.
    let absent = fenced(root.path(), (pid, &current), (ABSENT, &earlier));
    let reused = fenced(root.path(), (pid, &earlier), (pid, &current));
    while records.len() < 512 {
        let gone = if records.len() % 2 == 0 {
            &absent
        } else {
            &reused
        };
        records.push(stored(gone, "Uncertain"));
    }
    fs::write(
        &path,
        serde_json::to_vec(&json!({"version":1,"records":records})).unwrap(),
    )
    .unwrap();
    let mut ledger = DeliveryLedger::open(&path).unwrap();
    let recipient: RecipientAddress = serde_json::from_value(live).unwrap();
    let task = Uuid::new_v4();
    ledger
        .queue(
            task,
            recipient.clone(),
            DeliveryCause::RecoveryQuery,
            100,
            100,
        )
        .unwrap();
    let kept: Vec<_> = ledger.records().iter().map(|r| (r.id, r.state)).collect();
    let mut expected: Vec<_> = records[..4]
        .iter()
        .map(|r| serde_json::from_value::<amux::delivery::DeliveryRecord>(r.clone()).unwrap())
        .map(|r| (r.id, r.state))
        .collect();
    // Cold open retires stale pending input; it is still kept as a fence.
    expected[0].1 = amux::delivery::DeliveryState::Retired;
    assert_eq!(kept[..4], expected[..], "every live-recipient fence kept");
    assert_eq!(kept.len(), 5);
    assert_eq!(ledger.records()[4].task_id, task);
    drop(ledger);
    assert_eq!(
        DeliveryLedger::open(&path).unwrap().records().len(),
        5,
        "durable"
    );
}

#[cfg(any(target_os = "linux", windows))]
#[test]
fn a_ledger_full_of_live_recipient_fences_refuses_new_reminders() {
    let root = fixture();
    let path = root.path().join("delivery.json");
    let pid = std::process::id();
    let current = amux::identity::process_generation(pid).unwrap();
    let live = fenced(root.path(), (pid, &current), (pid, &current));
    let records: Vec<_> = (0..512).map(|_| stored(&live, "Uncertain")).collect();
    fs::write(
        &path,
        serde_json::to_vec(&json!({"version":1,"records":records})).unwrap(),
    )
    .unwrap();
    let original = fs::read(&path).unwrap();
    let mut ledger = DeliveryLedger::open(&path).unwrap();
    let error = ledger
        .queue(
            Uuid::new_v4(),
            serde_json::from_value(live).unwrap(),
            DeliveryCause::RecoveryQuery,
            100,
            100,
        )
        .unwrap_err();
    assert!(format!("{error:#}").contains("full"), "{error:#}");
    assert_eq!(ledger.records().len(), 512);
    assert_eq!(fs::read(&path).unwrap(), original, "no fence removed");
}
