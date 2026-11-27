package storage

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestTaskQuestionRaiseListAnswerAndClear(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	actor := taskActor()
	task := makeTask(t, s, nil)
	if _, err := s.AskTaskQuestion(ctx, "tasks", task.ID, "Which?", nil, actor); !errors.Is(err, ErrTaskOwnership) {
		t.Fatalf("unowned task accepted a question: %v", err)
	}
	if _, err := s.ApplyTaskTransition(ctx, "tasks", task.ID, "in_progress", actor, TaskTransitionPolicy{}); err != nil {
		t.Fatal(err)
	}
	other := *actor
	other.InstanceID = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
	if _, err := s.AskTaskQuestion(ctx, "tasks", task.ID, "Which?", nil, &other); !errors.Is(err, ErrTaskOwnership) {
		t.Fatalf("non-owner raised a question: %v", err)
	}
	if _, err := s.AskTaskQuestion(ctx, "tasks", task.ID, "  ", nil, actor); !errors.Is(err, ErrTaskQuestionInput) {
		t.Fatalf("blank question accepted: %v", err)
	}
	if _, err := s.AskTaskQuestion(ctx, "tasks", task.ID, "Which?", []string{"a\nb"}, actor); !errors.Is(err, ErrTaskQuestionInput) {
		t.Fatalf("multi-line option accepted: %v", err)
	}

	question, err := s.AskTaskQuestion(ctx, "tasks", task.ID, " Drop the other task's commits? ", []string{"Drop them", "Rebase them"}, actor)
	if err != nil {
		t.Fatal(err)
	}
	if question.Question != "Drop the other task's commits?" || len(question.Options) != 2 || question.TaskTitle != "Work" {
		t.Fatalf("unexpected question: %+v", question)
	}
	detail, err := s.Task(ctx, "tasks", task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !detail.Task.WaitingOnUser || detail.Task.Status != "in_progress" || detail.Task.OwnerOrigin == nil || *detail.Task.OwnerOrigin != *actor {
		t.Fatalf("question changed ownership or did not flag waiting: %+v", detail.Task)
	}
	last := detail.Comments[len(detail.Comments)-1]
	if last.ID != question.CommentID || last.AuthorType != "agent" || !strings.Contains(last.Content, "2. Rebase them") {
		t.Fatalf("question comment missing from thread: %+v", last)
	}
	summaries, err := s.Tasks(ctx, "tasks", OpenTaskStatuses(), false)
	if err != nil || len(summaries) != 1 || !summaries[0].WaitingOnUser {
		t.Fatalf("task list does not flag waiting: %+v %v", summaries, err)
	}

	open, err := s.OpenTaskQuestions(ctx, "")
	if err != nil || len(open) != 1 || open[0].ID != question.ID || open[0].ProjectIdent != "tasks" || open[0].Origin == nil {
		t.Fatalf("open questions = %+v, %v", open, err)
	}
	if open, err := s.OpenTaskQuestions(ctx, "elsewhere"); err != nil || len(open) != 0 {
		t.Fatalf("project filter leaked questions: %+v %v", open, err)
	}

	if _, err := s.AddTaskComment(ctx, "tasks", task.ID, actor.SessionID, "agent", "still waiting", nil, actor); err != nil {
		t.Fatal(err)
	}
	if open, _ := s.OpenTaskQuestions(ctx, ""); len(open) != 1 {
		t.Fatal("an agent comment answered the question")
	}
	if _, err := s.AddTaskComment(ctx, "tasks", task.ID, "user", "user", "Unrelated note", nil, nil); err != nil {
		t.Fatal(err)
	}
	if open, _ := s.OpenTaskQuestions(ctx, ""); len(open) != 1 {
		t.Fatal("an unrelated user comment answered the question")
	}
	answer, err := s.AnswerTaskQuestion(ctx, "tasks", task.ID, question.ID, "Drop them")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AnswerTaskQuestion(ctx, "tasks", task.ID, question.ID, "Again"); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("closed question answered twice: %v", err)
	}
	if open, _ := s.OpenTaskQuestions(ctx, ""); len(open) != 0 {
		t.Fatalf("user answer did not clear the question: %+v", open)
	}
	var answered string
	if err := s.db.QueryRowContext(ctx, "SELECT answer_comment_id FROM task_questions WHERE id=?", question.ID).Scan(&answered); err != nil || answered != answer.ID {
		t.Fatalf("answer comment not recorded: %q %v", answered, err)
	}
	detail, _ = s.Task(ctx, "tasks", task.ID)
	if detail.Task.WaitingOnUser || detail.Task.Status != "in_progress" {
		t.Fatalf("answer did not clear waiting state: %+v", detail.Task)
	}
}

func TestAnsweringOneQuestionLeavesSiblingOpen(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	actor := taskActor()
	task := makeTask(t, s, nil)
	if _, err := s.ApplyTaskTransition(ctx, "tasks", task.ID, "in_progress", actor, TaskTransitionPolicy{}); err != nil {
		t.Fatal(err)
	}
	branch, err := s.AskTaskQuestion(ctx, "tasks", task.ID, "Which branch?", nil, actor)
	if err != nil {
		t.Fatal(err)
	}
	release, err := s.AskTaskQuestion(ctx, "tasks", task.ID, "Which release?", nil, actor)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AnswerTaskQuestion(ctx, "tasks", task.ID, branch.ID, "Use main"); err != nil {
		t.Fatal(err)
	}
	open, err := s.OpenTaskQuestions(ctx, "")
	if err != nil || len(open) != 1 || open[0].ID != release.ID {
		t.Fatalf("sibling question was closed: %+v %v", open, err)
	}
	if detail, _ := s.Task(ctx, "tasks", task.ID); !detail.Task.WaitingOnUser {
		t.Fatal("task stopped waiting with a question still open")
	}
	other := makeTask(t, s, nil)
	if _, err := s.AnswerTaskQuestion(ctx, "tasks", other.ID, release.ID, "v1"); !errors.Is(err, ErrTaskConflict) {
		t.Fatalf("question answered through another task: %v", err)
	}
}

func TestOpenTaskQuestionsSkipFinishedTasksNewestFirst(t *testing.T) {
	s := taskStore(t)
	ctx := context.Background()
	actor := taskActor()
	var ids []string
	for _, title := range []string{"First", "Second", "Third"} {
		task, err := s.InsertTask(ctx, TaskInsert{Project: "tasks", Title: title, Reporter: "fixture"}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.ApplyTaskTransition(ctx, "tasks", task.ID, "in_progress", actor, TaskTransitionPolicy{}); err != nil {
			t.Fatal(err)
		}
		if _, err := s.AskTaskQuestion(ctx, "tasks", task.ID, title+"?", nil, actor); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, task.ID)
	}
	if _, err := s.db.ExecContext(ctx, "UPDATE tasks SET status='cancelled' WHERE id=?", ids[1]); err != nil {
		t.Fatal(err)
	}
	open, err := s.OpenTaskQuestions(ctx, "tasks")
	if err != nil || len(open) != 2 || open[0].TaskID != ids[2] || open[1].TaskID != ids[0] {
		t.Fatalf("open questions = %+v, %v", open, err)
	}
}
