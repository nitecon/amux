package desktop

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
)

const (
	maxWorkspaceFileEntries  = 2000
	maxWorkspacePreviewBytes = 64 << 10
)

type workspaceFileEntry struct {
	name      string
	relative  string
	directory bool
}

type workspaceFilesPane struct {
	dispatch func(func())
	ctx      context.Context
	cancel   context.CancelFunc
	opCancel context.CancelFunc

	rootPath        string
	workspace       string
	generation      uint64
	navRevision     uint64
	previewRevision uint64
	directory       string
	entries         []workspaceFileEntry
	visible         []workspaceFileEntry
	history         []string
	historyAt       int
	selected        string
	truncated       bool
	showHidden      bool

	// These operations are replaceable in tests to gate stale workspace results.
	readDirectory func(context.Context, string, string) ([]workspaceFileEntry, bool, error)
	readPreview   func(context.Context, string, string) (string, error)

	location *widget.Label
	status   *widget.Label
	filter   *widget.Entry
	list     *widget.List
	preview  *widget.Label
	back     *widget.Button
	forward  *widget.Button
	up       *widget.Button
	rootView fyne.CanvasObject
}

func newWorkspaceFilesPane(dispatches ...func(func())) *workspaceFilesPane {
	dispatch := fyne.Do
	if len(dispatches) > 0 && dispatches[0] != nil {
		dispatch = dispatches[0]
	}
	p := &workspaceFilesPane{
		dispatch: dispatch, directory: ".", showHidden: true,
		readDirectory: readWorkspaceDirectoryPath,
		readPreview:   readWorkspacePreviewPath,
		location:      widget.NewLabel("No checkout attached"),
		status:        widget.NewLabel("Attach a checkout to this workspace to browse files."),
		filter:        widget.NewEntry(), preview: widget.NewLabel(""),
	}
	p.location.Truncation = fyne.TextTruncateClip
	p.filter.SetPlaceHolder("Filter files")
	p.back = widget.NewButton("Back", func() { p.moveHistory(-1) })
	p.forward = widget.NewButton("Forward", func() { p.moveHistory(1) })
	p.up = widget.NewButton("Up", p.goUp)
	hidden := widget.NewCheck("Hidden", func(show bool) {
		p.showHidden = show
		p.refreshVisible()
	})
	hidden.SetChecked(true)
	p.filter.OnChanged = func(string) { p.refreshVisible() }
	p.list = widget.NewList(func() int { return len(p.visible) }, func() fyne.CanvasObject { return widget.NewLabel("file") }, func(id widget.ListItemID, obj fyne.CanvasObject) {
		if id < 0 || id >= len(p.visible) {
			return
		}
		entry := p.visible[id]
		label := obj.(*widget.Label)
		if entry.directory {
			label.SetText("▸  " + entry.name)
		} else {
			label.SetText(entry.name)
		}
	})
	p.list.OnSelected = func(id widget.ListItemID) {
		if id < 0 || id >= len(p.visible) {
			return
		}
		entry := p.visible[id]
		if entry.directory {
			p.navigate(entry.relative, true)
			return
		}
		p.openPreview(entry)
	}
	toolbar := container.NewBorder(nil, nil, container.NewHBox(p.back, p.forward, p.up), hidden, p.location)
	left := container.NewBorder(container.NewVBox(toolbar, p.filter, p.status), nil, nil, nil, p.list)
	p.preview.Wrapping = fyne.TextWrapWord
	split := container.NewVSplit(left, container.NewVScroll(p.preview))
	split.Offset = .58
	p.rootView = split
	return p
}

func (p *workspaceFilesPane) Content() fyne.CanvasObject { return p.rootView }

func (b *Browser) syncWorkspaceFiles() {
	if b.workspaceFiles == nil {
		return
	}
	pane := b.workspaceFiles
	path := b.selection.CheckoutPath
	if local := b.localRailProjects[b.localRailSelection]; local != "" {
		path = local
	}
	if pane.workspace != b.activeWorkspaceID || pane.rootPath != path {
		pane.Bind(b.activeWorkspaceID, path)
	}
}

// Bind replaces the pane's checkout authority and clears prior navigation and
// preview state. Old jobs are canceled and their callbacks are fenced by token.
func (p *workspaceFilesPane) Bind(workspaceID, checkout string) {
	if p.opCancel != nil {
		p.opCancel()
		p.opCancel = nil
	}
	if p.cancel != nil {
		p.cancel()
	}
	p.ctx, p.cancel = context.WithCancel(context.Background())
	p.generation++
	p.navRevision++
	p.previewRevision++
	p.workspace = workspaceID
	p.rootPath = ""
	p.directory = "."
	p.entries = nil
	p.visible = nil
	p.history = nil
	p.historyAt = -1
	p.selected = ""
	p.truncated = false
	p.preview.SetText("")
	p.filter.SetText("")
	p.list.Refresh()
	if strings.TrimSpace(workspaceID) == "" || strings.TrimSpace(checkout) == "" {
		p.location.SetText("No checkout attached")
		p.status.SetText("Attach a checkout to this workspace to browse files.")
		p.updateNavigationButtons()
		return
	}
	rootPath, err := canonicalWorkspaceRoot(checkout)
	if err != nil {
		p.location.SetText(checkout)
		p.status.SetText("Checkout unavailable: " + err.Error())
		p.updateNavigationButtons()
		return
	}
	p.rootPath = rootPath
	p.location.SetText(rootPath)
	p.navigate(".", true)
}

func (p *workspaceFilesPane) Close() {
	if p.opCancel != nil {
		p.opCancel()
		p.opCancel = nil
	}
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
	p.ctx = nil
	p.generation++
	p.navRevision++
	p.previewRevision++
	p.workspace = ""
	p.rootPath = ""
	p.entries = nil
	p.visible = nil
	p.history = nil
	p.historyAt = -1
	p.directory = "."
	p.selected = ""
	p.truncated = false
	p.preview.SetText("")
	p.list.Refresh()
}

func canonicalWorkspaceRoot(checkout string) (string, error) {
	absolute, err := filepath.Abs(checkout)
	if err != nil {
		return "", err
	}
	canonical, err := filepath.EvalSymlinks(absolute)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return "", errors.New("attached checkout is not a directory")
	}
	return canonical, nil
}

func listWorkspaceDirectory(root *os.Root, relative string) ([]workspaceFileEntry, bool, error) {
	return listWorkspaceDirectoryContext(context.Background(), root, relative)
}

func listWorkspaceDirectoryContext(ctx context.Context, root *os.Root, relative string) ([]workspaceFileEntry, bool, error) {
	if root == nil || !validWorkspaceRelative(relative) {
		return nil, false, errors.New("directory is outside this workspace")
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	info, err := root.Stat(relative)
	if err != nil {
		return nil, false, err
	}
	if !info.IsDir() {
		return nil, false, errors.New("workspace entry is not a directory")
	}
	directoryRoot, err := root.OpenRoot(relative)
	if err != nil {
		return nil, false, err
	}
	directory, err := directoryRoot.Open(".")
	if err != nil {
		_ = directoryRoot.Close()
		return nil, false, err
	}
	names, err := directory.Readdirnames(maxWorkspaceFileEntries + 1)
	closeErr := directory.Close()
	rootCloseErr := directoryRoot.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	if closeErr != nil {
		return nil, false, closeErr
	}
	if rootCloseErr != nil {
		return nil, false, rootCloseErr
	}
	truncated := len(names) > maxWorkspaceFileEntries
	if truncated {
		names = names[:maxWorkspaceFileEntries]
	}
	entries := make([]workspaceFileEntry, 0, len(names))
	for _, name := range names {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		child := name
		if relative != "." {
			child = path.Join(relative, name)
		}
		// Stat follows only links that remain inside os.Root and never opens a
		// FIFO/device. Special files and links outside the checkout are omitted.
		info, err := root.Stat(child)
		if err != nil || (!info.IsDir() && !info.Mode().IsRegular()) {
			continue
		}
		entries = append(entries, workspaceFileEntry{name: name, relative: child, directory: info.IsDir()})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].directory != entries[j].directory {
			return entries[i].directory
		}
		a, b := strings.ToLower(entries[i].name), strings.ToLower(entries[j].name)
		if a == b {
			return entries[i].name < entries[j].name
		}
		return a < b
	})
	return entries, truncated, nil
}

func readWorkspaceDirectoryPath(ctx context.Context, rootPath, relative string) ([]workspaceFileEntry, bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, false, err
	}
	defer root.Close()
	entries, truncated, err := listWorkspaceDirectoryContext(ctx, root, relative)
	if ctx.Err() != nil {
		return nil, false, ctx.Err()
	}
	return entries, truncated, err
}

func readWorkspacePreview(root *os.Root, relative string) (string, error) {
	return readWorkspacePreviewContext(context.Background(), root, relative)
}

func readWorkspacePreviewContext(ctx context.Context, root *os.Root, relative string) (string, error) {
	if root == nil || relative == "." || !validWorkspaceRelative(relative) {
		return "", errors.New("choose a file inside this workspace")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	// Check the rooted file type before opening, so a named pipe or device
	// cannot block a preview request. Open uses os.Root for the confinement check.
	info, err := root.Stat(relative)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("preview is available for regular files only")
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	file, err := openWorkspacePreviewFile(root, relative)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("preview is available for regular files only")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxWorkspacePreviewBytes+1))
	if err != nil {
		return "", err
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	truncated := len(data) > maxWorkspacePreviewBytes
	if truncated {
		data = data[:maxWorkspacePreviewBytes]
	}
	if strings.IndexByte(string(data), 0) >= 0 {
		return "Binary file — preview unavailable", nil
	}
	preview := strings.ToValidUTF8(string(data), "�")
	if truncated {
		preview += "\n[Preview limited to 64 KiB]"
	}
	return preview, nil
}

func readWorkspacePreviewPath(ctx context.Context, rootPath, relative string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return "", err
	}
	defer root.Close()
	preview, err := readWorkspacePreviewContext(ctx, root, relative)
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	return preview, err
}

func validWorkspaceRelative(relative string) bool {
	if !fs.ValidPath(relative) {
		return false
	}
	native := filepath.FromSlash(relative)
	return !filepath.IsAbs(native) && filepath.VolumeName(native) == ""
}

func (p *workspaceFilesPane) navigate(relative string, record bool) {
	if p.ctx == nil || p.rootPath == "" || !validWorkspaceRelative(relative) {
		p.status.SetText("Choose a directory inside the attached checkout.")
		return
	}
	p.requestDirectory(relative, record, nil)
}

func (p *workspaceFilesPane) requestDirectory(relative string, record bool, historyIndex *int) {
	p.cancelOperation()
	workspaceCtx := p.ctx
	ctx, cancel := context.WithCancel(workspaceCtx)
	p.opCancel = cancel
	p.navRevision++
	p.previewRevision++
	p.selected = ""
	p.preview.SetText("")
	p.status.SetText("Loading workspace files…")
	p.updateNavigationButtons()
	workspace, rootPath := p.workspace, p.rootPath
	generation, revision := p.generation, p.navRevision
	reader := p.readDirectory
	go func() {
		entries, truncated, err := reader(ctx, rootPath, relative)
		p.dispatch(func() {
			if ctx.Err() != nil || p.ctx != workspaceCtx || p.workspace != workspace || p.rootPath != rootPath || p.generation != generation || p.navRevision != revision {
				return
			}
			p.opCancel = nil
			cancel()
			if err != nil {
				p.status.SetText("Cannot browse this workspace folder: " + err.Error())
				p.updateNavigationButtons()
				return
			}
			p.directory = relative
			p.entries = entries
			p.truncated = truncated
			if historyIndex != nil {
				p.historyAt = *historyIndex
			} else if record {
				p.history = p.history[:p.historyAt+1]
				if len(p.history) == 0 || p.history[len(p.history)-1] != relative {
					p.history = append(p.history, relative)
				}
				p.historyAt = len(p.history) - 1
			}
			p.location.SetText(p.rootPath)
			if relative != "." {
				p.location.SetText(filepath.Join(p.rootPath, filepath.FromSlash(relative)))
			}
			p.status.SetText(fmt.Sprintf("%d items%s", len(entries), map[bool]string{true: " · showing first 2000", false: ""}[truncated]))
			p.refreshVisible()
			p.updateNavigationButtons()
		})
	}()
}

func (p *workspaceFilesPane) openPreview(entry workspaceFileEntry) {
	if p.ctx == nil || !validWorkspaceRelative(entry.relative) || entry.directory {
		return
	}
	p.cancelOperation()
	workspaceCtx := p.ctx
	ctx, cancel := context.WithCancel(workspaceCtx)
	p.opCancel = cancel
	p.selected = entry.relative
	p.preview.SetText("Loading preview…")
	p.previewRevision++
	workspace, rootPath := p.workspace, p.rootPath
	generation, revision, selected := p.generation, p.previewRevision, p.selected
	reader := p.readPreview
	go func() {
		preview, err := reader(ctx, rootPath, selected)
		p.dispatch(func() {
			if ctx.Err() != nil || p.ctx != workspaceCtx || p.workspace != workspace || p.rootPath != rootPath || p.generation != generation || p.previewRevision != revision || p.selected != selected {
				return
			}
			p.opCancel = nil
			cancel()
			if err != nil {
				p.preview.SetText("Preview unavailable: " + err.Error())
			} else {
				p.preview.SetText(preview)
			}
		})
	}()
}

func (p *workspaceFilesPane) refreshVisible() {
	query := strings.ToLower(strings.TrimSpace(p.filter.Text))
	p.visible = p.visible[:0]
	for _, entry := range p.entries {
		if !p.showHidden && strings.HasPrefix(entry.name, ".") {
			continue
		}
		if query != "" && !strings.Contains(strings.ToLower(entry.name), query) {
			continue
		}
		p.visible = append(p.visible, entry)
	}
	if p.list != nil {
		p.list.Refresh()
	}
}

func (p *workspaceFilesPane) moveHistory(delta int) {
	next := p.historyAt + delta
	if next < 0 || next >= len(p.history) {
		return
	}
	p.requestDirectory(p.history[next], false, &next)
}

func (p *workspaceFilesPane) goUp() {
	if p.directory != "." {
		p.navigate(path.Dir(p.directory), true)
	}
}

func (p *workspaceFilesPane) cancelOperation() {
	if p.opCancel != nil {
		p.opCancel()
		p.opCancel = nil
	}
}

func (p *workspaceFilesPane) updateNavigationButtons() {
	p.back.Disable()
	p.forward.Disable()
	p.up.Disable()
	if p.ctx == nil || p.rootPath == "" {
		return
	}
	if p.historyAt > 0 {
		p.back.Enable()
	}
	if p.historyAt+1 < len(p.history) {
		p.forward.Enable()
	}
	if p.directory != "." {
		p.up.Enable()
	}
}
