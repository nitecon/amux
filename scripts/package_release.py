"""Validate CI bundles and produce the flat archives consumed by amux --update."""

import argparse
import hashlib
import json
from pathlib import Path
import re
import struct
import tarfile
import tomllib
import zipfile


def sha(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


def check_tag(tag):
    version = tomllib.loads(Path("Cargo.toml").read_text())["package"]["version"]
    if not re.fullmatch(r"v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)", tag):
        raise ValueError("release tag must be vMAJOR.MINOR.PATCH")
    if tag != f"v{version}":
        raise ValueError(f"release tag {tag} differs from Cargo version {version}")
    return version


def package(artifacts, output, tag, revision):
    version = check_tag(tag)
    if not re.fullmatch(r"[0-9a-f]{40}", revision):
        raise ValueError("revision must be a full commit SHA")
    output.mkdir(parents=True, exist_ok=True)
    sums = []
    for target, platform, cli, gui in [
        ("x86_64-unknown-linux-gnu", "linux", "amux", "amux-desktop"),
        ("x86_64-pc-windows-msvc", "windows", "amux.exe", "amux-desktop.exe"),
    ]:
        bundle = artifacts / f"amux-{target}-{revision}"
        names = {cli, gui, "BUILD.json", "SHA256SUMS", "LICENSE", "ICED_TERM_LICENSE",
                 "REVIEW.md", "notification-validation.md"}
        if {p.name for p in bundle.iterdir()} != names:
            raise ValueError(f"{target}: incomplete or unexpected bundle files")
        if any(p.is_symlink() or not p.is_file() for p in bundle.iterdir()):
            raise ValueError(f"{target}: bundle contains a non-regular file")
        build = json.loads((bundle / "BUILD.json").read_text(encoding="utf-8-sig"))
        if (build["revision"], build["target"], build["version"], build["gui_binary"],
            build["candidate_only"]) != (revision, target, version, gui, False):
            raise ValueError(f"{target}: release metadata differs from tag/source/platform")
        checksums = dict(line.split()[::-1] for line in
                         (bundle / "SHA256SUMS").read_text(encoding="utf-8-sig").splitlines()
                         if line.strip())
        if set(checksums) != {cli, gui}:
            raise ValueError(f"{target}: executable checksum list differs")
        for name, field in [(cli, "binary_sha256"), (gui, "gui_binary_sha256")]:
            path = bundle / name
            if sha(path) != checksums[name] or sha(path) != build[field]:
                raise ValueError(f"{target}: {name} checksum differs")
            data = path.read_bytes()
            if platform == "linux":
                if data[:4] != b"\x7fELF":
                    raise ValueError(f"{name}: expected ELF executable")
                path.chmod(0o755)
            else:
                pe = struct.unpack_from("<I", data, 0x3c)[0]
                subsystem = struct.unpack_from("<H", data, pe + 24 + 68)[0]
                if (data[:2] != b"MZ" or data[pe:pe + 4] != b"PE\0\0"
                        or subsystem != (3 if name == cli else 2)):
                    raise ValueError(f"{name}: incorrect Windows executable subsystem")
        if platform == "linux":
            archive = output / f"amux-linux-x86_64-{tag}.tar.gz"
            with tarfile.open(archive, "w:gz") as handle:
                for name in sorted(names):
                    handle.add(bundle / name, arcname=name)
            with tarfile.open(archive) as handle:
                if set(handle.getnames()) != names:
                    raise ValueError("tar must contain only flat bundle files")
                for name in names:
                    if handle.extractfile(name).read() != (bundle / name).read_bytes():
                        raise ValueError(f"tar roundtrip differs: {name}")
        else:
            archive = output / f"amux-windows-x86_64-{tag}.zip"
            with zipfile.ZipFile(archive, "w", compression=zipfile.ZIP_DEFLATED) as handle:
                for name in sorted(names):
                    handle.write(bundle / name, name)
            with zipfile.ZipFile(archive) as handle:
                if set(handle.namelist()) != names or handle.testzip() is not None:
                    raise ValueError("zip must contain valid flat bundle files")
                for name in names:
                    if handle.read(name) != (bundle / name).read_bytes():
                        raise ValueError(f"zip roundtrip differs: {name}")
        sums.append(f"{sha(archive)}  {archive.name}\n")
    (output / "SHA256SUMS").write_text("".join(sums))
    print("".join(sums), end="")


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--check-tag")
    parser.add_argument("--tag")
    parser.add_argument("--revision")
    parser.add_argument("--artifacts", type=Path)
    parser.add_argument("--output", type=Path)
    args = parser.parse_args()
    if args.check_tag:
        check_tag(args.check_tag)
    elif all([args.tag, args.revision, args.artifacts, args.output]):
        package(args.artifacts, args.output, args.tag, args.revision)
    else:
        parser.error("use --check-tag or provide --tag, --revision, --artifacts and --output")
