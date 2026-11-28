package project

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func initCheckout(t *testing.T, directory string) {
	t.Helper()
	if err := os.MkdirAll(directory, 0o755); err != nil {
		t.Fatal(err)
	}
	command := exec.Command("git", "-C", directory, "init", "--quiet")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("initialize fixture checkout: %s: %v", output, err)
	}
}

func TestDiscoverLocalCheckoutsListsImmediateCheckoutsReadOnly(t *testing.T) {
	parent := t.TempDir()
	alpha := filepath.Join(parent, "alpha with spaces")
	zeta := filepath.Join(parent, "zeta")
	invalid := filepath.Join(parent, "unsupported origin")
	initCheckout(t, alpha)
	initCheckout(t, zeta)
	initCheckout(t, invalid)
	initCheckout(t, filepath.Join(parent, "nested", "checkout"))
	if output, err := exec.Command("git", "-C", alpha, "remote", "add", "origin", "https://github.com/example/alpha.git").CombinedOutput(); err != nil {
		t.Fatalf("configure fixture origin: %s: %v", output, err)
	}
	if output, err := exec.Command("git", "-C", invalid, "remote", "add", "origin", "file:///tmp/not-a-supported-origin").CombinedOutput(); err != nil {
		t.Fatalf("configure invalid fixture origin: %s: %v", output, err)
	}
	alphaConfig, err := os.ReadFile(filepath.Join(alpha, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}
	invalidConfig, err := os.ReadFile(filepath.Join(invalid, ".git", "config"))
	if err != nil {
		t.Fatal(err)
	}

	checkouts := DiscoverLocalCheckouts(context.Background(), parent)
	if len(checkouts) != 3 {
		t.Fatalf("discovered %d checkouts, want immediate alpha, unsupported origin, and zeta: %+v", len(checkouts), checkouts)
	}
	wantDirectories := []string{alpha, invalid, zeta}
	for i, want := range wantDirectories {
		resolved, err := filepath.EvalSymlinks(want)
		if err != nil {
			t.Fatal(err)
		}
		if checkouts[i].Directory != resolved {
			t.Fatalf("checkout[%d].Directory=%q, want %q", i, checkouts[i].Directory, resolved)
		}
	}
	if got := checkouts[0].CanonicalRemote; got == nil || *got != "github.com/example/alpha" {
		t.Fatalf("canonical remote=%v", got)
	}
	if checkouts[1].CanonicalRemote != nil || checkouts[2].CanonicalRemote != nil {
		t.Fatalf("unsupported or absent origins should remain unbound: %+v", checkouts)
	}
	after, err := os.ReadFile(filepath.Join(alpha, ".git", "config"))
	if err != nil || string(after) != string(alphaConfig) {
		t.Fatalf("discovery modified alpha config: err=%v", err)
	}
	after, err = os.ReadFile(filepath.Join(invalid, ".git", "config"))
	if err != nil || string(after) != string(invalidConfig) {
		t.Fatalf("discovery modified invalid-origin config: err=%v", err)
	}
}

func TestDiscoverLocalCheckoutsMissingOrEmptyParent(t *testing.T) {
	if got := DiscoverLocalCheckouts(context.Background(), filepath.Join(t.TempDir(), "missing")); len(got) != 0 {
		t.Fatalf("missing parent returned checkouts: %+v", got)
	}
	if got := DiscoverLocalCheckouts(context.Background(), " "); len(got) != 0 {
		t.Fatalf("empty parent returned checkouts: %+v", got)
	}
}
