package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/nitecon/amux/appupdate"
)

func verifyBundle(files map[string][]byte, target string, p platform) (appupdate.Build, error) {
	var b appupdate.Build
	if err := json.Unmarshal(files["BUILD.json"], &b); err != nil {
		return b, err
	}
	if b.Target != target || b.GoOS != p.os || b.GoArch != p.arch || !tagPattern.MatchString(b.Tag) || b.Version != strings.TrimPrefix(b.Tag, "v") || !revisionPattern.MatchString(b.Revision) || len(files) != 8 {
		return b, fmt.Errorf("invalid Go bundle metadata or file set")
	}
	names := p.binaries()
	expected := []string{b.BinarySHA256, b.GUIBinarySHA256, b.BundledSHA256[names[2]], b.BundledSHA256[names[3]]}
	if b.GUIBinary != names[1] || len(b.BundledSHA256) != 2 {
		return b, fmt.Errorf("invalid Go bundle executable metadata")
	}
	for i, name := range names {
		data, ok := files[name]
		if !ok || digest(data) != expected[i] || digest(data) != checksum(files["SHA256SUMS"], name) {
			return b, fmt.Errorf("%s failed checksum verification", name)
		}
	}
	if _, ok := files["LICENSE"]; !ok {
		return b, fmt.Errorf("missing bundle license")
	}
	if data, ok := files["models.json"]; !ok || digest(data) != b.ModelCatalogSHA256 {
		return b, fmt.Errorf("model catalog failed checksum verification")
	}
	return b, nil
}

func smoke(target string, p platform, directory, archivePath, revision string) error {
	previous, baseline, err := previousBundle(target, p)
	if err != nil {
		return err
	}
	if err = writeFiles(directory, previous); err != nil {
		return err
	}
	if err = smokeExecutables(directory, p, baseline.GoOS == ""); err != nil {
		return err
	}
	file, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	data, err := readBounded(file)
	file.Close()
	if err != nil {
		return err
	}
	files, err := selectArchive(data, false, flatName)
	if err != nil {
		return err
	}
	b, err := verifyBundle(files, target, p)
	if err != nil {
		return err
	}
	if b.Revision != revision {
		return fmt.Errorf("candidate revision does not match %s", revision)
	}
	if err = writeFiles(directory, files); err != nil {
		return err
	}
	fmt.Printf("Replaced baseline %s (%s) with %s (%s)\n", baseline.Version, baseline.Revision, b.Version, b.Revision)
	return smokeExecutables(directory, p, false)
}

func smokeExecutables(dir string, p platform, rust bool) error {
	dir, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	names := p.binaries()
	cli := exec.Command(filepath.Join(dir, names[0]))
	if rust {
		cli.Args = append(cli.Args, "--version")
	}
	output, err := cli.CombinedOutput()
	if rust {
		if err != nil || !regexp.MustCompile(`amux [0-9]+\.[0-9]+\.[0-9]+`).Match(output) {
			return fmt.Errorf("Rust CLI smoke failed: %s (%v)", output, err)
		}
	} else {
		var exit *exec.ExitError
		if !errors.As(err, &exit) || exit.ExitCode() != 1 || !bytes.Contains(output, []byte("usage: amux")) {
			return fmt.Errorf("Go CLI smoke failed: %s (%v)", output, err)
		}
	}
	for i, name := range names[1:] {
		flag := "--version"
		if i == 0 {
			flag = "--help"
		} else if rust {
			break
		}
		cmd := exec.Command(filepath.Join(dir, name), flag)
		cmd.Env = append(os.Environ(), "AGENT_TOOLS_NO_UPDATE=1")
		if output, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%s smoke failed: %s (%v)", name, output, err)
		}
	}
	return nil
}
