package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nitecon/amux/taskplatform"
)

// ErrTaskMetadataInput identifies malformed metadata at the request boundary.
// HTTP callers can map it to 400; policy and state conflicts remain conflicts.
var ErrTaskMetadataInput = errors.New("invalid task metadata")

// TaskMetadataUpdate is embedded in TaskWorkflowUpdate so its fields stay
// flattened on the task workflow JSON contract. Raw nullable fields distinguish
// omitted (unchanged) from explicit null (clear).
type TaskMetadataUpdate struct {
	Title         *string         `json:"title,omitempty"`
	Goal          json.RawMessage `json:"goal,omitempty"`
	Description   json.RawMessage `json:"description,omitempty"`
	Details       json.RawMessage `json:"details,omitempty"`
	Specification json.RawMessage `json:"specification,omitempty"`
	Hostname      json.RawMessage `json:"hostname,omitempty"`
	Labels        json.RawMessage `json:"labels,omitempty"`
	Rank          *int64          `json:"rank,omitempty"`
}

// DetailsValue returns the effective details field. Rust's route gives a
// present specification precedence over the legacy details alias, including
// when specification is null.
func (m TaskMetadataUpdate) DetailsValue() json.RawMessage {
	if m.Specification != nil {
		return m.Specification
	}
	return m.Details
}

// ValidateTaskMetadata checks wire shapes before policy validation. A null
// labels value is the Rust Option<Vec<String>> no-op; a null element is invalid.
func ValidateTaskMetadata(m TaskMetadataUpdate) error {
	for name, raw := range map[string]json.RawMessage{
		"goal": m.Goal, "description": m.Description,
		"details": m.DetailsValue(), "hostname": m.Hostname,
	} {
		if raw == nil {
			continue
		}
		if _, err := decodeMetadataNullableString(raw); err != nil {
			return fmt.Errorf("%w: %s must be a string or null", ErrTaskMetadataInput, name)
		}
	}
	if m.Goal != nil {
		goal, _ := decodeMetadataNullableString(m.Goal)
		if _, err := NormalizeTaskGoal(goal); err != nil {
			return fmt.Errorf("%w: %v", ErrTaskMetadataInput, err)
		}
	}
	if m.Labels != nil && !bytes.Equal(bytes.TrimSpace(m.Labels), []byte("null")) {
		wrapped := append([]byte(`{"labels":`), m.Labels...)
		wrapped = append(wrapped, '}')
		if _, err := taskplatform.ParseLabels(wrapped); err != nil {
			return fmt.Errorf("%w: %v", ErrTaskMetadataInput, err)
		}
	}
	return nil
}

func decodeMetadataNullableString(raw json.RawMessage) (*string, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return &value, nil
}

func metadataPresent(m TaskMetadataUpdate) bool {
	return m.Title != nil || m.Goal != nil || m.Description != nil || m.DetailsValue() != nil || m.Hostname != nil || m.Labels != nil || m.Rank != nil
}

// applyTaskMetadata applies metadata before lifecycle status writes so any
// status-triggered durable event contains the final metadata and rank.
func applyTaskMetadata(ctx context.Context, tx *sql.Tx, current *Task, update TaskMetadataUpdate, status string, policy TaskTransitionPolicy, actor *AgentOrigin) (*Task, error) {
	if !metadataPresent(update) {
		if status == "" {
			if _, err := tx.ExecContext(ctx, "UPDATE tasks SET updated_at=? WHERE id=? AND project_ident=?", time.Now().UnixMilli(), current.ID, current.ProjectIdent); err != nil {
				return nil, err
			}
			return readTask(ctx, tx, current.ProjectIdent, current.ID)
		}
		return current, nil
	}
	if err := ValidateTaskMetadata(update); err != nil {
		return nil, err
	}
	goalChanged := update.Goal != nil
	var goal *string
	if goalChanged {
		var err error
		goal, err = decodeMetadataNullableString(update.Goal)
		if err != nil {
			return nil, fmt.Errorf("%w: goal must be a string or null", ErrTaskMetadataInput)
		}
		goal, err = NormalizeTaskGoal(goal)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrTaskMetadataInput, err)
		}
	}
	description, err := decodeSelectedNullable(update.Description)
	if err != nil {
		return nil, fmt.Errorf("%w: description must be a string or null", ErrTaskMetadataInput)
	}
	details, err := decodeSelectedNullable(update.DetailsValue())
	if err != nil {
		return nil, fmt.Errorf("%w: specification must be a string or null", ErrTaskMetadataInput)
	}
	hostname, err := decodeSelectedNullable(update.Hostname)
	if err != nil {
		return nil, fmt.Errorf("%w: hostname must be a string or null", ErrTaskMetadataInput)
	}
	labelsChanged := update.Labels != nil && !bytes.Equal(bytes.TrimSpace(update.Labels), []byte("null"))
	labels := append([]string(nil), current.Labels...)
	if labelsChanged {
		wrapped := append([]byte(`{"labels":`), update.Labels...)
		wrapped = append(wrapped, '}')
		labels, err = taskplatform.ParseLabels(wrapped)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrTaskMetadataInput, err)
		}
		if hasLabel(current.Labels, "task-review") && !sameReviewMarkers(current.Labels, labels) {
			return nil, fmt.Errorf("%w: completion review source, exclusions and provider preference are immutable", ErrTaskConflict)
		}
		if actor != nil && hasLabel(current.Labels, "autoreview-finding") && !hasLabel(labels, "autoreview-finding") {
			return nil, fmt.Errorf("%w: only a user may clear the review finding hold", ErrTaskConflict)
		}
	}
	currentOS, _ := taskplatform.RequiredOS(current.Labels) // legacy invalid labels are tolerated.
	nextOS := currentOS
	if labelsChanged {
		nextOS, err = taskplatform.RequiredOS(labels)
		if err != nil {
			return nil, fmt.Errorf("%w: %v", ErrTaskMetadataInput, err)
		}
	}
	osChanged := currentOS != nextOS
	if osChanged && status != "" {
		return nil, fmt.Errorf("%w: change the task os: label without a status or owner change", ErrTaskConflict)
	}
	if osChanged && actor != nil && current.Status != "todo" && !IsActiveTaskStatus(current.Status) {
		return nil, fmt.Errorf("%w: only active tasks can change their os: label", ErrTaskConflict)
	}
	if actor != nil && policy.CheckGoal && current.Goal != nil && (status == "in_progress" || goalChanged) && strings.TrimSpace(policy.SelectedGoal) != *current.Goal {
		return nil, fmt.Errorf("%w: select the task's goal with X-Task-Goal before claiming or changing its goal", ErrTaskConflict)
	}
	requiresOwner := (status != "" && status != "done") || goalChanged || osChanged
	if actor != nil && requiresOwner {
		if (current.OwnerOrigin == nil && (current.OwnerAgentID != nil || IsActiveTaskStatus(current.Status))) || (current.OwnerOrigin != nil && (actor.SessionID != current.OwnerOrigin.SessionID || actor.InstanceID != current.OwnerOrigin.InstanceID)) {
			return nil, ErrTaskOwnership
		}
	}
	if actor != nil && current.Status == "user_review" && status != "" && status != "done" {
		return nil, fmt.Errorf("%w: tasks in user review require a user decision", ErrTaskConflict)
	}
	releaseOwner := osChanged && (current.OwnerAgentID != nil || current.OwnerOrigin != nil) && taskOSMismatch(nextOS, current.OwnerOrigin)
	resetRouting := releaseOwner || (osChanged && routingOSMismatch(current.Routing, nextOS))
	sets := []string{"updated_at=?"}
	args := []any{time.Now().UnixMilli()}
	add := func(column string, value any) { sets = append(sets, column+"=?"); args = append(args, value) }
	if update.Title != nil {
		add("title", *update.Title)
	}
	if goalChanged {
		add("goal", goal)
	}
	if update.Description != nil {
		add("description", description)
	}
	if update.DetailsValue() != nil {
		add("details", details)
	}
	if labelsChanged {
		raw, _ := json.Marshal(labels)
		add("labels", string(raw))
	}
	if update.Hostname != nil {
		add("hostname", hostname)
	}
	if update.Rank != nil {
		add("rank", *update.Rank)
	}
	if resetRouting {
		add("routing_json", `{"mode":"auto"}`)
	}
	args = append(args, current.ID, current.ProjectIdent)
	if _, err := tx.ExecContext(ctx, "UPDATE tasks SET "+strings.Join(sets, ",")+" WHERE id=? AND project_ident=?", args...); err != nil {
		return nil, err
	}
	if releaseOwner {
		if _, err := tx.ExecContext(ctx, "UPDATE tasks SET status='todo',owner_agent_id=NULL,owner_origin_json=NULL,started_at=NULL,updated_at=? WHERE id=? AND project_ident=?", time.Now().UnixMilli(), current.ID, current.ProjectIdent); err != nil {
			return nil, err
		}
		if _, err := insertTaskComment(ctx, tx, current.ID, "agent-gateway", "system", fmt.Sprintf("Task OS requirement changed to %s; released the incompatible claim to TODO for a compatible agent.", taskOSName(nextOS)), nil, actor); err != nil {
			return nil, err
		}
	}
	return readTask(ctx, tx, current.ProjectIdent, current.ID)
}

func decodeSelectedNullable(raw json.RawMessage) (*string, error) {
	if raw == nil {
		return nil, nil
	}
	return decodeMetadataNullableString(raw)
}

func sameReviewMarkers(current, next []string) bool {
	markers := func(labels []string) map[string]bool {
		set := map[string]bool{}
		for _, label := range labels {
			if label == "task-review" || label == "documentation-review" || strings.HasPrefix(label, "review-of:") || strings.HasPrefix(label, "review-exclude:") || strings.HasPrefix(label, "review-provider:") {
				set[label] = true
			}
		}
		return set
	}
	a, b := markers(current), markers(next)
	if len(a) != len(b) {
		return false
	}
	for label := range a {
		if !b[label] {
			return false
		}
	}
	return true
}

func taskOSMismatch(required string, owner *AgentOrigin) bool {
	return required != "" && (owner == nil || owner.OS != required)
}

func taskOSName(required string) string {
	if required == "" {
		return "any"
	}
	return required
}

func routingOSMismatch(raw json.RawMessage, required string) bool {
	var routing struct {
		Mode   string `json:"mode"`
		Target struct {
			Origin AgentOrigin `json:"origin"`
		} `json:"target"`
	}
	return required != "" && json.Unmarshal(raw, &routing) == nil && routing.Mode == "session" && routing.Target.Origin.OS != required
}
