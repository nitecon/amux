package router

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"sort"
)

const retryingEventID = "router.retrying"

// EventDefinition declares one event payload version.
type EventDefinition struct {
	ID         string
	Version    uint32
	RemoteEmit bool
	Validate   func(json.RawMessage) error
}

// Subscription declares the event versions a plugin accepts and the plugins
// that must complete first for those event instances.
type Subscription struct {
	Event      string
	MinVersion uint32
	MaxVersion uint32
	DependsOn  []string
}

// PluginDefinition declares a plugin and its event subscriptions.
type PluginDefinition struct {
	ID            string
	Subscriptions []Subscription
	FIFO          bool
	// MaxAttempts counts failed runs, including the first; zero retries forever.
	MaxAttempts uint32
}

// Registry is an immutable index of event definitions and plugin subscriptions.
type Registry struct {
	events      map[eventKey]EventDefinition
	plugins     map[string]map[string][]string
	subscribers map[string][]registeredSubscription
	policies    map[string]PluginDefinition
}

type eventKey struct {
	id      string
	version uint32
}

type registeredSubscription struct {
	plugin string
	min    uint32
	max    uint32
}

// NewRegistry validates and indexes event and plugin declarations. The built-in
// router.retrying v1 object event is always registered and cannot be replaced.
func NewRegistry(events []EventDefinition, plugins []PluginDefinition) (*Registry, error) {
	r := &Registry{
		events:      make(map[eventKey]EventDefinition, len(events)+1),
		plugins:     make(map[string]map[string][]string, len(plugins)),
		subscribers: make(map[string][]registeredSubscription),
		policies:    make(map[string]PluginDefinition),
	}
	allEvents := make([]EventDefinition, 0, len(events)+1)
	allEvents = append(allEvents, EventDefinition{ID: retryingEventID, Version: 1, Validate: Object})
	allEvents = append(allEvents, events...)
	for _, def := range allEvents {
		key := eventKey{id: def.ID, version: def.Version}
		if _, exists := r.events[key]; exists {
			return nil, fmt.Errorf("event %q v%d is declared twice", def.ID, def.Version)
		}
		if def.Validate == nil {
			return nil, fmt.Errorf("event %q v%d has no validator", def.ID, def.Version)
		}
		// Values contain only immutable strings, numbers and a function pointer.
		r.events[key] = def
	}

	for _, plugin := range plugins {
		if _, exists := r.plugins[plugin.ID]; exists {
			return nil, fmt.Errorf("plugin %q is registered twice", plugin.ID)
		}
		depsByEvent := make(map[string][]string, len(plugin.Subscriptions))
		for _, sub := range plugin.Subscriptions {
			if sub.MinVersion > sub.MaxVersion {
				return nil, fmt.Errorf("plugin %q has invalid version range for event %q", plugin.ID, sub.Event)
			}
			if _, exists := depsByEvent[sub.Event]; exists {
				return nil, fmt.Errorf("plugin %q subscribes to event %q twice", plugin.ID, sub.Event)
			}
			matched := false
			for key := range r.events {
				if key.id == sub.Event && key.version >= sub.MinVersion && key.version <= sub.MaxVersion {
					matched = true
					break
				}
			}
			if !matched {
				return nil, fmt.Errorf("plugin %q subscribes to no declared version of event %q", plugin.ID, sub.Event)
			}
			deps := append([]string(nil), sub.DependsOn...)
			depsByEvent[sub.Event] = deps
			r.subscribers[sub.Event] = append(r.subscribers[sub.Event], registeredSubscription{
				plugin: plugin.ID,
				min:    sub.MinVersion,
				max:    sub.MaxVersion,
			})
		}
		r.plugins[plugin.ID] = depsByEvent
		r.policies[plugin.ID] = PluginDefinition{ID: plugin.ID, FIFO: plugin.FIFO, MaxAttempts: plugin.MaxAttempts}
	}

	for event, subscribers := range r.subscribers {
		versions := make(map[uint32]struct{})
		for key := range r.events {
			if key.id == event {
				versions[key.version] = struct{}{}
			}
		}
		for version := range versions {
			active := make(map[string]struct{})
			for _, sub := range subscribers {
				if version >= sub.min && version <= sub.max {
					active[sub.plugin] = struct{}{}
				}
			}
			for plugin := range active {
				for _, dependency := range r.plugins[plugin][event] {
					if _, exists := active[dependency]; !exists {
						return nil, fmt.Errorf("plugin %q depends on %q, which does not subscribe to %q v%d", plugin, dependency, event, version)
					}
				}
			}
			state := make(map[string]uint8, len(active))
			var visit func(string) error
			visit = func(plugin string) error {
				switch state[plugin] {
				case 1:
					return fmt.Errorf("depends_on cycle on %q v%d at plugin %q", event, version, plugin)
				case 2:
					return nil
				}
				state[plugin] = 1
				for _, dependency := range r.plugins[plugin][event] {
					if err := visit(dependency); err != nil {
						return err
					}
				}
				state[plugin] = 2
				return nil
			}
			for plugin := range active {
				if err := visit(plugin); err != nil {
					return nil, err
				}
			}
		}
	}
	for event := range r.subscribers {
		sort.Slice(r.subscribers[event], func(i, j int) bool {
			return r.subscribers[event][i].plugin < r.subscribers[event][j].plugin
		})
	}
	return r, nil
}

// Object accepts exactly one valid JSON object.
func Object(payload json.RawMessage) error {
	dec := json.NewDecoder(bytes.NewReader(payload))
	var object map[string]json.RawMessage
	if err := dec.Decode(&object); err != nil || object == nil {
		return fmt.Errorf("payload must be a JSON object")
	}
	if err := ensureJSONEnd(dec); err != nil {
		return fmt.Errorf("payload must be one JSON value")
	}
	return nil
}

// Validate verifies that the event version exists and its payload is valid.
func (r *Registry) Validate(id string, version uint32, payload json.RawMessage) error {
	def, ok := r.events[eventKey{id: id, version: version}]
	if !ok {
		return fmt.Errorf("%w: event %q v%d is not declared", ErrInvalidEvent, id, version)
	}
	dec := json.NewDecoder(bytes.NewReader(payload))
	var value json.RawMessage
	if err := dec.Decode(&value); err != nil {
		return fmt.Errorf("%w: event %q v%d payload is not valid JSON: %v", ErrInvalidEvent, id, version, err)
	}
	if err := ensureJSONEnd(dec); err != nil {
		return fmt.Errorf("%w: event %q v%d payload has trailing JSON", ErrInvalidEvent, id, version)
	}
	if err := def.Validate(payload); err != nil {
		return fmt.Errorf("%w: event %q v%d: %v", ErrInvalidEvent, id, version, err)
	}
	return nil
}

func ensureJSONEnd(dec *json.Decoder) error {
	var extra json.RawMessage
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON value")
		}
		return err
	}
	return nil
}

// Subscribers returns the sorted plugin IDs subscribed to an event version.
func (r *Registry) Subscribers(id string, version uint32) []string {
	var result []string
	for _, sub := range r.subscribers[id] {
		if version >= sub.min && version <= sub.max {
			result = append(result, sub.plugin)
		}
	}
	return result
}

// Subscribes reports whether plugin accepts this event version.
func (r *Registry) Subscribes(plugin, id string, version uint32) bool {
	for _, subscriber := range r.subscribers[id] {
		if subscriber.plugin == plugin && version >= subscriber.min && version <= subscriber.max {
			return true
		}
	}
	return false
}

// AcceptsRemote reports whether remote clients may emit this event version.
func (r *Registry) AcceptsRemote(id string, version uint32) bool {
	def, ok := r.events[eventKey{id: id, version: version}]
	return ok && def.RemoteEmit
}

// Dependencies returns a copy of the plugin's dependency list for an event.
func (r *Registry) Dependencies(plugin, event string) []string {
	return append([]string(nil), r.plugins[plugin][event]...)
}

// PluginIDs returns a stable copy of all registered plugin identities.
func (r *Registry) PluginIDs() []string {
	ids := make([]string, 0, len(r.plugins))
	for id := range r.plugins {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
func (r *Registry) FIFO(plugin string) bool          { return r.policies[plugin].FIFO }
func (r *Registry) MaxAttempts(plugin string) uint32 { return r.policies[plugin].MaxAttempts }
