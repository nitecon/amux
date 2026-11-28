package gatewayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/nitecon/amux/identity/agent"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/taskplatform"
)

// SetTaskOS changes the OS requirement only for the exact owner of an active
// task, and verifies the gateway atomically releases an incompatible owner.
func (c *Client) SetTaskOS(ctx context.Context, project, id string, requiredOS *string, actor agent.Origin, goal *string) (json.RawMessage, error) {
	if !canonicalDelegationID(id) {
		return nil, fmt.Errorf("task ID must be a canonical non-nil UUID")
	}
	osValue := ""
	if requiredOS != nil {
		osValue = *requiredOS
	}
	if _, err := taskplatform.LabelsWithOS(nil, osValue); err != nil {
		return nil, err
	}
	normalized, err := agent.NormalizeOrigin(&actor)
	if err != nil || normalized == nil {
		return nil, fmt.Errorf("task OS change requires a complete native actor")
	}
	actor = *normalized
	current, err := c.TaskDetail(ctx, project, id)
	if err != nil {
		return nil, err
	}
	if (current.Task.Status != "todo" && !storage.IsActiveTaskStatus(current.Task.Status)) || current.Task.OwnerOrigin == nil || *current.Task.OwnerOrigin != actor {
		return nil, fmt.Errorf("only the exact owning agent may change the OS requirement of an active task")
	}
	labels, err := taskplatform.LabelsWithOS(current.Task.Labels, osValue)
	if err != nil {
		return nil, err
	}
	encoded, _ := json.Marshal(labels)
	raw, err := c.TaskUpdate(ctx, project, id, storage.TaskWorkflowUpdate{TaskMetadataUpdate: storage.TaskMetadataUpdate{Labels: encoded}}, &actor, goal)
	if err != nil {
		return nil, fmt.Errorf("task OS change not confirmed; inspect the task before retrying: %w", err)
	}
	ack, err := taskAcknowledgement(raw, project, id)
	if err != nil || len(ack.Labels) != len(labels) {
		return nil, fmt.Errorf("task OS receipt did not confirm the requested labels; inspect before retrying")
	}
	for i := range labels {
		if ack.Labels[i] != labels[i] {
			return nil, fmt.Errorf("task OS receipt did not confirm the requested labels; inspect before retrying")
		}
	}
	if requiredOS != nil && *requiredOS != actor.OS {
		if ack.Status != "todo" || ack.OwnerAgentID != nil || ack.OwnerOrigin != nil || ack.StartedAt != nil || ack.DoneAt != nil {
			return nil, fmt.Errorf("OS handoff receipt did not confirm owner release; inspect before retrying")
		}
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) != nil {
			return nil, fmt.Errorf("OS handoff receipt did not confirm owner release; inspect before retrying")
		}
		for _, field := range []string{"owner_agent_id", "owner_origin", "started_at", "done_at"} {
			value, exists := fields[field]
			if !exists || !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
				return nil, fmt.Errorf("OS handoff receipt did not explicitly confirm cleared %s; inspect before retrying", field)
			}
		}
		var route struct {
			Mode string `json:"mode"`
		}
		if json.Unmarshal(ack.Routing, &route) != nil || (route.Mode != "auto" && route.Mode != "unassigned" && route.Mode != "broadcast") {
			return nil, fmt.Errorf("OS handoff receipt retained session routing; inspect before retrying")
		}
	}
	return raw, nil
}
