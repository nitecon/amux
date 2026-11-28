package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/fism"
	"github.com/nitecon/amux/identity/agent"
)

var switchablePluginIDs = [...]string{"assignment", "notifier", "reminder", "review", "reclaim"}

var ErrUnknownPlugin = errors.New("unknown switchable plugin")

// SwitchablePluginIDs returns the fixed Rust-compatible API order.
func SwitchablePluginIDs() []string { return append([]string(nil), switchablePluginIDs[:]...) }

func isSwitchablePlugin(id string) bool {
	for _, known := range switchablePluginIDs {
		if id == known {
			return true
		}
	}
	return false
}

// PluginEnabled reports whether a supported native plugin is enabled. Plugins
// default to enabled; only the literal setting value "false" disables one.
func (s *Store) PluginEnabled(ctx context.Context, id string) (bool, error) {
	return pluginEnabled(ctx, s.db, id)
}

func pluginEnabled(ctx context.Context, q queryer, id string) (bool, error) {
	if !isSwitchablePlugin(id) {
		return false, fmt.Errorf("%w: %q", ErrUnknownPlugin, id)
	}
	var value string
	err := q.QueryRowContext(ctx, "SELECT value FROM settings WHERE key=?", "plugin."+id+".enabled").Scan(&value)
	if err == sql.ErrNoRows {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return value != "false", nil
}

// ReconcileRouterAssignments allocates from stored presence on a native clock
// tick. It deliberately does not reconcile presence or advance its clock.
func (s *Store) ReconcileRouterAssignments(ctx context.Context, event fism.Event) error {
	if event.ID != "clock.tick" || event.Version != 1 || !canonicalUUID(event.UID) {
		return fmt.Errorf("assignment reconciliation requires a clock.tick v1 event with a canonical UID")
	}
	return s.taskMutation(ctx, nil, func(tx *sql.Tx, _ *agent.Origin) error {
		return allocateAssignments(ctx, tx, event.EmittedAt)
	})
}

// ReconcileRouterAssignmentsWithRegistry is the runtime entry point. The
// registry is needed by the allocator's transaction-scoped offer emitter.
func (s *Store) ReconcileRouterAssignmentsWithRegistry(ctx context.Context, registry *fism.Registry, event fism.Event) error {
	if registry == nil {
		return fmt.Errorf("assignment reconciliation requires a router registry")
	}
	if !validAssignmentCause(event) {
		return fmt.Errorf("assignment reconciliation requires a supported v1 event with a canonical UID")
	}
	err := s.taskMutation(ctx, nil, func(tx *sql.Tx, _ *agent.Origin) error {
		return allocateAssignmentsWithOffers(ctx, tx, event.EmittedAt, registry, event)
	})
	if err == nil {
		_ = s.bus.Publish(eventbus.Event{Topic: eventbus.RouterChanged})
	}
	return err
}

func validAssignmentCause(event fism.Event) bool {
	switch event.ID {
	case "clock.tick", "agent.idle", "task.created", "task.unclaimed":
		return event.Version == 1 && canonicalUUID(event.UID)
	default:
		return false
	}
}
