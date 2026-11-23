package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nitecon/amux/internal/testfixture"
)

func TestTasksReportOfflineUsesFixtureAndDoesNotWriteRepositoryState(t *testing.T) {
	root := t.TempDir()
	gitReportFixture(t, root, "init", "--initial-branch=main", "--template=")
	gitReportFixture(t, root, "config", "user.name", "Report Fixture")
	gitReportFixture(t, root, "config", "user.email", "report@example.invalid")
	if err := os.WriteFile(filepath.Join(root, "fixture.txt"), []byte("fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitReportFixture(t, root, "add", "fixture.txt")
	gitReportFixture(t, root, "commit", "-m", "fixture report commit")
	if err := os.WriteFile(filepath.Join(root, "fixture.txt"), []byte("next fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitReportFixture(t, root, "commit", "-am", "second fixture commit")
	headBefore := gitReportFixture(t, root, "rev-parse", "HEAD")
	statusBefore := gitReportFixture(t, root, "status", "--porcelain=v1", "-z")

	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	stateDir := filepath.Join(t.TempDir(), "absent-state")
	var stdout, stderr bytes.Buffer
	err = runTasks(context.Background(), []string{"--json", "report", "HEAD~1..HEAD", "--offline"}, &stdout, &stderr, func(name string) string {
		if name == "AMUX_STATE_DIR" {
			return stateDir
		}
		return ""
	})
	if err != nil {
		t.Fatalf("report: %v (stderr %q)", err, stderr.String())
	}
	var result struct {
		Commits []struct {
			Subject string `json:"subject"`
		} `json:"commits"`
		Partial []string `json:"partial"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("invalid report JSON %q: %v", stdout.String(), err)
	}
	if len(result.Commits) != 1 || result.Commits[0].Subject != "second fixture commit" {
		t.Fatalf("unexpected report: %+v", result)
	}
	if len(result.Partial) == 0 || !strings.Contains(result.Partial[0], "Offline") {
		t.Fatalf("offline partial coverage not explicit: %v", result.Partial)
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("offline report created state directory: stat error %v", err)
	}
	if got := gitReportFixture(t, root, "rev-parse", "HEAD"); got != headBefore {
		t.Fatalf("report changed HEAD: %s -> %s", headBefore, got)
	}
	if got := gitReportFixture(t, root, "status", "--porcelain=v1", "-z"); got != statusBefore {
		t.Fatalf("report changed working tree: %q -> %q", statusBefore, got)
	}
}

func TestTasksReportOnlineUsesSyntheticGatewayAndStubGitHub(t *testing.T) {
	root := t.TempDir()
	gitReportFixture(t, root, "init", "--initial-branch=main", "--template=")
	gitReportFixture(t, root, "config", "user.name", "Report Fixture")
	gitReportFixture(t, root, "config", "user.email", "report@example.invalid")
	if err := os.WriteFile(filepath.Join(root, "fixture.txt"), []byte("one\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitReportFixture(t, root, "add", "fixture.txt")
	gitReportFixture(t, root, "commit", "-m", "base")
	if err := os.WriteFile(filepath.Join(root, "fixture.txt"), []byte("two\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitReportFixture(t, root, "commit", "-am", "reported commit")
	sha := gitReportFixture(t, root, "rev-parse", "HEAD")

	var gatewayCalls int
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gatewayCalls++
		if r.Method != http.MethodGet || r.URL.Path != "/v1/projects/demo/tasks" || r.Header.Get("Authorization") != "Bearer fixture-key" {
			t.Errorf("unexpected gateway request %s %s auth=%q", r.Method, r.URL, r.Header.Get("Authorization"))
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte("[]"))
	}))
	defer host.Close()
	pages := `{"workflow_runs":[{"id":1,"run_attempt":1,"head_sha":"` + sha + `","status":"completed","conclusion":"success"}]}`
	testfixture.InstallGH(t, "fixture/project", pages, "repos/fixture/project/actions/runs?head_sha="+sha+"&per_page=100")
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	var stdout, stderr bytes.Buffer
	err = runTasks(context.Background(), []string{"--server", host.URL, "--project", "demo", "--json", "report", "HEAD~1..HEAD"}, &stdout, &stderr, func(string) string { return "fixture-key" })
	if err != nil {
		t.Fatalf("report: %v (stderr %q)", err, stderr.String())
	}
	var report struct {
		CIRuns []struct {
			HeadSHA string `json:"head_sha"`
		} `json:"ci_runs"`
		Partial []string `json:"partial"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("invalid report JSON: %v\n%s", err, stdout.String())
	}
	if gatewayCalls != 1 || len(report.CIRuns) != 1 || report.CIRuns[0].HeadSHA != sha {
		t.Fatalf("gateway calls=%d CI=%+v", gatewayCalls, report.CIRuns)
	}
	if len(report.Partial) != 0 {
		t.Fatalf("unexpected partial coverage: %v", report.Partial)
	}
}

func TestTasksReportMarksMissingGatewayAndGitHubCoveragePartial(t *testing.T) {
	root := t.TempDir()
	gitReportFixture(t, root, "init", "--initial-branch=main", "--template=")
	gitReportFixture(t, root, "config", "user.name", "Report Fixture")
	gitReportFixture(t, root, "config", "user.email", "report@example.invalid")
	if err := os.WriteFile(filepath.Join(root, "fixture.txt"), []byte("fixture\n"), 0600); err != nil {
		t.Fatal(err)
	}
	gitReportFixture(t, root, "add", "fixture.txt")
	gitReportFixture(t, root, "commit", "-m", "fixture")
	testfixture.InstallGH(t, "", "", "")
	old, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(root); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })
	var stdout, stderr bytes.Buffer
	err = runTasks(context.Background(), []string{"--json", "report", "HEAD~0..HEAD"}, &stdout, &stderr, func(string) string { return "" })
	if err != nil {
		t.Fatalf("report: %v (stderr %q)", err, stderr.String())
	}
	var report struct {
		Partial []string `json:"partial"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &report); err != nil {
		t.Fatalf("invalid report JSON: %v", err)
	}
	joined := strings.Join(report.Partial, "\n")
	if !strings.Contains(joined, "Gateway task/review facts unavailable") || !strings.Contains(joined, "GitHub CI facts unavailable") {
		t.Fatalf("missing explicit unavailable coverage markers: %v", report.Partial)
	}
}

func gitReportFixture(t *testing.T, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}
