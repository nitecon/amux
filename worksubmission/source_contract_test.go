package worksubmission_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/nitecon/amux/worksubmission"
)

const sourceTask = "018fd19a-7cb8-74a7-b629-85b9f5d132e7"

type sourceFixture struct {
	t            *testing.T
	root, remote string
}

func sourceGit(t *testing.T, directory string, args ...string) string {
	t.Helper()
	return strings.TrimSpace(sourceGitRaw(t, directory, args...))
}

func sourceGitRaw(t *testing.T, directory string, args ...string) string {
	t.Helper()
	output, err := sourceGitResult(directory, args...)
	if err != nil {
		t.Fatalf("synthetic git %v: %v\n%s", args, err, output)
	}
	return output
}

func sourceGitResult(directory string, args ...string) (string, error) {
	command := exec.Command("git", append([]string{"-C", directory}, args...)...)
	for _, variable := range os.Environ() {
		if !strings.HasPrefix(strings.ToUpper(variable), "GIT_") {
			command.Env = append(command.Env, variable)
		}
	}
	command.Env = append(command.Env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_TERMINAL_PROMPT=0")
	output, err := command.CombinedOutput()
	return string(output), err
}

func newSourceFixture(t *testing.T) *sourceFixture {
	t.Helper()
	base := t.TempDir()
	f := &sourceFixture{t: t, root: filepath.Join(base, "checkout"), remote: filepath.Join(base, "remote.git")}
	for _, directory := range []string{f.root, f.remote} {
		if err := os.Mkdir(directory, 0700); err != nil {
			t.Fatal(err)
		}
	}
	sourceGit(t, f.remote, "init", "--bare", "--initial-branch=main", "--template=")
	sourceGit(t, f.root, "init", "--initial-branch=main", "--template=")
	configureSourceRepo(t, f.root, base)
	for _, file := range []string{"a.txt", "b.txt", "gone.txt"} {
		f.write(file, "base "+file+"\n")
	}
	sourceGit(t, f.root, "add", "--", ".")
	sourceGit(t, f.root, "commit", "-m", "Synthetic base")
	sourceGit(t, f.root, "remote", "add", "origin", f.remote)
	sourceGit(t, f.root, "push", "--set-upstream", "origin", "main")
	return f
}

func configureSourceRepo(t *testing.T, root, base string) {
	t.Helper()
	for key, value := range map[string]string{
		"user.name": "Synthetic Fixture", "user.email": "fixture@example.invalid",
		"commit.gpgSign": "false", "protocol.allow": "never", "protocol.file.allow": "always",
		"core.autocrlf": "false", "core.safecrlf": "false",
		"core.hooksPath": filepath.Join(base, "empty-hooks"),
	} {
		sourceGit(t, root, "config", "--local", key, value)
	}
}

func (f *sourceFixture) advance(name, body string) {
	f.t.Helper()
	peer := f.t.TempDir()
	sourceGit(f.t, peer, "init", "--initial-branch=main", "--template=")
	configureSourceRepo(f.t, peer, peer)
	sourceGit(f.t, peer, "remote", "add", "origin", f.remote)
	sourceGit(f.t, peer, "fetch", "origin")
	sourceGit(f.t, peer, "checkout", "-B", "main", "origin/main")
	path := filepath.Join(peer, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		f.t.Fatal(err)
	}
	sourceGit(f.t, peer, "add", "--", name)
	sourceGit(f.t, peer, "commit", "-m", "other "+name)
	sourceGit(f.t, peer, "push", "origin", "main")
}

func (f *sourceFixture) write(name, body string) {
	f.t.Helper()
	path := filepath.Join(f.root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		f.t.Fatal(err)
	}
}

func (f *sourceFixture) snapshot() []string {
	return []string{
		sourceGit(f.t, f.root, "rev-parse", "HEAD"),
		sourceGit(f.t, f.root, "symbolic-ref", "HEAD"),
		sourceGitRaw(f.t, f.root, "diff", "--binary"),
		sourceGitRaw(f.t, f.root, "diff", "--cached", "--binary"),
		sourceGitRaw(f.t, f.root, "status", "--porcelain=v1", "-z", "--untracked-files=all"),
	}
}

func assertSourceCommit(t *testing.T, root, commit string, files []string) {
	t.Helper()
	raw := sourceGitRaw(t, root, "diff-tree", "--no-commit-id", "--name-only", "--no-renames", "-r", "-z", commit)
	changed := strings.Split(strings.TrimSuffix(raw, "\x00"), "\x00")
	want := append([]string(nil), files...)
	sort.Strings(changed)
	sort.Strings(want)
	if !reflect.DeepEqual(changed, want) {
		t.Fatalf("committed paths=%q want=%q", changed, want)
	}
	message := sourceGit(t, root, "log", "-1", "--format=%B", commit)
	if strings.Count(message, "AmuxID: "+sourceTask) != 1 || strings.Contains(strings.ToLower(message), "co-authored-by") || strings.Contains(message, "Generated with") {
		t.Fatalf("submission message was not cleaned: %q", message)
	}
	if author := sourceGit(t, root, "log", "-1", "--format=%an <%ae>", commit); author != "Synthetic Fixture <fixture@example.invalid>" {
		t.Fatalf("submission changed configured author: %q", author)
	}
}

func TestSourceCommitAndPushOnlyListedPathsPreservesForeignIndexAndDrafts(t *testing.T) {
	f := newSourceFixture(t)
	f.write("a.txt", "task edit\n")
	f.write("new file.txt", "new task file\n")
	if err := os.Remove(filepath.Join(f.root, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	f.write("b.txt", "another agent's staged edit\n")
	sourceGit(t, f.root, "add", "--", "b.txt")
	f.write("b.txt", "another agent's unstaged edit\n")
	f.write("untracked.txt", "unrelated untracked draft\n")
	index := sourceGit(t, f.root, "diff", "--cached", "--binary")
	request := worksubmission.Request{Root: f.root, TaskID: sourceTask,
		Message: "Fix the parser\n\nCo-Authored-By: Fixture <fixture@example.invalid>\nGenerated with Codex\nAmuxID: forged",
		Paths:   []string{filepath.Join(f.root, "a.txt"), "new file.txt", "./gone.txt", "a.txt"}}
	outcome, err := worksubmission.Submit(context.Background(), request, worksubmission.DefaultPreferences(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || !reflect.DeepEqual(outcome.Files, []string{"a.txt", "new file.txt", "gone.txt"}) {
		t.Fatalf("wrong receipt: %+v", outcome)
	}
	if head := sourceGit(t, f.root, "rev-parse", "HEAD"); head != outcome.Reference || sourceGit(t, f.remote, "rev-parse", "refs/heads/main") != head {
		t.Fatal("direct submission was not published as HEAD")
	}
	assertSourceCommit(t, f.root, outcome.Reference, []string{"a.txt", "new file.txt", "gone.txt"})
	if sourceGit(t, f.root, "diff", "--cached", "--binary") != index || sourceGit(t, f.root, "show", ":b.txt") != "another agent's staged edit" {
		t.Fatal("submission changed unrelated index state")
	}
	if data, err := os.ReadFile(filepath.Join(f.root, "b.txt")); err != nil || string(data) != "another agent's unstaged edit\n" {
		t.Fatal("submission lost unrelated working-tree content")
	}
	if data, err := os.ReadFile(filepath.Join(f.root, "untracked.txt")); err != nil || string(data) != "unrelated untracked draft\n" {
		t.Fatal("submission lost untracked content")
	}
	if !strings.HasPrefix(outcome.Comment(), "Work submitted: "+outcome.Reference) || !strings.HasSuffix(outcome.Comment(), "\n- a.txt\n- new file.txt\n- gone.txt") {
		t.Fatal("task receipt comment lost published reference or exact selected paths")
	}
}

func TestSourceRejectedRequestsLeaveCheckoutAndRemoteUntouched(t *testing.T) {
	f := newSourceFixture(t)
	f.write("a.txt", "task edit\n")
	f.write("b.txt", "foreign staged edit\n")
	sourceGit(t, f.root, "add", "--", "b.txt")
	outside := filepath.Join(filepath.Dir(f.root), "outside.txt")
	if err := os.WriteFile(outside, []byte("outside\n"), 0600); err != nil {
		t.Fatal(err)
	}
	before, remote := f.snapshot(), sourceGit(t, f.remote, "rev-parse", "refs/heads/main")
	for _, item := range []struct {
		name, message string
		paths         []string
	}{
		{"empty paths", "Task edit", nil},
		{"absolute outside", "Task edit", []string{outside}},
		{"relative outside", "Task edit", []string{"a.txt", "../outside.txt"}},
		{"root", "Task edit", []string{"."}},
		{"unchanged selected file", "Task edit", []string{"a.txt", "gone.txt"}},
		{"git metadata", "Task edit", []string{".git/config"}},
		{"nul", "Task edit", []string{"a.txt", "bad\x00path"}},
		{"empty cleaned message", "Co-Authored-By: Fixture <fixture@example.invalid>", []string{"a.txt"}},
		{"nul message", "Task edit\x00invalid Git argument", []string{"a.txt"}},
	} {
		t.Run(item.name, func(t *testing.T) {
			_, err := worksubmission.Submit(context.Background(), worksubmission.Request{Root: f.root, TaskID: sourceTask, Message: item.message, Paths: item.paths}, worksubmission.DefaultPreferences(), nil)
			if err == nil {
				t.Fatal("invalid request succeeded")
			}
			if !reflect.DeepEqual(f.snapshot(), before) || sourceGit(t, f.remote, "rev-parse", "refs/heads/main") != remote {
				t.Fatal("rejected request changed the checkout or published history")
			}
		})
	}
}

func TestSourcePullRequestPushesTaskBranchWithoutChangingCheckout(t *testing.T) {
	f := newSourceFixture(t)
	f.advance("c.txt", "from another machine\n")
	f.write("a.txt", "task edit\n")
	f.write("new.txt", "task new file\n")
	if err := os.Remove(filepath.Join(f.root, "gone.txt")); err != nil {
		t.Fatal(err)
	}
	f.write("b.txt", "foreign staged edit\n")
	sourceGit(t, f.root, "add", "--", "b.txt")
	f.write("b.txt", "foreign unstaged edit\n")
	before, remote := f.snapshot(), sourceGit(t, f.remote, "rev-parse", "refs/heads/main")
	preferences := worksubmission.DefaultPreferences()
	preferences.Mode = worksubmission.PullRequestMode
	var opened *worksubmission.PullRequest
	outcome, err := worksubmission.Submit(context.Background(), worksubmission.Request{Root: f.root, TaskID: sourceTask, Message: "Fix the parser", Paths: []string{"a.txt", "new.txt", "gone.txt"}}, preferences,
		func(_ context.Context, request worksubmission.PullRequest) (string, error) {
			opened = &request
			return "https://example.invalid/pull/1", nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if outcome == nil || outcome.Reference != "https://example.invalid/pull/1" || opened == nil || opened.Head != "amux/"+sourceTask || opened.Base != "main" || opened.Title != "Fix the parser" {
		t.Fatalf("wrong PR receipt: %+v %+v", outcome, opened)
	}
	if !reflect.DeepEqual(f.snapshot(), before) || sourceGit(t, f.remote, "rev-parse", "refs/heads/main") != remote {
		t.Fatal("PR submission changed caller checkout or upstream branch")
	}
	commit := sourceGit(t, f.remote, "rev-parse", "refs/heads/"+opened.Head)
	if sourceGit(t, f.remote, "rev-parse", commit+"^") != remote {
		t.Fatal("task branch was not based on upstream")
	}
	assertSourceCommit(t, f.remote, commit, []string{"a.txt", "new.txt", "gone.txt"})
	if sourceGit(t, f.remote, "show", commit+":c.txt") != "from another machine" {
		t.Fatal("PR branch lost work from the fetched upstream")
	}
}

func TestSourceRemoteAdvanceRebasesAndRestoresBothIndexSnapshots(t *testing.T) {
	f := newSourceFixture(t)
	f.write("gone.txt", "older saved work\n")
	sourceGit(t, f.root, "stash", "push", "-m", "older stash")
	priorStash := sourceGit(t, f.root, "rev-parse", "refs/stash")
	sourceGit(t, f.root, "config", "rebase.autoStash", "true")
	f.advance("c.txt", "from another machine\n")
	f.write("a.txt", "task edit\n")
	f.write("b.txt", "staged draft\n")
	sourceGit(t, f.root, "add", "--", "b.txt")
	f.write("b.txt", "later unstaged draft\n")
	f.write("untracked.txt", "untracked draft\n")
	outcome, err := worksubmission.Submit(context.Background(), worksubmission.Request{Root: f.root, TaskID: sourceTask, Message: "Fix the parser", Paths: []string{"a.txt"}}, worksubmission.DefaultPreferences(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if outcome.Reference != sourceGit(t, f.remote, "rev-parse", "refs/heads/main") || outcome.Reference != sourceGit(t, f.root, "rev-parse", "HEAD") {
		t.Fatal("rebased task was not published")
	}
	if sourceGit(t, f.root, "log", "-1", "--format=%s", "HEAD~1") != "other c.txt" {
		t.Fatal("submission did not rebase onto the new upstream")
	}
	assertSourceCommit(t, f.root, "HEAD", []string{"a.txt"})
	if sourceGit(t, f.root, "show", ":b.txt") != "staged draft" {
		t.Fatal("rebase lost the foreign staged snapshot")
	}
	for name, want := range map[string]string{"b.txt": "later unstaged draft\n", "untracked.txt": "untracked draft\n"} {
		if data, err := os.ReadFile(filepath.Join(f.root, name)); err != nil || string(data) != want {
			t.Fatalf("rebase lost %s", name)
		}
	}
	if sourceGit(t, f.root, "rev-parse", "refs/stash") != priorStash {
		t.Fatal("submission removed or replaced another stash")
	}
}

func TestSourceRebaseConflictKeepsTaskCommitAndRestoresForeignDrafts(t *testing.T) {
	f := newSourceFixture(t)
	f.advance("a.txt", "conflicting upstream edit\n")
	remote := sourceGit(t, f.remote, "rev-parse", "refs/heads/main")
	f.write("a.txt", "conflicting task edit\n")
	f.write("b.txt", "foreign staged draft\n")
	sourceGit(t, f.root, "add", "--", "b.txt")
	f.write("b.txt", "foreign unstaged draft\n")
	f.write("untracked.txt", "untracked draft\n")
	outcome, err := worksubmission.Submit(context.Background(), worksubmission.Request{Root: f.root, TaskID: sourceTask, Message: "Fix the parser", Paths: []string{"a.txt"}}, worksubmission.DefaultPreferences(), nil)
	if err == nil || outcome != nil {
		t.Fatalf("conflicting submission succeeded: %+v %v", outcome, err)
	}
	kept := sourceGit(t, f.root, "rev-parse", "HEAD")
	if !strings.Contains(err.Error(), kept) || !strings.Contains(err.Error(), "resume-commit") {
		t.Fatalf("recovery error lost retained commit and retry instruction: %v", err)
	}
	if sourceGit(t, f.remote, "rev-parse", "refs/heads/main") != remote {
		t.Fatal("conflicting task was published")
	}
	assertSourceCommit(t, f.root, kept, []string{"a.txt"})
	if sourceGit(t, f.root, "show", ":b.txt") != "foreign staged draft" {
		t.Fatal("conflict recovery lost foreign staged snapshot")
	}
	if data, err := os.ReadFile(filepath.Join(f.root, "b.txt")); err != nil || string(data) != "foreign unstaged draft\n" {
		t.Fatal("conflict recovery lost foreign working-tree snapshot")
	}
	if sourceGit(t, f.root, "diff", "--name-only", "--diff-filter=U") != "" {
		t.Fatal("submission left unresolved rebase index conflicts")
	}
	for _, step := range []string{"git stash push --include-untracked", "git rev-parse refs/stash", "Run git pull --rebase", "git add the resolved files", "git -c core.editor=true rebase --continue", "retry amux submit-work --resume-commit", "git stash apply --index <saved-stash-id>", "before dropping only that saved stash"} {
		if !strings.Contains(err.Error(), step) {
			t.Fatalf("recovery instructions omit %q: %v", step, err)
		}
	}
	if _, err := sourceGitResult(f.root, "pull", "--rebase"); err == nil {
		t.Fatal("dirty recovery unexpectedly started a rebase")
	}
	sourceGit(t, f.root, "stash", "push", "--include-untracked")
	saved := sourceGit(t, f.root, "rev-parse", "refs/stash")
	if _, err := sourceGitResult(f.root, "pull", "--rebase"); err == nil {
		t.Fatal("conflicting recovery unexpectedly rebased without a resolution")
	}
	if sourceGit(t, f.root, "diff", "--name-only", "--diff-filter=U") != "a.txt" {
		t.Fatal("recovery did not expose the expected task conflict")
	}
	f.write("a.txt", "resolved task edit\n")
	sourceGit(t, f.root, "add", "--", "a.txt")
	sourceGit(t, f.root, "-c", "core.editor=true", "rebase", "--continue")
	sourceGit(t, f.root, "push")
	sourceGit(t, f.root, "stash", "apply", "--index", saved)
	if sourceGit(t, f.root, "show", ":b.txt") != "foreign staged draft" {
		t.Fatal("documented recovery lost staged work")
	}
	for name, want := range map[string]string{"b.txt": "foreign unstaged draft\n", "untracked.txt": "untracked draft\n"} {
		if data, err := os.ReadFile(filepath.Join(f.root, name)); err != nil || string(data) != want {
			t.Fatalf("documented recovery lost %s", name)
		}
	}
	if sourceGit(t, f.root, "rev-parse", "stash@{0}") != saved {
		t.Fatal("documented recovery lost its saved stash")
	}
	sourceGit(t, f.root, "stash", "drop", "stash@{0}")
	if sourceGit(t, f.root, "stash", "list") != "" {
		t.Fatal("documented recovery left an unexpected stash")
	}
}

func TestSourceLargeExactMovesPreserveArtistEditsInBothModes(t *testing.T) {
	for _, mode := range []worksubmission.Mode{worksubmission.CommitAndPush, worksubmission.PullRequestMode} {
		t.Run(string(mode), func(t *testing.T) {
			f := newSourceFixture(t)
			paths := make([]string, 0, 16350)
			for i := 0; i < 8174; i++ {
				name := fmt.Sprintf("actor_%05d_with_a_long_exact_asset_filename.scene", i)
				old, newPath := "before/"+name, "after/"+name
				f.write(old, fmt.Sprintf("actor %d\n", i))
				paths = append(paths, old, newPath)
			}
			sourceGit(t, f.root, "add", "--all")
			sourceGit(t, f.root, "commit", "-m", "seed assets")
			sourceGit(t, f.root, "push")
			base := sourceGit(t, f.remote, "rev-parse", "refs/heads/main")
			if err := os.Mkdir(filepath.Join(f.root, "after"), 0700); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < len(paths); i += 2 {
				if err := os.Rename(filepath.Join(f.root, paths[i]), filepath.Join(f.root, paths[i+1])); err != nil {
					t.Fatal(err)
				}
			}
			for _, name := range []string{"-option.txt", "literal [1] ü.txt"} {
				f.write(name, "selected\n")
				paths = append(paths, name)
			}
			f.write("a.txt", "artist staged\n")
			sourceGit(t, f.root, "add", "--", "a.txt")
			f.write("a.txt", "artist later unstaged\n")
			f.write("b.txt", "second artist edit\n")
			f.write("gone.txt", "third artist edit\n")
			index := sourceGitRaw(t, f.root, "diff", "--cached", "--binary")
			prefs := worksubmission.DefaultPreferences()
			prefs.Mode = mode
			out, err := worksubmission.Submit(context.Background(), worksubmission.Request{Root: f.root, TaskID: sourceTask, Message: "Move exact assets", Paths: paths}, prefs, func(context.Context, worksubmission.PullRequest) (string, error) {
				return "https://example.invalid/pull/1", nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(out.Files, paths) {
				t.Fatal("large path receipt lost order, literal names or exact paths")
			}
			commit := out.Reference
			if mode == worksubmission.PullRequestMode {
				if sourceGit(t, f.remote, "rev-parse", "refs/heads/main") != base || sourceGit(t, f.root, "rev-parse", "HEAD") != base {
					t.Fatal("large PR submission changed checkout/upstream HEAD")
				}
				commit = sourceGit(t, f.remote, "rev-parse", "refs/heads/amux/"+sourceTask)
			}
			if sourceGit(t, f.remote, "rev-parse", commit+"^") != base {
				t.Fatal("large submission created extra commits")
			}
			assertSourceCommit(t, f.remote, commit, paths)
			if sourceGitRaw(t, f.root, "diff", "--cached", "--binary") != index || sourceGit(t, f.root, "show", ":a.txt") != "artist staged" {
				t.Fatal("large submission changed the foreign index snapshot")
			}
			for name, want := range map[string]string{"a.txt": "artist later unstaged\n", "b.txt": "second artist edit\n", "gone.txt": "third artist edit\n"} {
				if data, err := os.ReadFile(filepath.Join(f.root, name)); err != nil || string(data) != want {
					t.Fatalf("large submission lost %s", name)
				}
			}
		})
	}
}

func (f *sourceFixture) keepTaskCommit() string {
	f.t.Helper()
	f.write("a.txt", "retained task edit\n")
	sourceGit(f.t, f.root, "add", "--", "a.txt")
	sourceGit(f.t, f.root, "commit", "-m", "Fix the parser\n\nAmuxID: "+sourceTask)
	return sourceGit(f.t, f.root, "rev-parse", "HEAD")
}

func TestSourceResumeRejectsWrongHeadTaskFilesDirtySelectionAndUnpublishedParent(t *testing.T) {
	for _, name := range []string{"wrong HEAD", "wrong task", "other task", "wrong files", "dirty selection", "unpublished parent", "index conflict"} {
		t.Run(name, func(t *testing.T) {
			f := newSourceFixture(t)
			if name == "unpublished parent" {
				f.write("unrelated.txt", "unpublished unrelated history\n")
				sourceGit(t, f.root, "add", "--", "unrelated.txt")
				sourceGit(t, f.root, "commit", "-m", "Unpublished foreign commit")
			}
			kept := f.keepTaskCommit()
			request := worksubmission.Request{Root: f.root, TaskID: sourceTask, Message: "Retry publication", Paths: []string{"a.txt"}}
			switch name {
			case "wrong HEAD":
				kept = sourceGit(t, f.root, "rev-parse", "HEAD^")
			case "wrong task":
				sourceGit(t, f.root, "commit", "--amend", "-m", "Other task without matching trailer")
				kept = sourceGit(t, f.root, "rev-parse", "HEAD")
			case "other task":
				request.TaskID = "018fd19a-7cb8-74a7-b629-85b9f5d132e8"
			case "wrong files":
				request.Paths = []string{"b.txt"}
			case "dirty selection":
				f.write("a.txt", "new uncommitted selected work\n")
			case "index conflict":
				f.advance("a.txt", "conflicting upstream work\n")
				sourceGit(t, f.root, "fetch", "origin")
				if _, err := sourceGitResult(f.root, "merge", "--no-commit", "origin/main"); err == nil {
					t.Fatal("fixture merge did not produce the expected index conflict")
				}
			}
			before, remote := f.snapshot(), sourceGit(t, f.remote, "rev-parse", "refs/heads/main")
			if out, err := worksubmission.Resume(context.Background(), request, worksubmission.DefaultPreferences(), kept, nil); err == nil || out != nil {
				t.Fatalf("unsafe resume accepted: %+v %v", out, err)
			}
			if !reflect.DeepEqual(f.snapshot(), before) || sourceGit(t, f.remote, "rev-parse", "refs/heads/main") != remote {
				t.Fatal("rejected resume changed the checkout or published history")
			}
		})
	}
}

func TestSourceResumeRebasesOnceAndPreservesUnrelatedDrafts(t *testing.T) {
	f := newSourceFixture(t)
	kept := f.keepTaskCommit()
	f.advance("c.txt", "upstream work\n")
	f.write("b.txt", "staged draft\n")
	sourceGit(t, f.root, "add", "--", "b.txt")
	f.write("b.txt", "unstaged draft\n")
	request := worksubmission.Request{Root: f.root, TaskID: sourceTask, Message: "Must not replace the retained message", Paths: []string{"a.txt"}}
	out, err := worksubmission.Resume(context.Background(), request, worksubmission.DefaultPreferences(), kept, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out.Reference == kept || sourceGit(t, f.remote, "rev-parse", "refs/heads/main") != out.Reference {
		t.Fatal("resume did not publish the rebased retained commit")
	}
	if sourceGit(t, f.remote, "rev-list", "--count", "main") != "3" {
		t.Fatal("resume created an additional task commit")
	}
	if sourceGit(t, f.root, "log", "-1", "--format=%s") != "Fix the parser" || sourceGit(t, f.root, "show", ":b.txt") != "staged draft" {
		t.Fatal("resume changed retained message or foreign index")
	}
	if data, err := os.ReadFile(filepath.Join(f.root, "b.txt")); err != nil || string(data) != "unstaged draft\n" {
		t.Fatal("resume lost foreign working-tree draft")
	}
	assertSourceCommit(t, f.remote, "main", []string{"a.txt"})
	before := f.snapshot()
	request.Message = "Already-published retry must not commit"
	repeated, err := worksubmission.Resume(context.Background(), request, worksubmission.DefaultPreferences(), out.Reference, nil)
	if err != nil || repeated.Reference != out.Reference || !reflect.DeepEqual(f.snapshot(), before) {
		t.Fatalf("already-published retry changed retained commit or drafts: %+v %v", repeated, err)
	}
}

func TestSourceResumeHonorsPullRequestSettingsWithoutRewritingCommitOrCheckout(t *testing.T) {
	f := newSourceFixture(t)
	kept := f.keepTaskCommit()
	f.advance("c.txt", "upstream work\n")
	f.write("b.txt", "unrelated draft\n")
	before, remote := f.snapshot(), sourceGit(t, f.remote, "rev-parse", "refs/heads/main")
	preferences := worksubmission.DefaultPreferences()
	preferences.Mode = worksubmission.PullRequestMode
	preferences.BranchPrefix = "review/"
	var opened *worksubmission.PullRequest
	out, err := worksubmission.Resume(context.Background(), worksubmission.Request{Root: f.root, TaskID: sourceTask, Message: "Retry publication", Paths: []string{"a.txt"}}, preferences, kept, func(_ context.Context, pull worksubmission.PullRequest) (string, error) {
		opened = &pull
		return "https://example.invalid/pull/1", nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Reference != "https://example.invalid/pull/1" || opened == nil || opened.Head != "review/"+sourceTask || opened.Title != "Fix the parser" || !strings.HasSuffix(opened.Body, "AmuxID: "+sourceTask) {
		t.Fatalf("resume replaced retained PR request: %+v %+v", out, opened)
	}
	if !reflect.DeepEqual(f.snapshot(), before) || sourceGit(t, f.remote, "rev-parse", "refs/heads/main") != remote || sourceGit(t, f.remote, "rev-parse", "refs/heads/"+opened.Head) != kept {
		t.Fatal("PR resume changed checkout, upstream or retained commit")
	}
}

func TestSourcePullRequestRejectsUpstreamSelectedEditsWithoutLosingWork(t *testing.T) {
	for _, pair := range [][2]string{{"new header\nmiddle\nold tail\n", "old header\nmiddle\nlocal tail\n"}, {"upstream replacement\n", "local replacement\n"}} {
		t.Run(strings.Split(pair[0], "\n")[0], func(t *testing.T) {
			f := newSourceFixture(t)
			f.write("a.txt", "old header\nmiddle\nold tail\n")
			sourceGit(t, f.root, "add", "--", "a.txt")
			sourceGit(t, f.root, "commit", "-m", "base text")
			sourceGit(t, f.root, "push")
			f.advance("a.txt", pair[0])
			remote := sourceGit(t, f.remote, "rev-parse", "refs/heads/main")
			f.write("a.txt", pair[1])
			f.write("b.txt", "another agent's staged work\n")
			sourceGit(t, f.root, "add", "--", "b.txt")
			before := f.snapshot()
			preferences := worksubmission.DefaultPreferences()
			preferences.Mode = worksubmission.PullRequestMode
			_, err := worksubmission.Submit(context.Background(), worksubmission.Request{Root: f.root, TaskID: sourceTask, Message: "Task edit", Paths: []string{"a.txt"}}, preferences, func(context.Context, worksubmission.PullRequest) (string, error) {
				t.Fatal("invalid PR was opened")
				return "", nil
			})
			if err == nil || !strings.Contains(err.Error(), "selected paths differ") {
				t.Fatalf("upstream selected changes were accepted: %v", err)
			}
			if !reflect.DeepEqual(f.snapshot(), before) || sourceGit(t, f.remote, "rev-parse", "refs/heads/main") != remote || sourceGit(t, f.remote, "for-each-ref", "refs/heads/amux/") != "" {
				t.Fatal("rejected PR lost work or published a task branch")
			}
		})
	}
}

func TestSourceProjectSubdirectoriesPreserveWhitespaceAndGuardUpstreamEdits(t *testing.T) {
	for _, directory := range []string{"sub", " leading"} {
		t.Run(directory, func(t *testing.T) {
			f := newSourceFixture(t)
			file := directory + "/f.txt"
			f.write(file, "base\n")
			sourceGit(t, f.root, "add", "--", file)
			sourceGit(t, f.root, "commit", "-m", "add sub")
			sourceGit(t, f.root, "push")
			request := worksubmission.Request{Root: filepath.Join(f.root, directory), TaskID: sourceTask, Message: "Task edit", Paths: []string{"f.txt"}}
			f.write(file, "committed\n")
			out, err := worksubmission.Submit(context.Background(), request, worksubmission.DefaultPreferences(), nil)
			if err != nil || !reflect.DeepEqual(out.Files, []string{"f.txt"}) {
				t.Fatalf("subdirectory submission failed: %+v %v", out, err)
			}
			assertSourceCommit(t, f.remote, "main", []string{file})
			f.advance(file, "upstream\n")
			f.write(file, "local\n")
			preferences := worksubmission.DefaultPreferences()
			preferences.Mode = worksubmission.PullRequestMode
			before := f.snapshot()
			_, err = worksubmission.Submit(context.Background(), request, preferences, func(context.Context, worksubmission.PullRequest) (string, error) {
				t.Fatal("invalid subdirectory PR opened")
				return "", nil
			})
			if err == nil || !strings.Contains(err.Error(), "selected paths differ") || !reflect.DeepEqual(f.snapshot(), before) || sourceGit(t, f.remote, "for-each-ref", "refs/heads/amux/") != "" {
				t.Fatalf("subdirectory upstream guard failed: %v", err)
			}
		})
	}
}

func TestSourceInvalidBranchPrefixIsRejectedBeforePublishing(t *testing.T) {
	f := newSourceFixture(t)
	f.write("a.txt", "task edit\n")
	before, refs := f.snapshot(), sourceGit(t, f.remote, "for-each-ref")
	preferences := worksubmission.DefaultPreferences()
	preferences.Mode = worksubmission.PullRequestMode
	preferences.BranchPrefix = "bad prefix.."
	_, err := worksubmission.Submit(context.Background(), worksubmission.Request{Root: f.root, TaskID: sourceTask, Message: "Task edit", Paths: []string{"a.txt"}}, preferences, func(context.Context, worksubmission.PullRequest) (string, error) {
		t.Fatal("invalid branch PR opened")
		return "", nil
	})
	if err == nil || !reflect.DeepEqual(f.snapshot(), before) || sourceGit(t, f.remote, "for-each-ref") != refs {
		t.Fatalf("invalid branch prefix changed checkout or remote: %v", err)
	}
}

func TestSourceRebaseThatCannotStartRestoresStagedAndUnstagedWork(t *testing.T) {
	f := newSourceFixture(t)
	f.advance("untracked.txt", "upstream file\n")
	remote := sourceGit(t, f.remote, "rev-parse", "refs/heads/main")
	f.write("a.txt", "task edit\n")
	f.write("b.txt", "staged draft\n")
	sourceGit(t, f.root, "add", "--", "b.txt")
	f.write("b.txt", "later unstaged draft\n")
	f.write("untracked.txt", "untracked draft\n")
	_, err := worksubmission.Submit(context.Background(), worksubmission.Request{Root: f.root, TaskID: sourceTask, Message: "Task edit", Paths: []string{"a.txt"}}, worksubmission.DefaultPreferences(), nil)
	if err == nil || !strings.Contains(err.Error(), "staged and unstaged work restored") || sourceGit(t, f.remote, "rev-parse", "refs/heads/main") != remote {
		t.Fatalf("blocked rebase was not safely retained: %v", err)
	}
	if sourceGit(t, f.root, "show", ":b.txt") != "staged draft" || sourceGit(t, f.root, "stash", "list") != "" {
		t.Fatal("blocked rebase did not restore the original index/stash state")
	}
	for name, want := range map[string]string{"b.txt": "later unstaged draft\n", "untracked.txt": "untracked draft\n"} {
		if data, err := os.ReadFile(filepath.Join(f.root, name)); err != nil || string(data) != want {
			t.Fatalf("blocked rebase lost %s", name)
		}
	}
}

func TestSourceStashRestoreConflictRetainsBothSnapshotsAndResumeDoesNotRepeatCommit(t *testing.T) {
	f := newSourceFixture(t)
	for _, name := range []string{"c.txt", "d.txt", "e.txt", "f.txt"} {
		f.write(name, "base\n")
	}
	sourceGit(t, f.root, "add", "--all")
	sourceGit(t, f.root, "commit", "-m", "unrelated files")
	sourceGit(t, f.root, "push")
	f.advance("b.txt", "upstream edit\n")
	remote := sourceGit(t, f.remote, "rev-parse", "refs/heads/main")
	f.write("a.txt", "task edit\n")
	f.write("gone.txt", "second task edit\n")
	f.write("b.txt", "staged draft\n")
	sourceGit(t, f.root, "add", "--", "b.txt")
	f.write("b.txt", "later unstaged draft\n")
	for _, name := range []string{"c.txt", "d.txt", "e.txt", "f.txt"} {
		f.write(name, "unrelated draft\n")
	}
	request := worksubmission.Request{Root: f.root, TaskID: sourceTask, Message: "Fix the parser", Paths: []string{"a.txt", "gone.txt"}}
	_, err := worksubmission.Submit(context.Background(), request, worksubmission.DefaultPreferences(), nil)
	if err == nil || !strings.Contains(err.Error(), "could not restore staged and unstaged work") {
		t.Fatalf("stash conflict did not retain a recoverable failure: %v", err)
	}
	stash := sourceGit(t, f.root, "rev-parse", "refs/stash")
	if !strings.Contains(err.Error(), stash) || sourceGit(t, f.root, "show", stash+"^2:b.txt") != "staged draft" || sourceGit(t, f.root, "show", stash+":b.txt") != "later unstaged draft" {
		t.Fatal("stash conflict lost either foreign snapshot or its recovery reference")
	}
	if sourceGit(t, f.remote, "rev-parse", "refs/heads/main") != remote || sourceGit(t, f.root, "show", "HEAD:a.txt") != "task edit" {
		t.Fatal("stash conflict published work or lost task commit")
	}
	kept := sourceGit(t, f.root, "rev-parse", "HEAD")
	if !strings.Contains(err.Error(), kept) {
		t.Fatalf("stash recovery reported an obsolete commit instead of retained HEAD %s: %v", kept, err)
	}
	// As in the source contract, the fixture owner explicitly discards only the
	// obsolete conflicting draft after inspecting both retained snapshots.
	sourceGit(t, f.root, "restore", "--source=HEAD", "--staged", "--worktree", "--", "b.txt")
	sourceGit(t, f.root, "restore", "--source="+stash, "--worktree", "--", "c.txt", "d.txt", "e.txt", "f.txt")
	before, stashes := f.snapshot(), sourceGit(t, f.root, "stash", "list")
	out, err := worksubmission.Resume(context.Background(), request, worksubmission.DefaultPreferences(), kept, nil)
	if err != nil || out.Reference != kept || sourceGit(t, f.remote, "rev-parse", "refs/heads/main") != kept {
		t.Fatalf("recovered task commit could not be resumed: %+v %v", out, err)
	}
	if !reflect.DeepEqual(f.snapshot(), before) || sourceGit(t, f.root, "stash", "list") != stashes || sourceGit(t, f.root, "rev-parse", "refs/stash") != stash {
		t.Fatal("resume changed foreign drafts or removed retained recovery snapshots")
	}
	assertSourceCommit(t, f.remote, "main", []string{"a.txt", "gone.txt"})
	for _, name := range []string{"c.txt", "d.txt", "e.txt", "f.txt"} {
		if data, err := os.ReadFile(filepath.Join(f.root, name)); err != nil || string(data) != "unrelated draft\n" {
			t.Fatalf("resume lost %s draft", name)
		}
	}
	repeated, err := worksubmission.Resume(context.Background(), request, worksubmission.DefaultPreferences(), kept, nil)
	if err != nil || repeated.Reference != out.Reference || !reflect.DeepEqual(f.snapshot(), before) {
		t.Fatalf("second resume changed the recovered state: %+v %v", repeated, err)
	}
}

func TestSourceCommitCaptureSupportsLegalClosingBracketInBranchName(t *testing.T) {
	f := newSourceFixture(t)
	sourceGit(t, f.root, "branch", "-m", "topic]name")
	f.write("a.txt", "task edit\n")
	out, err := worksubmission.Submit(context.Background(), worksubmission.Request{Root: f.root, TaskID: sourceTask, Message: "Task edit", Paths: []string{"a.txt"}}, worksubmission.DefaultPreferences(), nil)
	if err != nil || out == nil {
		t.Fatalf("legal branch name prevented task publication: %+v %v", out, err)
	}
	if out.Reference != sourceGit(t, f.remote, "rev-parse", "refs/heads/main") {
		t.Fatal("legal branch name lost exact published commit identity")
	}
}
