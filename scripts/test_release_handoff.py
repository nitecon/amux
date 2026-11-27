"""Install a native amux release using the updater shipped in v0.0.82."""

import argparse
from functools import partial
import hashlib
from http.server import SimpleHTTPRequestHandler, ThreadingHTTPServer
import json
import os
from pathlib import Path
import platform
import re
import shutil
import subprocess
import tarfile
import tempfile
from threading import Thread
import time
import tomllib
from urllib.request import urlopen
import zipfile


BASELINE = "v0.0.82"
BASELINE_REVISION = "26f5951f45661a44794c380cd0b4a7417fc8ca47"
RELEASES = "https://github.com/nitecon/amux/releases/download"
ROOT = Path(__file__).resolve().parents[1]

HARNESS = r"""
#![allow(dead_code)]
mod identity {
    use serde::{Deserialize, Serialize};
    #[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
    #[serde(rename_all = "lowercase")]
    pub enum Provider { Codex, Claude }
    impl Provider {
        pub fn command(self) -> &'static str {
            match self { Self::Codex => "codex", Self::Claude => "claude" }
        }
    }
}
mod model_profiles;
mod updates;

use anyhow::{Context, Result, ensure};
use std::{path::PathBuf, process::Command};
use updates::{ReleaseSource, Running};

#[tokio::main(flavor = "current_thread")]
async fn main() -> Result<()> {
    let mut args = std::env::args().skip(1);
    let desktop = PathBuf::from(args.next().context("desktop path")?);
    let origin = args.next().context("fixture origin")?;
    let tag = args.next().context("release tag")?;
    let project = args.next().context("project directory")?;
    let source = ReleaseSource {
        api_url: format!("{origin}/releases.json"),
        download_prefix: format!("{origin}/download/"),
    };
    let running = Running::at(desktop.clone());
    let check = updates::check(&running, &source).await?;
    ensure!(check.available.as_ref().map(|release| release.tag.as_str()) == Some(&tag),
        "shipped updater did not offer {tag}: {check:?}");
    println!("{}", updates::update(&running, &source).await?);
    let restart = updates::restart_status(&running);
    ensure!(restart.required && restart.path.as_ref() == Some(&desktop),
        "shipped updater did not identify the replacement desktop");
    let status = Command::new(desktop)
        .args(["--project-dir", &project, "--help"])
        .status()?;
    ensure!(status.success(), "replacement desktop rejected the restart arguments");
    Ok(())
}
"""


def sha(path):
    with path.open("rb") as stream:
        return hashlib.file_digest(stream, "sha256").hexdigest()


def fetch(name, directory):
    path = directory / name
    with urlopen(f"{RELEASES}/{BASELINE}/{name}", timeout=120) as response:
        with path.open("wb") as stream:
            shutil.copyfileobj(response, stream)
    return path


def unpack(archive, target):
    if archive.suffix == ".zip":
        with zipfile.ZipFile(archive) as bundle:
            for entry in bundle.infolist():
                path = Path(bundle.extract(entry, target))
                if os.name != "nt" and (mode := entry.external_attr >> 16 & 0o777):
                    path.chmod(mode)
    else:
        with tarfile.open(archive) as bundle:
            bundle.extractall(target, filter="data")


def source(path):
    return subprocess.check_output(
        ["git", "show", f"{BASELINE_REVISION}:{path}"], cwd=ROOT
    )


def build_harness(directory):
    src = directory / "src"
    src.mkdir(parents=True, exist_ok=True)
    for name in ("updates.rs", "model_profiles.rs"):
        (src / name).write_bytes(source(f"src/{name}"))
    (src / "main.rs").write_text(HARNESS, encoding="utf-8")
    original_manifest = tomllib.loads(source("Cargo.toml").decode())
    dependencies = (
        "anyhow", "flate2", "reqwest", "self-replace", "serde", "serde_json",
        "sha2", "tar", "tempfile", "tokio", "uuid", "zip",
    )
    manifest = [
        '[package]\nname = "amux-handoff"\nversion = "0.0.82"\nedition = "2024"',
        "[dependencies]",
    ]
    for name in dependencies:
        spec = original_manifest["dependencies"][name]
        if isinstance(spec, dict):
            spec = "{ " + ", ".join(f"{key} = {json.dumps(value)}" for key, value in spec.items()) + " }"
        else:
            spec = json.dumps(spec)
        manifest.append(f"{name} = {spec}")
    (directory / "Cargo.toml").write_text("\n".join(manifest), encoding="utf-8")
    # Use the historical dependency resolutions where they remain applicable.
    (directory / "Cargo.lock").write_bytes(source("Cargo.lock"))
    subprocess.run(
        ["cargo", "build", "--quiet", "--target-dir", directory / "target"],
        cwd=directory,
        check=True,
    )


def verify_source_handoff(archive, directory, expected, source_bin, tag):
    """Run the Go updater from a loose source build with no release metadata."""
    installed = directory / "source-install"
    installed.mkdir()
    suffix = ".exe" if os.name == "nt" else ""
    cli = installed / f"amux{suffix}"
    shutil.copy2(source_bin / cli.name, cli)
    desktop_name = f"amux-desktop{suffix}"
    shutil.copy2(source_bin / desktop_name, installed / desktop_name)
    marker = installed / "user-file.txt"
    marker.write_text("preserve me", encoding="utf-8")

    env = os.environ.copy()
    cache = directory / "source-cache"
    if os.name == "nt":
        env["LOCALAPPDATA"] = str(cache)
    elif platform.system() == "Darwin":
        env["HOME"] = str(cache)
        cache = cache / "Library" / "Caches"
    else:
        env["XDG_CACHE_HOME"] = str(cache)

    stage = cache / "amux" / "updates" / tag
    stage.mkdir(parents=True)
    staged = stage / ("bundle.zip" if archive.suffix == ".zip" else "bundle.tar.gz")
    shutil.copyfile(archive, staged)
    (stage / "verified.json").write_text(json.dumps({
        "tag": tag, "digest": sha(staged),
    }), encoding="utf-8")
    helper = directory / f"amux-handoff-fixture{suffix}"
    shutil.copy2(cli, helper)
    subprocess.run([
        helper, "update", "--staged", staged, "--executable", cli, "--parent-pid", "0",
        "--wait-pids", "[]", "--restart-args", '["--help"]', "--cache-root", cache,
    ], env=env, check=True, timeout=60)
    log = stage / "install.log"
    deadline = time.monotonic() + 60
    while time.monotonic() < deadline:
        if log.is_file() and (result := log.read_text(encoding="utf-8").strip()):
            if result != "update completed":
                raise ValueError(f"source updater failed: {result}")
            break
        time.sleep(0.2)
    else:
        raise TimeoutError("source updater did not finish")

    if marker.read_text(encoding="utf-8") != "preserve me":
        raise ValueError("source updater changed an unrelated file")
    for path in expected.rglob("*"):
        if path.is_file() and sha(installed / path.relative_to(expected)) != sha(path):
            raise ValueError(f"source install differs from release: {path.relative_to(expected)}")
    print(f"Verified source build installed {tag} and started the replacement desktop")


def verify_handoff(archive, directory, source_bin=None):
    tag_match = re.fullmatch(r"(amux-(?:linux-x86_64|windows-x86_64|macos-aarch64))-"
                             r"(v\d+\.\d+\.\d+)\.(tar\.gz|zip)", archive.name)
    if not tag_match:
        raise ValueError(f"expected a canonical tagged amux archive: {archive.name}")
    prefix, tag, extension = tag_match.groups()
    if tuple(map(int, tag[1:].split("."))) <= (0, 0, 82):
        raise ValueError("handoff requires a stable release newer than v0.0.82")
    suffix = ".exe" if os.name == "nt" else ""
    bundle = Path("amux.app/Contents") if platform.system() == "Darwin" else Path()
    binaries = bundle / "MacOS" if bundle.parts else bundle
    resources = bundle / "Resources" if bundle.parts else bundle
    desktop = binaries / f"amux-desktop{suffix}"
    baseline_name = f"{prefix}-{BASELINE}.{extension}"
    sums = fetch("SHA256SUMS", directory).read_text(encoding="utf-8")
    expected_baseline = dict(line.split()[::-1] for line in sums.splitlines())
    baseline = fetch(baseline_name, directory)
    if sha(baseline) != expected_baseline[baseline_name]:
        raise ValueError(f"{baseline_name} checksum mismatch")
    installed, expected = directory / "installed", directory / "expected"
    unpack(baseline, installed)
    unpack(archive, expected)
    build = json.loads((expected / resources / "BUILD.json").read_bytes())
    if build["candidate_only"] or f'v{build["version"]}' != tag:
        raise ValueError("archive metadata is not a stable build of its tag")
    if source_bin is not None:
        verify_source_handoff(archive, directory, expected, source_bin, tag)

    served = directory / "public"
    download = served / "download" / tag
    download.mkdir(parents=True)
    offered = download / archive.name
    shutil.copyfile(archive, offered)
    checksums = download / "SHA256SUMS"
    checksums.write_text(f"{sha(offered)}  {offered.name}\n", encoding="utf-8")
    handler = partial(SimpleHTTPRequestHandler, directory=served)
    with ThreadingHTTPServer(("127.0.0.1", 0), handler) as server:
        origin = f"http://127.0.0.1:{server.server_port}"
        releases = served / "releases.json"
        releases.write_text(json.dumps([{
            "tag_name": tag,
            "html_url": f"{origin}/{tag}",
            "target_commitish": build["revision"],
            "published_at": "2026-01-01T00:00:00Z",
            "assets": [{
                "name": path.name, "state": "uploaded", "size": path.stat().st_size,
                "digest": f"sha256:{sha(path)}",
                "browser_download_url": f"{origin}/download/{tag}/{path.name}",
            } for path in (offered, checksums)],
        }]), encoding="utf-8")
        runner = directory / "harness"
        build_harness(runner)
        project = directory / "project with spaces"
        project.mkdir()
        thread = Thread(target=server.serve_forever, daemon=True)
        thread.start()
        try:
            subprocess.run([
                runner / "target" / "debug" / f"amux-handoff{suffix}",
                installed / desktop, origin, tag, project,
            ], check=True, timeout=180)
        finally:
            server.shutdown()
            thread.join()
    for path in expected.rglob("*"):
        if path.is_file() and sha(installed / path.relative_to(expected)) != sha(path):
            raise ValueError(f"installed file differs from release: {path.relative_to(expected)}")
    print(f"Verified shipped {BASELINE} updater installed and identified {tag} for restart")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("archive", type=Path)
    parser.add_argument("--source-bin", type=Path)
    args = parser.parse_args()
    scratch = ROOT / "target"
    scratch.mkdir(exist_ok=True)
    with tempfile.TemporaryDirectory(prefix="amux-handoff-", dir=scratch) as temporary:
        verify_handoff(args.archive.resolve(), Path(temporary), args.source_bin)
