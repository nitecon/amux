//! Exercise the actual PTY launch boundary; no installed provider is required.
#![cfg(all(feature = "desktop", any(target_os = "linux", windows)))]
use iced_term::{
    Terminal,
    settings::{BackendSettings, Settings},
};
use std::{
    fs,
    path::{Path, PathBuf},
    time::{Duration, Instant},
};

const WINDOWS_PROJECT: &str = r"C:\Users\nitec\Documents\Unreal Projects\X";

#[test]
fn pty_path_fixture() {
    let Some(output) = std::env::var_os("AMUX_PATH_FIXTURE_OUTPUT") else {
        return;
    };
    let value = serde_json::json!({
        "directory": std::env::current_dir().unwrap(),
        "arguments": std::env::args().collect::<Vec<_>>(),
    });
    fs::write(output, serde_json::to_vec(&value).unwrap()).unwrap();
}

#[test]
fn native_pty_preserves_executable_directory_and_arguments_with_spaces() {
    let root = Path::new(env!("CARGO_MANIFEST_DIR")).join("target/test-fixtures");
    fs::create_dir_all(&root).unwrap();
    let fixture = tempfile::tempdir_in(root).unwrap();
    let directory = fixture.path().join("Unreal Projects").join("X");
    fs::create_dir_all(&directory).unwrap();
    let exe = directory.join(format!("fixture program{}", std::env::consts::EXE_SUFFIX));
    let current = std::env::current_exe().unwrap();
    if fs::hard_link(&current, &exe).is_err() {
        fs::copy(current, &exe).unwrap();
    }
    let output = fixture.path().join("child-output.json");
    let values = [
        WINDOWS_PROJECT,
        r"C:\folder with spaces\",
        "a\"quoted value",
        "",
    ];
    let mut args = vec![
        "--exact".into(),
        "pty_path_fixture".into(),
        "--nocapture".into(),
    ];
    // libtest accepts arbitrary strings as --skip arguments. The helper verifies
    // what reached argv, including spaces, quotes, empty values and trailing slash.
    for value in values {
        args.extend(["--skip".into(), value.into()]);
    }
    let terminal = Terminal::new(
        980_401,
        Settings {
            backend: BackendSettings {
                program: exe.to_str().unwrap().into(),
                args,
                env: [(
                    "AMUX_PATH_FIXTURE_OUTPUT".into(),
                    output.to_str().unwrap().into(),
                )]
                .into(),
                working_directory: Some(directory.clone()),
            },
            ..Default::default()
        },
    )
    .unwrap();
    let deadline = Instant::now() + Duration::from_secs(10);
    let value: serde_json::Value = loop {
        if let Ok(bytes) = fs::read(&output)
            && let Ok(value) = serde_json::from_slice(&bytes)
        {
            break value;
        }
        assert!(
            Instant::now() < deadline,
            "PTY child failed to receive its path arguments"
        );
        std::thread::sleep(Duration::from_millis(10));
    };
    assert_eq!(
        PathBuf::from(value["directory"].as_str().unwrap())
            .canonicalize()
            .unwrap(),
        directory.canonicalize().unwrap()
    );
    let actual: Vec<_> = value["arguments"]
        .as_array()
        .unwrap()
        .iter()
        .map(|v| v.as_str().unwrap())
        .collect();
    for (pair, expected) in actual[4..].chunks_exact(2).zip(values) {
        assert_eq!(pair, ["--skip", expected]);
    }
    assert_eq!(actual.len(), 4 + values.len() * 2);
    drop(terminal);
}
