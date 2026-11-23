package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ExecutionRun is retained executor history exposed for administrative reads.
type ExecutionRun struct {
	ID            string            `json:"id"`
	ProjectIdent  string            `json:"project_ident"`
	TaskID        *string           `json:"task_id"`
	Trigger       string            `json:"trigger"`
	Status        string            `json:"status"`
	CreatedAt     int64             `json:"created_at"`
	FinishedAt    *int64            `json:"finished_at"`
	Output        string            `json:"output"`
	Attempts      []json.RawMessage `json:"attempts"`
	TaskTitle     *string           `json:"task_title"`
	StartedAt     *int64            `json:"started_at"`
	UpdatedAt     int64             `json:"updated_at"`
	Client        *string           `json:"client"`
	Model         *string           `json:"model"`
	Executor      string            `json:"executor"`
	Progress      string            `json:"progress"`
	Summary       string            `json:"summary"`
	SummarySource string            `json:"summary_source"`
	SessionKey    *string           `json:"session_key"`
	LastSequence  int64             `json:"last_sequence"`
}

// ListExecutionRuns reads the retained history without creating/upgrading its
// tables. Older Rust schemas receive the same additive-column defaults.
func (s *Store) ListExecutionRuns(ctx context.Context, project, taskID *string) ([]ExecutionRun, error) {
	columns, exists, err := sqliteTableColumns(ctx, s.db, "execution_runs")
	if err != nil {
		return nil, err
	}
	if !exists {
		return []ExecutionRun{}, nil
	}
	started := historyColumn(columns, "started_at", "NULL")
	updated := historyColumn(columns, "updated_at", "COALESCE(r.finished_at,r.created_at)")
	if columns["updated_at"] {
		updated = "COALESCE(r.updated_at,r.finished_at,r.created_at)"
	}
	client := historyColumn(columns, "client", "NULL")
	model := historyColumn(columns, "model", "NULL")
	executor := historyColumn(columns, "executor", "'headless'")
	progress := historyColumn(columns, "progress", "''")
	summary := historyColumn(columns, "summary", "''")
	summarySource := historyColumn(columns, "summary_source", "'missing'")
	sessionKey := historyColumn(columns, "session_key", "NULL")
	lastSequence := historyColumn(columns, "last_sequence", "0")
	taskTitle, hasTasks, err := sqliteTableColumns(ctx, s.db, "tasks")
	if err != nil {
		return nil, err
	}
	titleExpr, taskJoin := "NULL", ""
	if hasTasks && taskTitle["title"] {
		titleExpr, taskJoin = "t.title", " LEFT JOIN tasks t ON t.id=r.task_id"
	}
	query := fmt.Sprintf(`SELECT r.id,r.project_ident,r.task_id,r.trigger,r.status,r.created_at,r.finished_at,r.output,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s,%s
FROM execution_runs r%s WHERE (?1 IS NULL OR r.project_ident=?1) AND (?2 IS NULL OR r.task_id=?2)
ORDER BY CASE WHEN r.finished_at IS NULL THEN 0 ELSE 1 END,r.created_at DESC,r.id DESC LIMIT 100`, titleExpr, started, updated, client, model, executor, progress, summary, summarySource, sessionKey, lastSequence, taskJoin)
	rows, err := s.db.QueryContext(ctx, query, project, taskID)
	if err != nil {
		return nil, err
	}
	runs := make([]ExecutionRun, 0, 100)
	for rows.Next() {
		var run ExecutionRun
		if err := rows.Scan(&run.ID, &run.ProjectIdent, &run.TaskID, &run.Trigger, &run.Status, &run.CreatedAt, &run.FinishedAt, &run.Output,
			&run.TaskTitle, &run.StartedAt, &run.UpdatedAt, &run.Client, &run.Model, &run.Executor, &run.Progress, &run.Summary,
			&run.SummarySource, &run.SessionKey, &run.LastSequence); err != nil {
			rows.Close()
			return nil, err
		}
		run.Attempts = []json.RawMessage{}
		runs = append(runs, run)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}

	_, hasAttempts, err := sqliteTableColumns(ctx, s.db, "execution_attempts")
	if err != nil {
		return nil, err
	}
	if hasAttempts {
		for i := range runs {
			attemptRows, err := s.db.QueryContext(ctx, `SELECT body FROM execution_attempts WHERE run_id=? ORDER BY position`, runs[i].ID)
			if err != nil {
				return nil, err
			}
			for attemptRows.Next() {
				var body string
				if err := attemptRows.Scan(&body); err != nil {
					attemptRows.Close()
					return nil, err
				}
				if !json.Valid([]byte(body)) {
					attemptRows.Close()
					return nil, fmt.Errorf("decode execution attempt JSON: invalid JSON")
				}
				runs[i].Attempts = append(runs[i].Attempts, json.RawMessage(body))
			}
			if err := attemptRows.Err(); err != nil {
				attemptRows.Close()
				return nil, err
			}
			if err := attemptRows.Close(); err != nil {
				return nil, err
			}
		}
	}
	return runs, nil
}

func historyColumn(columns map[string]bool, name, fallback string) string {
	if columns[name] {
		return "r." + name
	}
	return fallback
}

func sqliteTableColumns(ctx context.Context, db queryer, table string) (map[string]bool, bool, error) {
	var exists int
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name=?)`, table).Scan(&exists); err != nil {
		return nil, false, err
	}
	columns := map[string]bool{}
	if exists == 0 {
		return columns, false, nil
	}
	rows, err := db.QueryContext(ctx, "PRAGMA table_info("+strings.ReplaceAll(table, "'", "''")+")")
	if err != nil {
		return nil, false, err
	}
	for rows.Next() {
		var cid, notnull, primary int
		var name, dataType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &dataType, &notnull, &defaultValue, &primary); err != nil {
			rows.Close()
			return nil, false, err
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, false, err
	}
	if err := rows.Close(); err != nil {
		return nil, false, err
	}
	return columns, true, nil
}
