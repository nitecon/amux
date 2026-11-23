package projectidentity

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Rust: tests/project_identity_contract.rs::upstream_and_checkout_directory_define_identity.
func TestCheckoutIdentity(t *testing.T) {
	first, second := t.TempDir(), t.TempDir()
	ssh, err := FromParts(first, "git@github.com:Nitecon/AMUX.git")
	if err != nil {
		t.Fatal(err)
	}
	https, err := FromParts(first, "https://github.com/nitecon/amux")
	if err != nil {
		t.Fatal(err)
	}
	if ssh != https || ssh.GatewayProject != "amux" {
		t.Fatalf("equivalent origins diverge: %+v, %+v", ssh, https)
	}
	other, err := FromParts(second, "https://github.com/nitecon/amux")
	if err != nil {
		t.Fatal(err)
	}
	changed, err := FromParts(first, "https://github.com/nitecon/SRE.git")
	if err != nil {
		t.Fatal(err)
	}
	if other.ID == ssh.ID || changed.ID == ssh.ID || changed.GatewayProject != "sre" {
		t.Fatal("checkout/upstream identity collision")
	}
	if ssh.SessionID("codex-1") != https.SessionID("codex-1") || ssh.SessionID("codex-1") == ssh.SessionID("codex-2") || ssh.SessionID("codex-1") == ssh.SessionID("claude-1") {
		t.Fatal("session namespace collision")
	}
}

// Golden generated with Python uuid.uuid5 from Rust's exact JSON input format.
func TestUUIDCompatibility(t *testing.T) {
	id := projectID("/tmp/example", "github.com/nitecon/amux")
	if id != "1b97e7a9-372c-57c2-b772-d3d6b90d61e8" {
		t.Fatalf("UUID format changed: %s", id)
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("fixture git: %s: %v", output, err)
	}
}

// Rust: tests/project_identity_contract.rs::git_subdirectory_resolves_to_same_checkout
// and directory_without_origin_has_stable_local_identity.
func TestResolveCheckout(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "--quiet")
	local, err := Resolve(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	again, err := Resolve(context.Background(), dir)
	if err != nil || local != again || local.Upstream != "" || local.GatewayProject != "" || local.ID == "" {
		t.Fatalf("unstable local identity: %+v, %v", again, err)
	}
	git(t, dir, "remote", "add", "origin", "git@github.com:nitecon/amux.git")
	child := filepath.Join(dir, "subdir")
	if err := os.Mkdir(child, 0700); err != nil {
		t.Fatal(err)
	}
	root, err := Resolve(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	nested, err := Resolve(context.Background(), child)
	if err != nil || root != nested || root.GatewayProject != "amux" {
		t.Fatalf("nested checkout: %+v, %v", nested, err)
	}
	// Inherited Git overrides must not redirect a worker to another checkout.
	t.Setenv("GIT_DIR", filepath.Join(t.TempDir(), "missing"))
	nested, err = Resolve(context.Background(), child)
	if err != nil || root != nested {
		t.Fatalf("inherited Git override: %+v, %v", nested, err)
	}
}

func TestReadWorktreeStatus(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "--quiet", "-b", "main")
	git(t, dir, "config", "user.name", "Fixture")
	git(t, dir, "config", "user.email", "fixture@example.test")
	for i := 1; i <= 9; i++ {
		if err := os.WriteFile(filepath.Join(dir, "commit.txt"), []byte(fmt.Sprint(i)), 0600); err != nil {
			t.Fatal(err)
		}
		git(t, dir, "add", "commit.txt")
		git(t, dir, "commit", "--quiet", "-m", fmt.Sprintf("commit %d", i))
	}
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadWorktreeStatus(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Branch != "main" || got.Changes != "?? untracked.txt" || len(got.Commits) != 8 || !strings.Contains(got.Commits[0], "commit 9") || strings.Contains(strings.Join(got.Commits, "\n"), "commit 1\n") {
		t.Fatalf("status = %+v", got)
	}
}

func TestReadWorktreeStatusHonorsCancellation(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "--quiet")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ReadWorktreeStatus(ctx, dir); err == nil {
		t.Fatal("cancelled status read succeeded")
	}
}

func TestReadWorktreeStatusForUnbornCheckout(t *testing.T) {
	dir := t.TempDir()
	git(t, dir, "init", "--quiet", "-b", "initial")
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err := ReadWorktreeStatus(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	if got.Branch != "initial" || got.Changes != "?? untracked.txt" || len(got.Commits) != 0 {
		t.Fatalf("unborn checkout status = %+v", got)
	}
}

func TestReadWorktreeStatusDisablesConfiguredFSMonitor(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a local shell hook")
	}
	dir := t.TempDir()
	git(t, dir, "init", "--quiet", "-b", "main")
	marker := filepath.Join(t.TempDir(), "fsmonitor-ran")
	hook := filepath.Join(t.TempDir(), "fsmonitor")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch \""+marker+"\"\necho token\n"), 0700); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "config", "core.fsmonitor", hook)
	if _, err := ReadWorktreeStatus(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("configured fsmonitor executed during status read: stat err=%v", err)
	}
}

// Rust: tests/project_identity_contract.rs::symlink_alias_does_not_create_a_second_project.
func TestSymlinkAlias(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink privilege is not guaranteed")
	}
	dir := t.TempDir()
	alias := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(dir, alias); err != nil {
		t.Fatal(err)
	}
	a, err := FromParts(dir, "git@github.com:nitecon/amux.git")
	if err != nil {
		t.Fatal(err)
	}
	b, err := FromParts(alias, "https://github.com/nitecon/amux")
	if err != nil || a != b {
		t.Fatalf("alias identity changed: %+v, %v", b, err)
	}
}

func TestIdentityCompatibilityForHTMLCharacters(t *testing.T) {
	for directory, want := range map[string]string{
		"/tmp/a&b": "50f566ea-9738-5328-b0d8-a6d3cf3f421e",
	} {
		if got := projectID(directory, "github.com/nitecon/amux"); got != want {
			t.Fatalf("identity for %q = %s, want %s", directory, got, want)
		}
	}
}

func TestSessionUUIDCompatibility(t *testing.T) {
	i := Identity{ID: "1b97e7a9-372c-57c2-b772-d3d6b90d61e8"}
	if got := i.SessionID("codex-1"); got != "67df23bf-fc73-582a-b193-090cf4e651d9" {
		t.Fatalf("session UUID changed: %s", got)
	}
}

func TestUnicodeIdentityCompatibility(t *testing.T) {
	if got := projectID("/tmp/a\u2028b", "github.com/nitecon/amux"); got != "b2fa5486-7d5d-5962-8baa-69670fefc7a9" {
		t.Fatalf("Unicode identity changed: %s", got)
	}
	// A literal backslash-u sequence must remain distinct from a Unicode separator.
	if projectID(`/tmp/a\u2028b`, "github.com/nitecon/amux") == projectID("/tmp/a\u2028b", "github.com/nitecon/amux") {
		t.Fatal("escaped path collision")
	}
}
