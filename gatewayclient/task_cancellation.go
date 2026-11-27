package gatewayclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"github.com/google/uuid"
	"github.com/nitecon/amux/projects"
	"github.com/nitecon/amux/storage"
)

func taskCancellationPath(project, id string) string {
	return taskPath(project) + "/" + url.PathEscape(id) + "/cancellation"
}

// TaskCancellation reads the cancellation report for one task.
func (c *Client) TaskCancellation(ctx context.Context, project, id string) (*storage.Cancellation, error) {
	canonicalID, err := canonicalCancellationTaskID(id)
	if err != nil {
		return nil, err
	}
	var raw json.RawMessage
	if err := c.do(ctx, "GET", taskCancellationPath(project, canonicalID), nil, nil, &raw); err != nil {
		return nil, err
	}
	var result storage.Cancellation
	if json.Unmarshal(raw, &result) != nil || !executorStopFieldIsFalse(raw, false) {
		return nil, fmt.Errorf("invalid gateway cancellation response")
	}
	if err := validateCancellation(&result, project, canonicalID, nil, ""); err != nil {
		return nil, err
	}
	return &result, nil
}

// TaskCancel records an explicit native cancellation report. The server
// authorizes it atomically against current ownership and lifecycle state.
func (c *Client) TaskCancel(ctx context.Context, project, id, reason string, changeset json.RawMessage, origin *storage.AgentOrigin) (*storage.CancelResponse, error) {
	canonicalID, err := canonicalCancellationTaskID(id)
	if err != nil {
		return nil, err
	}
	canonicalOrigin, err := storage.NormalizeOrigin(origin)
	if err != nil || canonicalOrigin == nil {
		return nil, fmt.Errorf("task cancellation requires a complete native agent origin")
	}
	reason, err = storage.ValidateTaskCancellationReason(reason)
	if err != nil {
		return nil, err
	}
	if len(changeset) > 0 {
		if _, err := storage.ValidateChangeset(changeset); err != nil {
			return nil, fmt.Errorf("invalid task cancellation changeset")
		}
	}
	input := struct {
		Reason    string          `json:"reason"`
		Changeset json.RawMessage `json:"changeset,omitempty"`
	}{Reason: reason, Changeset: changeset}
	var raw json.RawMessage
	if err := c.do(ctx, "POST", taskCancellationPath(project, canonicalID), input, canonicalOrigin, &raw); err != nil {
		return nil, err
	}
	var result storage.CancelResponse
	if json.Unmarshal(raw, &result) != nil || !executorStopFieldIsFalse(raw, true) {
		return nil, fmt.Errorf("invalid gateway cancellation response")
	}
	if result.Task == nil {
		return nil, fmt.Errorf("gateway did not acknowledge task cancellation")
	}
	taskRaw, marshalErr := json.Marshal(result.Task)
	if marshalErr != nil {
		return nil, fmt.Errorf("gateway did not acknowledge the cancelled task")
	}
	if _, err := taskAcknowledgement(taskRaw, project, canonicalID); err != nil || result.Task.Status != "cancelled" || result.Task.DoneAt != nil {
		return nil, fmt.Errorf("gateway did not acknowledge the cancelled task")
	}
	if result.Cancellation == nil {
		return nil, fmt.Errorf("gateway did not acknowledge task cancellation")
	}
	if err := validateCancellation(result.Cancellation, project, canonicalID, canonicalOrigin, strings.TrimSpace(reason)); err != nil {
		return nil, err
	}
	cancellation := result.Cancellation
	if !sameString(result.Task.OwnerAgentID, cancellation.PriorOwnerAgentID) || !sameOrigin(result.Task.OwnerOrigin, cancellation.PriorOwnerOrigin) || !sameInt64(result.Task.StartedAt, cancellation.PriorStartedAt) {
		return nil, fmt.Errorf("gateway cancellation did not preserve prior claim provenance")
	}
	return &result, nil
}

func canonicalCancellationTaskID(id string) (string, error) {
	parsed, err := uuid.Parse(id)
	if err != nil || parsed == uuid.Nil {
		return "", fmt.Errorf("task ID must be a non-zero UUID")
	}
	return parsed.String(), nil
}

func validateCancellation(cancellation *storage.Cancellation, project, id string, reporter *storage.AgentOrigin, reason string) error {
	if cancellation == nil || cancellation.TaskID != id || cancellation.ProjectIdent != projects.NormalizeIdent(project) || cancellation.Reason == "" || strings.TrimSpace(cancellation.Reason) != cancellation.Reason || cancellation.CancelledAt <= 0 || cancellation.ExecutorStopConfirmed {
		return fmt.Errorf("gateway cancellation response did not match the requested task")
	}
	validatedReason, err := storage.ValidateTaskCancellationReason(cancellation.Reason)
	if err != nil || validatedReason != cancellation.Reason {
		return fmt.Errorf("gateway cancellation response has an invalid reason")
	}
	if cancellation.PriorStartedAt != nil && *cancellation.PriorStartedAt < 0 {
		return fmt.Errorf("gateway cancellation response has an invalid prior start time")
	}
	if cancellation.PriorStatus != "todo" && !storage.IsActiveTaskStatus(cancellation.PriorStatus) {
		return fmt.Errorf("gateway cancellation response has an invalid prior status")
	}
	if (cancellation.PriorStatus == "in_progress" || cancellation.PriorOwnerAgentID != nil) && cancellation.PriorOwnerOrigin == nil {
		return fmt.Errorf("gateway cancellation response has an unbound prior claim")
	}
	if cancellation.PriorOwnerOrigin != nil {
		canonical, err := storage.NormalizeOrigin(cancellation.PriorOwnerOrigin)
		if err != nil || *canonical != *cancellation.PriorOwnerOrigin {
			return fmt.Errorf("gateway cancellation response has an invalid prior owner origin")
		}
	}
	canonicalReporter, err := storage.NormalizeOrigin(&cancellation.ReporterOrigin)
	if err != nil || canonicalReporter == nil || *canonicalReporter != cancellation.ReporterOrigin {
		return fmt.Errorf("gateway cancellation response has an invalid reporter origin")
	}
	if reporter != nil && *canonicalReporter != *reporter {
		return fmt.Errorf("gateway cancellation response did not acknowledge the reporting origin")
	}
	if reporter != nil && cancellation.PriorOwnerOrigin != nil && *cancellation.PriorOwnerOrigin != *reporter {
		return fmt.Errorf("gateway cancellation response belongs to another claiming execution")
	}
	if reason != "" && cancellation.Reason != reason {
		return fmt.Errorf("gateway cancellation response did not acknowledge the requested reason")
	}
	return nil
}

func sameOrigin(a, b *storage.AgentOrigin) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameString(a, b *string) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func sameInt64(a, b *int64) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return *a == *b
}

func executorStopFieldIsFalse(raw json.RawMessage, nested bool) bool {
	var envelope struct {
		ExecutorStopConfirmed *bool           `json:"executor_stop_confirmed"`
		Cancellation          json.RawMessage `json:"cancellation"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return false
	}
	field := envelope.ExecutorStopConfirmed
	if nested {
		if len(envelope.Cancellation) == 0 {
			return false
		}
		var nested struct {
			ExecutorStopConfirmed *bool `json:"executor_stop_confirmed"`
		}
		if json.Unmarshal(envelope.Cancellation, &nested) != nil {
			return false
		}
		field = nested.ExecutorStopConfirmed
	}
	return field != nil && !*field
}
