package controllers

import (
	"context"

	"github.com/nitecon/amux/fism"
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
	GlobalNotifyEventID,
	"router.retrying",
}

// RemoteForwardedEvents returns the event IDs subscribed to by the remote
// forwarding plugin. The returned slice is independent of the package list.
func RemoteForwardedEvents() []string {
	return append([]string(nil), remoteForwardedEvents[:]...)
}

// RemoteController forwards durable router events to remote consumers. It snapshots
// and deduplicates the supplied allowlist so later caller mutation cannot alter
// its subscriptions.
func RemoteController(forwarded []string) fism.Controller {
	seen := make(map[string]struct{}, len(forwarded))
	subscriptions := make([]fism.Subscription, 0, len(forwarded))
	for _, event := range forwarded {
		if _, ok := seen[event]; ok {
			continue
		}
		seen[event] = struct{}{}
		subscriptions = append(subscriptions, fism.Subscription{
			Event: event, MinVersion: 1, MaxVersion: ^uint32(0),
		})
	}
	return fism.Controller{
		Definition: fism.ControllerDefinition{ID: remotePluginID, Subscriptions: subscriptions},
		Exec: []fism.Step{func(context.Context, fism.Event, *fism.StepContext) fism.Outcome {
			return fism.Outcome{Kind: fism.Continue}
		}},
	}
}
