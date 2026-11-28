package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/fism"
)

const (
	ClientHeartbeatID   = "client.heartbeat"
	heartbeatSourceName = "heartbeat"
)

// ClientHeartbeatDefinition declares the remote client inventory event.
func ClientHeartbeatDefinition() fism.EventDefinition {
	return fism.EventDefinition{ID: ClientHeartbeatID, Version: 1, RemoteEmit: true, Validate: validateClientHeartbeat}
}

// HeartbeatDrainer reconciles durable native heartbeat reports into the router.
type HeartbeatDrainer interface {
	DrainRouterHeartbeatOutbox(context.Context, *fism.Registry, int) (int, error)
}

// RunHeartbeatSource drains startup heartbeat state and wakes on native
// presence changes or shared clock notifications. The shared source loop owns
// the only retry timer; this adapter adds no polling schedule.
func RunHeartbeatSource(ctx context.Context, drainer HeartbeatDrainer, registry *fism.Registry, bus *eventbus.Bus) error {
	if ctx == nil || drainer == nil || registry == nil || bus == nil {
		return errors.New("heartbeat source requires context, drainer, registry, and event bus")
	}
	return runReconciliationSource(ctx, registry, bus, heartbeatSourceName, drainer.DrainRouterHeartbeatOutbox, eventbus.PresenceChanged, eventbus.ObservationAccepted)
}

func validateClientHeartbeat(payload json.RawMessage) error {
	var object map[string]json.RawMessage
	if err := json.Unmarshal(payload, &object); err != nil || object == nil {
		return errors.New("payload must be a JSON object")
	}
	if raw, exists := object["client_run"]; exists && !isJSONNull(raw) {
		var run string
		if json.Unmarshal(raw, &run) != nil {
			return errors.New("client_run must be a UUID when present")
		}
		if _, err := uuid.Parse(run); err != nil {
			return errors.New("client_run must be a UUID when present")
		}
	}
	if raw, exists := object["os"]; exists && !isJSONNull(raw) {
		var os string
		if json.Unmarshal(raw, &os) != nil || (os != "linux" && os != "windows" && os != "macos") {
			return errors.New("client os must be linux, windows or macos when present")
		}
	}
	if raw, exists := object["hostname"]; exists && !isJSONNull(raw) {
		var hostname string
		if json.Unmarshal(raw, &hostname) != nil || len(hostname) > 255 {
			return errors.New("client hostname must be a string of at most 255 bytes when present")
		}
	}
	rawAgents, exists := object["agents"]
	if !exists {
		return errors.New("agents must be a list")
	}
	var agents []json.RawMessage
	if err := json.Unmarshal(rawAgents, &agents); err != nil || agents == nil {
		return errors.New("agents must be a list")
	}
	for index, rawAgent := range agents {
		var agent map[string]json.RawMessage
		if err := json.Unmarshal(rawAgent, &agent); err != nil || agent == nil {
			return fmt.Errorf("agent %d must be an object", index)
		}
		for _, field := range []string{"session_id", "instance_id", "provider", "os", "project"} {
			if !jsonString(agent[field]) {
				return fmt.Errorf("agent %s must be a string", field)
			}
		}
		for _, field := range []string{"name", "goal", "agent_id", "generation"} {
			if raw, exists := agent[field]; exists && !isJSONNull(raw) && !jsonString(raw) {
				return fmt.Errorf("agent %s must be a string when present", field)
			}
		}
	}
	return nil
}

func jsonString(raw json.RawMessage) bool {
	var value string
	return len(raw) > 0 && !isJSONNull(raw) && json.Unmarshal(raw, &value) == nil
}

func isJSONNull(raw json.RawMessage) bool { return bytes.Equal(bytes.TrimSpace(raw), []byte("null")) }
