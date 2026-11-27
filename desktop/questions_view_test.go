package desktop

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	fynetest "fyne.io/fyne/v2/test"
	"github.com/nitecon/amux/gatewayclient"
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

func TestQuestionsRefreshKeepsDraftAndSendsAnswerForItsQuestion(t *testing.T) {
	app := fynetest.NewApp()
	defer app.Quit()
	posted := make(chan map[string]string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			_, _ = w.Write([]byte("[]"))
			return
		}
		var body map[string]string
		_ = json.NewDecoder(r.Body).Decode(&body)
		posted <- body
		_ = json.NewEncoder(w).Encode(storage.TaskComment{ID: "0190a1b2-c3d4-7e5f-8a9b-0c1d2e3f4a5b", TaskID: "t1", Author: "user", AuthorType: "user", Content: storage.TaskAnswerComment("Which branch?", body["content"])})
	}))
	defer server.Close()
	client, err := gatewayclient.New(server.URL, "test-key", server.Client())
	if err != nil {
		t.Fatal(err)
	}
	b := &Browser{ctx: context.Background(), client: client, dispatch: func(fn func()) { fn() }, questions: newQuestionsView()}
	q1 := storage.TaskQuestion{ID: "q1", TaskID: "t1", ProjectIdent: "demo", Question: "Which branch?"}
	b.questions.items = []storage.TaskQuestion{q1}
	b.renderQuestions()
	card := b.questions.cards["q1"]
	card.answer.SetText("Use the branch with")

	// An unchanged refresh, then a new question, must not replace the composer.
	b.renderQuestions()
	b.questions.items = []storage.TaskQuestion{{ID: "q2", TaskID: "t1", ProjectIdent: "demo", Question: "Which release?"}, q1}
	b.renderQuestions()
	if b.questions.cards["q1"] != card || card.answer.Text != "Use the branch with" || len(b.questions.rows.Objects) != 2 || b.questions.rows.Objects[1] != card.object {
		t.Fatalf("refresh replaced the draft: %q", b.questions.cards["q1"].answer.Text)
	}

	card.send(card.answer.Text)
	select {
	case body := <-posted:
		if body["question_id"] != "q1" || body["content"] != "Use the branch with" || body["author_type"] != "user" {
			t.Fatalf("answer not bound to its question: %+v", body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("answer was not sent")
	}
	b.wg.Wait()
	if status := b.questions.status.Text; strings.HasPrefix(status, "Answer not sent") {
		t.Fatalf("acknowledged answer reported failure: %s", status)
	}
}
