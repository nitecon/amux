package worksubmission

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/uuid"
)

type fixture struct{ root, remote string }

func newFixture(t *testing.T) fixture {
	t.Helper()
	base := t.TempDir()
	f := fixture{root: filepath.Join(base, "work"), remote: filepath.Join(base, "remote.git")}
	if err := os.Mkdir(f.root, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(f.remote, 0755); err != nil {
		t.Fatal(err)
	}
	f.git(t, f.remote, "init", "--bare", "--initial-branch=main")
	f.git(t, f.root, "init", "--initial-branch=main")
	f.git(t, f.root, "config", "user.name", "Configured Author")
	f.git(t, f.root, "config", "user.email", "configured@example.invalid")
	f.git(t, f.root, "config", "commit.gpgsign", "false")
	write(t, filepath.Join(f.root, "base.txt"), "base\n")
	f.git(t, f.root, "add", "base.txt")
	f.git(t, f.root, "commit", "-m", "initial")
	f.git(t, f.root, "remote", "add", "origin", f.remote)
	f.git(t, f.root, "push", "-u", "origin", "main")
	return f
}

func (f fixture) git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", append([]string{"-C", dir}, args...)...)
	c.Env = os.Environ()
	b, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, b)
	}
	return strings.TrimSpace(string(b))
}
func (f fixture) head(t *testing.T) string { return f.git(t, f.root, "rev-parse", "HEAD") }
func (f fixture) remoteHead(t *testing.T) string {
	return f.git(t, f.remote, "rev-parse", "refs/heads/main")
}
func write(t *testing.T, p, s string) {
	t.Helper()
	if e := os.MkdirAll(filepath.Dir(p), 0755); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(p, []byte(s), 0644); e != nil {
		t.Fatal(e)
	}
}
func taskID() string { return "01a1ea33-8180-7ce1-b5d1-e4b263a0454c" }

func TestSubmitSanitizesGitEnvironmentAndPreservesConfiguredIdentityAndHooks(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.root, "chosen.txt"), "submitted\n")
	f.git(t, f.root, "add", "chosen.txt")
	hook := filepath.Join(t.TempDir(), "hooks")
	if e := os.Mkdir(hook, 0755); e != nil {
		t.Fatal(e)
	}
	marker := filepath.Join(t.TempDir(), "hook-ran")
	script := "#!/bin/sh\nprintf ran > " + fmt.Sprintf("%q", marker) + "\n"
	write(t, filepath.Join(hook, "pre-commit"), script)
	if e := os.Chmod(filepath.Join(hook, "pre-commit"), 0755); e != nil {
		t.Fatal(e)
	}
	f.git(t, f.root, "config", "core.hooksPath", hook)
	t.Setenv("GIT_AUTHOR_NAME", "Injected Author")
	t.Setenv("GIT_AUTHOR_EMAIL", "injected@example.invalid")
	t.Setenv("GIT_COMMITTER_NAME", "Injected Committer")
	t.Setenv("GIT_COMMITTER_EMAIL", "injected-committer@example.invalid")
	t.Setenv("GIT_INDEX_FILE", filepath.Join(t.TempDir(), "poison-index"))
	t.Setenv("GIT_LITERAL_PATHSPECS", "0")
	out, e := Submit(context.Background(), Request{Root: f.root, TaskID: taskID(), Message: "Do work\n\nCo-Authored-By: fake <fake@example.invalid>\nAmuxID: wrong", Paths: []string{"chosen.txt"}}, DefaultPreferences(), nil)
	if e != nil {
		t.Fatal(e)
	}
	if out.Reference != f.remoteHead(t) {
		t.Fatalf("reference %s != remote head", out.Reference)
	}
	if got := f.git(t, f.root, "show", "-s", "--format=%an <%ae>%n%B", out.Reference); !strings.HasPrefix(got, "Configured Author <configured@example.invalid>\nDo work\n\nAmuxID: "+taskID()) {
		t.Fatalf("unexpected author/message: %q", got)
	}
	if got, _ := os.ReadFile(marker); string(got) != "ran" {
		t.Fatalf("configured hook did not run: %q", got)
	}
}

func TestHookStderrIsNotIncludedInSubmissionError(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.root, "chosen.txt"), "submitted\n")
	hookDir := filepath.Join(t.TempDir(), "hooks")
	if err := os.Mkdir(hookDir, 0755); err != nil {
		t.Fatal(err)
	}
	f.git(t, f.root, "config", "core.hooksPath", hookDir)
	const secret = "synthetic-hook-secret-DO-NOT-LEAK"
	write(t, filepath.Join(hookDir, "pre-commit"), "#!/bin/sh\necho '"+secret+"' >&2\nexit 1\n")
	if err := os.Chmod(filepath.Join(hookDir, "pre-commit"), 0755); err != nil {
		t.Fatal(err)
	}
	_, err := Submit(context.Background(), Request{Root: f.root, TaskID: taskID(), Message: "Try commit", Paths: []string{"chosen.txt"}}, DefaultPreferences(), nil)
	if err == nil {
		t.Fatal("failing hook unexpectedly succeeded")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("subprocess stderr leaked into error: %v", err)
	}
	if !strings.Contains(err.Error(), "git commit failed") {
		t.Fatalf("safe error lost command context: %v", err)
	}
}

func TestSubmitAllowsDeletedPathThroughSymlinkedParentAndRejectsIgnoredPath(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.root, "real", "deleted.txt"), "old\n")
	f.git(t, f.root, "add", "real/deleted.txt")
	f.git(t, f.root, "commit", "-m", "add deleted target")
	f.git(t, f.root, "push")
	if e := os.Symlink(filepath.Join(f.root, "real"), filepath.Join(f.root, "alias")); e != nil {
		t.Fatal(e)
	}
	if e := os.Remove(filepath.Join(f.root, "real", "deleted.txt")); e != nil {
		t.Fatal(e)
	}
	out, e := Submit(context.Background(), Request{Root: f.root, TaskID: taskID(), Message: "Remove file", Paths: []string{"alias/deleted.txt", "real/deleted.txt"}}, DefaultPreferences(), nil)
	if e != nil {
		t.Fatal(e)
	}
	if len(out.Files) != 1 || out.Files[0] != "real/deleted.txt" {
		t.Fatalf("canonical/deduplicated files: %#v", out.Files)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	write(t, filepath.Join(outside, "escape.txt"), "outside edit\n")
	if e := os.Symlink(outside, filepath.Join(f.root, "escape")); e != nil {
		t.Fatal(e)
	}
	if _, e = Submit(context.Background(), Request{Root: f.root, TaskID: taskID(), Message: "Escape", Paths: []string{"escape/escape.txt"}}, DefaultPreferences(), nil); e == nil || !strings.Contains(e.Error(), "not inside the project") {
		t.Fatalf("symlink escape should be rejected: %v", e)
	}
	f.git(t, f.root, "config", "core.excludesfile", filepath.Join(t.TempDir(), "ignore"))
	ignore := f.git(t, f.root, "config", "--get", "core.excludesfile")
	write(t, ignore, "ignored.txt\n")
	write(t, filepath.Join(f.root, "ignored.txt"), "ignored\n")
	_, e = Submit(context.Background(), Request{Root: f.root, TaskID: taskID(), Message: "Ignore", Paths: []string{"ignored.txt"}}, DefaultPreferences(), nil)
	if e == nil || !strings.Contains(e.Error(), "has no changes to submit") {
		t.Fatalf("ignored path should be rejected: %v", e)
	}
}

func TestSubmitHandlesLongNULPathspecListInInputOrder(t *testing.T) {
	f := newFixture(t)
	paths := make([]string, 0, 850)
	for i := 0; i < 850; i++ {
		name := fmt.Sprintf("dir/file-%04d.txt", i)
		write(t, filepath.Join(f.root, name), "content\n")
		paths = append(paths, name)
	}
	paths = append([]string{paths[17], "dir/file-0000.txt"}, paths[1:]...)
	out, e := Submit(context.Background(), Request{Root: f.root, TaskID: taskID(), Message: "Many files", Paths: paths}, DefaultPreferences(), nil)
	if e != nil {
		t.Fatal(e)
	}
	if len(out.Files) != 850 || out.Files[0] != "dir/file-0017.txt" || out.Files[1] != "dir/file-0000.txt" {
		t.Fatalf("unexpected path order/dedup: len=%d first=%v", len(out.Files), out.Files[:2])
	}
}

func TestPreflightAndMessageCleaning(t *testing.T) {
	f := newFixture(t)
	write(t, filepath.Join(f.root, "a"), "edit")
	before := f.head(t)
	if _, e := Submit(context.Background(), Request{Root: f.root, TaskID: taskID(), Message: "x", Paths: []string{"a"}}, Preferences{Mode: Mode("unknown")}, nil); e == nil {
		t.Fatal("unknown mode accepted")
	}
	if _, e := Submit(context.Background(), Request{Root: f.root, TaskID: taskID(), Message: "x", Paths: []string{"a"}}, Preferences{Mode: PullRequestMode}, nil); e == nil {
		t.Fatal("nil PR opener accepted")
	}
	if _, e := Submit(context.Background(), Request{Root: f.root, TaskID: taskID(), Message: "bad\x00message", Paths: []string{"a"}}, DefaultPreferences(), nil); e == nil {
		t.Fatal("NUL message accepted")
	}
	if got := f.head(t); got != before {
		t.Fatal("invalid request mutated checkout")
	}
	got := cleanMessage("\n Keep  \t\n\n\nGenerated with [Copilot]\nRefs: #4\n")
	if got != " Keep\n\nRefs: #4" {
		t.Fatalf("cleaned message = %q", got)
	}
	if _, e := uuid.Parse(taskID()); e != nil {
		t.Fatal(e)
	}
}

func TestSubmitRejectsEmptyRootBeforeGit(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, err := Submit(context.Background(), Request{
		Root: "", TaskID: taskID(), Message: "Task change", Paths: []string{"selected.txt"},
	}, DefaultPreferences(), nil)
	if err == nil || err.Error() != "project root must not be empty" {
		t.Fatalf("empty root was not rejected during preflight: %v", err)
	}
}
