package plugins

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/nitecon/amux/router"
)

const assignmentPluginID = "assignment"

// AssignmentFeedback is the validated agent.notify_result v1 payload.
type AssignmentFeedback struct {
	Target string `json:"target"`
	Status string `json:"status"`
}

// DecodeAssignmentFeedback applies the public event contract before decoding
// fields, keeping callback behavior aligned with router event validation.
func DecodeAssignmentFeedback(payload json.RawMessage) (AssignmentFeedback, error) {
	defs := NotificationEventDefinitions()
	var validate func(json.RawMessage) error
	for _, definition := range defs {
		if definition.ID == NotifyResultEventID && definition.Version == 1 {
			validate = definition.Validate
			break
		}
	}
	if validate == nil {
		return AssignmentFeedback{}, fmt.Errorf("agent.notify_result v1 validator is not registered")
	}
	if err := validate(payload); err != nil {
		return AssignmentFeedback{}, fmt.Errorf("invalid assignment feedback: %w", err)
	}
	var result AssignmentFeedback
	if err := json.Unmarshal(payload, &result); err != nil {
		return AssignmentFeedback{}, fmt.Errorf("decode assignment feedback: %w", err)
	}
	return result, nil
}

// AssignmentStore provides the settings check and native clock allocation
// operation consumed by the assignment plugin.
type AssignmentStore interface {
	PluginEnabled(context.Context, string) (bool, error)
	ReconcileRouterAssignmentsWithRegistry(context.Context, *router.Registry, router.Event) error
	ReconcileRouterAssignmentFeedback(context.Context, router.Event) error
}

// AssignmentPlugin allocates work when an agent becomes idle and reconciles
// pending work on clock events after presence has reconciled.
func AssignmentPlugin(store AssignmentStore) router.Plugin {
	return router.Plugin{
		Definition: router.PluginDefinition{
			ID: assignmentPluginID,
			Subscriptions: []router.Subscription{
				{Event: "clock.tick", MinVersion: 1, MaxVersion: 1, DependsOn: []string{"presence"}},
				{Event: "agent.idle", MinVersion: 1, MaxVersion: 1},
				{Event: "task.created", MinVersion: 1, MaxVersion: 1},
				{Event: "task.unclaimed", MinVersion: 1, MaxVersion: 1},
				{Event: NotifyResultEventID, MinVersion: 1, MaxVersion: 1},
			},
		},
		PreExec: []router.Step{enabledStep(store, assignmentPluginID, "assignment plugin store is nil")},
		Exec: []router.Step{func(ctx context.Context, event router.Event, step *router.StepContext) router.Outcome {
			if store == nil {
				return router.Outcome{Kind: router.Fail, Error: "assignment plugin store is nil"}
			}
			var err error
			switch event.ID {
			case NotifyResultEventID:
				err = store.ReconcileRouterAssignmentFeedback(ctx, event)
			case "clock.tick", "agent.idle", "task.created", "task.unclaimed":
				if step == nil || step.Registry() == nil {
					return router.Outcome{Kind: router.Fail, Error: "assignment plugin registry is nil"}
				}
				err = store.ReconcileRouterAssignmentsWithRegistry(ctx, step.Registry(), event)
			default:
				return router.Outcome{Kind: router.Stop}
			}
			if err != nil {
				return router.Outcome{Kind: router.Fail, Error: err.Error()}
			}
			return router.Outcome{Kind: router.Continue}
		}},
	}
}
