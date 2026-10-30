"""Download the agent-tools and memory releases pinned in
packaging/bundled-tools.json, verify their checksums and extract the
`agent-tools` and `memory` executables into one flat directory.

    python scripts/fetch_bundled_tools.py --target <rust-target> --output <dir>

agent-tools is checked against its release SHA256SUMS.txt; memory publishes no
checksum file, so its archives are checked against hashes pinned here.
"""
import argparse
import hashlib
import io
import json
import tarfile
import urllib.request
import zipfile
from pathlib import Path

ROOT = Path(__file__).resolve().parent.parent
PINS = ROOT / "packaging" / "bundled-tools.json"


def download(repo, version, name):
    url = f"https://github.com/{repo}/releases/download/{version}/{name}"
    with urllib.request.urlopen(url, timeout=120) as response:
        return response.read()


def expected_sha(name, pin):
    if "sha256" in pin:
        return pin["sha256"][name]
    sums = download(pin["repo"], pin["version"], pin["checksums"]).decode()
    for line in sums.splitlines():
        parts = line.split()
        if len(parts) == 2 and parts[1].lstrip("*") == name:
            return parts[0]
    raise ValueError(f"{name}: not listed in {pin['checksums']}")


def extract(archive, name, binary):
    """Return the bytes of the one archive member whose file name is `binary`."""
    if name.endswith(".zip"):
        with zipfile.ZipFile(io.BytesIO(archive)) as handle:
            members = [m for m in handle.namelist() if m.rsplit("/", 1)[-1] == binary]
            if len(members) != 1:
                raise ValueError(f"{name}: expected one {binary}, found {members}")
            return handle.read(members[0])
    with tarfile.open(fileobj=io.BytesIO(archive), mode="r:gz") as handle:
        members = [m for m in handle.getmembers()
                   if m.isfile() and m.name.rsplit("/", 1)[-1] == binary]
        if len(members) != 1:
            raise ValueError(f"{name}: expected one {binary}, found {[m.name for m in members]}")
        return handle.extractfile(members[0]).read()


def fetch(target, output):
    output.mkdir(parents=True, exist_ok=True)
    suffix = ".exe" if "windows" in target else ""
    for binary, pin in json.loads(PINS.read_text()).items():
        name = pin["assets"][target]
        archive = download(pin["repo"], pin["version"], name)
        actual = hashlib.sha256(archive).hexdigest()
        if actual != expected_sha(name, pin):
            raise ValueError(f"{name}: checksum {actual} does not match the pin")
        path = output / f"{binary}{suffix}"
        path.write_bytes(extract(archive, name, f"{binary}{suffix}"))
        path.chmod(0o755)
        print(f"{binary} {pin['version']} {hashlib.sha256(path.read_bytes()).hexdigest()}  {path}")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--target", required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    fetch(args.target, args.output)
