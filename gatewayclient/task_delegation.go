package gatewayclient

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/nitecon/amux/projects"
	"github.com/nitecon/amux/storage"
)

type TaskDelegateInput struct {
	TargetProject string                            `json:"target_project_ident"`
	Title         string                            `json:"title"`
	ParentID      *string                           `json:"parent_task_id,omitempty"`
	Specification *string                           `json:"specification,omitempty"`
	Description   *string                           `json:"description,omitempty"`
	Labels        []string                          `json:"labels,omitempty"`
	Hostname      *string                           `json:"hostname,omitempty"`
	Reporter      *string                           `json:"reporter,omitempty"`
	Requester     *storage.RegisteredTargetIdentity `json:"requester,omitempty"`
}

// RequireTaskHierarchy prevents an older gateway from silently ignoring parent
// links. It sends no creation request on failure.
func (c *Client) RequireTaskHierarchy(ctx context.Context) error {
	return c.requireTaskCapability(ctx, "task_hierarchy", "No task creation sent")
}

func (c *Client) requireTaskCapability(ctx context.Context, name, refusal string) error {
	var capabilities map[string]json.RawMessage
	if err := c.do(ctx, "GET", "/v1/tasks/capabilities", nil, nil, &capabilities); err != nil {
		return fmt.Errorf("gateway capability not confirmed: %s: %w", refusal, err)
	}
	var version int
	if json.Unmarshal(capabilities[name], &version) != nil || version != 1 {
		return fmt.Errorf("gateway does not support %s version 1: %s", name, refusal)
	}
	return nil
}

// DelegateTask never retries an uncertain creation. Its receipt must prove both
// requested project links before the caller can treat the handoff as accepted.
func (c *Client) DelegateTask(ctx context.Context, project string, input TaskDelegateInput, origin *storage.AgentOrigin, goal *string) (*storage.TaskDelegationResult, error) {
	if err := validateDelegationProject(project); err != nil {
		return nil, err
	}
	if err := validateDelegationProject(input.TargetProject); err != nil {
		return nil, err
	}
	project = projects.NormalizeIdent(project)
	input.TargetProject = projects.NormalizeIdent(input.TargetProject)
	input.Title = strings.TrimSpace(input.Title)
	if project == input.TargetProject {
		return nil, fmt.Errorf("delegation requires a different target project")
	}
	if input.Title == "" || len(input.Title) > 1024 || !utf8.ValidString(input.Title) || strings.IndexFunc(input.Title, unicode.IsControl) >= 0 {
		return nil, fmt.Errorf("delegation title must be non-empty, at most 1 KiB and contain no control characters")
	}
	if input.Specification != nil && (len(*input.Specification) > 65536 || !utf8.ValidString(*input.Specification)) {
		return nil, fmt.Errorf("delegation context exceeds 64 KiB or is invalid UTF-8")
	}
	if input.ParentID != nil && !canonicalDelegationID(*input.ParentID) {
		return nil, fmt.Errorf("delegation parent must be a canonical non-nil UUID")
	}
	if origin != nil {
		normalized, err := storage.NormalizeOrigin(origin)
		if err != nil || normalized == nil || !canonicalDelegationID(normalized.SessionID) || !canonicalDelegationID(normalized.InstanceID) {
			return nil, fmt.Errorf("delegation requires a complete native origin")
		}
		origin = normalized
		if input.ParentID == nil {
			return nil, fmt.Errorf("agent delegation requires a parent task")
		}
		if input.Reporter == nil {
			reporter := origin.SessionID
			input.Reporter = &reporter
		}
	}
	if input.Requester != nil {
		if origin == nil || input.Requester.Origin != *origin || !canonicalDelegationID(input.Requester.Generation) {
			return nil, fmt.Errorf("requester must be the calling agent with a valid generation")
		}
		if err := c.requireTaskCapability(ctx, "requester_subscriptions", "No subscription mutation sent"); err != nil {
			return nil, err
		}
	}
	var raw json.RawMessage
	err := c.doTaskGoal(ctx, "POST", taskPath(project)+"/delegate", input, origin, goal, &raw)
	if err != nil {
		return nil, fmt.Errorf("delegation not confirmed; inspect tasks in both projects before retrying: %w", err)
	}
	var result storage.TaskDelegationResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return nil, fmt.Errorf("delegation receipt invalid; inspect both projects before retrying")
	}
	if err := validateDelegationReceipt(&result, project, input); err != nil {
		return nil, err
	}
	return &result, nil
}

func validateDelegationProject(project string) error {
	if project == "" || project == "." || project == ".." {
		return fmt.Errorf("delegation requires a project identifier, not a URL or path")
	}
	for _, r := range project {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_.", r)) {
			return fmt.Errorf("delegation requires a project identifier, not a URL or path")
		}
	}
	return nil
}

func canonicalDelegationID(value string) bool {
	id, err := uuid.Parse(value)
	return err == nil && id != uuid.Nil && id.String() == value
}

func validateDelegationReceipt(receipt *storage.TaskDelegationResult, project string, input TaskDelegateInput) error {
	return validateDelegationLinks(receipt, project, input, func(status string) bool {
		return status == "todo" || storage.IsActiveTaskStatus(status) || status == "done"
	})
}

func validateDelegationLinks(receipt *storage.TaskDelegationResult, project string, input TaskDelegateInput, validStatus func(string) bool) error {
	uncertain := func() error {
		return fmt.Errorf("gateway did not acknowledge the requested linked delegation; inspect both projects before retrying")
	}
	source, target, link := receipt.SourceTask, receipt.TargetTask, receipt.Delegation
	if !canonicalDelegationID(source.ID) || !canonicalDelegationID(target.ID) || source.ID == target.ID || !canonicalDelegationID(link.ID) || receipt.MessageID <= 0 || source.ProjectIdent != project || target.ProjectIdent != input.TargetProject || !validStatus(source.Status) || !validStatus(target.Status) || source.Kind != "delegated" || source.DelegatedToProjectIdent == nil || *source.DelegatedToProjectIdent != input.TargetProject || source.DelegatedToTaskID == nil || *source.DelegatedToTaskID != target.ID || link.SourceProjectIdent != project || link.TargetProjectIdent != input.TargetProject || link.SourceTaskID != source.ID || link.TargetTaskID != target.ID {
		return uncertain()
	}
	if input.ParentID == nil {
		if source.Parent != nil {
			return uncertain()
		}
	} else if source.Parent == nil || source.Parent.ID != *input.ParentID || source.Parent.ProjectIdent != project {
		return uncertain()
	}
	if target.Parent == nil || target.Parent.ID != source.ID || target.Parent.ProjectIdent != project {
		return uncertain()
	}
	if input.Requester != nil {
		var requester *storage.RegisteredTargetIdentity
		if json.Unmarshal(source.Requester, &requester) != nil || requester == nil || *requester != *input.Requester {
			return uncertain()
		}
	}
	return nil
}
