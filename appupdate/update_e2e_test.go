//go:build !windows

package appupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestStagedFlatUpdateRunsHelperAndRestartsDesktop(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the fixture uses a flat Linux release")
	}
	_, cliName, guiName, _, err := target()
	if err != nil {
		t.Skip(err)
	}
	root := t.TempDir()
	cacheRoot := filepath.Join(root, "cache")
	if err := os.MkdirAll(cacheRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	installDir := filepath.Join(root, "install")
	if err := os.MkdirAll(installDir, 0o700); err != nil {
		t.Fatal(err)
	}
	cli := filepath.Join(installDir, cliName)
	projectRoot := projectRoot(t)
	cliBuild := exec.Command("go", "build", "-trimpath", "-o", cli, "./cmd/amux")
	cliBuild.Dir = projectRoot
	if output, err := cliBuild.CombinedOutput(); err != nil {
		t.Fatalf("build CLI helper: %v\n%s", err, output)
	}
	if err := os.Chmod(cli, 0o755); err != nil {
		t.Fatal(err)
	}
	oldGUI := writeExecutable(t, installDir, guiName, "#!/bin/sh\necho old > \"$AMUX_RESTART_MARKER\"\n")
	oldTools := writeExecutable(t, installDir, toolName("agent-tools"), "#!/bin/sh\nexit 0\n")
	oldMemory := writeExecutable(t, installDir, toolName("memory"), "#!/bin/sh\nexit 0\n")
	models := []byte("{\"version\":1,\"models\":[]}\n")
	license := []byte("license\n")
	writeFile(t, filepath.Join(installDir, "models.json"), models, 0o600)
	writeFile(t, filepath.Join(installDir, "LICENSE"), license, 0o600)
	oldCLIHash := mustHash(t, cli)
	oldGUIHash := mustHash(t, oldGUI)
	oldToolsHash := mustHash(t, oldTools)
	oldMemoryHash := mustHash(t, oldMemory)
	oldBuild := map[string]any{
		"revision": strings.Repeat("a", 40), "version": "1.0.0", "target": targetTriple(),
		"gui_binary": guiName, "binary_sha256": oldCLIHash, "gui_binary_sha256": oldGUIHash,
		"bundled_sha256":       map[string]string{toolName("agent-tools"): oldToolsHash, toolName("memory"): oldMemoryHash},
		"model_catalog_sha256": hashBytes(models), "candidate_only": false,
	}
	writeJSON(t, filepath.Join(installDir, "BUILD.json"), oldBuild)
	writeFile(t, filepath.Join(installDir, "SHA256SUMS"), []byte(fmt.Sprintf("%s  %s\n%s  %s\n%s  %s\n%s  %s\n", oldCLIHash, cliName, oldGUIHash, guiName, oldToolsHash, toolName("agent-tools"), oldMemoryHash, toolName("memory"))), 0o600)

	marker := filepath.Join(root, "restarted")
	newGUI := []byte("#!/bin/sh\nprintf '%s' \"$*\" > \"$AMUX_RESTART_MARKER\"\n")
	newFiles := map[string][]byte{
		cliName: []byte("#!/bin/sh\nexit 0\n"), guiName: newGUI,
		toolName("agent-tools"): []byte("#!/bin/sh\nexit 0\n"),
		toolName("memory"):      []byte("#!/bin/sh\nexit 0\n"),
		"models.json":           models, "LICENSE": license,
	}
	build := Build{
		Revision: strings.Repeat("b", 40), Version: "1.0.1", Tag: "v1.0.1", Target: targetTriple(),
		GoOS: runtime.GOOS, GoArch: runtime.GOARCH, GUIBinary: guiName,
		CandidateOnly: false,
	}
	build.BinarySHA256 = hashBytes(newFiles[cliName])
	build.GUIBinarySHA256 = hashBytes(newGUI)
	build.BundledSHA256 = map[string]string{
		toolName("agent-tools"): hashBytes(newFiles[toolName("agent-tools")]),
		toolName("memory"):      hashBytes(newFiles[toolName("memory")]),
	}
	build.ModelCatalogSHA256 = hashBytes(models)
	buildData, _ := json.Marshal(build)
	newFiles["BUILD.json"] = append(buildData, '\n')
	var sums strings.Builder
	for _, name := range []string{cliName, guiName, toolName("agent-tools"), toolName("memory")} {
		sums.WriteString(hashBytes(newFiles[name]) + "  " + name + "\n")
	}
	newFiles["SHA256SUMS"] = []byte(sums.String())
	archive := makeBundleArchive(t, newFiles)
	stageDir := filepath.Join(cacheRoot, "amux", "updates", build.Tag)
	if err := os.MkdirAll(stageDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(stageDir, 0o700); err != nil {
		t.Fatal(err)
	}
	archivePath := filepath.Join(stageDir, "bundle.tar.gz")
	writeFile(t, archivePath, archive, 0o600)
	metadata, _ := json.Marshal(stageMetadata{Tag: build.Tag, Digest: hashBytes(archive)})
	writeFile(t, filepath.Join(stageDir, "verified.json"), append(metadata, '\n'), 0o600)

	t.Setenv("AMUX_RESTART_MARKER", marker)
	if err := startApplyAt(cli, archivePath, cacheRoot, nil, []string{"--smoke", "candidate"}, 0); err != nil {
		t.Fatalf("start update helper: %v", err)
	}
	deadline := time.Now().Add(12 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err != nil {
			if log, logErr := os.ReadFile(filepath.Join(stageDir, "install.log")); logErr == nil && strings.TrimSpace(string(log)) != "update completed" {
				t.Fatalf("update helper failed: %s", log)
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}
		installed, err := readInstalled(installDir)
		if err == nil && installed.Revision == build.Revision {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if _, err := os.Stat(marker); err != nil {
		log, _ := os.ReadFile(filepath.Join(stageDir, "install.log"))
		t.Fatalf("timed out waiting for staged update and restart; helper log: %s", log)
	}
	if got, err := os.ReadFile(marker); err != nil || string(got) != "--smoke candidate" {
		t.Fatalf("restarted desktop marker = %q, err=%v", got, err)
	}
	if err := verifyInstalled(installDir, build, cliName, guiName); err != nil {
		t.Fatalf("updated bundle failed verification: %v", err)
	}
}

func TestRestartLaunchSupportsNoDesktopArguments(t *testing.T) {
	_, _, guiName, _, err := target()
	if err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "restarted")
	t.Setenv("AMUX_RESTART_MARKER", marker)
	desktop := writeExecutable(t, dir, guiName, "#!/bin/sh\n: > \"$AMUX_RESTART_MARKER\"\n")
	if err := RelaunchDesktop(desktop, []string{}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(marker); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("desktop was not relaunched with an empty argument list")
}

func projectRoot(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locate test source")
	}
	return filepath.Dir(filepath.Dir(source))
}

func targetTriple() string {
	switch runtime.GOOS + "/" + runtime.GOARCH {
	case "linux/amd64":
		return "x86_64-unknown-linux-gnu"
	case "darwin/arm64":
		return "aarch64-apple-darwin"
	default:
		return "x86_64-pc-windows-msvc"
	}
}

func toolName(name string) string {
	if runtime.GOOS == "windows" {
		return name + ".exe"
	}
	return name
}

func writeExecutable(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	writeFile(t, path, []byte(content), 0o755)
	return path
}

func writeFile(t *testing.T, path string, content []byte, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, content, mode); err != nil {
		t.Fatal(err)
	}
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	content, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, path, append(content, '\n'), 0o600)
}

func mustHash(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return hashBytes(data)
}

func makeBundleArchive(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var output bytes.Buffer
	compressor := gzip.NewWriter(&output)
	archive := tar.NewWriter(compressor)
	for name, content := range files {
		mode := int64(0o600)
		if strings.HasPrefix(name, "amux") || strings.HasPrefix(name, "agent-tools") || name == "memory" {
			mode = 0o755
		}
		if err := archive.WriteHeader(&tar.Header{Name: name, Mode: mode, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := archive.Write(content); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressor.Close(); err != nil {
		t.Fatal(err)
	}
	return output.Bytes()
}
