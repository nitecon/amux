package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"strings"
	"time"

	"github.com/nitecon/amux/update"
)

var httpClient = &http.Client{Timeout: 2 * time.Minute}

type asset struct {
	Name        string
	URL         string
	DownloadURL string `json:"browser_download_url"`
	Digest      string
}

type release struct {
	Tag    string `json:"tag_name"`
	Assets []asset
}

func download(address, accept string) ([]byte, int, error) {
	req, err := http.NewRequest(http.MethodGet, address, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", accept)
	if req.URL.Scheme == "https" && req.URL.Host == "api.github.com" && strings.HasPrefix(req.URL.Path, "/repos/nitecon/amux/releases/") {
		if token := os.Getenv("AMUX_RELEASE_GITHUB_TOKEN"); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.StatusCode, fmt.Errorf("%s returned HTTP %d", address, resp.StatusCode)
	}
	data, err := readBounded(resp.Body)
	return data, resp.StatusCode, err
}

func downloadGitHub(address string) ([]byte, error) {
	u, err := url.Parse(address)
	if err != nil || u.Scheme != "https" || u.User != nil || (u.Host != "github.com" && u.Host != "api.github.com") {
		return nil, fmt.Errorf("invalid GitHub asset URL")
	}
	data, _, err := download(address, "application/octet-stream")
	return data, err
}

func readRelease(endpoint string) (release, error) {
	var r release
	data, _, err := download("https://api.github.com/repos/"+endpoint, "application/vnd.github+json")
	if err != nil {
		return r, err
	}
	err = json.Unmarshal(data, &r)
	return r, err
}

func namedAsset(r release, name string) (asset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return asset{}, false
}

func checksum(sums []byte, name string) string {
	for _, line := range strings.Split(strings.TrimPrefix(string(sums), "\ufeff"), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && strings.TrimPrefix(fields[1], "*") == name {
			return strings.ToLower(fields[0])
		}
	}
	return ""
}

func pinnedAsset(repo, tag, name string) ([]byte, error) {
	data, status, err := download("https://github.com/"+repo+"/releases/download/"+tag+"/"+name, "application/octet-stream")
	if status < 500 || status >= 600 {
		return data, err
	}
	r, err := readRelease(repo + "/releases/tags/" + tag)
	if err != nil {
		return nil, err
	}
	a, ok := namedAsset(r, name)
	if !ok {
		return nil, fmt.Errorf("missing pinned asset %s", name)
	}
	return downloadGitHub(a.URL)
}

func fetchTools(target string, p platform, output string) error {
	var pins map[string]struct {
		Repo, Version, Checksums string
		Assets, SHA256           map[string]string
	}
	data, err := os.ReadFile("packaging/bundled-tools.json")
	if err != nil {
		return err
	}
	if err = json.Unmarshal(data, &pins); err != nil {
		return err
	}
	files := map[string][]byte{}
	for _, tool := range []string{"agent-tools", "memory"} {
		pin := pins[tool]
		name := pin.Assets[target]
		if name == "" {
			return fmt.Errorf("no %s asset for %s", tool, target)
		}
		archive, err := pinnedAsset(pin.Repo, pin.Version, name)
		if err != nil {
			return err
		}
		expected := pin.SHA256[name]
		if pin.Checksums != "" {
			sums, err := pinnedAsset(pin.Repo, pin.Version, pin.Checksums)
			if err != nil {
				return err
			}
			expected = checksum(sums, name)
		}
		if digest(archive) != expected {
			return fmt.Errorf("%s checksum mismatch", name)
		}
		binary := tool + p.suffix
		selected, err := selectArchive(archive, strings.HasSuffix(name, ".zip"), func(name string) string {
			if path.Base(name) == binary {
				return binary
			}
			return ""
		})
		if err != nil {
			return err
		}
		if len(selected) != 1 {
			return fmt.Errorf("archive does not contain %s", binary)
		}
		files[binary] = selected[binary]
	}
	return writeFiles(output, files)
}

func verifiedAsset(a asset) ([]byte, error) {
	data, err := downloadGitHub(a.DownloadURL)
	if err != nil {
		return nil, err
	}
	if a.Digest != "" && a.Digest != "sha256:"+digest(data) {
		return nil, fmt.Errorf("%s differs from GitHub checksum", a.Name)
	}
	return data, nil
}

func previousBundle(target string, p platform) (map[string][]byte, update.Build, error) {
	var empty update.Build
	r, err := readRelease("nitecon/amux/releases/latest")
	if err != nil {
		return nil, empty, err
	}
	if !tagPattern.MatchString(r.Tag) {
		return nil, empty, fmt.Errorf("invalid baseline release tag")
	}
	name := p.archiveName(r.Tag)
	a, _ := namedAsset(r, name)
	sums, ok := namedAsset(r, "SHA256SUMS")
	if a.Name == "" || !ok {
		return nil, empty, fmt.Errorf("missing baseline assets for %s", target)
	}
	archive, err := verifiedAsset(a)
	if err != nil {
		return nil, empty, err
	}
	sumBytes, err := verifiedAsset(sums)
	if err != nil {
		return nil, empty, err
	}
	if digest(archive) != checksum(sumBytes, name) {
		return nil, empty, fmt.Errorf("baseline archive checksum mismatch")
	}
	selector := flatName
	if p.os == "darwin" {
		selector = macMemberName
	}
	files, err := selectArchive(archive, p.archiveExtension() == ".zip", selector)
	if err != nil {
		return nil, empty, err
	}
	var build update.Build
	payload := bundlePayload(files, p)
	if err = json.Unmarshal(payload["BUILD.json"], &build); err != nil {
		return nil, empty, err
	}
	if build.GoOS != "" {
		if build.Tag != r.Tag || build.CandidateOnly {
			return nil, empty, fmt.Errorf("invalid Go baseline metadata")
		}
		if _, err = verifyBundle(payload, target, p); err != nil {
			return nil, empty, err
		}
	}
	return files, build, nil
}
