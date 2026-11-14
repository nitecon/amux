"""Exercise catalog inclusion and validation in portable release archives."""
import hashlib
import json
from pathlib import Path
import struct
import subprocess
import tempfile
import tomllib
import unittest
import zipfile
import tarfile

import package_release


class ModelCatalogPackaging(unittest.TestCase):
    def fixture(self, root):
        revision = "a" * 40
        version = tomllib.loads(Path("Cargo.toml").read_text())["package"]["version"]
        catalog = Path("assets/models.json").read_bytes()
        bundles = []
        for target, suffix in [("x86_64-unknown-linux-gnu", ""), ("x86_64-pc-windows-msvc", ".exe")]:
            directory = root / f"amux-{target}-{revision}"
            directory.mkdir()
            cli, gui = "amux" + suffix, "amux-desktop" + suffix
            tools = ["agent-tools" + suffix, "memory" + suffix]
            hashes = {}
            for name in [cli, gui] + tools:
                data = bytearray(256)
                if suffix:
                    data[:2] = b"MZ"
                    struct.pack_into("<I", data, 0x3c, 64)
                    data[64:68] = b"PE\0\0"
                    struct.pack_into("<H", data, 64 + 24 + 68, 2 if name == gui else 3)
                else:
                    data[:4] = b"\x7fELF"
                (directory / name).write_bytes(data)
                hashes[name] = hashlib.sha256(data).hexdigest()
            (directory / "models.json").write_bytes(catalog)
            for name in ["LICENSE", "ICED_TERM_LICENSE", "REVIEW.md", "notification-validation.md"]:
                (directory / name).write_text("fixture")
            (directory / "SHA256SUMS").write_text("".join(f"{h}  {n}\n" for n, h in hashes.items()))
            (directory / "BUILD.json").write_text(json.dumps({
                "revision": revision, "target": target, "version": version,
                "gui_binary": gui, "candidate_only": False,
                "binary_sha256": hashes[cli], "gui_binary_sha256": hashes[gui],
                "bundled_sha256": {name: hashes[name] for name in tools},
                "model_catalog_sha256": hashlib.sha256(catalog).hexdigest(),
            }))
            bundles.append(directory)
        return revision, f"v{version}", bundles

    def test_catalog_survives_both_archive_formats_and_is_required(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            revision, tag, bundles = self.fixture(root)
            output = root / "dist"
            package_release.package(root, output, tag, revision)
            for path in output.iterdir():
                if path.suffix == ".zip":
                    with zipfile.ZipFile(path) as archive:
                        self.assertEqual(archive.read("models.json"), Path("assets/models.json").read_bytes())
                elif path.name.endswith(".tar.gz"):
                    with tarfile.open(path) as archive:
                        self.assertEqual(archive.extractfile("models.json").read(), Path("assets/models.json").read_bytes())
            (bundles[0] / "models.json").unlink()
            with self.assertRaisesRegex(ValueError, "incomplete"):
                package_release.package(root, output, tag, revision)

    def test_windows_style_checkout_keeps_the_catalog_identical(self):
        with tempfile.TemporaryDirectory() as temporary:
            prefix = Path(temporary).as_posix() + "/"
            subprocess.run(["git", "-c", "core.autocrlf=true", "-c", "core.eol=crlf",
                            "checkout-index", "--prefix=" + prefix, "--", "assets/models.json"],
                           check=True, capture_output=True)
            self.assertEqual((Path(temporary) / "assets/models.json").read_bytes(),
                             Path("assets/models.json").read_bytes())

    def test_catalog_drift_is_rejected(self):
        with tempfile.TemporaryDirectory() as temporary:
            root = Path(temporary)
            revision, tag, bundles = self.fixture(root)
            (bundles[0] / "models.json").write_text('{"version":1,"models":[{}]}')
            with self.assertRaisesRegex(ValueError, "differs from source"):
                package_release.package(root, root / "dist", tag, revision)


if __name__ == "__main__":
    unittest.main()
