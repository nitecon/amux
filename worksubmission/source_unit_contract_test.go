package worksubmission

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestSourceAttributionLinesAreRemovedAndBlankLinesCollapsed(t *testing.T) {
	message := "\nFix the parser\n\n\nHandles empty input.\n\nCo-Authored-By: Claude <noreply@anthropic.com>\nco-authored-by : x <y>\nAmuxID: 123\n🤖 Generated with [Claude Code](https://claude.com/claude-code)\nRefs: #4\n"
	if got, want := cleanMessage(message), "Fix the parser\n\nHandles empty input.\n\nRefs: #4"; got != want {
		t.Fatalf("cleanMessage() = %q, want %q", got, want)
	}
	if got := cleanMessage("Generated with care"); got != "Generated with care" {
		t.Fatalf("cleanMessage() removed ordinary text: %q", got)
	}

	f := newFixture(t)
	request := Request{
		Root: f.root, TaskID: uuid.Nil.String(),
		Message: "Subject\nCo-authored-by: a <b>", Paths: []string{"base.txt"},
	}
	write(t, f.root+"/base.txt", "changed for nil UUID source contract\n")
	out, err := Submit(context.Background(), request, DefaultPreferences(), nil)
	if err != nil {
		t.Fatal(err)
	}
	got := f.git(t, f.root, "show", "-s", "--format=%B", out.Reference)
	want := "Subject\n\nAmuxID: " + uuid.Nil.String()
	if got != want {
		t.Fatalf("submitted commit message = %q, want %q", got, want)
	}

	request.Message = "CO-AUTHORED-BY: a <b>"
	if _, err := Submit(context.Background(), request, DefaultPreferences(), nil); err == nil || !strings.Contains(err.Error(), "empty once attribution lines are removed") {
		t.Fatalf("attribution-only submission error = %v", err)
	}
}

func TestSourcePreferencesDefaultToDirectPushAndUseFullUUIDBranchNames(t *testing.T) {
	preferences := DefaultPreferences()
	if preferences.Mode != CommitAndPush || preferences.BranchPrefix != "amux/" {
		t.Fatalf("default preferences = %+v", preferences)
	}
	first := "01a19a44-e705-743e-86f7-126db8e7b5eb"
	second := "01a19a44-e705-7c01-9a2b-3c4d5e6f7081"
	if got, want := branchName(" amux/ ", first), "amux/"+first; got != want {
		t.Fatalf("branchName() = %q, want %q", got, want)
	}
	if branchName("amux/", first) == branchName("amux/", second) {
		t.Fatal("UUIDv7 branch names collided after trimming to a shared timestamp prefix")
	}
}
