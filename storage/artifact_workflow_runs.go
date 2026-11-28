package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	projectid "github.com/nitecon/amux/identity/project"
)

const artifactWorkflowRunsMigration = 39
const workflowRunReadSetMaxRefs = 256

type WorkflowRun struct {
	WorkflowRunID            string          `json:"workflow_run_id"`
	ArtifactID               string          `json:"artifact_id"`
	WorkflowKind             string          `json:"workflow_kind"`
	Phase                    *string         `json:"phase"`
	RoundID                  *string         `json:"round_id"`
	CoordinatorActorID       string          `json:"coordinator_actor_id"`
	ParticipantActorIDs      []string        `json:"participant_actor_ids"`
	SourceArtifactVersionID  *string         `json:"source_artifact_version_id"`
	ReadSet                  json.RawMessage `json:"read_set"`
	IdempotencyKey           *string         `json:"idempotency_key"`
	IsResumable              bool            `json:"is_resumable"`
	State                    string          `json:"state"`
	GeneratedContributionIDs []string        `json:"generated_contribution_ids"`
	GeneratedVersionIDs      []string        `json:"generated_version_ids"`
	GeneratedTaskIDs         []string        `json:"generated_task_ids"`
	GeneratedLinkIDs         []string        `json:"generated_link_ids"`
	GeneratedChunkIDs        []string        `json:"generated_chunk_ids"`
	FailureReason            *string         `json:"failure_reason"`
	StartedAt                int64           `json:"started_at"`
	EndedAt                  *int64          `json:"ended_at"`
}

type WorkflowRunInsert struct {
	ArtifactID              string
	WorkflowKind            string
	Phase                   *string
	RoundID                 *string
	CoordinatorActorID      string
	ParticipantActorIDs     []string
	SourceArtifactVersionID *string
	ReadSet                 json.RawMessage
	IdempotencyKey          *string
	IsResumable             bool
}

type WorkflowRunUpdate struct {
	State                    *string
	FailureReason            **string
	GeneratedContributionIDs *[]string
	GeneratedVersionIDs      *[]string
	GeneratedTaskIDs         *[]string
	GeneratedLinkIDs         *[]string
	GeneratedChunkIDs        *[]string
	EndedAt                  **int64
}

type WorkflowRunWrite struct {
	Record   *WorkflowRun
	Replayed bool
}

// migrateArtifactWorkflowRuns is additive for early Go fixtures and old local
// databases. A preexisting workflow_runs table is extended in place; rows are
// never rebuilt or discarded.
func migrateArtifactWorkflowRuns(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS workflow_runs (
 workflow_run_id TEXT PRIMARY KEY,
 artifact_id TEXT NOT NULL REFERENCES artifacts(artifact_id) ON DELETE CASCADE,
 workflow_kind TEXT NOT NULL CHECK(workflow_kind IN ('design_review_round','spec_iteration','spec_acceptance','spec_task_generation')),
 phase TEXT,
 round_id TEXT,
 coordinator_actor_id TEXT NOT NULL REFERENCES artifact_actors(actor_id),
 participant_actor_ids TEXT,
 source_artifact_version_id TEXT REFERENCES artifact_versions(artifact_version_id),
 read_set TEXT,
 idempotency_key TEXT,
 is_resumable INTEGER NOT NULL DEFAULT 0,
 state TEXT NOT NULL DEFAULT 'started' CHECK(state IN ('started','succeeded','failed','cancelled')),
 generated_contribution_ids TEXT,
 generated_version_ids TEXT,
 generated_task_ids TEXT,
 generated_link_ids TEXT,
 generated_chunk_ids TEXT,
 failure_reason TEXT,
 started_at INTEGER NOT NULL,
 ended_at INTEGER
);`)
	if err != nil {
		return fmt.Errorf("create workflow run schema: %w", err)
	}
	if err := addWorkflowRunColumns(ctx, tx); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `
CREATE INDEX IF NOT EXISTS idx_workflow_runs_artifact ON workflow_runs(artifact_id,started_at DESC);
CREATE INDEX IF NOT EXISTS idx_workflow_runs_state ON workflow_runs(state,started_at DESC);
CREATE UNIQUE INDEX IF NOT EXISTS idx_workflow_runs_idempotency ON workflow_runs(coordinator_actor_id,artifact_id,workflow_kind,idempotency_key) WHERE idempotency_key IS NOT NULL;
CREATE TRIGGER IF NOT EXISTS prevent_workflow_run_state_after_terminal
 BEFORE UPDATE OF state ON workflow_runs FOR EACH ROW
 WHEN (OLD.state='cancelled' AND NEW.state IS NOT OLD.state)
 OR (OLD.state='succeeded' AND NEW.state IS NOT OLD.state)
 OR (OLD.state='failed' AND NEW.state='succeeded' AND OLD.is_resumable=0)
 OR (OLD.state='failed' AND NEW.state IN ('started','cancelled'))
 BEGIN SELECT RAISE(ABORT,'invalid workflow_run state transition'); END;`)
	if err != nil {
		return fmt.Errorf("index workflow run schema: %w", err)
	}
	if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(?)", artifactWorkflowRunsMigration); err != nil {
		return fmt.Errorf("record workflow run migration: %w", err)
	}
	return nil
}

func addWorkflowRunColumns(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, "PRAGMA table_info(workflow_runs)")
	if err != nil {
		return err
	}
	columns := make(map[string]bool)
	for rows.Next() {
		var cid, notnull, primary int
		var name, dataType string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &dataType, &notnull, &defaultValue, &primary); err != nil {
			rows.Close()
			return err
		}
		columns[name] = true
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, column := range []struct{ name, definition string }{
		{"artifact_id", "TEXT"},
		{"workflow_kind", "TEXT"},
		{"phase", "TEXT"},
		{"round_id", "TEXT"},
		{"coordinator_actor_id", "TEXT"},
		{"participant_actor_ids", "TEXT"},
		{"source_artifact_version_id", "TEXT"},
		{"read_set", "TEXT"},
		{"idempotency_key", "TEXT"},
		{"is_resumable", "INTEGER NOT NULL DEFAULT 0"},
		{"state", "TEXT NOT NULL DEFAULT 'started'"},
		{"generated_contribution_ids", "TEXT"},
		{"generated_version_ids", "TEXT"},
		{"generated_task_ids", "TEXT"},
		{"generated_link_ids", "TEXT"},
		{"generated_chunk_ids", "TEXT"},
		{"failure_reason", "TEXT"},
		{"started_at", "INTEGER NOT NULL DEFAULT 0"},
		{"ended_at", "INTEGER"},
	} {
		if columns[column.name] {
			continue
		}
		if _, err := tx.ExecContext(ctx, "ALTER TABLE workflow_runs ADD COLUMN "+column.name+" "+column.definition); err != nil {
			return fmt.Errorf("add workflow run column %s: %w", column.name, err)
		}
	}
	return nil
}

const workflowRunSelect = `SELECT workflow_run_id,artifact_id,workflow_kind,phase,round_id,coordinator_actor_id,participant_actor_ids,source_artifact_version_id,read_set,idempotency_key,is_resumable,state,generated_contribution_ids,generated_version_ids,generated_task_ids,generated_link_ids,generated_chunk_ids,failure_reason,started_at,ended_at FROM workflow_runs`

func scanWorkflowRun(row interface{ Scan(...any) error }) (*WorkflowRun, error) {
	var run WorkflowRun
	var artifactID, workflowKind, coordinatorID sql.NullString
	var participants, contributions, versions, tasks, links, chunks sql.NullString
	var readSet, key, failure sql.NullString
	var resumable int
	if err := row.Scan(&run.WorkflowRunID, &artifactID, &workflowKind, &run.Phase, &run.RoundID, &coordinatorID, &participants, &run.SourceArtifactVersionID, &readSet, &key, &resumable, &run.State, &contributions, &versions, &tasks, &links, &chunks, &failure, &run.StartedAt, &run.EndedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, nil
		}
		return nil, err
	}
	run.ArtifactID = artifactID.String
	run.WorkflowKind = workflowKind.String
	run.CoordinatorActorID = coordinatorID.String
	run.ParticipantActorIDs = decodeStringArray(participants.String)
	run.GeneratedContributionIDs = decodeStringArray(contributions.String)
	run.GeneratedVersionIDs = decodeStringArray(versions.String)
	run.GeneratedTaskIDs = decodeStringArray(tasks.String)
	run.GeneratedLinkIDs = decodeStringArray(links.String)
	run.GeneratedChunkIDs = decodeStringArray(chunks.String)
	run.IsResumable = resumable != 0
	if readSet.Valid {
		if !json.Valid([]byte(readSet.String)) {
			return nil, fmt.Errorf("decode workflow run read_set: invalid JSON")
		}
		run.ReadSet = json.RawMessage(readSet.String)
	}
	if key.Valid {
		run.IdempotencyKey = &key.String
	}
	if failure.Valid {
		run.FailureReason = &failure.String
	}
	return &run, nil
}

func decodeStringArray(value string) []string {
	var result []string
	if value != "" {
		_ = json.Unmarshal([]byte(value), &result)
	}
	if result == nil {
		result = []string{}
	}
	return result
}

func workflowRunArray(values []string) any {
	if len(values) == 0 {
		return nil
	}
	encoded, _ := json.Marshal(values)
	return string(encoded)
}

func (s *Store) StartWorkflowRun(ctx context.Context, input WorkflowRunInsert) (*WorkflowRunWrite, error) {
	if !validWorkflowKind(input.WorkflowKind) {
		return nil, fmt.Errorf("invalid workflow kind")
	}
	if len(input.ReadSet) > 0 && !json.Valid(input.ReadSet) {
		return nil, fmt.Errorf("workflow run read_set must be valid JSON")
	}
	if countWorkflowReadSetRefs(input.ReadSet) > workflowRunReadSetMaxRefs {
		return nil, fmt.Errorf("read_set_too_large")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var artifactExists int
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM artifacts WHERE artifact_id=?", input.ArtifactID).Scan(&artifactExists); err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("artifact not found")
		}
		return nil, err
	}
	if input.SourceArtifactVersionID != nil {
		var artifactID string
		if err := tx.QueryRowContext(ctx, "SELECT artifact_id FROM artifact_versions WHERE artifact_version_id=?", *input.SourceArtifactVersionID).Scan(&artifactID); err != nil || artifactID != input.ArtifactID {
			return nil, fmt.Errorf("source artifact version does not belong to artifact")
		}
	}
	if input.IdempotencyKey != nil {
		existing, err := scanWorkflowRun(tx.QueryRowContext(ctx, workflowRunSelect+` WHERE coordinator_actor_id=? AND artifact_id=? AND workflow_kind=? AND idempotency_key=?`, input.CoordinatorActorID, input.ArtifactID, input.WorkflowKind, *input.IdempotencyKey))
		if err != nil {
			return nil, err
		}
		if existing != nil {
			if err := tx.Commit(); err != nil {
				return nil, err
			}
			return &WorkflowRunWrite{Record: existing, Replayed: true}, nil
		}
	}
	id, err := uuid.NewV7()
	if err != nil {
		return nil, err
	}
	participants := workflowRunArray(input.ParticipantActorIDs)
	var readSet any
	if len(input.ReadSet) > 0 {
		readSet = nullJSON(input.ReadSet)
	}
	now := time.Now().UnixMilli()
	_, err = tx.ExecContext(ctx, `INSERT INTO workflow_runs(workflow_run_id,artifact_id,workflow_kind,phase,round_id,coordinator_actor_id,participant_actor_ids,source_artifact_version_id,read_set,idempotency_key,is_resumable,state,generated_contribution_ids,generated_version_ids,generated_task_ids,generated_link_ids,generated_chunk_ids,failure_reason,started_at,ended_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,'started',NULL,NULL,NULL,NULL,NULL,NULL,?,NULL)`, id.String(), input.ArtifactID, input.WorkflowKind, input.Phase, input.RoundID, input.CoordinatorActorID, participants, input.SourceArtifactVersionID, readSet, input.IdempotencyKey, input.IsResumable, now)
	if err != nil {
		return nil, err
	}
	run, err := scanWorkflowRun(tx.QueryRowContext(ctx, workflowRunSelect+` WHERE workflow_run_id=?`, id.String()))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &WorkflowRunWrite{Record: run}, nil
}

func validWorkflowKind(kind string) bool {
	switch kind {
	case "design_review_round", "spec_iteration", "spec_acceptance", "spec_task_generation":
		return true
	default:
		return false
	}
}

func countWorkflowReadSetRefs(raw json.RawMessage) int {
	if len(raw) == 0 {
		return 0
	}
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return 0
	}
	var count func(any) int
	count = func(value any) int {
		switch typed := value.(type) {
		case string:
			return 1
		case []any:
			total := 0
			for _, item := range typed {
				total += count(item)
			}
			return total
		case map[string]any:
			total := 0
			for _, item := range typed {
				total += count(item)
			}
			return total
		default:
			return 0
		}
	}
	return count(value)
}

func (s *Store) WorkflowRun(ctx context.Context, workflowRunID string) (*WorkflowRun, error) {
	return scanWorkflowRun(s.db.QueryRowContext(ctx, workflowRunSelect+` WHERE workflow_run_id=?`, workflowRunID))
}

func (s *Store) ArtifactWorkflowRun(ctx context.Context, projectIdent, artifactID, workflowRunID string) (*WorkflowRun, error) {
	return scanWorkflowRun(s.db.QueryRowContext(ctx, workflowRunSelect+`
 WHERE workflow_run_id=? AND artifact_id=?
 AND EXISTS (SELECT 1 FROM artifacts WHERE project_ident=? AND artifacts.artifact_id=workflow_runs.artifact_id)`,
		workflowRunID, artifactID, projectid.NormalizeIdent(projectIdent)))
}

func (s *Store) WorkflowRuns(ctx context.Context, projectIdent, artifactID string) ([]WorkflowRun, error) {
	var exists int
	err := s.db.QueryRowContext(ctx, "SELECT 1 FROM artifacts WHERE project_ident=? AND artifact_id=?", projectid.NormalizeIdent(projectIdent), artifactID).Scan(&exists)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("artifact not found in project")
	}
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, workflowRunSelect+` WHERE artifact_id=? ORDER BY started_at DESC`, artifactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := []WorkflowRun{}
	for rows.Next() {
		run, err := scanWorkflowRun(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *run)
	}
	return result, rows.Err()
}

func (s *Store) UpdateWorkflowRun(ctx context.Context, workflowRunID string, update WorkflowRunUpdate) (*WorkflowRun, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	current, err := scanWorkflowRun(tx.QueryRowContext(ctx, workflowRunSelect+` WHERE workflow_run_id=?`, workflowRunID))
	if err != nil || current == nil {
		return current, err
	}
	if update.State != nil && *update.State == "failed" && (update.FailureReason == nil || *update.FailureReason == nil) {
		return nil, fmt.Errorf("failed workflow runs require failure_reason")
	}
	if update.State != nil && !validWorkflowRunState(*update.State) {
		return nil, fmt.Errorf("invalid workflow run state")
	}
	assignments := make([]string, 0, 9)
	args := make([]any, 0, 10)
	add := func(column string, value any) {
		assignments = append(assignments, column+"=?")
		args = append(args, value)
	}
	if update.State != nil {
		add("state", *update.State)
	}
	if update.FailureReason != nil {
		add("failure_reason", *update.FailureReason)
	}
	if update.GeneratedContributionIDs != nil {
		add("generated_contribution_ids", workflowRunArray(*update.GeneratedContributionIDs))
	}
	if update.GeneratedVersionIDs != nil {
		add("generated_version_ids", workflowRunArray(*update.GeneratedVersionIDs))
	}
	if update.GeneratedTaskIDs != nil {
		add("generated_task_ids", workflowRunArray(*update.GeneratedTaskIDs))
	}
	if update.GeneratedLinkIDs != nil {
		add("generated_link_ids", workflowRunArray(*update.GeneratedLinkIDs))
	}
	if update.GeneratedChunkIDs != nil {
		add("generated_chunk_ids", workflowRunArray(*update.GeneratedChunkIDs))
	}
	if update.EndedAt != nil {
		add("ended_at", *update.EndedAt)
	} else if update.State != nil && (*update.State == "succeeded" || *update.State == "failed" || *update.State == "cancelled") {
		add("ended_at", time.Now().UnixMilli())
	}
	if len(assignments) > 0 {
		args = append(args, workflowRunID)
		if _, err := tx.ExecContext(ctx, "UPDATE workflow_runs SET "+strings.Join(assignments, ",")+" WHERE workflow_run_id=?", args...); err != nil {
			return nil, err
		}
		current, err = scanWorkflowRun(tx.QueryRowContext(ctx, workflowRunSelect+` WHERE workflow_run_id=?`, workflowRunID))
		if err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return current, nil
}

func (s *Store) AppendWorkflowRunOutputs(ctx context.Context, workflowRunID string, contributionID, versionID *string) (*WorkflowRun, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	run, err := appendWorkflowRunOutputsTx(ctx, tx, workflowRunID, contributionID, versionID)
	if err != nil || run == nil {
		return run, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return run, nil
}

func appendWorkflowRunOutputsTx(ctx context.Context, tx *sql.Tx, workflowRunID string, contributionID, versionID *string) (*WorkflowRun, error) {
	run, err := scanWorkflowRun(tx.QueryRowContext(ctx, workflowRunSelect+` WHERE workflow_run_id=?`, workflowRunID))
	if err != nil || run == nil {
		return run, err
	}
	if contributionID != nil {
		run.GeneratedContributionIDs = appendUnique(run.GeneratedContributionIDs, *contributionID)
	}
	if versionID != nil {
		run.GeneratedVersionIDs = appendUnique(run.GeneratedVersionIDs, *versionID)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE workflow_runs SET generated_contribution_ids=?,generated_version_ids=? WHERE workflow_run_id=?`, workflowRunArray(run.GeneratedContributionIDs), workflowRunArray(run.GeneratedVersionIDs), workflowRunID); err != nil {
		return nil, err
	}
	return run, nil
}

func appendUnique(values []string, value string) []string {
	for _, current := range values {
		if current == value {
			return values
		}
	}
	return append(values, value)
}

func (s *Store) ValidateWorkflowRunForMutation(ctx context.Context, artifactID string, workflowRunID *string) (*WorkflowRun, error) {
	return validateWorkflowRunForMutation(ctx, s.db, artifactID, workflowRunID)
}

func validateWorkflowRunForMutation(ctx context.Context, q queryer, artifactID string, workflowRunID *string) (*WorkflowRun, error) {
	if workflowRunID == nil || strings.TrimSpace(*workflowRunID) == "" {
		return nil, nil
	}
	run, err := scanWorkflowRun(q.QueryRowContext(ctx, workflowRunSelect+` WHERE workflow_run_id=?`, *workflowRunID))
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, fmt.Errorf("workflow_run not found")
	}
	if run.ArtifactID != artifactID {
		return nil, fmt.Errorf("workflow_run does not belong to artifact")
	}
	if err := ensureWorkflowRunRetryable(run); err != nil {
		return nil, err
	}
	return run, nil
}

// validateAndCompleteResumedWorkflowRunTx keeps workflow validation, the
// artifact write, and resumable output completion in the caller's transaction.
func validateAndCompleteResumedWorkflowRunTx(ctx context.Context, tx *sql.Tx, artifactID string, workflowRunID *string, contributionID, versionID, linkID *string) error {
	run, err := validateWorkflowRunForMutation(ctx, tx, artifactID, workflowRunID)
	if err != nil || run == nil || run.State != "failed" || !run.IsResumable {
		return err
	}
	contributions := appendIfPresent(run.GeneratedContributionIDs, contributionID)
	versions := appendIfPresent(run.GeneratedVersionIDs, versionID)
	links := appendIfPresent(run.GeneratedLinkIDs, linkID)
	result, err := tx.ExecContext(ctx, `UPDATE workflow_runs SET state='succeeded',failure_reason=NULL,generated_contribution_ids=?,generated_version_ids=?,generated_link_ids=?,ended_at=? WHERE workflow_run_id=? AND state='failed' AND is_resumable=1`,
		workflowRunArray(contributions), workflowRunArray(versions), workflowRunArray(links), time.Now().UnixMilli(), run.WorkflowRunID)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil {
		return err
	} else if changed != 1 {
		return fmt.Errorf("workflow_run changed during mutation")
	}
	return nil
}

func ensureWorkflowRunRetryable(run *WorkflowRun) error {
	switch run.State {
	case "cancelled":
		return fmt.Errorf("cancelled workflow_run is not retryable")
	case "failed":
		if !run.IsResumable {
			return fmt.Errorf("non-resumable failed workflow_run is not retryable")
		}
	}
	return nil
}

func validWorkflowRunState(state string) bool {
	switch state {
	case "started", "succeeded", "failed", "cancelled":
		return true
	default:
		return false
	}
}

func (s *Store) CompleteResumedWorkflowRun(ctx context.Context, run *WorkflowRun, contributionID, versionID, linkID *string) error {
	if run == nil || run.State != "failed" || !run.IsResumable {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := validateAndCompleteResumedWorkflowRunTx(ctx, tx, run.ArtifactID, &run.WorkflowRunID, contributionID, versionID, linkID); err != nil {
		return err
	}
	return tx.Commit()
}

func appendIfPresent(values []string, value *string) []string {
	if value == nil {
		return values
	}
	return appendUnique(values, *value)
}
