//! Codex startup memory never replaces the user's own developer instructions
//! or duplicates the memory hook. One test per binary: it sets CODEX_HOME.

#[test]
fn codex_context_respects_user_instructions_and_the_memory_hook() {
    let home = tempfile::tempdir().unwrap();
    // SAFETY: the only test in this binary; nothing else reads the environment.
    unsafe { std::env::set_var("CODEX_HOME", home.path()) };
    let config = home.path().join("config.toml");
    let project = std::path::Path::new(env!("CARGO_MANIFEST_DIR"));

    std::fs::write(&config, "developer_instructions = \"mine\"\n").unwrap();
    assert!(amux::mcp::codex_context_arguments(project).is_empty());
    std::fs::write(
        &config,
        "[profiles.work]\ndeveloper_instructions = \"mine\"\n",
    )
    .unwrap();
    assert!(amux::mcp::codex_context_arguments(project).is_empty());

    // Trusted project config, with nothing at the user level.
    std::fs::write(&config, "").unwrap();
    let repo = tempfile::tempdir().unwrap();
    let nested = repo.path().join("src");
    std::fs::create_dir_all(repo.path().join(".codex")).unwrap();
    std::fs::create_dir_all(&nested).unwrap();
    std::fs::write(
        repo.path().join(".codex").join("config.toml"),
        "developer_instructions = \"project rules\"\n",
    )
    .unwrap();
    assert!(amux::mcp::codex_context_arguments(&nested).is_empty());

    std::fs::write(
        &config,
        "[[hooks.UserPromptSubmit]]\ncommand = \"memory hook --agent codex\"\n",
    )
    .unwrap();
    assert!(amux::mcp::memory_hook_installed("codex"));
    assert!(amux::mcp::codex_context_arguments(project).is_empty());
}
