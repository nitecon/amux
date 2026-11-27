package desktop

import (
	"testing"

	"github.com/nitecon/amux/storage"
)

func TestQuestionsViewNotifiesOnlyNewQuestionsAfterFirstLoad(t *testing.T) {
	view := newQuestionsView()
	first := []storage.TaskQuestion{{ID: "q1"}}
	if fresh := view.newQuestionIDs(first); len(fresh) != 0 {
		t.Fatalf("initial load notified: %+v", fresh)
	}
	fresh := view.newQuestionIDs([]storage.TaskQuestion{{ID: "q2"}, {ID: "q1"}})
	if len(fresh) != 1 || fresh[0].ID != "q2" || len(view.items) != 2 {
		t.Fatalf("fresh = %+v, items = %+v", fresh, view.items)
	}
	if fresh := view.newQuestionIDs(nil); len(fresh) != 0 || len(view.items) != 0 {
		t.Fatalf("cleared list = %+v, %+v", fresh, view.items)
	}
}

func TestQuestionsButtonLabelShowsOpenCount(t *testing.T) {
	if got := questionsButtonLabel(0); got != "Questions" {
		t.Fatalf("empty label %q", got)
	}
	if got := questionsButtonLabel(3); got != "Questions · 3" {
		t.Fatalf("count label %q", got)
	}
}
