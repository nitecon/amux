package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/nitecon/amux/projects"
)

const (
	maxTaskQuestionBytes = 8192
	maxTaskQuestionOpts  = 8
	maxTaskQuestionOpt   = 200
)

// ErrTaskQuestionInput reports an invalid question or option list.
var ErrTaskQuestionInput = errors.New("invalid task question")

// TaskQuestion is a decision an owning agent needs from the user. It stays
// open until any user comment is added to the task.
type TaskQuestion struct {
	ID           string       `json:"id"`
	TaskID       string       `json:"task_id"`
	ProjectIdent string       `json:"project_ident"`
	TaskTitle    string       `json:"task_title"`
	Question     string       `json:"question"`
	Options      []string     `json:"options"`
	CommentID    string       `json:"comment_id"`
	Origin       *AgentOrigin `json:"origin"`
	CreatedAt    int64        `json:"created_at"`
}

func normalizeTaskQuestion(question string, options []string) (string, []string, error) {
	question = strings.TrimSpace(question)
	if question == "" || len(question) > maxTaskQuestionBytes {
		return "", nil, fmt.Errorf("question must be non-empty and at most %d bytes", maxTaskQuestionBytes)
	}
	if len(options) > maxTaskQuestionOpts {
		return "", nil, fmt.Errorf("at most %d options are allowed", maxTaskQuestionOpts)
	}
	clean := make([]string, 0, len(options))
	for _, option := range options {
		option = strings.TrimSpace(option)
		if option == "" || len(option) > maxTaskQuestionOpt || strings.ContainsAny(option, "\r\n") {
			return "", nil, fmt.Errorf("each option must be a single non-empty line of at most %d bytes", maxTaskQuestionOpt)
		}
		clean = append(clean, option)
	}
	return question, clean, nil
}

func taskQuestionComment(question string, options []string) string {
	var body strings.Builder
	body.WriteString("Question for the user:\n\n" + question)
	if len(options) > 0 {
		body.WriteString("\n\nSuggested answers:")
		for i, option := range options {
			fmt.Fprintf(&body, "\n%d. %s", i+1, option)
		}
	}
	return body.String()
}

// AskTaskQuestion records an open question from the active owner of a task.
// The question is also posted as an agent comment so the task thread keeps
// the full history. Ownership and status are unchanged.
func (s *Store) AskTaskQuestion(ctx context.Context, project, id, question string, options []string, origin *AgentOrigin) (*TaskQuestion, error) {
	question, options, err := normalizeTaskQuestion(question, options)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTaskQuestionInput, err)
	}
	if origin == nil {
		return nil, ErrTaskOwnership
	}
	var result *TaskQuestion
	err = s.taskMutation(ctx, origin, func(tx *sql.Tx, actor *AgentOrigin) error {
		t, err := readTask(ctx, tx, project, id)
		if err != nil {
			return err
		}
		if t == nil {
			return ErrTaskNotFound
		}
		if !IsActiveTaskStatus(t.Status) || t.OwnerOrigin == nil || *t.OwnerOrigin != *actor {
			return ErrTaskOwnership
		}
		comment, err := insertTaskComment(ctx, tx, id, actor.SessionID, "agent", taskQuestionComment(question, options), nil, actor)
		if err != nil {
			return err
		}
		questionID, err := uuid.NewV7()
		if err != nil {
			return err
		}
		encodedOptions, err := json.Marshal(options)
		if err != nil {
			return err
		}
		encodedOrigin, err := json.Marshal(actor)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO task_questions(id,task_id,comment_id,question,options_json,origin_json,created_at) VALUES(?,?,?,?,?,?,?)",
			questionID.String(), id, comment.ID, question, string(encodedOptions), string(encodedOrigin), comment.CreatedAt); err != nil {
			return err
		}
		result = &TaskQuestion{ID: questionID.String(), TaskID: id, ProjectIdent: t.ProjectIdent, TaskTitle: t.Title, Question: question, Options: options, CommentID: comment.ID, Origin: actor, CreatedAt: comment.CreatedAt}
		return nil
	})
	return result, err
}

// answerTaskQuestions closes every open question on a task with the given
// user comment.
func answerTaskQuestions(ctx context.Context, q queryer, taskID, commentID string, at int64) error {
	_, err := q.ExecContext(ctx, "UPDATE task_questions SET answered_at=?,answer_comment_id=? WHERE task_id=? AND answered_at IS NULL", at, commentID, taskID)
	return err
}

// OpenTaskQuestions lists unanswered questions on tasks that are not finished,
// newest first. An empty project lists questions across all projects.
func (s *Store) OpenTaskQuestions(ctx context.Context, project string) ([]TaskQuestion, error) {
	statement := `SELECT q.id,q.task_id,t.project_ident,t.title,q.question,q.options_json,q.comment_id,q.origin_json,q.created_at
 FROM task_questions q JOIN tasks t ON t.id=q.task_id
 WHERE q.answered_at IS NULL AND t.status NOT IN ('done','cancelled')`
	var params []any
	if project != "" {
		statement += " AND t.project_ident=?"
		params = append(params, projects.NormalizeIdent(project))
	}
	rows, err := s.db.QueryContext(ctx, statement+" ORDER BY q.created_at DESC,q.id DESC", params...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []TaskQuestion{}
	for rows.Next() {
		var question TaskQuestion
		var options string
		var origin *string
		if err := rows.Scan(&question.ID, &question.TaskID, &question.ProjectIdent, &question.TaskTitle, &question.Question, &options, &question.CommentID, &origin, &question.CreatedAt); err != nil {
			return nil, err
		}
		if json.Unmarshal([]byte(options), &question.Options) != nil || question.Options == nil {
			question.Options = []string{}
		}
		if origin != nil {
			_ = json.Unmarshal([]byte(*origin), &question.Origin)
		}
		result = append(result, question)
	}
	return result, rows.Err()
}
