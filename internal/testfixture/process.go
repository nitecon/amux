// Package testfixture provides portable subprocess fixtures for tests.
package testfixture

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// SameDirectory compares paths by filesystem identity, including Windows short names.
func SameDirectory(a, b string) bool {
	left, err := os.Stat(a)
	if err != nil {
		return false
	}
	right, err := os.Stat(b)
	return err == nil && left.IsDir() && right.IsDir() && os.SameFile(left, right)
}

// InstallGH places a copy of the test executable on PATH and configures its responses.
func InstallGH(t *testing.T, repository, pages, endpoint string) string {
	t.Helper()
	dir := t.TempDir()
	name := "gh"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	source, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0700); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "calls.log")
	t.Setenv("AMUX_TEST_GH_LOG", logPath)
	t.Setenv("AMUX_TEST_GH_REPO", repository)
	t.Setenv("AMUX_TEST_GH_PAGES", pages)
	t.Setenv("AMUX_TEST_GH_ENDPOINT", endpoint)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// RunGH handles invocations of the copied test executable, before tests start.
func RunGH() {
	name := filepath.Base(os.Args[0])
	logPath := os.Getenv("AMUX_TEST_GH_LOG")
	if (name != "gh" && name != "gh.exe") || logPath == "" {
		return
	}
	args := strings.Join(os.Args[1:], " ")
	log, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		os.Exit(1)
	}
	_, writeErr := fmt.Fprintln(log, args)
	closeErr := log.Close()
	if writeErr != nil || closeErr != nil {
		os.Exit(1)
	}
	repository := os.Getenv("AMUX_TEST_GH_REPO")
	if repository != "" && args == "repo view --json nameWithOwner" {
		fmt.Printf("{\"nameWithOwner\":%q}\n", repository)
		os.Exit(0)
	}
	if repository != "" && args == "api --paginate --method GET "+os.Getenv("AMUX_TEST_GH_ENDPOINT") {
		fmt.Println(os.Getenv("AMUX_TEST_GH_PAGES"))
		os.Exit(0)
	}
	os.Exit(41)
}
