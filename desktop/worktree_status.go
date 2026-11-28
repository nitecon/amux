package desktop

import (
	"context"
	"strings"
	"time"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
	projectid "github.com/nitecon/amux/identity/project"
)

type worktreeStatusView struct {
	accordion  *widget.Accordion
	branch     *widget.Label
	changes    *widget.Label
	commits    *widget.Label
	status     *widget.Label
	refresh    *widget.Button
	cancel     context.CancelFunc
	generation uint64
}

func newWorktreeStatusView(onRefresh func()) *worktreeStatusView {
	v := &worktreeStatusView{branch: widget.NewLabel(""), changes: widget.NewLabel(""), commits: widget.NewLabel(""), status: widget.NewLabel("Attach a checkout to view Git status."), refresh: widget.NewButton("Refresh Git status", onRefresh)}
	v.branch.Wrapping = fyne.TextWrapWord
	v.changes.Wrapping = fyne.TextWrapWord
	v.commits.Wrapping = fyne.TextWrapWord
	body := container.NewVBox(v.status, widget.NewLabelWithStyle("Branch", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}), v.branch, widget.NewLabelWithStyle("Working tree", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}), v.changes, widget.NewLabelWithStyle("Recent commits", fyne.TextAlignLeading, fyne.TextStyle{Bold: true}), v.commits, v.refresh)
	v.accordion = widget.NewAccordion(widget.NewAccordionItem("Git Worktree Status", container.NewVScroll(body)))
	return v
}

func (v *worktreeStatusView) bind(checkout string, parent context.Context) (uint64, context.Context) {
	if v.cancel != nil {
		v.cancel()
		v.cancel = nil
	}
	v.generation++
	generation := v.generation
	v.branch.SetText("")
	v.changes.SetText("")
	v.commits.SetText("")
	v.refresh.Disable()
	if strings.TrimSpace(checkout) == "" {
		v.status.SetText("Attach a checkout to view Git status.")
		return generation, nil
	}
	v.status.SetText("Reading Git status…")
	if parent == nil {
		v.status.SetText("Git status will load when the browser starts.")
		return generation, nil
	}
	ctx, cancel := context.WithCancel(parent)
	v.cancel = cancel
	v.refresh.Enable()
	return generation, ctx
}

func (v *worktreeStatusView) finish(generation uint64, result projectid.WorktreeStatus, err error) {
	if generation != v.generation {
		return
	}
	v.cancel = nil
	v.refresh.Enable()
	if err != nil {
		v.status.SetText("Git status unavailable: " + err.Error())
		return
	}
	v.status.SetText(" ")
	v.branch.SetText(result.Branch)
	if result.Changes == "" {
		v.changes.SetText("Clean working tree")
	} else {
		v.changes.SetText(result.Changes)
	}
	if len(result.Commits) == 0 {
		v.commits.SetText("No commits yet")
	} else {
		v.commits.SetText(strings.Join(result.Commits, "\n"))
	}
}

func (v *worktreeStatusView) close() {
	if v.cancel != nil {
		v.cancel()
		v.cancel = nil
	}
	v.generation++
}

func (b *Browser) refreshWorktreeStatus() {
	if b.worktreeStatus == nil {
		return
	}
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		return
	}
	parent := b.ctx
	workspace, project, checkout := b.activeWorkspaceID, b.selection.Project, b.selection.CheckoutPath
	revision := b.workspaceBindingGeneration[workspace]
	if parent == nil {
		b.mu.Unlock()
		b.worktreeStatus.bind(checkout, nil)
		return
	}
	b.wg.Add(1)
	b.mu.Unlock()
	generation, ctx := b.worktreeStatus.bind(checkout, parent)
	if ctx == nil {
		b.wg.Done()
		return
	}
	go func() {
		defer b.wg.Done()
		readCtx, cancel := context.WithTimeout(ctx, 11*time.Second)
		defer cancel()
		result, err := projectid.ReadWorktreeStatus(readCtx, checkout)
		b.dispatch(func() {
			b.mu.Lock()
			closing := b.closing
			b.mu.Unlock()
			if closing || workspace != b.activeWorkspaceID || revision != b.workspaceBindingGeneration[workspace] || project != b.selection.Project || checkout != b.selection.CheckoutPath {
				return
			}
			b.worktreeStatus.finish(generation, result, err)
		})
	}()
}
