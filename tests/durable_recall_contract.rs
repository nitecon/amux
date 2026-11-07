//! Process-isolated environment: verify the actual recall subprocess contract.
#![cfg(unix)]
use std::{fs, os::unix::fs::PermissionsExt};

#[test]
fn recall_uses_durable_search_and_hints_only_at_present_capabilities() {
    let root = tempfile::tempdir().unwrap();
    let memory = root.path().join("memory");
    fs::write(&memory, "#!/bin/sh\n[ \"$1\" = search ] || exit 42\n[ \"$2\" = --limit=5 ] || exit 43\n[ \"$3\" = -- ] || exit 44\nprintf 'durable rule\\n'\n").unwrap();
    fs::set_permissions(&memory, fs::Permissions::from_mode(0o700)).unwrap();
    // SAFETY: sole test in this binary, no concurrent environment access.
    unsafe {
        std::env::set_var("AMUX_BUNDLED_DIR", root.path());
    }
    let recall = amux::mcp::durable_memories(root.path(), "- a query").unwrap();
    assert_eq!(recall, "durable rule\n");
    let hints = amux::guidance::session_hints(true);
    assert!(hints.contains("memory_search"));
    assert!(hints.contains("memory_update"));
    assert!(!hints.contains("code_symbols"));
    assert!(!hints.contains("WorkingContext"));
    assert_eq!(
        amux::guidance::terminal_text("one\n two\r\nthree"),
        "one two three"
    );
}
