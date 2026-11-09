import hashlib
import io
import json
from pathlib import Path
import tempfile
import unittest
import urllib.error
from unittest.mock import patch

import fetch_bundled_tools as bundled


def metadata(name="tool.zip"):
    return io.BytesIO(json.dumps({"assets": [{
        "name": name,
        "url": "https://api.github.com/repos/owner/repo/releases/assets/1",
    }]}).encode())


class DownloadTests(unittest.TestCase):
    def test_healthy_download_uses_only_the_canonical_url(self):
        with patch.object(bundled.urllib.request, "urlopen", return_value=io.BytesIO(b"archive")) as request:
            self.assertEqual(bundled.download("owner/repo", "v1.0.0", "tool.zip"), b"archive")
        request.assert_called_once_with(
            "https://github.com/owner/repo/releases/download/v1.0.0/tool.zip", timeout=120
        )

    def test_server_error_uses_the_named_public_asset_with_binary_accept(self):
        error = urllib.error.HTTPError("url", 504, "timeout", {}, None)
        with patch.object(bundled.urllib.request, "urlopen", side_effect=[
            error, metadata(), io.BytesIO(b"archive"),
        ]) as request:
            self.assertEqual(bundled.download("owner/repo", "v1.0.0", "tool.zip"), b"archive")
        self.assertEqual(request.call_count, 3)
        release_request = request.call_args_list[1].args[0]
        self.assertEqual(release_request.full_url, "https://api.github.com/repos/owner/repo/releases/tags/v1.0.0")
        asset_request = request.call_args_list[2].args[0]
        self.assertEqual(asset_request.full_url, "https://api.github.com/repos/owner/repo/releases/assets/1")
        self.assertEqual(asset_request.get_header("Accept"), "application/octet-stream")
        self.assertFalse(asset_request.has_header("Authorization"))

    def test_not_found_is_not_retried(self):
        error = urllib.error.HTTPError("url", 404, "missing", {}, None)
        with patch.object(bundled.urllib.request, "urlopen", side_effect=error) as request:
            with self.assertRaises(urllib.error.HTTPError) as result:
                bundled.download("owner/repo", "v1.0.0", "tool.zip")
        self.assertIs(result.exception, error)
        self.assertEqual(request.call_count, 1)

    def test_missing_named_asset_does_not_download_another_asset(self):
        error = urllib.error.HTTPError("url", 504, "timeout", {}, None)
        with patch.object(bundled.urllib.request, "urlopen", side_effect=[error, metadata("other.zip")]) as request:
            with self.assertRaisesRegex(ValueError, "tool.zip not found"):
                bundled.download("owner/repo", "v1.0.0", "tool.zip")
        self.assertEqual(request.call_count, 2)

    def test_fallback_bytes_still_require_the_pinned_hash_before_writing(self):
        error = urllib.error.HTTPError("url", 504, "timeout", {}, None)
        with tempfile.TemporaryDirectory() as directory:
            root = Path(directory)
            pins = root / "pins.json"
            pins.write_text(json.dumps({"memory": {
                "repo": "owner/repo", "version": "v1.0.0",
                "assets": {"test-target": "tool.zip"},
                "sha256": {"tool.zip": hashlib.sha256(b"trusted").hexdigest()},
            }}))
            with patch.object(bundled, "PINS", pins), patch.object(
                bundled.urllib.request, "urlopen",
                side_effect=[error, metadata(), io.BytesIO(b"tampered")],
            ):
                with self.assertRaisesRegex(ValueError, "checksum .* does not match"):
                    bundled.fetch("test-target", root / "output")
            self.assertFalse((root / "output" / "memory").exists())


if __name__ == "__main__":
    unittest.main()
