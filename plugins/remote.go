package plugins

import (
	"context"

	"github.com/nitecon/amux/router"
)

const remotePluginID = "remote"

// remoteForwardedEvents matches the task and presence event definitions plus
// the two additional forwarded events in the Rust gateway.
var remoteForwardedEvents = [...]string{
	"task.created",
	"task.updated",
	"task.commented",
	"task.assigned",
	"task.started",
	"task.user_review",
	"task.completed",
	"task.cancelled",
	"task.unclaimed",
	"agent.online",
	"agent.idle",
	"agent.busy",
	"agent.gone",
	NotifyEventID,
	"router.retrying",
}

// RemoteForwardedEvents returns the event IDs subscribed to by the remote
// forwarding plugin. The returned slice is independent of the package list.
func RemoteForwardedEvents() []string {
	return append([]string(nil), remoteForwardedEvents[:]...)
}

// RemotePlugin forwards durable router events to remote consumers. It snapshots
// and deduplicates the supplied allowlist so later caller mutation cannot alter
// its subscriptions.
func RemotePlugin(forwarded []string) router.Plugin {
	seen := make(map[string]struct{}, len(forwarded))
	subscriptions := make([]router.Subscription, 0, len(forwarded))
	for _, event := range forwarded {
		if _, ok := seen[event]; ok {
			continue
		}
		seen[event] = struct{}{}
		subscriptions = append(subscriptions, router.Subscription{
			Event: event, MinVersion: 1, MaxVersion: ^uint32(0),
		})
	}
	return router.Plugin{
		Definition: router.PluginDefinition{ID: remotePluginID, Subscriptions: subscriptions},
		Exec: []router.Step{func(context.Context, router.Event, *router.StepContext) router.Outcome {
			return router.Outcome{Kind: router.Continue}
		}},
	}
}
