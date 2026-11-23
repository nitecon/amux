package plugins

import (
	"encoding/json"
	"testing"
)

func TestActivityEventDefinitions(t *testing.T) {
	defs := ActivityEventDefinitions()
	if len(defs) != 2 || defs[0].ID != ClientActivityID || defs[1].ID != LegacyActivityID {
		t.Fatalf("activity definitions = %+v", defs)
	}
	for _, def := range defs {
		if def.Version != 1 || !def.RemoteEmit {
			t.Errorf("activity definition is not remotely emittable v1: %+v", def)
		}
		if err := def.Validate(json.RawMessage(`{"session_id":"s","instance_id":"i","output_at":42,"draft":true,"unknown":9007199254740993}`)); err != nil {
			t.Errorf("valid activity rejected: %v", err)
		}
		if err := def.Validate(json.RawMessage(`{"session_id":"","instance_id":"","output_at":-7,"draft":false}`)); err != nil {
			t.Errorf("valid empty legacy identity/activity rejected: %v", err)
		}
		for _, payload := range []string{`[]`, `{}`, `{"session_id":"s"}`, `{"session_id":"s","instance_id":"i","draft":1}`, `{"session_id":"s","instance_id":"i","output_at":1}`, `{"session_id":"s","instance_id":"i","output_at":null,"draft":true} trailing`} {
			if err := def.Validate(json.RawMessage(payload)); err == nil {
				t.Errorf("invalid activity accepted: %s", payload)
			}
		}
	}
}
