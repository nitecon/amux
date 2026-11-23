package appupdate

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

var revisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

func Stage(ctx context.Context, executable string) (string, UpdateInfo, error) {
	installed, latest, archive, sums, triple, err := check(ctx, executable)
	if err != nil {
		return "", UpdateInfo{}, err
	}
	if archive.Name == "" {
		return "", UpdateInfo{Available: false, Version: installed.Tag, Revision: installed.Revision}, nil
	}
	archiveBytes, err := downloadAsset(ctx, archive)
	if err != nil {
		return "", UpdateInfo{}, err
	}
	sumsBytes, err := downloadAsset(ctx, sums)
	if err != nil {
		return "", UpdateInfo{}, err
	}
	if hashBytes(archiveBytes) != strings.TrimPrefix(archive.Digest, "sha256:") || hashBytes(sumsBytes) != strings.TrimPrefix(sums.Digest, "sha256:") {
		return "", UpdateInfo{}, errors.New("release bytes do not match GitHub's published SHA-256 digest")
	}
	listed, err := listedDigest(string(sumsBytes), archive.Name)
	if err != nil || listed != hashBytes(archiveBytes) {
		return "", UpdateInfo{}, errors.New("release archive does not match the published SHA256SUMS asset")
	}
	build, err := verifyArchive(archiveBytes, latest.TagName, triple)
	if err != nil {
		return "", UpdateInfo{}, fmt.Errorf("release package verification failed: %w", err)
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", UpdateInfo{}, err
	}
	stageDir := filepath.Join(cache, "amux", "updates", latest.TagName)
	if err := privateDirectory(stageDir); err != nil {
		return "", UpdateInfo{}, err
	}
	archivePath := filepath.Join(stageDir, "bundle.tar.gz")
	if err := writePrivateFile(archivePath, archiveBytes); err != nil {
		return "", UpdateInfo{}, err
	}
	metadata, _ := json.Marshal(stageMetadata{Tag: latest.TagName, Digest: hashBytes(archiveBytes)})
	if err := writePrivateFile(filepath.Join(stageDir, "verified.json"), append(metadata, '\n')); err != nil {
		return "", UpdateInfo{}, err
	}
	return archivePath, UpdateInfo{Available: true, Version: build.Tag, Revision: build.Revision, Archive: archivePath}, nil
}

func verifyArchive(data []byte, expectedTag, expectedTarget string) (Build, error) {
	var build Build
	if int64(len(data)) > maxReleaseBody || !versionPattern.MatchString(expectedTag) {
		return build, errors.New("invalid archive size or release tag")
	}
	root, err := os.MkdirTemp("", "amux-verify-")
	if err != nil {
		return build, err
	}
	defer os.RemoveAll(root)
	triple, cli, gui, _, err := target()
	if err != nil || triple != expectedTarget {
		return build, errors.New("unsupported package target for this computer")
	}
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return build, err
	}
	archive := tar.NewReader(reader)
	expected := map[string]bool{"LICENSE": true, "models.json": true, "BUILD.json": true, "SHA256SUMS": true}
	switch expectedTarget {
	case "x86_64-unknown-linux-gnu":
		expected["amux"], expected["amux-desktop"], expected["agent-tools"], expected["memory"] = true, true, true, true
	case "x86_64-pc-windows-msvc":
		expected["amux.exe"], expected["amux-desktop.exe"], expected["agent-tools.exe"], expected["memory.exe"] = true, true, true, true
	case "aarch64-apple-darwin":
		expected["amux"], expected["amux-desktop"], expected["agent-tools"], expected["memory"] = true, true, true, true
	default:
		return build, errors.New("unsupported package target")
	}
	seen := make(map[string]bool, len(expected))
	total := int64(0)
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return build, err
		}
		name := path.Clean(header.Name)
		if name != header.Name || strings.Contains(name, "\\") || name == "." || strings.HasPrefix(name, "../") || strings.Contains(name, "/") || !expected[name] || seen[name] || header.Typeflag != tar.TypeReg || header.Size < 0 {
			return build, fmt.Errorf("archive contains invalid member %q", header.Name)
		}
		total += header.Size
		if total > maxReleaseBody {
			return build, errors.New("archive expands beyond the allowed size")
		}
		mode := os.FileMode(0o600)
		if strings.HasPrefix(name, "amux") || strings.HasPrefix(name, "agent-tools") || name == "memory" || name == "memory.exe" {
			mode = 0o700
		}
		file, err := os.OpenFile(filepath.Join(root, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return build, err
		}
		_, copyErr := io.CopyN(file, archive, header.Size)
		closeErr := file.Close()
		if copyErr != nil {
			return build, copyErr
		}
		if closeErr != nil {
			return build, closeErr
		}
		seen[name] = true
	}
	if err := reader.Close(); err != nil {
		return build, err
	}
	if len(seen) != len(expected) {
		return build, errors.New("archive is missing required files")
	}
	buildData, err := os.ReadFile(filepath.Join(root, "BUILD.json"))
	if err != nil || json.Unmarshal(buildData, &build) != nil {
		return build, errors.New("invalid BUILD.json")
	}
	if build.Tag != expectedTag || build.Version != strings.TrimPrefix(expectedTag, "v") || build.Target != expectedTarget || build.CandidateOnly || !revisionPattern.MatchString(build.Revision) {
		return build, errors.New("BUILD.json does not match stable release metadata")
	}
	if (build.GoOS + "/" + build.GoArch) != runtime.GOOS+"/"+runtime.GOARCH {
		return build, errors.New("BUILD.json runtime differs from this computer")
	}
	checksums, err := readChecksums(filepath.Join(root, "SHA256SUMS"))
	if err != nil {
		return build, err
	}
	for name, expectedDigest := range checksums {
		actual, err := fileSHA256(filepath.Join(root, name))
		if err != nil || actual != expectedDigest {
			return build, fmt.Errorf("%s does not match SHA256SUMS", name)
		}
	}
	modelHash, modelErr := fileSHA256(filepath.Join(root, "models.json"))
	tools := []string{"agent-tools", "memory"}
	if runtime.GOOS == "windows" {
		tools = []string{"agent-tools.exe", "memory.exe"}
	}
	if build.GUIBinary != gui || checksums[cli] != build.BinarySHA256 || checksums[gui] != build.GUIBinarySHA256 || modelErr != nil || modelHash != build.ModelCatalogSHA256 || len(build.BundledSHA256) != len(tools) {
		return build, errors.New("package hashes differ from BUILD.json")
	}
	checksumNames := map[string]bool{cli: true, gui: true}
	for name, digest := range build.BundledSHA256 {
		if !isBundledTool(tools, name) || checksums[name] != digest {
			return build, fmt.Errorf("%s hash differs from BUILD.json", name)
		}
		checksumNames[name] = true
	}
	if len(checksums) != len(checksumNames) {
		return build, errors.New("SHA256SUMS has an unexpected file set")
	}
	for name := range checksumNames {
		if _, ok := checksums[name]; !ok {
			return build, errors.New("SHA256SUMS is missing a required executable")
		}
	}
	return build, nil
}

func isBundledTool(names []string, candidate string) bool {
	for _, name := range names {
		if name == candidate {
			return true
		}
	}
	return false
}

func readChecksums(file string) (map[string]string, error) {
	content, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	checksums := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(content)), "\n") {
		parts := strings.Fields(line)
		if len(parts) != 2 || len(parts[0]) != sha256.Size*2 {
			return nil, errors.New("invalid SHA256SUMS entry")
		}
		if _, err := hex.DecodeString(parts[0]); err != nil || filepath.Base(parts[1]) != parts[1] {
			return nil, errors.New("invalid SHA256SUMS path or digest")
		}
		if _, exists := checksums[parts[1]]; exists {
			return nil, errors.New("duplicate SHA256SUMS entry")
		}
		checksums[parts[1]] = strings.ToLower(parts[0])
	}
	if len(checksums) != 4 {
		return nil, errors.New("SHA256SUMS must cover CLI, desktop and bundled tools")
	}
	return checksums, nil
}

func listedDigest(content, name string) (string, error) {
	for _, line := range strings.Split(content, "\n") {
		parts := strings.Fields(line)
		if len(parts) == 2 && parts[1] == name && len(parts[0]) == sha256.Size*2 {
			if _, err := hex.DecodeString(parts[0]); err != nil {
				return "", err
			}
			return strings.ToLower(parts[0]), nil
		}
	}
	return "", fmt.Errorf("SHA256SUMS does not list %s", name)
}

func hashBytes(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func privateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("update staging path must be a real directory")
	}
	return os.Chmod(path, 0o700)
}

func writePrivateFile(path string, content []byte) error {
	temp, err := os.CreateTemp(filepath.Dir(path), ".amux-update-*")
	if err != nil {
		return err
	}
	defer os.Remove(temp.Name())
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(content); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(temp.Name(), path)
}
