package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/nitecon/amux/projects"
	"github.com/nitecon/amux/taskplatform"
)

// TaskDelegation is the durable link between a source tracking task and its
// target task in another project. Its JSON fields match the gateway contract.
type TaskDelegation struct {
	ID                  string  `json:"id"`
	SourceProjectIdent  string  `json:"source_project_ident"`
	SourceTaskID        string  `json:"source_task_id"`
	TargetProjectIdent  string  `json:"target_project_ident"`
	TargetTaskID        string  `json:"target_task_id"`
	RequesterAgentID    *string `json:"requester_agent_id"`
	RequesterHostname   *string `json:"requester_hostname"`
	CreatedAt           int64   `json:"created_at"`
	CompletedAt         *int64  `json:"completed_at"`
	CompletionMessageID *int64  `json:"completion_message_id"`
}

// TaskDelegationInput contains the source request and its destination scope.
type TaskDelegationInput struct {
	SourceProject, TargetProject, Title, Reporter string
	ParentID, Description, Details, Hostname      *string
	Labels                                        []string
	Requester                                     *RegisteredTargetIdentity
}

// TaskDelegationResult is the atomic receipt for creating a cross-project handoff.
type TaskDelegationResult struct {
	Delegation TaskDelegation `json:"delegation"`
	SourceTask Task           `json:"source_task"`
	TargetTask Task           `json:"target_task"`
	MessageID  int64          `json:"message_id"`
}

// TaskDelegationUpdate is the source project's mirrored contract edit. A nil
// text pointer means absent (including JSON null), matching the Rust route's
// Option<Option<String>> decoding; a non-nil empty Labels slice clears labels.
type TaskDelegationUpdate struct {
	Title         *string  `json:"title,omitempty"`
	Description   *string  `json:"description,omitempty"`
	Details       *string  `json:"details,omitempty"`
	Specification *string  `json:"specification,omitempty"`
	Labels        []string `json:"labels"`
}

var ErrTaskDelegation = errors.New("invalid task delegation")

func migrateTaskDelegations(ctx context.Context, tx *sql.Tx) error {
	if err := ensureTaskDelegationSchema(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(34)"); err != nil {
		return fmt.Errorf("record task delegation migration: %w", err)
	}
	return nil
}

func ensureTaskDelegationSchema(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS task_delegations (
		id TEXT PRIMARY KEY,
		source_project_ident TEXT NOT NULL REFERENCES projects(ident),
		source_task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
		target_project_ident TEXT NOT NULL REFERENCES projects(ident),
		target_task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
		requester_agent_id TEXT,
		requester_hostname TEXT,
		created_at INTEGER NOT NULL,
		completed_at INTEGER,
		completion_message_id INTEGER REFERENCES messages(id) ON DELETE SET NULL
	);
	`)
	if err != nil {
		return fmt.Errorf("create task delegations: %w", err)
	}
	var ddl string
	if err := tx.QueryRowContext(ctx, "SELECT sql FROM sqlite_master WHERE type='table' AND name='task_delegations'").Scan(&ddl); err != nil {
		return err
	}
	if !strings.Contains(strings.ToUpper(ddl), "ON DELETE SET NULL") {
		// SQLite cannot change a foreign key in place. This table has no inbound
		// foreign keys, so a transactional copy preserves all history safely.
		if _, err := tx.ExecContext(ctx, `ALTER TABLE task_delegations RENAME TO task_delegations_old;
			CREATE TABLE task_delegations (
				id TEXT PRIMARY KEY,
				source_project_ident TEXT NOT NULL REFERENCES projects(ident),
				source_task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
				target_project_ident TEXT NOT NULL REFERENCES projects(ident),
				target_task_id TEXT NOT NULL REFERENCES tasks(id) ON DELETE CASCADE,
				requester_agent_id TEXT, requester_hostname TEXT, created_at INTEGER NOT NULL,
				completed_at INTEGER, completion_message_id INTEGER REFERENCES messages(id) ON DELETE SET NULL
			);
			INSERT INTO task_delegations(id,source_project_ident,source_task_id,target_project_ident,target_task_id,requester_agent_id,requester_hostname,created_at,completed_at,completion_message_id)
			SELECT id,source_project_ident,source_task_id,target_project_ident,target_task_id,requester_agent_id,requester_hostname,created_at,completed_at,completion_message_id FROM task_delegations_old;
			DROP TABLE task_delegations_old;`); err != nil {
			return fmt.Errorf("repair task delegation message foreign key: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `CREATE UNIQUE INDEX IF NOT EXISTS idx_task_delegations_source ON task_delegations(source_project_ident,source_task_id);
		CREATE UNIQUE INDEX IF NOT EXISTS idx_task_delegations_target ON task_delegations(target_project_ident,target_task_id);`); err != nil {
		return fmt.Errorf("finish task delegations migration: %w", err)
	}
	return nil
}

// DelegateTask atomically creates the source tracking task, an unowned target
// task, their hierarchy/link records, comments, durable event snapshots, and
// the destination nudge.
func (s *Store) DelegateTask(ctx context.Context, input TaskDelegationInput, origin *AgentOrigin) (*TaskDelegationResult, error) {
	actor, err := NormalizeOrigin(origin)
	if err != nil {
		return nil, err
	}
	input.SourceProject = projects.NormalizeIdent(input.SourceProject)
	input.TargetProject = projects.NormalizeIdent(input.TargetProject)
	input.Title = strings.TrimSpace(input.Title)
	if input.SourceProject == "" || input.TargetProject == "" || input.SourceProject == input.TargetProject || input.Title == "" || len(input.Title) > 1024 || strings.IndexFunc(input.Title, unicode.IsControl) >= 0 {
		return nil, fmt.Errorf("%w: source and target projects must differ and title must be non-empty, at most 1024 bytes, and contain no control characters", ErrTaskDelegation)
	}
	for _, value := range []*string{input.Description, input.Details} {
		if value != nil && len(*value) > 64*1024 {
			return nil, fmt.Errorf("%w: description and specification are limited to 64 KiB", ErrTaskDelegation)
		}
	}
	if _, err := taskplatform.RequiredOS(input.Labels); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTaskDelegation, err)
	}
	if input.Hostname != nil {
		clean := strings.TrimSpace(*input.Hostname)
		if clean == "" {
			input.Hostname = nil
		} else if len(clean) > 255 || strings.IndexFunc(clean, unicode.IsControl) >= 0 {
			return nil, fmt.Errorf("%w: invalid hostname", ErrTaskDelegation)
		} else {
			input.Hostname = &clean
		}
	}
	input.Reporter = strings.TrimSpace(input.Reporter)
	if input.Reporter == "" {
		input.Reporter = "user"
	}
	if input.Requester != nil {
		normalized, err := NormalizeOrigin(&input.Requester.Origin)
		if err != nil {
			return nil, fmt.Errorf("%w: invalid requester origin", ErrTaskDelegation)
		}
		requester := *input.Requester
		requester.Origin = *normalized
		input.Requester = &requester
	}
	var result *TaskDelegationResult
	err = s.taskMutation(ctx, actor, func(tx *sql.Tx, current *AgentOrigin) error {
		sourceProject, err := getProject(ctx, tx, input.SourceProject)
		if err != nil {
			return err
		}
		targetProject, err := getProject(ctx, tx, input.TargetProject)
		if err != nil {
			return err
		}
		if sourceProject == nil || targetProject == nil {
			return ErrTaskNotFound
		}
		var parent *Task
		if input.ParentID != nil {
			parent, err = readTask(ctx, tx, input.SourceProject, *input.ParentID)
			if err != nil {
				return err
			}
			if parent == nil {
				return fmt.Errorf("source parent %w", ErrTaskNotFound)
			}
		} else if current != nil {
			return fmt.Errorf("%w: agent delegation requires a parent in the source project", ErrTaskDelegation)
		}
		if input.Requester != nil {
			if err := validateTaskRequester(ctx, tx, input.SourceProject, input.Labels, input.Requester, current); err != nil {
				return err
			}
		}

		sourceID, err := uuid.NewV7()
		if err != nil {
			return err
		}
		targetID, err := uuid.NewV7()
		if err != nil {
			return err
		}
		now := time.Now().UnixMilli()
		sourceTitle := input.Title + " (DELEGATED)"
		source, err := insertDelegationTask(ctx, tx, sourceID.String(), input.SourceProject, sourceTitle, input, current, true, input.Requester, now)
		if err != nil {
			return err
		}
		target, err := insertDelegationTask(ctx, tx, targetID.String(), input.TargetProject, input.Title, input, nil, false, nil, now)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "UPDATE tasks SET kind='delegated',delegated_to_project_ident=?,delegated_to_task_id=? WHERE id=? AND project_ident=?", input.TargetProject, target.ID, source.ID, input.SourceProject); err != nil {
			return err
		}
		source.Kind = "delegated"
		source.DelegatedToProjectIdent = ptr(input.TargetProject)
		source.DelegatedToTaskID = ptr(target.ID)
		if err := patchDelegationTaskSnapshots(ctx, tx, *source); err != nil {
			return err
		}
		if parent != nil {
			if err := attachTaskParent(ctx, tx, parent, source.ID); err != nil {
				return err
			}
		}
		if err := attachTaskParent(ctx, tx, source, target.ID); err != nil {
			return err
		}
		var requesterAgentID *string
		if current != nil {
			requesterAgentID = ptr(current.SessionID)
		}
		delegationID, err := uuid.NewV7()
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO task_delegations(id,source_project_ident,source_task_id,target_project_ident,target_task_id,requester_agent_id,requester_hostname,created_at,completed_at,completion_message_id) VALUES(?,?,?,?,?,?,?,?,NULL,NULL)`, delegationID.String(), input.SourceProject, source.ID, input.TargetProject, target.ID, requesterAgentID, input.Hostname, now); err != nil {
			return err
		}
		if _, err := insertTaskComment(ctx, tx, source.ID, "agent-gateway", "system", fmt.Sprintf("Delegated to project `%s` as task `%s`.", input.TargetProject, target.ID), nil, current); err != nil {
			return err
		}
		if _, err := insertTaskComment(ctx, tx, target.ID, "agent-gateway", "system", fmt.Sprintf("Delegated from project `%s`; source tracking task `%s`.", input.SourceProject, source.ID), nil, current); err != nil {
			return err
		}
		messageID, err := insertDelegationNudge(ctx, tx, input.TargetProject, "Delegated task created", fmt.Sprintf("Project `%s` delegated task `%s`: %s\n\nFetch task `%s` in project `%s` to claim and execute it.", input.SourceProject, target.ID, input.Title, target.ID, input.TargetProject))
		if err != nil {
			return err
		}
		delegation := TaskDelegation{ID: delegationID.String(), SourceProjectIdent: input.SourceProject, SourceTaskID: source.ID, TargetProjectIdent: input.TargetProject, TargetTaskID: target.ID, RequesterAgentID: requesterAgentID, RequesterHostname: input.Hostname, CreatedAt: now}
		source, err = readTask(ctx, tx, input.SourceProject, source.ID)
		if err != nil {
			return err
		}
		target, err = readTask(ctx, tx, input.TargetProject, target.ID)
		if err != nil {
			return err
		}
		result = &TaskDelegationResult{Delegation: delegation, SourceTask: *source, TargetTask: *target, MessageID: messageID}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// UpdateTaskDelegationContract mirrors source-owned contract metadata onto the
// linked target and records both task notes plus a destination nudge atomically.
func (s *Store) UpdateTaskDelegationContract(ctx context.Context, sourceProject, sourceTaskID string, update TaskDelegationUpdate, origin *AgentOrigin) (*TaskDelegationResult, error) {
	var result *TaskDelegationResult
	err := s.taskMutation(ctx, origin, func(tx *sql.Tx, actor *AgentOrigin) error {
		delegation, err := delegationBySource(ctx, tx, sourceProject, sourceTaskID)
		if err != nil {
			return err
		}
		if delegation == nil {
			return ErrTaskNotFound
		}

		// On the source wire, explicit null decodes as an absent outer Option.
		// Consequently a null specification falls back to details, while a
		// non-null specification takes precedence.
		effectiveDetails := update.Details
		if update.Specification != nil {
			effectiveDetails = update.Specification
		}
		metadata := TaskMetadataUpdate{Title: update.Title}
		if update.Description != nil {
			metadata.Description, err = json.Marshal(*update.Description)
			if err != nil {
				return err
			}
		}
		if effectiveDetails != nil {
			metadata.Details, err = json.Marshal(*effectiveDetails)
			if err != nil {
				return err
			}
		}
		if update.Labels != nil {
			metadata.Labels, err = json.Marshal(update.Labels)
			if err != nil {
				return err
			}
		}
		if err := ValidateTaskMetadata(metadata); err != nil {
			return err
		}

		source, err := readTask(ctx, tx, delegation.SourceProjectIdent, delegation.SourceTaskID)
		if err != nil {
			return err
		}
		target, err := readTask(ctx, tx, delegation.TargetProjectIdent, delegation.TargetTaskID)
		if err != nil {
			return err
		}
		if source == nil || target == nil {
			return ErrTaskNotFound
		}

		sourceMetadata := metadata
		if update.Title != nil {
			title := *update.Title
			if !strings.HasSuffix(title, " (DELEGATED)") {
				title += " (DELEGATED)"
			}
			sourceMetadata.Title = &title
		}
		if _, err := applyDelegationContractMetadata(ctx, tx, source, sourceMetadata, actor); err != nil {
			return err
		}

		targetMetadata := metadata
		if update.Title != nil {
			title := *update.Title
			for strings.HasSuffix(title, " (DELEGATED)") {
				title = strings.TrimSuffix(title, " (DELEGATED)")
			}
			targetMetadata.Title = &title
		}
		if _, err := applyDelegationContractMetadata(ctx, tx, target, targetMetadata, actor); err != nil {
			return err
		}

		const note = "Delegated task contract was updated by the source project."
		if _, err := insertTaskComment(ctx, tx, source.ID, "agent-gateway", "system", note, nil, actor); err != nil {
			return err
		}
		if _, err := insertTaskComment(ctx, tx, target.ID, "agent-gateway", "system", note, nil, actor); err != nil {
			return err
		}
		messageID, err := insertDelegationNudge(ctx, tx, delegation.TargetProjectIdent, "Delegated task updated", fmt.Sprintf("Project `%s` updated delegated task `%s`. Review the latest title, description, specification, and labels before continuing.", delegation.SourceProjectIdent, delegation.TargetTaskID))
		if err != nil {
			return err
		}
		source, err = readTask(ctx, tx, delegation.SourceProjectIdent, delegation.SourceTaskID)
		if err != nil {
			return err
		}
		target, err = readTask(ctx, tx, delegation.TargetProjectIdent, delegation.TargetTaskID)
		if err != nil {
			return err
		}
		result = &TaskDelegationResult{Delegation: *delegation, SourceTask: *source, TargetTask: *target, MessageID: messageID}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// applyDelegationContractMetadata uses the normal task metadata path and its
// managed-task, review-marker, label, and OS-release behavior. The Rust
// update_task path also enforces ownership for any OS change when no origin is
// present, so retain that fence here before the shared metadata writer.
func applyDelegationContractMetadata(ctx context.Context, tx *sql.Tx, current *Task, metadata TaskMetadataUpdate, actor *AgentOrigin) (*Task, error) {
	var dispatchTable bool
	if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type='table' AND name='task_dispatches')").Scan(&dispatchTable); err != nil {
		return nil, err
	}
	if dispatchTable {
		var managed bool
		if err := tx.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM task_dispatches WHERE task_id=?)", current.ID).Scan(&managed); err != nil {
			return nil, err
		}
		if managed {
			return nil, ErrTaskUnsupported
		}
	}

	currentOS, _ := taskplatform.RequiredOS(current.Labels)
	nextOS := currentOS
	if metadata.Labels != nil {
		if err := ValidateTaskMetadata(metadata); err != nil {
			return nil, err
		}
		var labels []string
		if err := json.Unmarshal(metadata.Labels, &labels); err != nil {
			return nil, fmt.Errorf("%w: labels must be an array of strings", ErrTaskMetadataInput)
		}
		nextOS, _ = taskplatform.RequiredOS(labels)
	}
	if currentOS != nextOS && ((current.OwnerOrigin == nil && (current.OwnerAgentID != nil || IsActiveTaskStatus(current.Status))) || (current.OwnerOrigin != nil && (actor == nil || actor.SessionID != current.OwnerOrigin.SessionID || actor.InstanceID != current.OwnerOrigin.InstanceID))) {
		return nil, ErrTaskOwnership
	}
	return applyTaskMetadata(ctx, tx, current, metadata, "", TaskTransitionPolicy{}, actor)
}

func validateTaskRequester(ctx context.Context, q queryer, project string, labels []string, target *RegisteredTargetIdentity, actor *AgentOrigin) error {
	if target == nil {
		return nil
	}
	origin, err := validateRegisteredTaskTarget(ctx, q, project, target, actor, time.Now())
	if err != nil {
		return err
	}
	if taskOS, _ := taskplatform.RequiredOS(labels); taskOS != "" && taskOS != origin.OS {
		return fmt.Errorf("%w: requester OS does not match task labels", ErrTaskConflict)
	}
	return nil
}

func validateRegisteredTaskTarget(ctx context.Context, q queryer, project string, target *RegisteredTargetIdentity, actor *AgentOrigin, now time.Time) (*AgentOrigin, error) {
	origin, err := NormalizeOrigin(&target.Origin)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid requester origin", ErrTaskDelegation)
	}
	if actor != nil && *actor != *origin {
		return nil, ErrTaskOwnership
	}
	generation, err := uuid.Parse(target.Generation)
	if err != nil || generation == uuid.Nil || generation.String() != target.Generation {
		return nil, fmt.Errorf("%w: requester generation must be a canonical UUID", ErrTaskDelegation)
	}
	var registeredOrigin, storedGeneration, storedRemote string
	var seenAt int64
	err = q.QueryRowContext(ctx, `SELECT origin_json,generation,seen_at,canonical_remote FROM task_target_presence WHERE project_ident=? AND instance_id=? AND session_id=? AND advertised=1`, projects.NormalizeIdent(project), origin.InstanceID, origin.SessionID).Scan(&registeredOrigin, &storedGeneration, &seenAt, &storedRemote)
	if err != nil || storedGeneration != target.Generation || seenAt < now.UnixMilli()-targetRegistrationLease {
		return nil, fmt.Errorf("%w: requester must be a current same-project task target", ErrTaskConflict)
	}
	var remote *string
	if storedRemote != "" {
		remote = &storedRemote
	}
	currentProject, err := getProject(ctx, q, project)
	if err != nil {
		return nil, err
	}
	if _, err := validateTargetProjectBinding(currentProject, remote); err != nil {
		return nil, fmt.Errorf("%w: requester registration no longer matches its project binding", ErrTaskConflict)
	}
	var stored AgentOrigin
	if err := json.Unmarshal([]byte(registeredOrigin), &stored); err != nil || stored != *origin {
		return nil, fmt.Errorf("%w: requester registration changed", ErrTaskConflict)
	}
	return origin, nil
}

func insertDelegationTask(ctx context.Context, tx *sql.Tx, id, project, title string, input TaskDelegationInput, owner *AgentOrigin, source bool, requester *RegisteredTargetIdentity, now int64) (*Task, error) {
	var rank int64
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(rank),0)+1 FROM tasks WHERE project_ident=? AND status='todo'", project).Scan(&rank); err != nil {
		return nil, err
	}
	var labels any
	if len(input.Labels) > 0 {
		encoded, err := json.Marshal(input.Labels)
		if err != nil {
			return nil, err
		}
		labels = string(encoded)
	}
	var ownerID, ownerJSON, requesterJSON any
	if source && owner != nil && taskplatform.Matches(input.Labels, owner.OS) {
		ownerID = owner.SessionID
		encoded, err := json.Marshal(owner)
		if err != nil {
			return nil, err
		}
		ownerJSON = string(encoded)
	}
	if source && requester != nil {
		encoded, err := json.Marshal(requester)
		if err != nil {
			return nil, err
		}
		requesterJSON = string(encoded)
	}
	var description, details, hostname any
	if input.Description != nil {
		description = *input.Description
	}
	if input.Details != nil {
		details = *input.Details
	}
	if input.Hostname != nil {
		hostname = *input.Hostname
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO tasks(id,project_ident,title,description,details,status,rank,labels,hostname,owner_agent_id,owner_origin_json,reporter,created_at,updated_at,kind,task_type,goal,routing_json,requester_json) VALUES(?,?,?,?,?,'todo',?,?,?,?,?,?,?,?,?,'general',NULL,'{"mode":"auto"}',?)`, id, project, title, description, details, rank, labels, hostname, ownerID, ownerJSON, input.Reporter, now, now, "normal", requesterJSON); err != nil {
		return nil, err
	}
	return readTask(ctx, tx, project, id)
}

func patchDelegationTaskSnapshots(ctx context.Context, tx *sql.Tx, task Task) error {
	return patchLatestCreatedTaskSnapshots(ctx, tx, task.ID, map[string]any{
		"kind":                       "delegated",
		"delegated_to_project_ident": task.DelegatedToProjectIdent,
		"delegated_to_task_id":       task.DelegatedToTaskID,
		"delegation_ready":           false,
	})
}

func insertDelegationNudge(ctx context.Context, tx *sql.Tx, project, subject, content string) (int64, error) {
	now := time.Now().UnixMilli()
	result, err := tx.ExecContext(ctx, `INSERT INTO messages(project_ident,source,content,sent_at,deliver_to_agents,message_type,subject,hostname,event_at,author_kind) VALUES(?,'system',?,?,1,'message',?,'agent-gateway',?,'system')`, project, content, now, subject, now)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

func readTaskDelegation(ctx context.Context, q queryer, where string, args ...any) (*TaskDelegation, error) {
	var value TaskDelegation
	err := q.QueryRowContext(ctx, `SELECT id,source_project_ident,source_task_id,target_project_ident,target_task_id,requester_agent_id,requester_hostname,created_at,completed_at,completion_message_id FROM task_delegations WHERE `+where, args...).Scan(
		&value.ID, &value.SourceProjectIdent, &value.SourceTaskID, &value.TargetProjectIdent, &value.TargetTaskID,
		&value.RequesterAgentID, &value.RequesterHostname, &value.CreatedAt, &value.CompletedAt, &value.CompletionMessageID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &value, nil
}

func delegationBySource(ctx context.Context, q queryer, project, id string) (*TaskDelegation, error) {
	return readTaskDelegation(ctx, q, "source_project_ident=? AND source_task_id=?", projects.NormalizeIdent(project), id)
}

func delegationByTarget(ctx context.Context, q queryer, project, id string) (*TaskDelegation, error) {
	return readTaskDelegation(ctx, q, "target_project_ident=? AND target_task_id=?", projects.NormalizeIdent(project), id)
}

func taskDelegationReady(ctx context.Context, q queryer, project, id string) (bool, error) {
	var ready bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM task_delegations d JOIN tasks target ON target.id=d.target_task_id AND target.project_ident=d.target_project_ident AND target.status='done' WHERE d.source_project_ident=? AND d.source_task_id=?)`, projects.NormalizeIdent(project), id).Scan(&ready)
	return ready, err
}

func completeTaskDelegation(ctx context.Context, tx *sql.Tx, before, after *Task, actor *AgentOrigin) error {
	if before.Status == after.Status {
		return nil
	}
	if after.Status != "done" {
		return nil
	}
	if link, err := delegationByTarget(ctx, tx, after.ProjectIdent, after.ID); err != nil {
		return err
	} else if link != nil {
		target, err := readTask(ctx, tx, link.TargetProjectIdent, link.TargetTaskID)
		if err != nil || target == nil {
			if err == nil {
				err = ErrTaskNotFound
			}
			return err
		}
		comments, err := readTaskComments(ctx, tx, target.ID, nil)
		if err != nil {
			return err
		}
		var summaryComments []string
		for _, comment := range comments {
			if comment.AuthorType != "system" {
				summaryComments = append(summaryComments, "- "+comment.Author+": "+comment.Content)
			}
		}
		commentText := "(none)"
		if len(summaryComments) > 0 {
			commentText = strings.Join(summaryComments, "\n")
		}
		summary := fmt.Sprintf("Delegated task completed in project `%s`.\n\nTitle: %s\nDescription: %s\nSpecification: %s\nComments:\n%s", target.ProjectIdent, target.Title, valueOrEmpty(target.Description), valueOrEmpty(target.Details), commentText)
		var eventWatermark, outboxWatermark int64
		if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(id),0) FROM task_events").Scan(&eventWatermark); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(id),0) FROM router_task_outbox").Scan(&outboxWatermark); err != nil {
			return err
		}
		if _, err := insertTaskComment(ctx, tx, link.SourceTaskID, "agent-gateway", "system", capTaskComment(summary), nil, actor); err != nil {
			return err
		}
		source, err := readTask(ctx, tx, link.SourceProjectIdent, link.SourceTaskID)
		if err != nil || source == nil {
			if err == nil {
				err = ErrTaskNotFound
			}
			return err
		}
		if source.Status == "todo" {
			if _, err := tx.ExecContext(ctx, `UPDATE tasks SET owner_agent_id=NULL,owner_origin_json=NULL,started_at=NULL,routing_json='{"mode":"auto"}',updated_at=? WHERE id=? AND project_ident=?`, time.Now().UnixMilli(), source.ID, source.ProjectIdent); err != nil {
				return err
			}
		}
		return patchNewDelegationSnapshots(ctx, tx, source.ID, eventWatermark, outboxWatermark)
	}
	link, err := delegationBySource(ctx, tx, after.ProjectIdent, after.ID)
	if err != nil || link == nil {
		return err
	}
	ready, err := taskDelegationReady(ctx, tx, after.ProjectIdent, after.ID)
	if err != nil || !ready {
		return err
	}
	confirmation := fmt.Sprintf("Project `%s` inspected completed task `%s` and confirmed completion by closing sender ticket `%s`.", link.SourceProjectIdent, link.TargetTaskID, link.SourceTaskID)
	if _, err := insertTaskComment(ctx, tx, link.TargetTaskID, "agent-gateway", "system", confirmation, nil, actor); err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, "UPDATE task_delegations SET completed_at=COALESCE(completed_at,?) WHERE id=?", time.Now().UnixMilli(), link.ID)
	return err
}

func patchNewDelegationSnapshots(ctx context.Context, tx *sql.Tx, taskID string, eventAfter, outboxAfter int64) error {
	if _, err := tx.ExecContext(ctx, `UPDATE task_events SET task_json=json_set(task_json,'$.delegation_ready',json('true'))
		WHERE id>? AND json_extract(task_json,'$.id')=?`, eventAfter, taskID); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `UPDATE router_task_outbox SET task_json=json_set(task_json,'$.delegation_ready',json('true'))
		WHERE id>? AND json_extract(task_json,'$.id')=?`, outboxAfter, taskID)
	return err
}

func recordDelegationInspection(ctx context.Context, tx *sql.Tx, task *Task, actor *AgentOrigin) error {
	link, err := delegationBySource(ctx, tx, task.ProjectIdent, task.ID)
	if err != nil {
		return err
	}
	if link == nil {
		return ErrTaskUnsupported
	}
	message := fmt.Sprintf("Project `%s` has selected an agent to inspect completed task `%s` under sender ticket `%s`. The sender ticket will close after inspection confirms completion. Clarifications will be recorded on the linked tickets.", link.SourceProjectIdent, link.TargetTaskID, link.SourceTaskID)
	_, err = insertTaskComment(ctx, tx, link.TargetTaskID, "agent-gateway", "system", message, nil, actor)
	return err
}

func valueOrEmpty(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}
