package update

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
	t.Run("downloads without a portable install", func(t *testing.T) {
		t.Setenv("LOCALAPPDATA", t.TempDir())
		name := "amux-windows-x86_64-v0.0.83.zip"
		sums := []byte(fmt.Sprintf("%s  %s\n", hashBytes(archive), name))
		base := "https://github.com/nitecon/amux/releases/download/v0.0.83/"
		payloads := map[string][]byte{base + name: archive, base + "SHA256SUMS": sums}
		latest := release{TagName: "v0.0.83", TargetCommitish: build.Revision}
		for _, asset := range []string{name, "SHA256SUMS"} {
			data := payloads[base+asset]
			latest.Assets = append(latest.Assets, releaseAsset{
				Name: asset, Size: int64(len(data)), Digest: "sha256:" + hashBytes(data), BrowserDownloadURL: base + asset,
			})
		}
		metadata, err := json.Marshal(latest)
		if err != nil {
			t.Fatal(err)
		}
		payloads["https://api.github.com/repos/nitecon/amux/releases/latest"] = metadata
		transport := http.DefaultTransport
		t.Cleanup(func() { http.DefaultTransport = transport })
		http.DefaultTransport = releaseTestTransport(payloads)

		path, info, err := Stage(context.Background(), "")
		if err != nil || !info.Available || filepath.Base(path) != "bundle.zip" {
			t.Fatalf("stage without an install: path=%q info=%+v err=%v", path, info, err)
		}
		data, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(data, archive) {
			t.Fatalf("downloaded archive differs from the release: %v", err)
		}
		if _, err := stagePathAt(path, os.Getenv("LOCALAPPDATA")); err != nil {
			t.Fatalf("download must be ready for the update helper: %v", err)
		}
	})
	files["amux.exe"] = []byte("tampered")
	if _, err := verifyArchive(zipFixture(t, files), "v0.0.83", build.Target); err == nil {
		t.Fatal("tampered CLI passed verification")
	}
}

type releaseTestTransport map[string][]byte

func (transport releaseTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	data, ok := transport[request.URL.String()]
	if !ok {
		return nil, fmt.Errorf("unexpected request to %s", request.URL)
	}
	return &http.Response{StatusCode: http.StatusOK, ContentLength: int64(len(data)), Body: io.NopCloser(bytes.NewReader(data))}, nil
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
