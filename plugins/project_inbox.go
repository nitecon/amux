package plugins

import (
	"context"

	"github.com/nitecon/amux/router"
)

const projectInboxPluginID = "project-inbox"

// ProjectInboxStore reconciles deliverable project messages for bound native
// conversations. The storage implementation owns authority checks and the
// atomic command/confirmation write.
type ProjectInboxStore interface {
	ReconcileProjectInbox(context.Context, router.Event) error
}

// ProjectInboxPlugin scans durable project inbox obligations on the shared
// router clock, after presence has refreshed current native bindings.
func ProjectInboxPlugin(store ProjectInboxStore) router.Plugin {
	return router.Plugin{
		Definition: router.PluginDefinition{
			ID: projectInboxPluginID,
			Subscriptions: []router.Subscription{{
				Event: "clock.tick", MinVersion: 1, MaxVersion: 1,
				DependsOn: []string{presencePluginID},
			}},
		},
		Exec: []router.Step{func(ctx context.Context, event router.Event, _ *router.StepContext) router.Outcome {
			if store == nil {
				return router.Outcome{Kind: router.Fail, Error: "project inbox plugin store is nil"}
			}
			if err := store.ReconcileProjectInbox(ctx, event); err != nil {
				return router.Outcome{Kind: router.Fail, Error: err.Error()}
			}
			return router.Outcome{Kind: router.Continue}
		}},
	}
}
