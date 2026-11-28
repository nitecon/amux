package controllers

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/nitecon/amux/fism"
)

const (
	NotifyEventID       = "agent.notify"
	NotifyResultEventID = "agent.notify_result"
	GlobalNotifyEventID = "notification.global"
)

// NotificationEventDefinitions declares the durable agent notification contract.
func NotificationEventDefinitions() []fism.EventDefinition {
	return []fism.EventDefinition{
		{ID: NotifyEventID, Version: 1, RemoteEmit: false, Validate: validateNotification},
		{ID: NotifyResultEventID, Version: 1, RemoteEmit: true, Validate: validateNotificationResult},
		{ID: GlobalNotifyEventID, Version: 1, Validate: ValidateGlobalNotification},
	}
}

func decodeNotificationObject(payload json.RawMessage) (map[string]any, error) {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("invalid JSON object: %w", err)
	}
	if _, ok := value.(map[string]any); !ok {
		return nil, errors.New("payload must be a JSON object")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("payload must contain exactly one JSON value")
		}
		return nil, fmt.Errorf("invalid trailing JSON: %w", err)
	}
	return value.(map[string]any), nil
}

func validateNotification(payload json.RawMessage) error {
	object, err := decodeNotificationObject(payload)
	if err != nil {
		return err
	}
	target, targetOK := object["target"].(string)
	project, projectOK := object["project"].(string)
	if !targetOK || !projectOK {
		return errors.New("target and project must be strings")
	}
	mode, ok := object["mode"].(string)
	if !ok || (mode != "assigned" && mode != "recipient") {
		return errors.New("mode must be assigned or recipient")
	}
	event, ok := object["event"].(map[string]any)
	if !ok {
		return errors.New("event needs a kind and a task")
	}
	if _, ok := event["kind"].(string); !ok {
		return errors.New("event needs a kind and a task")
	}
	if _, ok := event["task"].(map[string]any); !ok {
		return errors.New("event needs a kind and a task")
	}
	if receiver, exists := object["receiver"]; exists {
		receiverObject, ok := receiver.(map[string]any)
		if !ok || receiverObject["session_id"] != target || receiverObject["project"] != project {
			return errors.New("receiver must identify the target execution and project")
		}
		if _, ok := receiverObject["instance_id"].(string); !ok {
			return errors.New("receiver must identify the target execution and project")
		}
	}
	if _, ok := object["require_idle"].(bool); !ok {
		return errors.New("require_idle must be a boolean")
	}
	if newTask, exists := object["new_task"]; exists {
		if _, ok := newTask.(bool); !ok {
			return errors.New("new_task must be a boolean when present")
		}
	}
	return nil
}

func validateNotificationResult(payload json.RawMessage) error {
	object, err := decodeNotificationObject(payload)
	if err != nil {
		return err
	}
	if _, ok := object["target"].(string); !ok {
		return errors.New("target must be a string")
	}
	status, ok := object["status"].(string)
	if !ok || (status != "delivered" && status != "skipped" && status != "failed") {
		return errors.New("status must be delivered, skipped or failed")
	}
	return nil
}

// ValidateGlobalNotification validates the message shared with gateway subscribers.
func ValidateGlobalNotification(payload json.RawMessage) error {
	object, err := decodeNotificationObject(payload)
	if err != nil {
		return err
	}
	message, ok := object["message"].(string)
	if !ok || strings.TrimSpace(message) == "" || len(message) > 4096 || len(object) != 1 {
		return errors.New("expected a message of 1 to 4096 bytes")
	}
	return nil
}
