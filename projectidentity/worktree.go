package projectidentity

import (
	"context"
	"strings"
)

// WorktreeStatus is a bounded, read-only snapshot of a Git checkout.
type WorktreeStatus struct {
	Branch  string
	Changes string
	Commits []string
}

// ReadWorktreeStatus observes the current branch, short status, and the eight
// most recent commits using the same bounded, environment-scrubbed Git runner
// as checkout identity resolution.
func ReadWorktreeStatus(ctx context.Context, directory string) (WorktreeStatus, error) {
	checkout, err := ResolveCheckout(ctx, directory)
	if err != nil {
		return WorktreeStatus{}, err
	}
	branch, err := observe(ctx, checkout.Directory, "branch", "--show-current")
	if err != nil {
		return WorktreeStatus{}, err
	}
	changes, err := observe(ctx, checkout.Directory, "-c", "core.fsmonitor=false", "status", "--short")
	if err != nil {
		return WorktreeStatus{}, err
	}
	commits, err := observe(ctx, checkout.Directory, "log", "-8", "--format=%h  %s")
	if err != nil {
		if observeContextError(err) {
			return WorktreeStatus{}, err
		}
		// Git treats log on an unborn branch as an error; the branch and short
		// status remain useful, with no recent commits to display.
		if _, headErr := observe(ctx, checkout.Directory, "rev-parse", "--verify", "--quiet", "HEAD"); headErr == nil {
			return WorktreeStatus{}, err
		} else if observeContextError(headErr) {
			return WorktreeStatus{}, headErr
		}
		commits = ""
	}
	result := WorktreeStatus{Branch: strings.TrimRight(branch, "\r\n"), Changes: strings.TrimRight(changes, "\r\n")}
	for _, line := range strings.Split(strings.TrimRight(commits, "\r\n"), "\n") {
		if line != "" {
			result.Commits = append(result.Commits, strings.TrimSuffix(line, "\r"))
		}
	}
	return result, nil
}
