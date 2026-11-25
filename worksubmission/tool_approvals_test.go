package worksubmission

import (
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/nitecon/amux/conversation"
)

func TestToolApprovalSubjectReadsEachProviderShape(t *testing.T) {
	for name, test := range map[string]struct{ data, want string }{
		"claude bash":     {`{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"git  grep -i foo"}}`, "git grep -i foo"},
		"claude tool":     {`{"subtype":"can_use_tool","tool_name":"WebFetch","input":{"url":"https://x"}}`, "WebFetch"},
		"codex command":   {`{"threadId":"t","command":"git log -3"}`, "git log -3"},
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
	for _, rule := range []string{"git  grep *", "git grep *", "WebFetch"} {
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
