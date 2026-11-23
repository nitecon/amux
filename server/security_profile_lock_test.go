package server

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestLockSecurityProfileUsesRustPathAndPrivateDirectory(t *testing.T) {
	profile := filepath.Join(t.TempDir(), "nested", "server")
	first, err := LockSecurityProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	if _, err := os.Stat(filepath.Join(profile, ".security-owner.lock")); err != nil {
		t.Fatalf("Rust-compatible owner lock missing: %v", err)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(profile)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0700 {
			t.Fatalf("profile directory mode=%v, want 0700", info.Mode().Perm())
		}
	}
	if second, err := LockSecurityProfile(profile); err == nil {
		second.Close()
		t.Fatal("concurrent security profile ownership was accepted")
	}
}
