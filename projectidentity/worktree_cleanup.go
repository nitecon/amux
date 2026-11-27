package projectidentity

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/nitecon/amux/internal/hidewindow"
)

// RemoveTaskWorktree removes only a separate linked checkout of the pool's
// repository. The caller must stop and join its worker first. Git refuses
// dirty/locked worktrees; detached checkouts are retained to preserve commits.
func RemoveTaskWorktree(ctx context.Context, poolDirectory, taskDirectory string) error {
	pool, err := canonicalDirectory(poolDirectory)
	if err != nil {
		return err
	}
	task, err := canonicalDirectory(taskDirectory)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if pool == task {
		return nil
	} // shared pool checkout, never task-owned
	root, err := observe(ctx, task, "rev-parse", "--show-toplevel")
	if err != nil {
		return err
	}
	resolved, err := canonicalDirectory(strings.TrimSpace(root))
	if err != nil || resolved != task {
		return fmt.Errorf("task directory is not a worktree root")
	}
	common, err := observe(ctx, pool, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	other, err := observe(ctx, task, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return err
	}
	if common != other {
		return fmt.Errorf("task worktree belongs to another repository")
	}
	gitDir, err := observe(ctx, task, "rev-parse", "--path-format=absolute", "--git-dir")
	if err != nil {
		return err
	}
	if gitDir == other {
		return fmt.Errorf("cannot remove the repository's main checkout")
	}
	if _, err := observe(ctx, task, "symbolic-ref", "--quiet", "HEAD"); err != nil {
		return fmt.Errorf("retaining detached task worktree to preserve commits")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-c", "core.hooksPath="+os.DevNull, "-C", pool, "worktree", "remove", "--", task)
	cmd.Env = cloneEnvironment()
	cmd.WaitDelay = 100 * time.Millisecond
	hidewindow.Apply(cmd)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("task worktree retained: Git refused removal (check dirty files or locks): %w", err)
	}
	return nil
}
