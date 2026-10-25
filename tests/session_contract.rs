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
            terminal_number: Some(id + 1),
            id,
            title: "Agent".into(),
            provider: Some(Provider::Claude),
            running: true,
            directory: Some(fixture.path().into()),
        }],
    };
    let snapshot = Snapshot {
        version: 1,
        active: 0,
        workspace_browser_directory: Some(fixture.path().into()),
        workspaces: vec![SavedWorkspace {
            name: None,
            init_script: Some(fixture.path().join("not-executed.sh")),
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
    let mut legacy = serde_json::to_value(&snapshot).unwrap();
    legacy
        .as_object_mut()
        .unwrap()
        .remove("workspace_browser_directory");
    let legacy: Snapshot = serde_json::from_value(legacy).unwrap();
    assert!(legacy.workspace_browser_directory.is_none());
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
        workspace_browser_directory: None,
        workspaces: vec![SavedWorkspace {
            name: None,
            init_script: None,
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

#[test]
fn legacy_tabs_reopen_and_new_snapshots_preserve_explicit_stopped_state() {
    let root = Path::new(env!("CARGO_MANIFEST_DIR"));
    let legacy = serde_json::json!({"id":7,"title":"Terminal","provider":null});
    let tab: SavedTab = serde_json::from_value(legacy).unwrap();
    assert!(tab.running);
    assert!(tab.directory.is_none());
    let stopped = SavedTab {
        running: false,
        directory: Some(root.into()),
        ..tab
    };
    let again: SavedTab = serde_json::from_value(serde_json::to_value(&stopped).unwrap()).unwrap();
    assert!(!again.running);
    assert_eq!(again.directory, stopped.directory);
}
