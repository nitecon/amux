package storage

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/nitecon/amux/projects"
)

type TaskSummary struct {
	TaskType                string          `json:"-"`
	Parent                  *TaskParent     `json:"parent"`
	Routing                 json.RawMessage `json:"routing"`
	Requester               json.RawMessage `json:"requester"`
	ID                      string          `json:"id"`
	Title                   string          `json:"title"`
	Goal                    *string         `json:"goal"`
	Status                  string          `json:"status"`
	Rank                    int64           `json:"rank"`
	Labels                  []string        `json:"labels"`
	OwnerAgentID            *string         `json:"owner_agent_id"`
	Hostname                *string         `json:"hostname"`
	Reporter                string          `json:"reporter"`
	CommentCount            int64           `json:"comment_count"`
	CreatedAt               int64           `json:"created_at"`
	UpdatedAt               int64           `json:"updated_at"`
	Kind                    string          `json:"kind"`
	DelegatedToProjectIdent *string         `json:"delegated_to_project_ident"`
	DelegatedToTaskID       *string         `json:"delegated_to_task_id"`
	DelegationReady         bool            `json:"delegation_ready"`
}

func (s *Store) Tasks(ctx context.Context, project string, statuses []string, includeStale bool) ([]TaskSummary, error) {
	result := []TaskSummary{}
	if len(statuses) == 0 {
		return result, nil
	}
	params := []any{projects.NormalizeIdent(project)}
	placeholders := make([]string, 0, len(statuses))
	for _, status := range statuses {
		if status != "todo" && status != "in_progress" && status != "user_review" && status != "done" && status != "cancelled" {
			return nil, fmt.Errorf("invalid task status")
		}
		params = append(params, status)
		placeholders = append(placeholders, "?")
	}
	statement := "SELECT " + taskSelect + " FROM tasks t WHERE t.project_ident=? AND t.status IN (" + strings.Join(placeholders, ",") + ")"
	if !includeStale {
		statement += " AND (t.status!='done' OR (t.done_at IS NOT NULL AND t.done_at>?))"
		params = append(params, time.Now().Add(-7*24*time.Hour).UnixMilli())
	}
	statement += " ORDER BY t.rank ASC,t.updated_at DESC"
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, statement, params...)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		t, err := scanTask(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		result = append(result, TaskSummary{TaskType: t.TaskType, Parent: t.Parent, Routing: t.Routing, Requester: t.Requester, ID: t.ID, Title: t.Title, Goal: t.Goal, Status: t.Status, Rank: t.Rank, Labels: t.Labels, OwnerAgentID: t.OwnerAgentID, Hostname: t.Hostname, Reporter: t.Reporter, CreatedAt: t.CreatedAt, UpdatedAt: t.UpdatedAt, Kind: t.Kind, DelegatedToProjectIdent: t.DelegatedToProjectIdent, DelegatedToTaskID: t.DelegatedToTaskID, DelegationReady: t.DelegationReady})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	counts, err := tx.QueryContext(ctx, "SELECT c.task_id,COUNT(*) FROM task_comments c JOIN tasks t ON t.id=c.task_id WHERE t.project_ident=? GROUP BY c.task_id", projects.NormalizeIdent(project))
	if err != nil {
		return nil, err
	}
	byID := map[string]int64{}
	for counts.Next() {
		var id string
		var count int64
		if err := counts.Scan(&id, &count); err != nil {
			counts.Close()
			return nil, err
		}
		byID[id] = count
	}
	if err := counts.Err(); err != nil {
		counts.Close()
		return nil, err
	}
	if err := counts.Close(); err != nil {
		return nil, err
	}
	for i := range result {
		result[i].CommentCount = byID[result[i].ID]
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}
