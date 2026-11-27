package gatewayclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/nitecon/amux/projects"
	"github.com/nitecon/amux/storage"
)

// UpdateTaskDelegation edits one existing source contract. Its pre-read pins
// the expected linked tasks; the server remains responsible for transactional
// authorization and applying both sides. An uncertain PATCH is never retried.
func (c *Client) UpdateTaskDelegation(ctx context.Context, project, id string, input storage.TaskDelegationUpdate, origin *storage.AgentOrigin) (*storage.TaskDelegationResult, error) {
	before, err := c.readDelegationForUpdate(ctx, project, id)
	if err != nil {
		return nil, err
	}
	project = projects.NormalizeIdent(project)
	var receipt storage.TaskDelegationResult
	if err := c.do(ctx, http.MethodPatch, taskPath(project)+"/"+url.PathEscape(id)+"/delegation", input, origin, &receipt); err != nil {
		return nil, fmt.Errorf("delegation contract not confirmed; inspect both projects before retrying: %w", err)
	}
	if err := validateDelegationUpdateReceipt(&receipt, project, before, input); err != nil {
		return nil, err
	}
	return &receipt, nil
}

func (c *Client) readDelegationForUpdate(ctx context.Context, project, id string) (storage.Task, error) {
	if err := validateDelegationProject(project); err != nil {
		return storage.Task{}, err
	}
	if !canonicalDelegationID(id) {
		return storage.Task{}, fmt.Errorf("delegation source task must be a canonical non-nil UUID")
	}
	project = projects.NormalizeIdent(project)
	raw, err := c.TaskRead(ctx, project, id, 0)
	if err != nil {
		return storage.Task{}, err
	}
	var before storage.Task
	if json.Unmarshal(raw, &before) != nil || before.Kind != "delegated" || before.DelegatedToProjectIdent == nil || before.DelegatedToTaskID == nil || !canonicalDelegationID(*before.DelegatedToTaskID) || *before.DelegatedToTaskID == id {
		return storage.Task{}, fmt.Errorf("task did not acknowledge an existing source delegation; no contract update sent")
	}
	if err := validateDelegationProject(*before.DelegatedToProjectIdent); err != nil {
		return storage.Task{}, err
	}
	if *before.DelegatedToProjectIdent == project {
		return storage.Task{}, fmt.Errorf("delegation target must be a different project; no contract update sent")
	}
	return before, nil
}

func validateDelegationUpdateReceipt(receipt *storage.TaskDelegationResult, project string, before storage.Task, input storage.TaskDelegationUpdate) error {
	if before.DelegatedToProjectIdent == nil || before.DelegatedToTaskID == nil || receipt.SourceTask.ID != before.ID || receipt.TargetTask.ID != *before.DelegatedToTaskID {
		return fmt.Errorf("delegation contract links not confirmed; inspect both projects before retrying")
	}
	expected := TaskDelegateInput{TargetProject: *before.DelegatedToProjectIdent}
	if before.Parent != nil {
		expected.ParentID = &before.Parent.ID
	}
	if err := validateDelegationLinks(receipt, project, expected, storage.IsValidTaskStatus); err != nil {
		return err
	}
	uncertain := func() error {
		return fmt.Errorf("delegation contract metadata not confirmed; inspect both projects before retrying")
	}
	if input.Title != nil {
		sourceTitle := *input.Title
		if !strings.HasSuffix(sourceTitle, " (DELEGATED)") {
			sourceTitle += " (DELEGATED)"
		}
		targetTitle := *input.Title
		for strings.HasSuffix(targetTitle, " (DELEGATED)") {
			targetTitle = strings.TrimSuffix(targetTitle, " (DELEGATED)")
		}
		if receipt.SourceTask.Title != sourceTitle || receipt.TargetTask.Title != targetTitle {
			return uncertain()
		}
	}
	details := input.Specification
	if details == nil {
		details = input.Details
	}
	for _, task := range []storage.Task{receipt.SourceTask, receipt.TargetTask} {
		if input.Description != nil && (task.Description == nil || *task.Description != *input.Description) {
			return uncertain()
		}
		if details != nil && (task.Details == nil || *task.Details != *details) {
			return uncertain()
		}
		if input.Labels != nil && (task.Labels == nil || !slices.Equal(input.Labels, task.Labels)) {
			return uncertain()
		}
	}
	return nil
}
