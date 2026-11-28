package controllers

import (
	"encoding/json"
	"testing"
)

func TestNotificationEventDefinitions(t *testing.T) {
	definitions := NotificationEventDefinitions()
	if len(definitions) != 3 {
		t.Fatalf("got %d definitions, want 3", len(definitions))
	}
	want := []struct {
		id         string
		remoteEmit bool
	}{
		{NotifyEventID, false},
		{NotifyResultEventID, true},
		{GlobalNotifyEventID, false},
	}
	for i, definition := range definitions {
		if definition.ID != want[i].id || definition.Version != 1 || definition.RemoteEmit != want[i].remoteEmit || definition.Validate == nil {
			t.Errorf("definition %d = %+v, want id %q v1 remote=%t with validator", i, definition, want[i].id, want[i].remoteEmit)
		}
	}
}

func TestValidateNotification(t *testing.T) {
	valid := `{"target":"session","project":"project","mode":"assigned","event":{"kind":"task_created","task":{}},"require_idle":true}`
	tests := []struct {
		name    string
		payload string
		valid   bool
	}{
		{"assigned minimal", valid, true},
		{"recipient with receiver and optional values", `{"target":"s","project":"p","mode":"recipient","event":{"kind":"task_commented","task":{"id":"t"}},"receiver":{"session_id":"s","instance_id":"i","project":"p"},"require_idle":false,"new_task":true,"heading":null,"instructions":{"free":"form"},"extra":1}`, true},
		{"null target", `{"target":null,"project":"p","mode":"assigned","event":{"kind":"k","task":{}},"require_idle":true}`, false},
		{"wrong target type", `{"target":1,"project":"p","mode":"assigned","event":{"kind":"k","task":{}},"require_idle":true}`, false},
		{"null project", `{"target":"s","project":null,"mode":"assigned","event":{"kind":"k","task":{}},"require_idle":true}`, false},
		{"missing mode", `{"target":"s","project":"p","event":{"kind":"k","task":{}},"require_idle":true}`, false},
		{"invalid mode", `{"target":"s","project":"p","mode":"other","event":{"kind":"k","task":{}},"require_idle":true}`, false},
		{"null event kind", `{"target":"s","project":"p","mode":"assigned","event":{"kind":null,"task":{}},"require_idle":true}`, false},
		{"event task must be object", `{"target":"s","project":"p","mode":"assigned","event":{"kind":"k","task":null},"require_idle":true}`, false},
		{"event must be object", `{"target":"s","project":"p","mode":"assigned","event":null,"require_idle":true}`, false},
		{"missing require_idle", `{"target":"s","project":"p","mode":"assigned","event":{"kind":"k","task":{} }}`, false},
		{"null require_idle", `{"target":"s","project":"p","mode":"assigned","event":{"kind":"k","task":{}},"require_idle":null}`, false},
		{"wrong require_idle type", `{"target":"s","project":"p","mode":"assigned","event":{"kind":"k","task":{}},"require_idle":"true"}`, false},
		{"null new_task", `{"target":"s","project":"p","mode":"assigned","event":{"kind":"k","task":{}},"require_idle":true,"new_task":null}`, false},
		{"wrong new_task type", `{"target":"s","project":"p","mode":"assigned","event":{"kind":"k","task":{}},"require_idle":true,"new_task":0}`, false},
		{"receiver null", `{"target":"s","project":"p","mode":"assigned","event":{"kind":"k","task":{}},"receiver":null,"require_idle":true}`, false},
		{"receiver session mismatch", `{"target":"s","project":"p","mode":"assigned","event":{"kind":"k","task":{}},"receiver":{"session_id":"other","instance_id":"i","project":"p"},"require_idle":true}`, false},
		{"receiver project mismatch", `{"target":"s","project":"p","mode":"assigned","event":{"kind":"k","task":{}},"receiver":{"session_id":"s","instance_id":"i","project":"other"},"require_idle":true}`, false},
		{"receiver instance null", `{"target":"s","project":"p","mode":"assigned","event":{"kind":"k","task":{}},"receiver":{"session_id":"s","instance_id":null,"project":"p"},"require_idle":true}`, false},
		{"heading and instructions unrestricted", `{"target":"s","project":"p","mode":"assigned","event":{"kind":"k","task":{}},"require_idle":true,"heading":false,"instructions":null}`, true},
		{"array top level", `[]`, false},
		{"null top level", `null`, false},
		{"trailing value", valid + ` {}`, false},
		{"malformed trailing data", valid + ` garbage`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateNotification(json.RawMessage(tt.payload))
			if (err == nil) != tt.valid {
				t.Fatalf("validateNotification(%s) error = %v, want valid=%t", tt.payload, err, tt.valid)
			}
		})
	}
}

func TestValidateNotificationResult(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		valid   bool
	}{
		{"delivered", `{"target":"s","status":"delivered"}`, true},
		{"skipped with extra data", `{"target":"s","status":"skipped","reason":null}`, true},
		{"failed", `{"target":"s","status":"failed"}`, true},
		{"null target", `{"target":null,"status":"delivered"}`, false},
		{"wrong target type", `{"target":false,"status":"delivered"}`, false},
		{"missing status", `{"target":"s"}`, false},
		{"null status", `{"target":"s","status":null}`, false},
		{"invalid status", `{"target":"s","status":"queued"}`, false},
		{"object top level", `{}`, false},
		{"trailing value", `{"target":"s","status":"failed"} false`, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateNotificationResult(json.RawMessage(tt.payload))
			if (err == nil) != tt.valid {
				t.Fatalf("validateNotificationResult(%s) error = %v, want valid=%t", tt.payload, err, tt.valid)
			}
		})
	}
}
