package desktop

import (
	"context"
	"net/http"
	"os/exec"
	"testing"

	"github.com/nitecon/amux/projectidentity"
)

func TestBrowserWorktreeStatusLoadsAttachedCheckout(t *testing.T) {
	fixture := startEditorBrowser(t, func(next http.Handler) http.Handler { return next })
	b := fixture.browser
	checkout := t.TempDir()
	if output, err := exec.Command("git", "-C", checkout, "init", "--quiet", "-b", "fixture-branch").CombinedOutput(); err != nil {
		t.Fatalf("initialize fixture checkout: %v: %s", err, output)
	}
	identity, err := projectidentity.ResolveCheckout(context.Background(), checkout)
	if err != nil {
		t.Fatal(err)
	}
	workspace := b.activeWorkspaceID
	if !b.bindWorkspaceCheckoutWithOrigin(workspace, b.workspaceBindingGeneration[workspace], "demo", identity.Directory, nil) {
		t.Fatal("attach fixture checkout")
	}
	waitBrowser(t, fixture.callbacks, func() bool {
		return b.worktreeStatus.branch.Text == "fixture-branch"
	})
	if b.worktreeStatus.changes.Text != "Clean working tree" || b.worktreeStatus.commits.Text != "No commits yet" {
		t.Fatalf("unexpected worktree view: %q, %q", b.worktreeStatus.changes.Text, b.worktreeStatus.commits.Text)
	}
}

func TestWorktreeStatusRebindCancelsAndFencesOldRead(t *testing.T) {
	view := newWorktreeStatusView(func() {})
	first, firstCtx := view.bind(t.TempDir(), context.Background())
	second, _ := view.bind("", context.Background())
	if first == second {
		t.Fatal("rebind did not advance status generation")
	}
	select {
	case <-firstCtx.Done():
	default:
		t.Fatal("rebind did not cancel the old read")
	}
	view.finish(first, projectidentity.WorktreeStatus{Branch: "stale"}, nil)
	if view.branch.Text != "" || view.status.Text != "Attach a checkout to view Git status." {
		t.Fatalf("stale result replaced current view: branch=%q status=%q", view.branch.Text, view.status.Text)
	}
}

func TestWorktreeStatusViewRendersUnbornAndCleanResults(t *testing.T) {
	view := newWorktreeStatusView(func() {})
	generation, _ := view.bind("/checkout", context.Background())
	view.finish(generation, projectidentity.WorktreeStatus{Branch: "initial"}, nil)
	if view.branch.Text != "initial" || view.changes.Text != "Clean working tree" || view.commits.Text != "No commits yet" {
		t.Fatalf("unborn view branch=%q changes=%q commits=%q", view.branch.Text, view.changes.Text, view.commits.Text)
	}
}
