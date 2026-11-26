package worksubmission

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/nitecon/amux/conversation"
)

func TestToolApprovalSubjectReadsEachProviderShape(t *testing.T) {
	for name, test := range map[string]struct{ data, want string }{
		"claude bash":     {`{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"git  grep -i\tfoo"}}`, "git  grep -i\tfoo"},
		"claude tool":     {`{"subtype":"can_use_tool","tool_name":"WebFetch","input":{"url":"https://x"}}`, "WebFetch"},
		"codex command":   {`{"threadId":"t","command":"git log -3"}`, "git log -3"},
		"codex argv":      {`{"command":["git","log","-3"]}`, "git log -3"},
		"codex file":      {`{"threadId":"t","reason":"edit"}`, ""},
		"acp raw command": {`{"toolCall":{"name":"run_command","rawInput":{"command":"git status"}}}`, "git status"},
		"acp tool":        {`{"toolCall":{"name":"read_file"}}`, "read_file"},
		"invalid":         {`not json`, ""},
	} {
		if got := ToolApprovalSubject(conversation.Event{Data: json.RawMessage(test.data)}); got != test.want {
			t.Errorf("%s: subject = %q, want %q", name, got, test.want)
		}
	}
}

func TestDirectoryToolApprovalsMergeGlobalsAndMatchOnlyCleanedExactDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "prefs.json")
	root := filepath.Join(t.TempDir(), "checkout")
	if err := AddToolApproval(path, "WebFetch"); err != nil {
		t.Fatal(err)
	}
	if err := AddDirectoryToolApproval(path, filepath.Join(root, "."), "git log *"); err != nil {
		t.Fatal(err)
	}
	if err := AddDirectoryToolApproval(path, root, "git log *"); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		directory string
		want      []string
	}{
		{root, []string{"WebFetch", "git log *"}},
		{filepath.Join(root, "nested"), []string{"WebFetch"}},
		{"relative", []string{"WebFetch"}},
	} {
		got, err := LoadDirectoryToolApprovals(path, test.directory)
		if err != nil || !slices.Equal(got, test.want) {
			t.Errorf("rules for %q = %q, %v; want %q", test.directory, got, err, test.want)
		}
	}
	for _, bad := range []string{"", "relative"} {
		if err := AddDirectoryToolApproval(path, bad, "git log"); err == nil {
			t.Errorf("accepted non-absolute directory %q", bad)
		}
	}
}

func TestToolApprovalDirectoryUsesActualProviderCWD(t *testing.T) {
	repo := filepath.Join(t.TempDir(), "repo")
	other := filepath.Join(t.TempDir(), "other")
	fallback := filepath.Join(t.TempDir(), "fallback")
	for _, test := range []struct {
		data           any
		fallback, want string
	}{
		{map[string]any{"cwd": filepath.Join(repo, "sub", "..")}, fallback, repo},
		{map[string]any{"input": map[string]string{"cwd": other}}, fallback, other},
		{map[string]any{"toolCall": map[string]any{"rawInput": map[string]string{"cwd": repo}}}, fallback, repo},
		{map[string]any{}, filepath.Join(fallback, "."), fallback},
		{map[string]any{"cwd": "relative"}, fallback, ""},
	} {
		data, err := json.Marshal(test.data)
		if err != nil {
			t.Fatal(err)
		}
		got := ToolApprovalDirectory(conversation.Event{Data: data}, test.fallback)
		if got != test.want {
			t.Errorf("directory = %q, want %q", got, test.want)
		}
	}
}

func TestMatchToolApprovalUsesLiteralGlobAndRejectsShellControl(t *testing.T) {
	rules := []string{"git grep *", "WebFetch"}
	for subject, want := range map[string]bool{
		"git grep -i foo":          true,
		"git grep":                 false,
		"git log":                  false,
		"WebFetch":                 true,
		"git grep x; rm -rf ~":     false,
		"git grep x && rm -rf ~":   false,
		"git grep x | sh":          false,
		"git grep $(whoami)":       false,
		"git grep `whoami`":        false,
		"git grep x > /etc/passwd": false,
		"git grep x\nrm -rf ~":     false,
		"git grep [a].*(literal)?": true,
		"xgit grep foo":            false,
		"":                         false,
	} {
		if got := MatchToolApproval(rules, subject); got != want {
			t.Errorf("match %q = %v, want %v", subject, got, want)
		}
	}
	if MatchToolApproval([]string{"git (log)?"}, "git ") {
		t.Fatal("regular expression syntax in a rule must be literal")
	}
}

func TestMatchToolApprovalKeepsWhitespaceAndQuotesLiteral(t *testing.T) {
	for _, test := range []struct {
		rule, subject string
		want          bool
	}{
		{"printf 'a b'", "printf 'a b'", true},
		{"printf 'a b'", "printf 'a  b'", false},
		{"cat 'my file.txt'", "cat 'my  file.txt'", false},
		{"cat 'my file.txt'", "cat my file.txt", false},
		{"git log -3", "git  log -3", false},
		{"git log -3", "git\tlog -3", false},
		{"git log *", "git log  -3", true},
		{"printf *", "printf 'a  b'", true},
	} {
		if got := MatchToolApproval([]string{test.rule}, test.subject); got != test.want {
			t.Errorf("rule %q match %q = %v, want %v", test.rule, test.subject, got, test.want)
		}
	}
}

func TestAddToolApprovalConcurrentSavesKeepEveryRule(t *testing.T) {
	path := filepath.Join(t.TempDir(), "work-preferences.json")
	if err := SavePreferences(path, Preferences{Mode: PullRequestMode, BranchPrefix: "x/"}); err != nil {
		t.Fatal(err)
	}
	const count = 30
	var wait sync.WaitGroup
	errs := make(chan error, count+1)
	for i := range count {
		wait.Add(1)
		go func() {
			defer wait.Done()
			errs <- AddToolApproval(path, fmt.Sprintf("tool-%d", i))
		}()
	}
	wait.Add(1)
	go func() {
		defer wait.Done()
		errs <- SavePreferences(path, Preferences{Mode: PullRequestMode, BranchPrefix: "y/"})
	}()
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	rules, err := LoadToolApprovals(path)
	if err != nil || len(rules) != count {
		t.Fatalf("saved %d of %d rules: %v", len(rules), count, err)
	}
	if preferences, err := LoadPreferences(path); err != nil || preferences.BranchPrefix != "y/" {
		t.Fatalf("concurrent preference save lost: %+v, %v", preferences, err)
	}
}

func TestToolApprovalOptionSelectsAllowOnce(t *testing.T) {
	event := conversation.Event{Kind: "permission", Data: json.RawMessage(`{"command":"git log -3"}`),
		Options: []conversation.PermissionOption{{ID: "decline", Kind: "reject_once"}, {ID: "accept", Kind: "allow_once"}}}
	if got := ToolApprovalOption([]string{"git log *"}, event); got != "accept" {
		t.Fatalf("option = %q, want accept", got)
	}
	if got := ToolApprovalOption([]string{"git status"}, event); got != "" {
		t.Fatalf("unmatched option = %q", got)
	}
	event.Options = event.Options[:1]
	if got := ToolApprovalOption([]string{"git log *"}, event); got != "" {
		t.Fatalf("request without allow_once option = %q", got)
	}
}

func TestAddToolApprovalPreservesWorkSubmissionAndDeduplicates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "work-preferences.json")
	if err := SavePreferences(path, Preferences{Mode: PullRequestMode, BranchPrefix: "x/"}); err != nil {
		t.Fatal(err)
	}
	for _, rule := range []string{"  git grep * ", "git grep *", "WebFetch"} {
		if err := AddToolApproval(path, rule); err != nil {
			t.Fatal(err)
		}
	}
	rules, err := LoadToolApprovals(path)
	if err != nil || !slices.Equal(rules, []string{"git grep *", "WebFetch"}) {
		t.Fatalf("rules = %q, %v", rules, err)
	}
	preferences, err := LoadPreferences(path)
	if err != nil || preferences.Mode != PullRequestMode || preferences.BranchPrefix != "x/" {
		t.Fatalf("work submission preferences changed: %+v, %v", preferences, err)
	}
	if err := SavePreferences(path, Preferences{Mode: CommitAndPush, BranchPrefix: "y/"}); err != nil {
		t.Fatal(err)
	}
	if rules, _ := LoadToolApprovals(path); len(rules) != 2 {
		t.Fatalf("saving work submission dropped rules: %q", rules)
	}
	for _, bad := range []string{"", "  ", "*", "git grep * | sh", "git log; rm"} {
		if err := AddToolApproval(path, bad); err == nil {
			t.Errorf("rule %q was accepted", bad)
		}
	}
	if err := os.WriteFile(path, []byte(`{"tool_approvals":"git *"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := AddToolApproval(path, "git log"); err == nil {
		t.Fatal("invalid existing rules were overwritten")
	}
}

func TestToolApprovalRuleDraftKeepsRequestAsteriskLiteral(t *testing.T) {
	path := filepath.Join(t.TempDir(), "work-preferences.json")
	subject := `git grep 'a*b' C:\\x\*`
	draft := ToolApprovalRuleDraft(subject)
	if err := AddToolApproval(path, draft); err != nil {
		t.Fatal(err)
	}
	if err := AddToolApproval(path, "git log *"); err != nil {
		t.Fatal(err)
	}
	rules, err := LoadToolApprovals(path)
	if err != nil {
		t.Fatal(err)
	}
	for subject, want := range map[string]bool{
		subject:                   true,  // unchanged draft approves exactly this request
		`git grep 'acb' C:\\x\*`:  false, // request "*" stays literal
		`git grep 'a*b' C:\\x\yz`: false,
		"git log -3 --oneline":    true, // user-typed "*" widens
		"git log":                 false,
	} {
		if got := MatchToolApproval(rules, subject); got != want {
			t.Errorf("match %q = %v, want %v", subject, got, want)
		}
	}
	if !MatchToolApproval([]string{`a\\*`}, `a\anything`) {
		t.Error(`"\\" followed by "*" must be a literal backslash then a wildcard`)
	}
}
