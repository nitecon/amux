//! Independent CLI processes share one state directory. Creating one client's
//! transport must not exclude another session for the lifetime of an HTTP call.
use amux::dispatch::{Dispatcher, Machine};
use std::path::Path;
use uuid::Uuid;

#[test]
fn independent_dispatch_clients_can_share_the_machine_state_directory() {
    let fixtures = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
    std::fs::create_dir_all(&fixtures).unwrap();
    let state = tempfile::tempdir_in(fixtures).unwrap();
    let instance = Uuid::new_v4();
    let client = || {
        Dispatcher::new(
            "http://127.0.0.1:9",
            "fixture-only".into(),
            "same-project",
            Machine::new(instance, "a".repeat(64)).unwrap(),
            state.path().to_owned(),
        )
    };
    let _first = client().unwrap();
    let _second = client().expect("a peer session was excluded by another client's journal lock");
    let _third = client().expect("a third session was excluded by a peer client's journal lock");
}
