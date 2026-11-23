package desktop

import (
	"context"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	fynetest "fyne.io/fyne/v2/test"
	"github.com/nitecon/amux/projectidentity"
)

func TestBrowserFilesFollowAttachedWorkspace(t *testing.T) {
	fixture := startEditorBrowser(t, func(next http.Handler) http.Handler { return next })
	b := fixture.browser
	checkout := t.TempDir()
	if output, err := exec.Command("git", "-C", checkout, "init", "--quiet").CombinedOutput(); err != nil {
		t.Fatalf("initialize fixture checkout: %v: %s", err, output)
	}
	if err := os.WriteFile(filepath.Join(checkout, "notes.md"), []byte("workspace fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	identity, err := projectidentity.ResolveCheckout(context.Background(), checkout)
	if err != nil {
		t.Fatal(err)
	}
	workspace := b.activeWorkspaceID
	if !b.bindWorkspaceCheckoutWithOrigin(workspace, b.workspaceBindingGeneration[workspace], "demo", identity.Directory, nil) {
		t.Fatal("attach fixture checkout")
	}
	pane := b.workspaceFiles
	waitBrowser(t, fixture.callbacks, func() bool { return len(pane.visible) > 0 })
	previewStarted, releasePreview := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releasePreview) }) }
	t.Cleanup(release)
	readPreview := pane.readPreview
	pane.readPreview = func(ctx context.Context, root, relative string) (string, error) {
		close(previewStarted)
		select {
		case <-releasePreview:
			return readPreview(ctx, root, relative)
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	for i, entry := range pane.visible {
		if entry.name == "notes.md" {
			pane.list.Select(i)
			break
		}
	}
	waitBrowser(t, fixture.callbacks, func() bool {
		select {
		case <-previewStarted:
			return true
		default:
			return false
		}
	})
	if err := os.WriteFile(filepath.Join(checkout, "pending.txt"), []byte("new change"), 0600); err != nil {
		t.Fatal(err)
	}
	fynetest.Tap(b.worktreeStatus.refresh)
	waitBrowser(t, fixture.callbacks, func() bool {
		return strings.Contains(b.worktreeStatus.changes.Text, "?? pending.txt")
	})
	if pane.selected != "notes.md" || pane.preview.Text != "Loading preview…" {
		t.Fatalf("refreshing Git replaced the pending Files preview: %q", pane.preview.Text)
	}
	release()
	waitBrowser(t, fixture.callbacks, func() bool { return pane.preview.Text == "workspace fixture" })
	pane.readPreview = readPreview
	b.addWorkspace()
	if pane.workspace != b.activeWorkspaceID || pane.rootPath != "" || pane.preview.Text != "" {
		t.Fatal("new workspace retained the previous checkout or preview")
	}
	b.selectWorkspace(workspace)
	waitBrowser(t, fixture.callbacks, func() bool {
		return pane.workspace == workspace && pane.rootPath == identity.Directory && len(pane.visible) > 0
	})
}
