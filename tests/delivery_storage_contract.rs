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
