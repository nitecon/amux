package storage

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/nitecon/amux/plugins"
)

const nativeNotificationMaxBytes = 64 * 1024

// renderNativeNotification turns a public notification payload into a bounded
// prompt while preserving its immutable task and host snapshots.
func renderNativeNotification(payload json.RawMessage) (string, error) {
	definitions := plugins.NotificationEventDefinitions()
	if err := definitions[0].Validate(payload); err != nil {
		return "", fmt.Errorf("invalid native notification: %w", err)
	}
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return "", fmt.Errorf("decode native notification: %w", err)
	}
	var event map[string]json.RawMessage
	if err := json.Unmarshal(envelope["event"], &event); err != nil {
		return "", fmt.Errorf("decode native notification event: %w", err)
	}
	var task map[string]json.RawMessage
	if err := json.Unmarshal(event["task"], &task); err != nil {
		return "", fmt.Errorf("decode native notification task: %w", err)
	}

	project := rawString(envelope["project"])
	taskID := oneLine(rawString(task["id"]))
	if taskID == "" {
		taskID = "(unknown)"
	}
	if len(taskID) > 4096 {
		return "", errors.New("native notification task ID is too long")
	}

	var header strings.Builder
	header.WriteString("[amux notification]\n")
	writeMetadata(&header, "Heading", oneLine(rawString(envelope["heading"])), 512)
	writeMetadata(&header, "Project", oneLine(project), 512)
	writeMetadata(&header, "Task ID", taskID, 4096)
	writeMetadata(&header, "Title", oneLine(rawString(task["title"])), 1024)
	writeMetadata(&header, "Status", oneLine(rawString(task["status"])), 128)
	writeMetadata(&header, "Mode", oneLine(rawString(envelope["mode"])), 32)
	writeMetadata(&header, "Sender", oneLine(rawString(event["sender_name"])), 256)
	writeMetadata(&header, "Sender host snapshot", oneLine(rawString(event["sender_hostname"])), 512)
	writeMetadata(&header, "Owner host snapshot", oneLine(rawString(event["owner_hostname"])), 512)
	writeMetadata(&header, "Task host snapshot", oneLine(rawString(task["hostname"])), 512)
	writeMetadata(&header, "Comment author", oneLine(rawString(eventCommentField(event["comment"], "author"))), 256)

	instructions := oneLine(rawString(envelope["instructions"]))
	body := make([]string, 0, 5)
	for _, field := range []struct{ key, label string }{
		{"description", "Description"}, {"specification", "Specification"}, {"details", "Details"},
	} {
		if value := bodyText(rawString(task[field.key])); value != "" {
			body = append(body, field.label+":\n"+value)
		}
	}
	comment := bodyText(rawString(eventCommentField(event["comment"], "content")))
	if comment == "" {
		comment = bodyText(rawString(eventCommentField(event["comment"], "body")))
	}
	if comment != "" {
		body = append(body, "Comment:\n"+comment)
	}

	// Keep the complete identity and a bounded prefix of supplied instructions;
	// use the remaining space for task and comment text.
	const headerBudget = 12 * 1024
	headerText := boundedPrefix(header.String(), headerBudget)
	const instructionsBudget = 12 * 1024
	instructionText := ""
	if instructions != "" {
		instructionText = "Instructions:\n" + boundedPrefix(instructions, instructionsBudget) + "\n"
	}
	base := headerText + instructionText
	bodyText := strings.Join(body, "\n\n")
	if bodyText == "" {
		return fitNativeNotification(base, taskID), nil
	}
	separator := "\n"
	if !strings.HasSuffix(base, "\n") {
		separator = "\n\n"
	}
	available := nativeNotificationMaxBytes - len(base) - len(separator)
	if available <= 0 {
		return fitNativeNotification(base, taskID), nil
	}
	if len(bodyText) > available {
		cue := "\n\n[Text truncated; read task " + taskID + " for the full text.]"
		bodyBudget := available - len(cue)
		if bodyBudget <= 0 {
			return fitNativeNotification(base+cue, taskID), nil
		}
		bodyText = boundedPrefix(bodyText, bodyBudget) + cue
	}
	result := base + separator + bodyText
	return fitNativeNotification(result, taskID), nil
}

func rawString(raw json.RawMessage) string {
	var value string
	if len(raw) == 0 || json.Unmarshal(raw, &value) != nil {
		return ""
	}
	return value
}

func eventCommentField(raw json.RawMessage, field string) json.RawMessage {
	var comment map[string]json.RawMessage
	if json.Unmarshal(raw, &comment) != nil {
		return nil
	}
	return comment[field]
}

func oneLine(value string) string {
	var out strings.Builder
	lastSpace := false
	for _, r := range value {
		if unicode.IsControl(r) || unicode.IsSpace(r) {
			if out.Len() > 0 && !lastSpace {
				out.WriteByte(' ')
				lastSpace = true
			}
			continue
		}
		out.WriteRune(r)
		lastSpace = false
	}
	return strings.TrimSpace(out.String())
}

func bodyText(value string) string {
	return strings.ReplaceAll(value, "\x00", "")
}

func writeMetadata(out *strings.Builder, label, value string, maxBytes int) {
	if value == "" {
		return
	}
	value = boundedPrefix(value, maxBytes)
	out.WriteString(label)
	out.WriteString(": ")
	out.WriteString(value)
	out.WriteByte('\n')
}

func boundedPrefix(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	end := maxBytes
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end]
}

func fitNativeNotification(value, taskID string) string {
	if len(value) <= nativeNotificationMaxBytes && utf8.ValidString(value) {
		return value
	}
	cue := "\n[Text truncated; read task " + taskID + " for the full text.]"
	end := nativeNotificationMaxBytes - len(cue)
	if end < 0 {
		end = 0
	}
	for end > 0 && (!utf8.RuneStart(value[end]) || !utf8.ValidString(value[:end])) {
		end--
	}
	return strings.TrimRight(value[:end], "\n") + cue
}
