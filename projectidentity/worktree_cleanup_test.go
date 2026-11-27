package projectidentity

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestRemoveTaskWorktreePreservesSharedDirtyLockedAndForeignCheckouts(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	git(t, root, "init")
	git(t, root, "-c", "user.name=fixture", "-c", "user.email=fixture@example.test", "commit", "--allow-empty", "-m", "fixture")
	if err := RemoveTaskWorktree(ctx, root, root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []string{"clean", "dirty", "locked", "detached"} {
		t.Run(kind, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "task")
			args := []string{"worktree", "add", "-b", kind, dir}
			if kind == "detached" {
				args = []string{"worktree", "add", "--detach", dir}
			}
			git(t, root, args...)
			if kind == "dirty" {
				if err := os.WriteFile(filepath.Join(dir, "unpublished"), []byte("preserve"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if kind == "locked" {
				git(t, root, "worktree", "lock", dir)
			}
			err := RemoveTaskWorktree(ctx, root, dir)
			if kind == "clean" {
				if err != nil {
					t.Fatal(err)
				}
				if _, err := os.Stat(dir); !os.IsNotExist(err) {
					t.Fatalf("worktree remains: %v", err)
				}
			} else {
				if err == nil {
					t.Fatal("unsafe worktree removed")
				}
				if _, err := os.Stat(dir); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	foreign := t.TempDir()
	git(t, foreign, "init")
	if err := RemoveTaskWorktree(ctx, root, foreign); err == nil {
		t.Fatal("foreign checkout accepted")
	}
	if err := RemoveTaskWorktree(ctx, filepath.Join(root, ".."), root); err == nil {
		t.Fatal("non-checkout pool accepted")
	}
}
