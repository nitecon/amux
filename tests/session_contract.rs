use amux::{
    identity::Provider,
    session::{Layout, SavedTab, SavedWorkspace, Snapshot},
};
use std::{fs, path::Path};

#[test]
fn atomic_layout_round_trip_retains_agent_metadata_without_execution_or_ownership() {
    let target = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
    fs::create_dir_all(&target).unwrap();
    let fixture = tempfile::tempdir_in(target).unwrap();
    let path = fixture.path().join("layout.json");
    let pane = |id| Layout::Pane {
        active: 0,
        tabs: vec![SavedTab {
            id,
            title: "Agent".into(),
            provider: Some(Provider::Claude),
        }],
    };
    let snapshot = Snapshot {
        version: 1,
        active: 0,
        workspaces: vec![SavedWorkspace {
            directory: fixture.path().into(),
            layout: Layout::Split {
                vertical: true,
                ratio: 0.4,
                a: Box::new(pane(1)),
                b: Box::new(pane(2)),
            },
        }],
    };
    snapshot.save(&path).unwrap();
    let restored = Snapshot::load(&path).unwrap().unwrap();
    assert_eq!(
        serde_json::to_value(&restored).unwrap(),
        serde_json::to_value(&snapshot).unwrap()
    );
    // Saving a later snapshot replaces the same file, including on Windows.
    restored.save(&path).unwrap();
    let original = fs::read(&path).unwrap();
    let bad = Snapshot {
        version: 1,
        active: 0,
        workspaces: vec![SavedWorkspace {
            directory: fixture.path().into(),
            layout: Layout::Split {
                vertical: true,
                ratio: 0.5,
                a: Box::new(pane(1)),
                b: Box::new(pane(1)),
            },
        }],
    };
    assert!(bad.save(&path).is_err());
    assert_eq!(
        fs::read(&path).unwrap(),
        original,
        "rejected state must not damage the recovery snapshot"
    );
    assert!(Snapshot::load(&path).unwrap().is_some());
}

#[test]
fn corrupt_or_unknown_version_is_not_replaced_by_loading() {
    let target = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
    fs::create_dir_all(&target).unwrap();
    let fixture = tempfile::tempdir_in(target).unwrap();
    let path = fixture.path().join("layout.json");
    for bytes in [
        b"invalid".as_slice(),
        br#"{"version":2,"active":0,"workspaces":[]}"#,
    ] {
        fs::write(&path, bytes).unwrap();
        assert!(Snapshot::load(&path).is_err());
        assert_eq!(fs::read(&path).unwrap(), bytes);
    }
}
