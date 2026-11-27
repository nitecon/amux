package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var tagPattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
var revisionPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)

type platform struct{ os, arch, suffix, releaseName string }

var platforms = map[string]platform{
	"x86_64-unknown-linux-gnu": {"linux", "amd64", "", "linux-x86_64"},
	"x86_64-pc-windows-msvc":   {"windows", "amd64", ".exe", "windows-x86_64"},
	"aarch64-apple-darwin":     {"darwin", "arm64", "", "macos-aarch64"},
}

func (p platform) binaries() []string {
	return []string{"amux" + p.suffix, "amux-desktop" + p.suffix, "agent-tools" + p.suffix, "memory" + p.suffix}
}

func (p platform) archiveExtension() string {
	if p.os == "linux" {
		return ".tar.gz"
	}
	return ".zip"
}

func (p platform) archiveName(id string) string {
	return "amux-" + p.releaseName + "-" + id + p.archiveExtension()
}

func main() {
	log.SetFlags(0)
	if err := run(os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: amux-release tools|package|smoke|check-tag")
	}
	switch args[0] {
	case "check-tag":
		if len(args) != 2 || !tagPattern.MatchString(args[1]) {
			return fmt.Errorf("version must use vMAJOR.MINOR.PATCH")
		}
		return nil
	case "tools", "package", "smoke":
	default:
		return fmt.Errorf("unknown release operation %q", args[0])
	}
	flags := flag.NewFlagSet(args[0], flag.ContinueOnError)
	target := flags.String("target", "", "release target")
	output := flags.String("output", "", "output path")
	bundle := flags.String("bundle", "", "staged executables, license and model catalog")
	version := flags.String("version", "", "release version (vMAJOR.MINOR.PATCH)")
	revision := flags.String("revision", "", "source commit")
	candidate := flags.Bool("candidate-only", false, "exclude from stable updates")
	if err := flags.Parse(args[1:]); err != nil {
		return err
	}
	p, ok := platforms[*target]
	if !ok || *output == "" || flags.NArg() != 0 {
		return fmt.Errorf("valid --target and --output required")
	}
	switch args[0] {
	case "tools":
		return fetchTools(*target, p, *output)
	case "package":
		if !tagPattern.MatchString(*version) || !revisionPattern.MatchString(*revision) {
			return fmt.Errorf("invalid release version or revision")
		}
		return seal(*bundle, *target, p, *version, *revision, *candidate, *output)
	case "smoke":
		return smoke(*target, p, *output, *bundle, *revision)
	}
	return nil
}

func writeFiles(dir string, files map[string][]byte) error {
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	for name, data := range files {
		mode := os.FileMode(0644)
		switch strings.TrimSuffix(filepath.Base(name), ".exe") {
		case "amux", "amux-desktop", "agent-tools", "memory":
			mode = 0755
		}
		filename := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(filename), 0755); err != nil {
			return err
		}
		if err := os.WriteFile(filename, data, mode); err != nil {
			return err
		}
	}
	return nil
}
