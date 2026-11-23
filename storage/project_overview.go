package storage

import (
	"context"
	"sort"
	"time"
)

// ProjectOverview is the native browser's current remote inventory.
type ProjectOverview struct {
	Project
	Agents       int64  `json:"agents"`
	PendingTasks *int64 `json:"pending_tasks,omitempty"`
}

func (s *Store) ProjectOverviews(ctx context.Context, now time.Time) ([]ProjectOverview, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	projects, err := readProjects(ctx, tx, true)
	if err != nil {
		return nil, err
	}
	agents, _, err := livePresenceSnapshot(ctx, tx, now.UnixMilli())
	if err != nil {
		return nil, err
	}
	sessions := map[string]map[string]struct{}{}
	for _, agent := range agents {
		if sessions[agent.ProjectIdent] == nil {
			sessions[agent.ProjectIdent] = map[string]struct{}{}
		}
		sessions[agent.ProjectIdent][agent.Origin.SessionID] = struct{}{}
	}
	pending := map[string]int64{}
	rows, err := tx.QueryContext(ctx, "SELECT project_ident, COUNT(*) FROM tasks WHERE status='todo' GROUP BY project_ident")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var project string
		var count int64
		if err := rows.Scan(&project, &count); err != nil {
			return nil, err
		}
		pending[project] = count
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := make([]ProjectOverview, 0, len(projects))
	for _, project := range projects {
		if project.ArchivedAt == nil {
			count := pending[project.Ident]
			result = append(result, ProjectOverview{Project: project, Agents: int64(len(sessions[project.Ident])), PendingTasks: &count})
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Ident < result[j].Ident })
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return result, nil
}
