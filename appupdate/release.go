package appupdate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const repository = "nitecon/amux"
const maxReleaseBody = int64(512 << 20)

var versionPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

type Build struct {
	Revision           string            `json:"revision"`
	Version            string            `json:"version"`
	Tag                string            `json:"tag"`
	Target             string            `json:"target"`
	GoOS               string            `json:"goos"`
	GoArch             string            `json:"goarch"`
	GUIBinary          string            `json:"gui_binary"`
	BinarySHA256       string            `json:"binary_sha256"`
	GUIBinarySHA256    string            `json:"gui_binary_sha256"`
	BundledSHA256      map[string]string `json:"bundled_sha256"`
	ModelCatalogSHA256 string            `json:"model_catalog_sha256"`
	CandidateOnly      bool              `json:"candidate_only"`
}

type UpdateInfo struct {
	Available bool
	Version   string
	Revision  string
	Archive   string
}

type release struct {
	TagName         string         `json:"tag_name"`
	TargetCommitish string         `json:"target_commitish"`
	Draft           bool           `json:"draft"`
	Prerelease      bool           `json:"prerelease"`
	Assets          []releaseAsset `json:"assets"`
}

type releaseAsset struct {
	Name               string `json:"name"`
	Size               int64  `json:"size"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Digest             string `json:"digest"`
}

type stageMetadata struct {
	Tag    string `json:"tag"`
	Digest string `json:"digest"`
}

func target() (string, string, string, string, error) {
	cli, gui := "amux", "amux-desktop"
	if runtime.GOOS == "windows" {
		cli += ".exe"
		gui += ".exe"
	}
	triple := map[string]string{
		"linux/amd64":   "x86_64-unknown-linux-gnu",
		"windows/amd64": "x86_64-pc-windows-msvc",
		"darwin/arm64":  "aarch64-apple-darwin",
	}[runtime.GOOS+"/"+runtime.GOARCH]
	if triple == "" {
		return "", "", "", "", fmt.Errorf("amux updates are not available for %s/%s", runtime.GOOS, runtime.GOARCH)
	}
	prefix := map[string]string{
		"x86_64-unknown-linux-gnu": "amux-linux-x86_64",
		"x86_64-pc-windows-msvc":   "amux-windows-x86_64",
		"aarch64-apple-darwin":     "amux-macos-aarch64",
	}[triple]
	return triple, cli, gui, prefix, nil
}

func archiveExtension(triple string) string {
	if triple == "x86_64-unknown-linux-gnu" {
		return ".tar.gz"
	}
	return ".zip"
}

func latestRelease(ctx context.Context) (release, error) {
	var latest release
	if err := getJSON(ctx, "https://api.github.com/repos/"+repository+"/releases/latest", &latest); err != nil {
		return release{}, err
	}
	if latest.Draft || latest.Prerelease || !versionPattern.MatchString(latest.TagName) {
		return release{}, errors.New("latest GitHub release is not a stable semantic version")
	}
	return latest, nil
}

func findReleaseAssets(latest release, prefix, triple string) (releaseAsset, releaseAsset, error) {
	archiveName := prefix + "-" + latest.TagName + archiveExtension(triple)
	var archive, sums releaseAsset
	for _, asset := range latest.Assets {
		switch asset.Name {
		case archiveName:
			archive = asset
		case "SHA256SUMS":
			sums = asset
		}
	}
	if archive.Name == "" || sums.Name == "" {
		return releaseAsset{}, releaseAsset{}, fmt.Errorf("release %s has no package for %s", latest.TagName, triple)
	}
	for _, asset := range []releaseAsset{archive, sums} {
		if asset.Size <= 0 || asset.Size > maxReleaseBody || !validDigest(asset.Digest) ||
			asset.BrowserDownloadURL != "https://github.com/"+repository+"/releases/download/"+latest.TagName+"/"+asset.Name {
			return releaseAsset{}, releaseAsset{}, fmt.Errorf("release asset %s lacks valid size, SHA-256 provenance, or GitHub URL", asset.Name)
		}
	}
	return archive, sums, nil
}

func readInstalled(dir string) (Build, error) {
	if app := appBundle(dir); app != "" {
		dir = filepath.Join(app, "Contents", "Resources")
	}
	return readBuild(dir)
}

func readBuild(dir string) (Build, error) {
	var build Build
	data, err := os.ReadFile(filepath.Join(dir, "BUILD.json"))
	if err != nil {
		return build, err
	}
	if err := json.Unmarshal(bytes.TrimPrefix(data, []byte("\xef\xbb\xbf")), &build); err != nil {
		return build, err
	}
	// Earlier releases omit the explicit tag and runtime fields.
	if build.Tag == "" && versionPattern.MatchString("v"+build.Version) {
		build.Tag = "v" + build.Version
	}
	if build.GoOS == "" || build.GoArch == "" {
		switch build.Target {
		case "x86_64-unknown-linux-gnu":
			build.GoOS, build.GoArch = "linux", "amd64"
		case "x86_64-pc-windows-msvc":
			build.GoOS, build.GoArch = "windows", "amd64"
		case "aarch64-apple-darwin":
			build.GoOS, build.GoArch = "darwin", "arm64"
		}
	}
	if !versionPattern.MatchString(build.Tag) || build.Version != strings.TrimPrefix(build.Tag, "v") {
		return build, errors.New("invalid BUILD.json version")
	}
	return build, nil
}

func verifyInstalled(dir string, build Build, cli, gui string) error {
	triple, canonicalCLI, canonicalGUI, _, err := target()
	if err != nil {
		return err
	}
	flatSourceNames := runtime.GOOS == "linux" && cli == canonicalCLI+".bin" && gui == canonicalGUI+".bin"
	if (!flatSourceNames && (cli != canonicalCLI || gui != canonicalGUI)) || build.Target != triple || build.GUIBinary != canonicalGUI || !revisionPattern.MatchString(build.Revision) {
		return errors.New("installed build platform or revision metadata differs")
	}
	if app := appBundle(dir); app != "" {
		return verifyApp(app, cli, gui)
	}
	if triple == "aarch64-apple-darwin" {
		return errors.New("macOS updates require an amux app bundle")
	}
	files := map[string]string{cli: build.BinarySHA256, gui: build.GUIBinarySHA256,
		"models.json": build.ModelCatalogSHA256}
	tools := bundledToolNames(triple)
	if len(build.BundledSHA256) != len(tools) {
		return errors.New("BUILD.json does not include both bundled tools")
	}
	for _, name := range tools {
		files[name] = build.BundledSHA256[name]
	}
	for name, digest := range files {
		actual, err := fileSHA256(filepath.Join(dir, name))
		if err != nil || actual != digest {
			return fmt.Errorf("%s does not match BUILD.json", name)
		}
	}
	return nil
}

func getJSON(ctx context.Context, target string, output any) error {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, err := (&http.Client{Timeout: 20 * time.Second}).Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub release lookup returned HTTP %d", response.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(output)
}

func downloadAsset(ctx context.Context, asset releaseAsset) ([]byte, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, asset.BrowserDownloadURL, nil)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/octet-stream")
	client := &http.Client{
		Timeout: 2 * time.Minute,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			host := request.URL.Hostname()
			if request.URL.Scheme != "https" || (host != "github.com" && host != "release-assets.githubusercontent.com" && host != "objects.githubusercontent.com") {
				return fmt.Errorf("release download redirected to an untrusted host")
			}
			return nil
		},
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.ContentLength > maxReleaseBody {
		return nil, fmt.Errorf("release asset %s returned HTTP %d or exceeded size limit", asset.Name, response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxReleaseBody+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) != asset.Size {
		return nil, fmt.Errorf("release asset %s size differs from GitHub metadata", asset.Name)
	}
	return body, nil
}

func validDigest(value string) bool {
	if !strings.HasPrefix(value, "sha256:") {
		return false
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil && len(decoded) == sha256.Size
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	hasher := sha256.New()
	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func compareVersions(left, right string) int {
	a, b := versionPattern.FindStringSubmatch(left), versionPattern.FindStringSubmatch(right)
	if a == nil || b == nil {
		return 0
	}
	for index := 1; index <= 3; index++ {
		av, _ := strconv.Atoi(a[index])
		bv, _ := strconv.Atoi(b[index])
		if av < bv {
			return -1
		}
		if av > bv {
			return 1
		}
	}
	return 0
}
