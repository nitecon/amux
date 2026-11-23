package desktop

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestWorkspaceFilesSkipNamedPipe(t *testing.T) {
	mkfifo, err := exec.LookPath("mkfifo")
	if err != nil {
		t.Skip("mkfifo is unavailable")
	}
	directory := t.TempDir()
	if output, err := exec.Command(mkfifo, filepath.Join(directory, "pipe")).CombinedOutput(); err != nil {
		t.Fatalf("create fixture pipe: %v: %s", err, output)
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	entries, _, err := listWorkspaceDirectory(root, ".")
	if err != nil || len(entries) != 0 {
		t.Fatalf("listed a special file: %v, %v", entries, err)
	}
	if _, err := readWorkspacePreview(root, "pipe"); err == nil {
		t.Fatal("preview accepted a named pipe")
	}
}

func TestWorkspacePreviewIsRootedAndLimited(t *testing.T) {
	parent := t.TempDir()
	rootPath := filepath.Join(parent, "checkout")
	if err := os.Mkdir(rootPath, 0755); err != nil {
		t.Fatal(err)
	}
	inside := strings.Repeat("a", maxWorkspacePreviewBytes+17)
	if err := os.WriteFile(filepath.Join(rootPath, "notes.md"), []byte(inside), 0600); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(parent, "outside.txt")
	if err := os.WriteFile(outside, []byte("outside"), 0600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	preview, err := readWorkspacePreview(root, "notes.md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(preview, inside[:maxWorkspacePreviewBytes]) || !strings.HasSuffix(preview, "\n[Preview limited to 64 KiB]") {
		t.Fatalf("preview was not byte bounded or marked: length=%d", len(preview))
	}
	if _, err := readWorkspacePreview(root, "../outside.txt"); err == nil {
		t.Fatal("preview escaped the attached checkout root")
	}
	if err := os.Symlink(outside, filepath.Join(rootPath, "outside-link.txt")); err != nil {
		t.Skipf("cannot create symlink fixture: %v", err)
	}
	if _, err := readWorkspacePreview(root, "outside-link.txt"); err == nil {
		t.Fatal("preview followed a symlink outside the attached checkout")
	}
}

func TestWorkspaceFileListingStaysInsideAttachedRoot(t *testing.T) {
	parent := t.TempDir()
	rootPath := filepath.Join(parent, "checkout")
	if err := os.Mkdir(rootPath, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(rootPath, "inside"), 0755); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	entries, truncated, err := listWorkspaceDirectory(root, ".")
	if err != nil || truncated {
		t.Fatalf("list root = %v, %v, %v", entries, truncated, err)
	}
	if len(entries) != 1 || entries[0].relative != "inside" || !entries[0].directory {
		t.Fatalf("root listing = %+v", entries)
	}
	if _, _, err := listWorkspaceDirectory(root, "../outside"); err == nil {
		t.Fatal("directory navigation escaped the attached checkout root")
	}
}

func TestWorkspaceFilesBindClearsPriorWorkspaceSelectionAndPreview(t *testing.T) {
	first := t.TempDir()
	second := t.TempDir()
	if err := os.WriteFile(filepath.Join(first, "first.md"), []byte("first workspace content"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "second.md"), []byte("second workspace content"), 0600); err != nil {
		t.Fatal(err)
	}
	callbacks := newUICallbacks()
	pane := newWorkspaceFilesPane(callbacks.dispatch)
	t.Cleanup(pane.Close)
	pane.Bind("workspace-one", first)
	if pane.workspace != "workspace-one" || pane.rootPath == "" {
		t.Fatalf("first workspace did not bind: id=%q root=%q", pane.workspace, pane.rootPath)
	}
	pane.openPreview(workspaceFileEntry{name: "first.md", relative: "first.md"})
	waitBrowser(t, callbacks, func() bool {
		return strings.Contains(pane.preview.Text, "first workspace content")
	})

	pane.Bind("workspace-two", second)
	if pane.workspace != "workspace-two" || pane.directory != "." || pane.selected != "" || pane.preview.Text != "" {
		t.Fatalf("workspace switch retained prior file state: id=%q directory=%q selected=%q preview=%q", pane.workspace, pane.directory, pane.selected, pane.preview.Text)
	}
	waitBrowser(t, callbacks, func() bool {
		return len(pane.entries) == 1 && pane.entries[0].name == "second.md"
	})
	pane.openPreview(workspaceFileEntry{name: "first.md", relative: "first.md"})
	if strings.Contains(pane.preview.Text, "first workspace content") {
		t.Fatal("a stale selection exposed the previous workspace file")
	}
}

func TestWorkspaceFilesCancelAndDiscardResultsAfterWorkspaceSwitch(t *testing.T) {
	first, second, third := t.TempDir(), t.TempDir(), t.TempDir()
	for dir, name := range map[string]string{first: "first.md", second: "second.md", third: "third.md"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	firstRoot, _ := canonicalWorkspaceRoot(first)
	secondRoot, _ := canonicalWorkspaceRoot(second)
	thirdRoot, _ := canonicalWorkspaceRoot(third)
	callbacks := newUICallbacks()
	var applied atomic.Int32
	pane := newWorkspaceFilesPane(func(fn func()) {
		callbacks.dispatch(func() { fn(); applied.Add(1) })
	})
	t.Cleanup(pane.Close)

	firstListingStarted := make(chan struct{})
	firstListingCanceled := make(chan struct{})
	readDirectory := pane.readDirectory
	pane.readDirectory = func(ctx context.Context, root, relative string) ([]workspaceFileEntry, bool, error) {
		if root == firstRoot {
			close(firstListingStarted)
			<-ctx.Done()
			close(firstListingCanceled)
			return []workspaceFileEntry{{name: "first.md", relative: "first.md"}}, false, nil
		}
		return readDirectory(ctx, root, relative)
	}
	pane.Bind("first", first)
	select {
	case <-firstListingStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("first workspace listing did not start")
	}
	pane.Bind("second", second)
	waitBrowser(t, callbacks, func() bool {
		return pane.workspace == "second" && pane.rootPath == secondRoot && len(pane.entries) == 1 && pane.entries[0].name == "second.md"
	})
	select {
	case <-firstListingCanceled:
	case <-time.After(5 * time.Second):
		t.Fatal("workspace switch did not cancel the first listing")
	}
	waitApplied(t, callbacks, &applied, 2)
	if len(pane.entries) != 1 || pane.entries[0].name != "second.md" {
		t.Fatalf("stale first listing replaced the second workspace: %+v", pane.entries)
	}

	previewStarted := make(chan struct{})
	previewCanceled := make(chan struct{})
	readPreview := pane.readPreview
	pane.readPreview = func(ctx context.Context, root, relative string) (string, error) {
		if root == secondRoot {
			close(previewStarted)
			<-ctx.Done()
			close(previewCanceled)
			return "stale second workspace preview", nil
		}
		return readPreview(ctx, root, relative)
	}
	pane.openPreview(workspaceFileEntry{name: "second.md", relative: "second.md"})
	select {
	case <-previewStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("second workspace preview did not start")
	}
	pane.Bind("third", third)
	waitBrowser(t, callbacks, func() bool {
		return pane.workspace == "third" && pane.rootPath == thirdRoot && len(pane.entries) == 1 && pane.entries[0].name == "third.md"
	})
	select {
	case <-previewCanceled:
	case <-time.After(5 * time.Second):
		t.Fatal("workspace switch did not cancel the previous preview")
	}
	waitApplied(t, callbacks, &applied, 4)
	if pane.selected != "" || pane.preview.Text != "" {
		t.Fatalf("stale preview crossed workspace switch: selected=%q preview=%q", pane.selected, pane.preview.Text)
	}
}

func TestWorkspaceFilesDiscardStalePreviewWithinSameWorkspace(t *testing.T) {
	callbacks := newUICallbacks()
	var applied atomic.Int32
	pane := newWorkspaceFilesPane(func(fn func()) {
		callbacks.dispatch(func() { fn(); applied.Add(1) })
	})
	t.Cleanup(pane.Close)
	stale := make(chan struct{}, 1)
	defer close(stale)
	pane.readPreview = func(_ context.Context, _, relative string) (string, error) {
		if relative == "old.txt" {
			<-stale
			return "", errors.New("old preview failed")
		}
		return "latest preview", nil
	}
	pane.Bind("workspace", t.TempDir())
	waitApplied(t, callbacks, &applied, 1)
	pane.openPreview(workspaceFileEntry{relative: "old.txt"})
	pane.openPreview(workspaceFileEntry{relative: "new.txt"})
	waitBrowser(t, callbacks, func() bool { return pane.preview.Text == "latest preview" })
	stale <- struct{}{}
	waitApplied(t, callbacks, &applied, 3)
	if pane.selected != "new.txt" || pane.preview.Text != "latest preview" {
		t.Fatalf("stale error replaced latest preview: selected=%q preview=%q", pane.selected, pane.preview.Text)
	}
}

func TestWorkspaceFilesNavigationHistoryAndFailedPath(t *testing.T) {
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "nested folder"), 0755); err != nil {
		t.Fatal(err)
	}
	callbacks := newUICallbacks()
	pane := newWorkspaceFilesPane(callbacks.dispatch)
	t.Cleanup(pane.Close)
	pane.Bind("workspace", root)
	waitBrowser(t, callbacks, func() bool { return pane.directory == "." && len(pane.entries) == 1 })

	pane.navigate("nested folder", true)
	waitBrowser(t, callbacks, func() bool { return pane.directory == "nested folder" })
	pane.moveHistory(-1)
	waitBrowser(t, callbacks, func() bool { return pane.directory == "." && pane.historyAt == 0 })
	pane.moveHistory(1)
	waitBrowser(t, callbacks, func() bool { return pane.directory == "nested folder" && pane.historyAt == 1 })
	pane.goUp()
	waitBrowser(t, callbacks, func() bool { return pane.directory == "." && pane.historyAt == 2 })

	pane.navigate("../outside", true)
	if pane.directory != "." || !strings.Contains(pane.status.Text, "inside the attached checkout") {
		t.Fatalf("failed navigation changed directory or omitted error: directory=%q status=%q", pane.directory, pane.status.Text)
	}
}

func waitApplied(t *testing.T, callbacks *uiCallbacks, applied *atomic.Int32, want int32) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		callbacks.pump()
		if applied.Load() >= want {
			callbacks.pump()
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	callbacks.pump()
	if applied.Load() < want {
		t.Fatalf("processed callbacks=%d, want at least %d", applied.Load(), want)
	}
}
