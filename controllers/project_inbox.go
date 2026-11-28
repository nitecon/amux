package controllers

import (
	"context"

	"github.com/nitecon/amux/fism"
)

const projectInboxPluginID = "project-inbox"

// ProjectInboxStore reconciles deliverable project messages for bound native
// conversations. The storage implementation owns authority checks and the
// atomic command/confirmation write.
type ProjectInboxStore interface {
	ReconcileProjectInbox(context.Context, fism.Event) error
}

// ProjectInboxController scans durable project inbox obligations on the shared
// router clock, after presence has refreshed current native bindings.
func ProjectInboxController(store ProjectInboxStore) fism.Controller {
	return fism.Controller{
		Definition: fism.ControllerDefinition{
			ID: projectInboxPluginID,
			Subscriptions: []fism.Subscription{{
				Event: "clock.tick", MinVersion: 1, MaxVersion: 1,
				DependsOn: []string{presencePluginID},
			}},
		},
		Exec: []fism.Step{func(ctx context.Context, event fism.Event, _ *fism.StepContext) fism.Outcome {
			if store == nil {
				return fism.Outcome{Kind: fism.Fail, Error: "project inbox plugin store is nil"}
			}
			if err := store.ReconcileProjectInbox(ctx, event); err != nil {
				return fism.Outcome{Kind: fism.Fail, Error: err.Error()}
			}
			return fism.Outcome{Kind: fism.Continue}
		}},
	}
}
