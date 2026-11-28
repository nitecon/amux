package gatewayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/nitecon/amux/identity/agent"
	"github.com/nitecon/amux/storage"
)

func (c *Client) RequireRequesterSubscriptions(ctx context.Context) error {
	return c.requireTaskCapability(ctx, "requester_subscriptions", "No subscription mutation sent")
}

func validateRequesterTarget(target *storage.RegisteredTargetIdentity, actor *agent.Origin) error {
	if target == nil {
		return nil
	}
	origin, err := agent.NormalizeOrigin(&target.Origin)
	if err != nil || origin == nil || !canonicalDelegationID(origin.SessionID) || !canonicalDelegationID(origin.InstanceID) || *origin != target.Origin || !canonicalDelegationID(target.Generation) {
		return fmt.Errorf("requester requires a complete native origin and canonical non-nil generation")
	}
	if actor != nil && *actor != target.Origin {
		return fmt.Errorf("requester must be the calling agent")
	}
	return nil
}

// RequesterTarget resolves only the caller's exact current advertisement. It
// never assigns work, guesses a generation, or substitutes a display-name peer.
// The mutation rechecks the target's lease and repository binding atomically.
func (c *Client) RequesterTarget(ctx context.Context, project string, actor agent.Origin) (*storage.RegisteredTargetIdentity, error) {
	if err := validateDelegationProject(project); err != nil {
		return nil, err
	}
	normalized, err := agent.NormalizeOrigin(&actor)
	if err != nil || normalized == nil || !canonicalDelegationID(normalized.SessionID) || !canonicalDelegationID(normalized.InstanceID) {
		return nil, fmt.Errorf("requester discovery requires a complete native actor")
	}
	var directory storage.TargetDirectory
	if err := c.do(ctx, http.MethodGet, taskTargetsPath(project), nil, nil, &directory); err != nil {
		return nil, err
	}
	if len(directory.Targets) > 4096 {
		return nil, fmt.Errorf("too many task requester targets")
	}
	now := time.Now().UnixMilli()
	var found *storage.RegisteredTargetIdentity
	for _, entry := range directory.Targets {
		if err := validateRequesterTarget(&entry.Target, nil); err != nil {
			return nil, fmt.Errorf("invalid task requester directory")
		}
		if entry.SeenAt <= 0 || entry.ExpiresAt <= entry.SeenAt || entry.ExpiresAt-entry.SeenAt > 120000 {
			return nil, fmt.Errorf("invalid task requester availability")
		}
		if entry.Target.Origin != *normalized || entry.ExpiresAt <= now {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("requesting agent has multiple executions; refresh availability")
		}
		target := entry.Target
		found = &target
	}
	if found == nil {
		return nil, fmt.Errorf("requesting agent is not currently advertised; refresh availability")
	}
	return found, nil
}

// SetTaskRequester changes result subscription only. Human callers pass nil
// origin, even when selecting a native watcher. Unknown acceptance never retries.
func (c *Client) SetTaskRequester(ctx context.Context, project, id string, target *storage.RegisteredTargetIdentity, origin *agent.Origin) (*storage.Task, error) {
	if err := validateDelegationProject(project); err != nil {
		return nil, err
	}
	if !canonicalDelegationID(id) {
		return nil, fmt.Errorf("task ID must be a canonical non-nil UUID")
	}
	if origin != nil {
		normalized, err := agent.NormalizeOrigin(origin)
		if err != nil || normalized == nil || !canonicalDelegationID(normalized.SessionID) || !canonicalDelegationID(normalized.InstanceID) {
			return nil, fmt.Errorf("requester mutation requires a complete native actor")
		}
		origin = normalized
	}
	if err := validateRequesterTarget(target, origin); err != nil {
		return nil, err
	}
	if err := c.RequireRequesterSubscriptions(ctx); err != nil {
		return nil, err
	}
	var raw json.RawMessage
	input := struct {
		Requester *storage.RegisteredTargetIdentity `json:"requester"`
	}{target}
	if err := c.do(ctx, http.MethodPut, taskPath(project)+"/"+url.PathEscape(id)+"/requester", input, origin, &raw); err != nil {
		return nil, fmt.Errorf("requester not confirmed; inspect the task before retrying: %w", err)
	}
	task, err := taskAcknowledgement(raw, project, id)
	if err != nil {
		return nil, fmt.Errorf("requester resource not confirmed; inspect the task before retrying")
	}
	if err := requesterAcknowledgement(raw, target); err != nil {
		return nil, err
	}
	return task, nil
}

func requesterAcknowledgement(raw json.RawMessage, target *storage.RegisteredTargetIdentity) error {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return fmt.Errorf("requester not confirmed; inspect the task before retrying")
	}
	value, present := fields["requester"]
	if present {
		if target == nil && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil
		}
		var acknowledged *storage.RegisteredTargetIdentity
		if target != nil && json.Unmarshal(value, &acknowledged) == nil && acknowledged != nil && *acknowledged == *target {
			return nil
		}
	}
	return fmt.Errorf("requester not confirmed; inspect the task before retrying")
}
