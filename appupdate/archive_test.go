package appupdate

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestFindCanonicalReleaseAssets(t *testing.T) {
	for _, platform := range []struct {
		triple, prefix, filename string
	}{
		{"x86_64-unknown-linux-gnu", "amux-linux-x86_64", "amux-linux-x86_64-v0.0.83.tar.gz"},
		{"x86_64-pc-windows-msvc", "amux-windows-x86_64", "amux-windows-x86_64-v0.0.83.zip"},
		{"aarch64-apple-darwin", "amux-macos-aarch64", "amux-macos-aarch64-v0.0.83.zip"},
	} {
		t.Run(platform.triple, func(t *testing.T) {
			latest := release{TagName: "v0.0.83"}
			for _, name := range []string{platform.filename, "SHA256SUMS", "amux-go-windows-amd64-v0.0.83.tar.gz"} {
				latest.Assets = append(latest.Assets, releaseAsset{
					Name: name, Size: 1, Digest: "sha256:" + strings.Repeat("a", 64),
					BrowserDownloadURL: "https://github.com/nitecon/amux/releases/download/v0.0.83/" + name,
				})
			}
			archive, sums, err := findReleaseAssets(latest, platform.prefix, platform.triple)
			if err != nil || archive.Name != platform.filename || sums.Name != "SHA256SUMS" {
				t.Fatalf("archive=%q checksum=%q error=%v", archive.Name, sums.Name, err)
			}
		})
	}
}

func TestVerifyWindowsReleaseWithShippedMetadata(t *testing.T) {
	if runtime.GOOS != "windows" || runtime.GOARCH != "amd64" {
		t.Skip("verification binds the package to the running platform")
	}
	files := map[string][]byte{
		"amux.exe": []byte("CLI"), "amux-desktop.exe": []byte("desktop"),
		"agent-tools.exe": []byte("agent tools"), "memory.exe": []byte("memory"),
		"models.json": []byte("catalog"), "LICENSE": []byte("license"), "REVIEW.md": []byte("review"),
	}
	build := Build{
		Revision: strings.Repeat("a", 40), Version: "0.0.83", Target: "x86_64-pc-windows-msvc",
		GUIBinary: "amux-desktop.exe", BinarySHA256: hashBytes(files["amux.exe"]),
		GUIBinarySHA256: hashBytes(files["amux-desktop.exe"]), ModelCatalogSHA256: hashBytes(files["models.json"]),
		BundledSHA256: map[string]string{"agent-tools.exe": hashBytes(files["agent-tools.exe"]), "memory.exe": hashBytes(files["memory.exe"])},
	}
	metadata, err := json.Marshal(build)
	if err != nil {
		t.Fatal(err)
	}
	files["BUILD.json"] = append([]byte("\xef\xbb\xbf"), metadata...)
	var sums strings.Builder
	sums.WriteString("\ufeff")
	for _, name := range []string{"amux.exe", "amux-desktop.exe", "agent-tools.exe", "memory.exe"} {
		fmt.Fprintf(&sums, "%s  %s\r\n", hashBytes(files[name]), name)
	}
	files["SHA256SUMS"] = []byte(sums.String())
	archive := zipFixture(t, files)
	if _, err := verifyArchive(archive, "v0.0.83", build.Target); err != nil {
		t.Fatal(err)
	}
	files["amux.exe"] = []byte("tampered")
	if _, err := verifyArchive(zipFixture(t, files), "v0.0.83", build.Target); err == nil {
		t.Fatal("tampered CLI passed verification")
	}
}

func TestExtractMacAppRejectsEscapingMembers(t *testing.T) {
	for _, member := range []string{
		"amux.app/Contents/MacOS/../../../elsewhere", "/amux.app/Contents/Info.plist", "__MACOSX/metadata",
	} {
		archive := zipFixture(t, map[string][]byte{member: []byte("bad")})
		if err := extractArchive(archive, t.TempDir(), "aarch64-apple-darwin"); err == nil {
			t.Fatalf("accepted %q", member)
		}
	}
}

func TestUpdateLockCanReuseShippedLockFile(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".amux-update.lock"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := acquireUpdateLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := acquireUpdateLock(dir); err == nil {
		_ = second.Close()
		_ = first.Close()
		t.Fatal("second updater acquired a held lock")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := acquireUpdateLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
}

func zipFixture(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var output bytes.Buffer
	archive := zip.NewWriter(&output)
	for name, data := range files {
		member, err := archive.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := member.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}
