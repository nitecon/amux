use amux::delegation::{Origin, Target};
use amux::delegation_actor::{self, Actor, ActorTarget};
use amux::identity::Provider;
#[cfg(target_os = "linux")]
use serde_json::Value;
use serde_json::json;
use uuid::Uuid;

const INSTANCE: &str = "00112233-4455-4677-8899-aabbccddeeff";

fn now() -> i64 {
    std::time::SystemTime::now()
        .duration_since(std::time::UNIX_EPOCH)
        .unwrap()
        .as_millis() as i64
}

fn origin(session: &str, instance: &str, provider: Provider, os: &str) -> Origin {
    Origin {
        session_id: session.parse().unwrap(),
        instance_id: instance.parse().unwrap(),
        provider,
        os: os.into(),
    }
}

fn v1(
    session: &str,
    instance: &str,
    provider: Provider,
    os: &str,
    native: &str,
    generation: &[&str],
) -> Actor {
    Actor {
        version: 1,
        origin: origin(session, instance, provider, os),
        provider_session_id: native.into(),
        executor_generation: generation.iter().map(|s| s.to_string()).collect(),
        base_id: None,
        session_slot: None,
        actor_id: None,
        goal: None,
    }
}

fn v2(session: &str, provider: Provider, os: &str, native: &str, base: &str, slot: u32) -> Actor {
    Actor {
        version: 2,
        origin: origin(session, INSTANCE, provider, os),
        provider_session_id: native.into(),
        executor_generation: Vec::new(),
        base_id: Some(base.into()),
        session_slot: Some(slot),
        actor_id: Some(format!("{base}-{slot}")),
        goal: None,
    }
}

/// A fixture registration for a native ID under the real v2 formulas.
fn registered(native: &str, provider: Provider, slot: u32) -> Actor {
    let namespace: Uuid = INSTANCE.parse().unwrap();
    let base = Uuid::new_v5(
        &namespace,
        json!([
            "agent-tools-actor-v2",
            "base",
            std::env::consts::OS,
            "/work/repo",
            "github.com/org/repo",
            provider
        ])
        .to_string()
        .as_bytes(),
    );
    let session = Uuid::new_v5(
        &base,
        json!(["agent-tools-actor-v2", native])
            .to_string()
            .as_bytes(),
    );
    v2(
        &session.to_string(),
        provider,
        std::env::consts::OS,
        native,
        &base.to_string(),
        slot,
    )
}

/// Vectors copied from the CMUX gateway fixtures (actor-origin / actor-registration).
#[test]
fn cmux_contract_vectors_and_forged_identity() {
    let gen_linux = [
        "linux-proc-v1",
        "11111111-2222-4333-8444-555555555555",
        "1234",
        "987654321",
    ];
    let valid = [
        v1(
            "6609e22a-f740-5d8e-ad37-7751cf2d2f95",
            INSTANCE,
            Provider::Codex,
            "linux",
            "01a117a5-2240-7e00-be27-d42910e71473",
            &gen_linux,
        ),
        v1(
            "d6caec74-bcd8-5559-8f62-f18c149773f3",
            INSTANCE,
            Provider::Codex,
            "linux",
            "01a11435-6625-7070-ae4d-668abecf45df",
            &gen_linux,
        ),
        v1(
            "8ded97e2-ca79-54cb-87ee-b3dce7415ac1",
            "00112233-4455-4677-8899-aabbccddeefe",
            Provider::Codex,
            "linux",
            "01a117a5-2240-7e00-be27-d42910e71473",
            &gen_linux,
        ),
        v1(
            "874785c3-a5b9-56f1-a688-1bb4cb9f91ae",
            INSTANCE,
            Provider::Claude,
            "windows",
            "opaque-native-id",
            &["windows-process-v1", "5678", "134042112001234567"],
        ),
        v1(
            "b8c116df-e470-5e0b-bb41-63ec775f00de",
            INSTANCE,
            Provider::Codex,
            "macos",
            "01a117a5-2240-7e00-be27-d42910e71473",
            &["macos-proc-v1", "5678", "1791403200123456"],
        ),
        v1(
            "6609e22a-f740-5d8e-ad37-7751cf2d2f95",
            INSTANCE,
            Provider::Codex,
            "linux",
            "01A117A5-2240-7E00-BE27-D42910E71473",
            &gen_linux,
        ),
        v2(
            "77b87f84-242a-5f01-87a6-d58dd18448d8",
            Provider::Codex,
            "linux",
            "01a117a5-2240-7e00-be27-d42910e71473",
            "e443f9c2-dd60-5411-8ed4-0ad39c447bed",
            1,
        ),
        v2(
            "18d27bac-588e-586f-9ecb-aa0535ff550c",
            Provider::Codex,
            "linux",
            "01a11435-6625-7070-ae4d-668abecf45df",
            "e443f9c2-dd60-5411-8ed4-0ad39c447bed",
            2,
        ),
        v2(
            "77b87f84-242a-5f01-87a6-d58dd18448d8",
            Provider::Codex,
            "linux",
            "01a117a5-2240-7e00-be27-d42910e71473",
            "e443f9c2-dd60-5411-8ed4-0ad39c447bed",
            7,
        ),
        v2(
            "327a6590-7b27-5ac7-b7eb-dd1c0c16b8bd",
            Provider::Claude,
            "windows",
            "opaque-native-id",
            "53de1997-ea76-54c9-a6b9-1a0a1af5272a",
            1,
        ),
        v2(
            "77b87f84-242a-5f01-87a6-d58dd18448d8",
            Provider::Codex,
            "linux",
            "01A117A5-2240-7E00-BE27-D42910E71473",
            "e443f9c2-dd60-5411-8ed4-0ad39c447bed",
            1,
        ),
        v2(
            "0a8e38b6-b370-5098-93be-802b9f95c94a",
            Provider::Claude,
            "linux",
            "01a117a5-2240-7e00-be27-d42910e71473",
            "ba673687-40d7-5566-b79c-663ce9191343",
            1,
        ),
    ];
    for actor in &valid {
        actor.validate().unwrap();
        let mut forged = actor.clone();
        forged.origin.session_id = Uuid::new_v4();
        assert!(forged.validate().is_err());
    }
    // The v2 base formula over project path, Git identity and provider.
    let base = Uuid::new_v5(
        &INSTANCE.parse().unwrap(),
        br#"["agent-tools-actor-v2","base","linux","/work/repo","github.com/org/repo","codex"]"#,
    );
    assert_eq!(base.to_string(), "e443f9c2-dd60-5411-8ed4-0ad39c447bed");
    let good = valid[6].clone();
    let mut bad = good.clone();
    bad.session_slot = Some(0);
    bad.actor_id = Some(format!("{}-0", good.base_id.as_deref().unwrap()));
    assert!(bad.validate().is_err());
    bad = good.clone();
    bad.actor_id = Some(format!("{}-2", good.base_id.as_deref().unwrap()));
    assert!(bad.validate().is_err());
    bad = good.clone();
    bad.executor_generation = vec!["legacy-runtime".into()];
    assert!(bad.validate().is_err());
    bad = good.clone();
    bad.base_id = Some(good.base_id.as_deref().unwrap().to_uppercase());
    assert!(bad.validate().is_err());
    bad = good.clone();
    bad.provider_session_id = "has spaces".into();
    assert!(bad.validate().is_err());
    bad = good.clone();
    bad.goal = Some(" padded ".into());
    assert!(bad.validate().is_err());
    bad.goal = Some("padded".into());
    bad.validate().unwrap();
    bad = valid[0].clone();
    bad.base_id = good.base_id.clone();
    assert!(bad.validate().is_err());
    // Versionless announcements are the original v1 contract.
    let legacy: Actor = serde_json::from_value(json!({
        "origin": {"session_id": "6609e22a-f740-5d8e-ad37-7751cf2d2f95", "instance_id": INSTANCE, "provider": "codex", "os": "linux"},
        "provider_session_id": "01a117a5-2240-7e00-be27-d42910e71473",
        "executor_generation": gen_linux,
    }))
    .unwrap();
    assert_eq!(legacy.version, 1);
    legacy.validate().unwrap();
}

fn target(pid: u32, generation: String, directory: &std::path::Path) -> Target {
    Target {
        workspace: Uuid::new_v4(),
        terminal: 1,
        session: Uuid::new_v4(),
        pid,
        generation,
        directory: directory.to_path_buf(),
        project: "github.com/org/repo".into(),
        origin: None,
        goal: None,
    }
}

#[test]
fn publish_is_bounded_private_and_fresh() {
    let scratch = tempfile::tempdir_in(env!("CARGO_TARGET_TMPDIR")).unwrap();
    let directory = scratch.path().join("delegation");
    let instance = Uuid::new_v4();
    let own = ActorTarget {
        target: target(
            std::process::id(),
            amux::identity::process_generation(std::process::id()).unwrap(),
            scratch.path(),
        ),
        provider: Provider::Claude,
    };
    let many = vec![own.clone(); 65];
    assert!(delegation_actor::publish(&directory, instance, &many, now()).is_err());
    assert!(
        delegation_actor::publish(&directory, Uuid::nil(), std::slice::from_ref(&own), now())
            .is_err()
    );
    delegation_actor::publish(&directory, instance, std::slice::from_ref(&own), now()).unwrap();
    #[cfg(unix)]
    {
        use std::os::unix::fs::PermissionsExt;
        let mode = |p: &std::path::Path| std::fs::metadata(p).unwrap().permissions().mode() & 0o777;
        assert_eq!(mode(&directory), 0o700);
        assert_eq!(mode(&directory.join("context.json")), 0o600);
    }
    let peers = delegation_actor::peers(&directory, now()).unwrap();
    assert_eq!(peers["peers"][0]["binding_state"], "unbound");
    assert!(peers["peers"][0].get("session").is_none());
    assert!(
        peers
            .to_string()
            .find(&own.target.session.to_string())
            .is_none()
    );
    assert!(delegation_actor::peers(&directory, now() + 6_000).is_err());
    // The test process is not inside the published terminal's provider executor.
    assert!(
        delegation_actor::announce(
            &directory,
            registered("fixture-native", Provider::Claude, 1),
            now()
        )
        .is_err()
    );
    assert!(
        delegation_actor::bindings(&directory, instance, &[own], now())
            .unwrap()
            .is_empty()
    );
}

/// Child mode: announce from inside a fixture executor, printing one result line.
#[test]
fn actor_child() {
    let Some(directory) = std::env::var_os("AMUX_ACTOR_TEST_DIR") else {
        return;
    };
    let actor: Actor =
        serde_json::from_str(&std::env::var("AMUX_ACTOR_TEST_JSON").unwrap()).unwrap();
    let directory = std::path::PathBuf::from(directory);
    let result = match std::env::var("AMUX_ACTOR_TEST_MODE").as_deref() {
        Ok("context") => delegation_actor::context(&directory, now()),
        // The fixture executor's real PID generation, as agent-tools would derive it.
        #[cfg(target_os = "linux")]
        Ok("v1") => {
            let parent = std::os::unix::process::parent_id();
            let generation = amux::identity::process_generation(parent).unwrap();
            let actor = process::v1_for(parent, &generation, &actor.provider_session_id);
            delegation_actor::announce(&directory, actor, now())
        }
        _ => delegation_actor::announce(&directory, actor, now()),
    };
    match result {
        Ok(value) => println!("RESULT {value}"),
        Err(error) => println!("ERROR {error:#}"),
    }
}

#[cfg(target_os = "linux")]
mod process {
    use super::*;
    use std::io::Write;
    use std::process::{Command, Stdio};

    /// One caller invocation: provider env value, actor and mode.
    pub struct Step {
        pub native: String,
        pub actor: Actor,
        pub mode: &'static str,
    }

    pub struct Run {
        pub directory: std::path::PathBuf,
        pub instance: Uuid,
        pub target: ActorTarget,
        pub outputs: Vec<Result<Value, String>>,
        _scratch: tempfile::TempDir,
    }

    /// A copied shell image named after a provider; it is the fixture terminal application.
    fn fixture(scratch: &std::path::Path, name: &str) -> std::path::PathBuf {
        let path = scratch.join(name);
        std::fs::copy("/bin/sh", &path).unwrap();
        path
    }

    /// Spawn the fixture executor, publish it as `published` provider (optionally with a
    /// stale generation), then run every step as a child of it (optionally through `inner`).
    pub fn run(published: Provider, stale: bool, inner: Option<&str>, steps: &[Step]) -> Run {
        let scratch = tempfile::tempdir_in(env!("CARGO_TARGET_TMPDIR")).unwrap();
        let directory = scratch.path().join("delegation");
        let outer = fixture(scratch.path(), "claude");
        let binary = std::env::current_exe().unwrap();
        let call = r#""$AMUX_ACTOR_TEST_BIN" --exact actor_child --nocapture --test-threads=1"#;
        let call = match inner {
            Some(name) => format!(
                r#""{}" -c '{call}; exit 0'"#,
                fixture(scratch.path(), name).display()
            ),
            None => call.to_owned(),
        };
        let mut script = String::from("read go\n");
        for index in 0..steps.len() {
            script.push_str(&format!(
                "CLAUDE_CODE_SESSION_ID=\"$N{index}\" AMUX_ACTOR_TEST_JSON=\"$J{index}\" AMUX_ACTOR_TEST_MODE=\"$M{index}\" {call}\n"
            ));
        }
        script.push_str("exit 0\n");
        let mut command = Command::new(&outer);
        command
            .arg("-c")
            .arg(&script)
            .env_remove("CLAUDE_CODE_SESSION_ID")
            .env_remove("CODEX_THREAD_ID")
            .env_remove("CODEX_SESSION_ID")
            .env("AMUX_ACTOR_TEST_BIN", &binary)
            .env("AMUX_ACTOR_TEST_DIR", &directory)
            .stdin(Stdio::piped())
            .stdout(Stdio::piped());
        for (index, step) in steps.iter().enumerate() {
            command
                .env(format!("N{index}"), &step.native)
                .env(
                    format!("J{index}"),
                    serde_json::to_string(&step.actor).unwrap(),
                )
                .env(format!("M{index}"), step.mode);
        }
        // A parallel test's fork may briefly hold the freshly copied image open for writing.
        let mut child = (0..50)
            .find_map(|_| match command.spawn() {
                Err(e) if e.kind() == std::io::ErrorKind::ExecutableFileBusy => {
                    std::thread::sleep(std::time::Duration::from_millis(20));
                    None
                }
                result => Some(result.unwrap()),
            })
            .unwrap();
        let pid = child.id();
        let mut generation = amux::identity::process_generation(pid).unwrap();
        if stale {
            generation.push('9');
        }
        let target = ActorTarget {
            target: target(pid, generation, scratch.path()),
            provider: published,
        };
        let instance = Uuid::new_v4();
        delegation_actor::publish(&directory, instance, std::slice::from_ref(&target), now())
            .unwrap();
        child.stdin.take().unwrap().write_all(b"go\n").unwrap();
        let output = child.wait_with_output().unwrap();
        let outputs = String::from_utf8(output.stdout)
            .unwrap()
            .lines()
            // The harness prefixes child output with its own test line.
            .filter_map(|line| {
                if let Some((_, value)) = line.split_once("RESULT ") {
                    Some(Ok(serde_json::from_str(value).unwrap()))
                } else {
                    line.split_once("ERROR ").map(|(_, e)| Err(e.to_owned()))
                }
            })
            .collect();
        Run {
            directory,
            instance,
            target,
            outputs,
            _scratch: scratch,
        }
    }

    /// The v1 executor generation agent-tools derives for a Linux process.
    pub fn v1_for(pid: u32, generation: &str, native: &str) -> Actor {
        let rest = generation.strip_prefix("linux:").unwrap();
        let (boot, ticks) = rest.split_once(':').unwrap();
        let executor = vec![
            "linux-proc-v1".to_owned(),
            boot.to_owned(),
            pid.to_string(),
            ticks.to_owned(),
        ];
        let namespace: Uuid = INSTANCE.parse().unwrap();
        let name = json!([
            "agent-tools-actor-v1",
            "linux",
            Provider::Claude,
            native,
            executor
        ])
        .to_string();
        let mut actor = v1(
            &Uuid::new_v5(&namespace, name.as_bytes()).to_string(),
            INSTANCE,
            Provider::Claude,
            "linux",
            native,
            &[],
        );
        actor.executor_generation = executor;
        actor
    }
}

#[cfg(target_os = "linux")]
#[test]
fn announce_binds_exact_caller_terminal() {
    use process::*;
    let native = "11111111-aaaa-4bbb-8ccc-000000000001";
    let mut actor = registered(native, Provider::Claude, 1);
    actor.goal = Some("parity".into());
    let run = run(
        Provider::Claude,
        false,
        None,
        &[
            Step {
                native: "11111111-aaaa-4bbb-8ccc-000000000009".into(),
                actor: actor.clone(),
                mode: "announce",
            },
            Step {
                native: native.into(),
                actor: actor.clone(),
                mode: "announce",
            },
            Step {
                native: native.into(),
                actor: actor.clone(),
                mode: "context",
            },
        ],
    );
    assert!(
        run.outputs[0]
            .as_ref()
            .unwrap_err()
            .contains("does not match")
    );
    let reply = run.outputs[1].as_ref().unwrap();
    assert_eq!(reply["binding_state"], "bound");
    assert_eq!(
        reply["origin"]["session_id"],
        actor.origin.session_id.to_string()
    );
    assert_eq!(reply["actor_id"], actor.actor_id.clone().unwrap());
    assert_eq!(reply["provider_session_id"], native);
    assert_eq!(reply["goal"], "parity");
    assert_ne!(
        reply["origin"]["session_id"],
        run.target.target.session.to_string()
    );
    let context = run.outputs[2].as_ref().unwrap();
    assert_eq!(context["binding_state"], "bound");
    assert_eq!(context["session_slot"], 1);
    let path = run
        .directory
        .join("actors")
        .join(format!("{}.json", actor.origin.session_id));
    use std::os::unix::fs::PermissionsExt;
    assert_eq!(
        std::fs::metadata(&path).unwrap().permissions().mode() & 0o777,
        0o600
    );
    // The fixture executor has exited: its generation can never be replayed onto a new PID.
    assert!(
        delegation_actor::bindings(
            &run.directory,
            run.instance,
            std::slice::from_ref(&run.target),
            now()
        )
        .unwrap()
        .is_empty()
    );
    let mut replacement = run.target.clone();
    replacement.target.pid = std::process::id();
    replacement.target.generation = amux::identity::process_generation(std::process::id()).unwrap();
    assert!(
        delegation_actor::bindings(&run.directory, run.instance, &[replacement], now())
            .unwrap()
            .is_empty()
    );
}

/// Bind while the executor is alive, keyed by delivery session, never by native conversation.
#[cfg(target_os = "linux")]
#[test]
fn bindings_while_live_and_wrong_instance() {
    let scratch = tempfile::tempdir_in(env!("CARGO_TARGET_TMPDIR")).unwrap();
    let directory = scratch.path().join("delegation");
    let own = ActorTarget {
        target: target(
            std::process::id(),
            amux::identity::process_generation(std::process::id()).unwrap(),
            scratch.path(),
        ),
        provider: Provider::Claude,
    };
    let instance = Uuid::new_v4();
    let actor = registered("fixture-native-live", Provider::Claude, 3);
    std::fs::create_dir_all(directory.join("actors")).unwrap();
    let record = |instance: Uuid, actor: &Actor| json!({"actor": actor, "target": own.target, "provider": "claude", "instance": instance, "announced_at": now()});
    let write = |actor: &Actor, value: Value| {
        std::fs::write(
            directory
                .join("actors")
                .join(format!("{}.json", actor.origin.session_id)),
            value.to_string(),
        )
        .unwrap();
    };
    write(&actor, record(Uuid::new_v4(), &actor));
    assert!(
        delegation_actor::bindings(&directory, instance, std::slice::from_ref(&own), now())
            .unwrap()
            .is_empty()
    );
    write(&actor, record(instance, &actor));
    let bound = delegation_actor::bindings(&directory, instance, std::slice::from_ref(&own), now())
        .unwrap();
    assert_eq!(bound, vec![(own.target.session, actor.clone())]);
    let mut codex = own.clone();
    codex.provider = Provider::Codex;
    assert!(
        delegation_actor::bindings(&directory, instance, &[codex], now())
            .unwrap()
            .is_empty()
    );
    let mut forged = registered("fixture-native-other", Provider::Claude, 4);
    forged.actor_id = Some("forged".into());
    write(&forged, record(instance, &forged));
    assert_eq!(
        delegation_actor::bindings(&directory, instance, std::slice::from_ref(&own), now())
            .unwrap()
            .len(),
        1
    );
    let other = registered("fixture-native-other", Provider::Claude, 4);
    write(&other, record(instance, &other));
    assert!(
        delegation_actor::bindings(&directory, instance, &[own], now())
            .unwrap()
            .is_empty()
    );
}

#[cfg(target_os = "linux")]
#[test]
fn same_process_different_conversations_are_ambiguous() {
    use process::*;
    let first = "22222222-aaaa-4bbb-8ccc-000000000001";
    let second = "22222222-aaaa-4bbb-8ccc-000000000002";
    // A v1 actor with a wrong executor generation is rejected; the exact v1 generation binds;
    // a second native conversation in the same process makes the attachment ambiguous.
    let run = run(
        Provider::Claude,
        false,
        None,
        &[
            Step {
                native: first.into(),
                actor: v1_for(1, "linux:11111111-2222-4333-8444-555555555555:1", first),
                mode: "announce",
            },
            Step {
                native: first.into(),
                actor: v1_for(1, "linux:11111111-2222-4333-8444-555555555555:1", first),
                mode: "v1",
            },
            Step {
                native: second.into(),
                actor: registered(second, Provider::Claude, 2),
                mode: "announce",
            },
            Step {
                native: second.into(),
                actor: registered(second, Provider::Claude, 2),
                mode: "context",
            },
        ],
    );
    assert!(
        run.outputs[0]
            .as_ref()
            .unwrap_err()
            .contains("executor generation")
    );
    assert_eq!(run.outputs[1].as_ref().unwrap()["binding_state"], "bound");
    assert_eq!(
        run.outputs[2].as_ref().unwrap()["binding_state"],
        "ambiguous"
    );
    assert_eq!(
        run.outputs[3].as_ref().unwrap()["binding_state"],
        "ambiguous"
    );
    assert!(run.outputs[3].as_ref().unwrap().get("actor_id").is_none());
}

#[cfg(target_os = "linux")]
#[test]
fn stale_generation_nested_and_wrong_provider_fail() {
    use process::*;
    let native = "33333333-aaaa-4bbb-8ccc-000000000001";
    let step = || Step {
        native: native.into(),
        actor: registered(native, Provider::Claude, 1),
        mode: "announce",
    };
    let stale = run(Provider::Claude, true, None, &[step()]);
    assert!(stale.outputs[0].is_err());
    let nested = run(Provider::Claude, false, Some("codex"), &[step()]);
    assert!(nested.outputs[0].as_ref().unwrap_err().contains("nested"));
    let wrong = run(Provider::Codex, false, None, &[step()]);
    assert!(wrong.outputs[0].is_err());
    assert_eq!(
        stale.outputs.len() + nested.outputs.len() + wrong.outputs.len(),
        3
    );
}
