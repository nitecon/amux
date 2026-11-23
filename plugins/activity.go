package plugins

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/nitecon/amux/router"
)

const (
	ClientActivityID = "client.activity"
	LegacyActivityID = "terminal.activity"
)

// ActivityEventDefinitions declares the remote activity event and its retained
// legacy alias. The alias remains registered for journal replay.
func ActivityEventDefinitions() []router.EventDefinition {
	return []router.EventDefinition{
		{ID: ClientActivityID, Version: 1, RemoteEmit: true, Validate: validateActivity},
		{ID: LegacyActivityID, Version: 1, RemoteEmit: true, Validate: validateActivity},
	}
}

func validateActivity(payload json.RawMessage) error {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	var object map[string]json.RawMessage
	if err := decoder.Decode(&object); err != nil || object == nil {
		return errors.New("activity payload must be a JSON object")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("activity payload must contain one JSON object")
	}
	for _, key := range []string{"session_id", "instance_id"} {
		var value string
		if raw, ok := object[key]; !ok || json.Unmarshal(raw, &value) != nil {
			return fmt.Errorf("activity %s must be a string", key)
		}
	}
	if raw, ok := object["output_at"]; ok && !bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		var value int64
		if json.Unmarshal(raw, &value) != nil {
			return errors.New("activity output_at must be an integer or null")
		}
	}
	var draft bool
	if raw, ok := object["draft"]; !ok || json.Unmarshal(raw, &draft) != nil {
		return errors.New("activity draft must be a boolean")
	}
	return nil
}
