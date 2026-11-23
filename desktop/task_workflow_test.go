package desktop

import (
	"testing"

	fynetest "fyne.io/fyne/v2/test"
	"github.com/nitecon/amux/storage"
)

func TestTaskWorkflowControlsFollowSupportedHumanTransitions(t *testing.T) {
	app := fynetest.NewApp()
	defer app.Quit()
	browser := &Browser{}
	w := newTaskWorkflow(browser)
	browser.taskWorkflow = w
	w.selectTask("demo", "task-a")

	w.setTask("demo", "task-a", &storage.Task{ID: "task-a", ProjectIdent: "demo", Status: "todo"})
	if !w.close.Visible() || !w.park.Visible() || w.resume.Visible() {
		t.Fatal("todo task controls do not match supported transitions")
	}
	if !w.reviewAction.Visible() {
		t.Fatal("active task should show the park reason entry")
	}
	w.reviewAction.SetText("Ask for one final review")
	if w.park.Disabled() {
		t.Fatal("valid review action should enable user review")
	}
	w.selectTask("demo", "task-b")
	w.selectTask("demo", "task-a")
	if w.reviewAction.Text != "Ask for one final review" {
		t.Fatal("review reason draft did not remain scoped to its task")
	}

	w.setTask("demo", "task-a", &storage.Task{ID: "task-a", ProjectIdent: "demo", Status: "user_review"})
	if !w.close.Visible() || w.close.Text != "Accept and close" || w.park.Visible() || !w.resume.Visible() {
		t.Fatal("user review controls do not match supported transitions")
	}
	if w.reviewAction.Visible() {
		t.Fatal("user review task showed an inactive park reason entry")
	}
	decision := "Check the deployment output before release"
	w.setTask("demo", "task-a", &storage.Task{ID: "task-a", ProjectIdent: "demo", Status: "user_review", ReviewAction: &decision})
	if !w.reviewPrompt.Visible() || w.reviewPrompt.Text != "Requested review: "+decision {
		t.Fatal("authoritative user-review request was not shown")
	}
	if w.reviewAction.Text != "Ask for one final review" {
		t.Fatal("server review prompt overwrote the local park reason draft")
	}
	w.setTask("demo", "task-a", &storage.Task{ID: "task-a", ProjectIdent: "demo", Status: "in_progress"})
	if !w.reviewAction.Visible() || w.reviewAction.Text != "Ask for one final review" {
		t.Fatal("park reason draft was not restored for an active task")
	}
	w.setTask("demo", "task-a", &storage.Task{ID: "task-a", ProjectIdent: "demo", Status: "done"})
	if w.close.Visible() || w.park.Visible() || w.resume.Visible() {
		t.Fatal("done task exposed a workflow transition")
	}
	if w.reviewPrompt.Visible() || w.reviewPrompt.Text != "" {
		t.Fatal("server review prompt remained visible outside user review")
	}
	if w.reviewAction.Visible() {
		t.Fatal("done task showed a park reason entry")
	}
}

func TestWorkflowAcknowledgmentAcceptsOnlyRequestedTransition(t *testing.T) {
	for _, tc := range []struct {
		action, status string
		want           bool
	}{
		{"close", "done", true},
		{"close", "todo", false},
		{"park", "user_review", true},
		{"park", "todo", false},
		{"resume", "todo", true},
		{"resume", "in_progress", false},
	} {
		if got := workflowAcknowledges(tc.action, tc.status); got != tc.want {
			t.Errorf("workflowAcknowledges(%q, %q) = %t, want %t", tc.action, tc.status, got, tc.want)
		}
	}
}
