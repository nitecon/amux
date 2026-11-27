package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

var revisionPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)

func Stage(ctx context.Context, executable string) (string, UpdateInfo, error) {
	triple, _, _, prefix, err := target()
	if err != nil {
		return "", UpdateInfo{}, err
	}
	if executable != "" {
		if err := checkInstallRestrictions(filepath.Dir(executable)); err != nil {
			return "", UpdateInfo{}, err
		}
	}
	latest, err := latestRelease(ctx)
	if err != nil {
		return "", UpdateInfo{}, err
	}
	if executable != "" {
		dir := filepath.Dir(executable)
		if installed, err := readInstalled(dir); err == nil {
			if compareVersions(latest.TagName, installed.Tag) <= 0 {
				return "", UpdateInfo{Version: installed.Tag, Revision: installed.Revision}, nil
			}
		}
	}
	archive, sums, err := findReleaseAssets(latest, prefix, triple)
	if err != nil {
		return "", UpdateInfo{}, err
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
	if revisionPattern.MatchString(latest.TargetCommitish) && build.Revision != latest.TargetCommitish {
		return "", UpdateInfo{}, errors.New("package revision differs from the GitHub release commit")
	}
	cache, err := os.UserCacheDir()
	if err != nil {
		return "", UpdateInfo{}, err
	}
	stageDir := filepath.Join(cache, "amux", "updates", latest.TagName)
	if err := privateDirectory(stageDir); err != nil {
		return "", UpdateInfo{}, err
	}
	archivePath := filepath.Join(stageDir, "bundle"+archiveExtension(triple))
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
	root, err := os.MkdirTemp("", "amux-verify-")
	if err != nil {
		return build, err
	}
	defer os.RemoveAll(root)
	return verifyArchiveAt(data, root, expectedTag, expectedTarget)
}

func verifyArchiveAt(data []byte, root, expectedTag, expectedTarget string) (Build, error) {
	var build Build
	if int64(len(data)) > maxReleaseBody || !versionPattern.MatchString(expectedTag) {
		return build, errors.New("invalid archive size or release tag")
	}
	triple, cli, gui, _, err := target()
	if err != nil || triple != expectedTarget {
		return build, errors.New("unsupported package target for this computer")
	}
	if err := extractArchive(data, root, triple); err != nil {
		return build, err
	}
	binaries := root
	bundle, hasBundle := native.(bundleUpdater)
	if hasBundle {
		binaries = bundle.ArchiveBinaryDir(root)
	}
	build, err = readInstalled(binaries)
	if err != nil {
		return build, fmt.Errorf("invalid BUILD.json: %w", err)
	}
	if build.Tag != expectedTag || build.CandidateOnly {
		return build, errors.New("BUILD.json does not match stable release metadata")
	}
	if (build.GoOS + "/" + build.GoArch) != runtime.GOOS+"/"+runtime.GOARCH {
		return build, errors.New("BUILD.json runtime differs from this computer")
	}
	if err := verifyInstalled(binaries, build, cli, gui); err != nil {
		return build, err
	}
	if hasBundle {
		return build, nil
	}
	if _, err := os.Stat(filepath.Join(root, "LICENSE")); err != nil {
		return build, err
	}
	checksums, err := readChecksums(filepath.Join(root, "SHA256SUMS"))
	if err != nil {
		return build, err
	}
	if checksums[cli] != build.BinarySHA256 || checksums[gui] != build.GUIBinarySHA256 {
		return build, errors.New("executable hashes in SHA256SUMS differ from BUILD.json")
	}
	for _, name := range bundledToolNames(triple) {
		if checksums[name] != build.BundledSHA256[name] {
			return build, fmt.Errorf("%s hash in SHA256SUMS differs from BUILD.json", name)
		}
	}
	return build, nil
}

func readChecksums(file string) (map[string]string, error) {
	content, err := os.ReadFile(file)
	if err != nil {
		return nil, err
	}
	checksums := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(bytes.TrimPrefix(content, []byte("\xef\xbb\xbf")))), "\n") {
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
	var digest string
	for _, line := range strings.Split(strings.TrimPrefix(content, "\ufeff"), "\n") {
		parts := strings.Fields(line)
		if len(parts) == 2 && parts[1] == name && len(parts[0]) == sha256.Size*2 {
			if _, err := hex.DecodeString(parts[0]); err != nil {
				return "", err
			}
			if digest != "" {
				return "", fmt.Errorf("SHA256SUMS lists %s more than once", name)
			}
			digest = strings.ToLower(parts[0])
		}
	}
	if digest != "" {
		return digest, nil
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
