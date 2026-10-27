use amux::harness::{CLAUDE_DENIES, END, Environment, Provider, RULES, START, State, merge_rules};
use amux::harness::{Choice, Choices};
use serde_json::json;
use std::{fs, path::Path, process::Command};

fn fixture() -> (tempfile::TempDir, Environment) {
    let target = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
    fs::create_dir_all(&target).unwrap();
    let temp = tempfile::tempdir_in(target).unwrap();
    let root = temp.path();
    let environment = Environment {
        home: root.into(),
        path: Default::default(),
        codex_home: root.join("codex-profile"),
        claude_home: root.join("claude-profile"),
        copilot_home: root.join("copilot-profile"),
    };
    (temp, environment)
}

#[test]
fn discovery_is_read_only_and_detects_profiles_or_path_executables() {
    let (temp, mut env) = fixture();
    assert!(
        env.reports()
            .iter()
            .all(|r| !r.detected && r.rules == State::Missing)
    );
    assert_eq!(fs::read_dir(temp.path()).unwrap().count(), 0);
    fs::create_dir(&env.claude_home).unwrap();
    assert!(env.report(Provider::Claude).detected);
    let bin = temp.path().join("bin");
    fs::create_dir(&bin).unwrap();
    let executable = bin.join(if cfg!(windows) {
        "copilot.exe"
    } else {
        "copilot"
    });
    fs::write(&executable, "fixture").unwrap();
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        fs::set_permissions(&executable, fs::Permissions::from_mode(0o700)).unwrap();
    }
    env.path = std::env::join_paths([bin]).unwrap();
    let report = env.report(Provider::Copilot);
    assert!(report.detected);
    assert_eq!(report.executable, Some(executable));
    assert!(!env.copilot_home.exists());
}

#[test]
fn managed_block_updates_are_idempotent_and_preserve_surrounding_user_text() {
    let original = "# User instructions\r\nKeep my text exactly.\r\n";
    let installed = merge_rules(original).unwrap();
    assert!(installed.starts_with(original));
    assert_eq!(merge_rules(&installed).unwrap(), installed);
    let old = format!("manual before\n{START}\nold instructions\n{END}\nmanual after\n");
    let updated = merge_rules(&old).unwrap();
    assert_eq!(
        updated,
        format!("manual before\n{START}\n{RULES}{END}\nmanual after\n")
    );
    for broken in [
        format!("{START}\nmanual"),
        format!("{END}\n{START}"),
        format!("{START}\n{END}\n{START}\n{END}"),
        format!("User mentions {START}\n{END}"),
    ] {
        assert!(merge_rules(&broken).is_err());
    }
}

#[test]
fn claude_install_preserves_settings_and_blocks_only_local_task_tools() {
    let (_temp, env) = fixture();
    fs::create_dir(&env.claude_home).unwrap();
    let rules = env.claude_home.join("CLAUDE.md");
    let settings = env.claude_home.join("settings.json");
    fs::write(&rules, "# My rules\nKeep this.\n").unwrap();
    let original = json!({"model":"user-model", "hooks":{"SessionStart":[]}, "permissions":{"allow":["Bash(git status)"],"deny":["Read(secret)","TaskCreate"]}, "env":{"USER_VALUE":"kept"}});
    fs::write(&settings, serde_json::to_string(&original).unwrap()).unwrap();
    assert_eq!(env.report(Provider::Claude).local_tasks, State::Missing);
    assert!(env.install(Provider::Claude).unwrap().current());
    let value: serde_json::Value =
        serde_json::from_str(&fs::read_to_string(&settings).unwrap()).unwrap();
    for key in ["model", "hooks", "env"] {
        assert_eq!(value[key], original[key]);
    }
    assert_eq!(
        value["permissions"]["allow"],
        original["permissions"]["allow"]
    );
    let deny = value["permissions"]["deny"].as_array().unwrap();
    assert_eq!(deny.len(), 6);
    for tool in CLAUDE_DENIES {
        assert!(deny.contains(&json!(tool)));
    }
    for tool in ["Task", "Agent", "TaskStop", "TaskOutput", "Bash"] {
        assert!(!deny.contains(&json!(tool)));
    }
    assert_eq!(
        fs::read_to_string(env.claude_home.join("CLAUDE.md.amux.bak")).unwrap(),
        "# My rules\nKeep this.\n"
    );
    let snapshot = fs::read_to_string(&settings).unwrap();
    env.install(Provider::Claude).unwrap();
    assert_eq!(fs::read_to_string(settings).unwrap(), snapshot);
    assert!(
        fs::read_to_string(rules)
            .unwrap()
            .starts_with("# My rules\nKeep this.\n")
    );
    assert!(!env.codex_home.exists());
    assert!(!env.copilot_home.exists());
}

#[test]
fn codex_uses_active_override_and_preserves_toml_comments_other_tools_and_profiles() {
    let (_temp, env) = fixture();
    fs::create_dir(&env.codex_home).unwrap();
    let normal = env.codex_home.join("AGENTS.md");
    let override_path = env.codex_home.join("AGENTS.override.md");
    fs::write(&normal, "Base instructions\n").unwrap();
    fs::write(&override_path, "Override instructions\n").unwrap();
    let config = env.codex_home.join("config.toml");
    let original = "# Keep comments\nmodel = 'user-model'\n[profiles.my-profile]\nmodel = 'other-model'\n[tools]\nview_image = true\n[tools.update_plan]\nenabled = true # inline comment\n";
    fs::write(&config, original).unwrap();
    let report = env.install(Provider::Codex).unwrap();
    assert_eq!(report.instructions, override_path);
    assert!(report.current());
    assert_eq!(fs::read_to_string(&normal).unwrap(), "Base instructions\n");
    let updated = fs::read_to_string(&config).unwrap();
    assert!(updated.starts_with("# Keep comments\nmodel = 'user-model'\n"));
    assert!(updated.contains("[profiles.my-profile]\nmodel = 'other-model'"));
    assert!(updated.contains("enabled = false # inline comment"));
    let parsed = updated.parse::<toml_edit::DocumentMut>().unwrap();
    assert_eq!(
        parsed["tools"]["update_plan"]["enabled"].as_bool(),
        Some(false)
    );
    assert_eq!(parsed["tools"]["view_image"].as_bool(), Some(true));
    env.install(Provider::Codex).unwrap();
    assert_eq!(fs::read_to_string(config).unwrap(), updated);
    fs::write(&override_path, " \n").unwrap();
    assert_eq!(env.report(Provider::Codex).instructions, normal);
}

#[test]
fn malformed_rules_or_settings_are_reported_and_not_overwritten() {
    for provider in [Provider::Claude, Provider::Codex] {
        for broken_rules in [false, true] {
            let (_temp, env) = fixture();
            let root = env.provider_home(provider);
            fs::create_dir(root).unwrap();
            let (rules, settings) = match provider {
                Provider::Claude => ("CLAUDE.md", "settings.json"),
                _ => ("AGENTS.md", "config.toml"),
            };
            let rules_text = if broken_rules {
                START
            } else {
                "Manual instructions\n"
            };
            let settings_text = if broken_rules { "" } else { "broken = [" };
            fs::write(root.join(rules), rules_text).unwrap();
            fs::write(root.join(settings), settings_text).unwrap();
            assert!(env.install(provider).is_err());
            assert!(env.report(provider).error.is_some());
            assert_eq!(fs::read_to_string(root.join(rules)).unwrap(), rules_text);
            assert_eq!(
                fs::read_to_string(root.join(settings)).unwrap(),
                settings_text
            );
        }
    }
    let (_temp, env) = fixture();
    fs::create_dir(&env.claude_home).unwrap();
    for malformed in [
        "[]",
        "null",
        r#"{"permissions":[]}"#,
        r#"{"permissions":{"deny":false}}"#,
        r#"{"permissions":{"deny":[12]}}"#,
    ] {
        fs::write(env.claude_home.join("settings.json"), malformed).unwrap();
        assert!(env.install(Provider::Claude).is_err());
        assert!(!env.claude_home.join("CLAUDE.md").exists());
    }
    fs::create_dir(&env.codex_home).unwrap();
    for malformed in [
        "tools=false",
        "[tools]\nupdate_plan=true",
        "[tools.update_plan]\nenabled='yes'",
    ] {
        fs::write(env.codex_home.join("config.toml"), malformed).unwrap();
        assert!(env.install(Provider::Codex).is_err());
        assert!(!env.codex_home.join("AGENTS.md").exists());
    }
}

#[test]
fn cli_installs_only_selected_profile_and_check_reports_copilot_enforcement_limit() {
    let (_temp, env) = fixture();
    let cli = |args: &[&str]| {
        Command::new(env!("CARGO_BIN_EXE_amux"))
            .args(args)
            .env("CODEX_HOME", &env.codex_home)
            .env("CLAUDE_CONFIG_DIR", &env.claude_home)
            .env("COPILOT_HOME", &env.copilot_home)
            .output()
            .unwrap()
    };
    let output = cli(&["harness", "check"]);
    assert!(output.status.success());
    assert!(!env.copilot_home.exists());
    let output = cli(&["harness", "install", "copilot"]);
    assert!(
        output.status.success(),
        "{}",
        String::from_utf8_lossy(&output.stderr)
    );
    let report = env.report(Provider::Copilot);
    assert!(report.current());
    assert_eq!(report.local_tasks, State::GuidanceOnly);
    assert!(report.settings.is_none());
    assert!(!env.claude_home.exists());
    assert!(!env.codex_home.exists());
    let output = cli(&["harness", "check"]);
    let reports: serde_json::Value = serde_json::from_slice(&output.stdout).unwrap();
    assert_eq!(reports[2]["local_tasks"], "GuidanceOnly");
}

#[test]
fn saved_manual_choice_never_prompts_or_edits_on_later_startups() {
    let (temp, env) = fixture();
    fs::create_dir(&env.claude_home).unwrap();
    fs::create_dir(&env.codex_home).unwrap();
    let path = temp.path().join("state/harness-preferences.json");
    let mut choices = Choices::load(&path).unwrap();
    assert!(choices.needs_prompt(&env.report(Provider::Claude)));
    choices.set(Provider::Claude, Choice::Manual);
    choices.save(&path).unwrap();
    for _ in 0..2 {
        let restored = Choices::load(&path).unwrap();
        let reports = env.maintain(&restored);
        assert!(!restored.needs_prompt(&reports[0]));
        assert!(restored.needs_prompt(&reports[1]));
        assert!(!env.claude_home.join("CLAUDE.md").exists());
        assert!(!env.claude_home.join("settings.json").exists());
        assert!(!env.codex_home.join("config.toml").exists());
        assert!(!env.copilot_home.exists());
    }
    fs::write(env.claude_home.join("CLAUDE.md"), "My manual text\n").unwrap();
    env.maintain(&Choices::load(&path).unwrap());
    assert_eq!(
        fs::read_to_string(env.claude_home.join("CLAUDE.md")).unwrap(),
        "My manual text\n"
    );
}

#[test]
fn opted_in_startup_repairs_rules_and_task_settings_but_preserves_broken_files() {
    let (temp, env) = fixture();
    fs::create_dir(&env.claude_home).unwrap();
    fs::create_dir(&env.codex_home).unwrap();
    fs::write(env.claude_home.join("CLAUDE.md"), "My instructions\n").unwrap();
    let path = temp.path().join("state/harness-preferences.json");
    let mut choices = Choices::default();
    choices.set(Provider::Claude, Choice::Automatic);
    choices.set(Provider::Codex, Choice::Manual);
    choices.save(&path).unwrap();
    let startup = || env.maintain(&Choices::load(&path).unwrap());
    assert!(startup()[0].current());
    fs::write(
        env.claude_home.join("CLAUDE.md"),
        format!("My instructions\n{START}\nold version\n{END}\nUser suffix\n"),
    )
    .unwrap();
    fs::write(
        env.claude_home.join("settings.json"),
        "{\"model\":\"keep\"}",
    )
    .unwrap();
    assert!(startup()[0].current());
    let rules = fs::read_to_string(env.claude_home.join("CLAUDE.md")).unwrap();
    assert!(rules.starts_with("My instructions\n"));
    assert!(rules.ends_with("User suffix\n"));
    assert!(rules.contains("Do not use `--json`, Python"));
    assert!(!env.codex_home.join("AGENTS.md").exists());
    let backup = fs::read(env.claude_home.join("CLAUDE.md.amux.bak")).unwrap();
    assert!(startup()[0].current());
    assert_eq!(
        fs::read(env.claude_home.join("CLAUDE.md.amux.bak")).unwrap(),
        backup
    );
    fs::write(env.claude_home.join("settings.json"), "invalid json").unwrap();
    let report = &startup()[0];
    assert!(
        report
            .error
            .as_ref()
            .unwrap()
            .contains("Automatic rule update incomplete")
    );
    assert!(!choices.needs_prompt(report));
    assert_eq!(
        fs::read_to_string(env.claude_home.join("settings.json")).unwrap(),
        "invalid json"
    );
    assert_eq!(
        fs::read_to_string(env.claude_home.join("CLAUDE.md")).unwrap(),
        rules
    );
    fs::write(&path, "invalid choices").unwrap();
    assert!(Choices::load(&path).is_err());
    assert_eq!(fs::read_to_string(path).unwrap(), "invalid choices");
}

#[test]
fn print_provides_concise_manual_rules_without_setting_up_any_files() {
    let (temp, env) = fixture();
    let before = fs::read_dir(temp.path()).unwrap().count();
    let output = Command::new(env!("CARGO_BIN_EXE_amux"))
        .args(["harness", "print", "claude"])
        .env("CLAUDE_CONFIG_DIR", &env.claude_home)
        .output()
        .unwrap();
    assert!(output.status.success());
    let text = String::from_utf8(output.stdout).unwrap();
    assert!(text.contains(&env.claude_home.join("CLAUDE.md").display().to_string()));
    assert!(text.contains("Do not use `--json`, Python, or scripts"));
    assert!(text.contains("permissions"));
    assert!(text.contains("TodoWrite"));
    assert_eq!(RULES.matches("\namux tasks ").count(), 11);
    assert!(
        !RULES
            .split("```sh")
            .nth(1)
            .unwrap()
            .split("```")
            .next()
            .unwrap()
            .contains("--json")
    );
    assert_eq!(fs::read_dir(temp.path()).unwrap().count(), before);
}
