package worksubmission

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const reportCITestSHA = "0123456789abcdef0123456789abcdef01234567"

func installReportCIGH(t *testing.T, pages string) string {
	t.Helper()
	dir := t.TempDir()
	gh := `#!/bin/sh
printf '%s\n' "$*" >> "$GH_CALL_LOG"
if [ "$1" = "repo" ]; then
  printf '%s\n' '{"nameWithOwner":"owner/repo"}'
  exit 0
fi
if [ "$1" != "api" ] || [ "$2" != "--paginate" ] || [ "$3" != "--method" ] || [ "$4" != "GET" ] || [ "$5" != "repos/owner/repo/actions/runs?head_sha=` + reportCITestSHA + `&per_page=100" ]; then
  exit 41
fi
cat "$GH_PAGES"
`
	path := filepath.Join(dir, "gh")
	if err := os.WriteFile(path, []byte(gh), 0700); err != nil {
		t.Fatal(err)
	}
	pagePath := filepath.Join(dir, "pages.json")
	if err := os.WriteFile(pagePath, []byte(pages), 0600); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, "calls.log")
	t.Setenv("GH_PAGES", pagePath)
	t.Setenv("GH_CALL_LOG", logPath)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

func TestReadReportCIPaginatesExactSHAAndPreservesAttemptsAndConclusions(t *testing.T) {
	first := `{"workflow_runs":[{"id":21,"run_attempt":3,"head_sha":"` + reportCITestSHA + `","name":"CI","event":"push","status":"completed","conclusion":"failure","html_url":"https://example.invalid/run/21"}]}`
	second := `{"workflow_runs":[{"id":22,"run_attempt":1,"head_sha":"` + reportCITestSHA + `","name":"CI","event":"push","status":"in_progress","conclusion":null,"html_url":"https://example.invalid/run/22"}]}`
	logPath := installReportCIGH(t, first+"\n"+second+"\n")
	runs, err := ReadReportCI(context.Background(), t.TempDir(), reportCITestSHA)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 2 || runs[0].ID != 21 || runs[0].Attempt != 3 || runs[0].HeadSHA != reportCITestSHA || string(runs[0].Conclusion) != `"failure"` || string(runs[1].Conclusion) != "null" {
		t.Fatalf("CI receipt = %+v", runs)
	}
	calls, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(calls), "repo view --json nameWithOwner") || !strings.Contains(string(calls), "api --paginate --method GET repos/owner/repo/actions/runs?head_sha="+reportCITestSHA+"&per_page=100") {
		t.Fatalf("unexpected gh invocations: %s", calls)
	}
}

func TestReadReportCIRejectsInvalidAndEmptyReceipts(t *testing.T) {
	valid := `{"id":1,"run_attempt":1,"head_sha":"` + reportCITestSHA + `","status":"completed","conclusion":"success"}`
	for _, tc := range []struct {
		name, pages string
	}{
		{name: "empty receipt", pages: `{"workflow_runs":[]}`},
		{name: "missing workflow_runs", pages: `{}`},
		{name: "unrelated SHA", pages: `{"workflow_runs":[{"id":1,"run_attempt":1,"head_sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","status":"completed","conclusion":"success"}]}`},
		{name: "missing attempt", pages: `{"workflow_runs":[{"id":1,"head_sha":"` + reportCITestSHA + `","status":"completed","conclusion":"success"}]}`},
		{name: "missing completed conclusion", pages: `{"workflow_runs":[{"id":1,"run_attempt":1,"head_sha":"` + reportCITestSHA + `","status":"completed"}]}`},
		{name: "invalid JSON", pages: `not json`},
		{name: "malformed later page", pages: `{"workflow_runs":[` + valid + `]}\n{broken`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			installReportCIGH(t, tc.pages)
			if _, err := ReadReportCI(context.Background(), t.TempDir(), reportCITestSHA); err == nil {
				t.Fatal("invalid CI receipt accepted")
			}
		})
	}
}

func TestReadReportCIRejectsInvalidInputsBeforeGitHubReads(t *testing.T) {
	logPath := installReportCIGH(t, `{"workflow_runs":[]}`)
	for _, tc := range []struct{ root, sha string }{{t.TempDir(), ""}, {"", reportCITestSHA}, {t.TempDir(), "HEAD"}, {t.TempDir(), strings.Repeat("g", 40)}} {
		if _, err := ReadReportCI(context.Background(), tc.root, tc.sha); err == nil {
			t.Fatalf("invalid input accepted: %+v", tc)
		}
	}
	if calls, err := os.ReadFile(logPath); err == nil && len(calls) != 0 {
		t.Fatalf("invalid input invoked gh: calls=%q", calls)
	} else if err != nil && !os.IsNotExist(err) {
		t.Fatalf("read gh invocation log: %v", err)
	}
}
