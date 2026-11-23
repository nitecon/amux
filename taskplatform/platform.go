// Package taskplatform validates the canonical ordinary-task OS constraint.
// Server storage, assignment, notification and clients share these rules.
package taskplatform

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// ParseLabels validates labels at a JSON task boundary. Null or absent labels
// are unrestricted; non-string elements and noncanonical OS labels fail closed.
func ParseLabels(task []byte) ([]string, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(task, &fields); err != nil {
		return nil, fmt.Errorf("invalid task: %w", err)
	}
	if fields == nil {
		return nil, fmt.Errorf("task must be an object")
	}
	raw, ok := fields["labels"]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, nil
	}
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, fmt.Errorf("invalid task labels: %w", err)
	}
	labels := make([]string, 0, len(values))
	for _, value := range values {
		var label string
		if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, fmt.Errorf("task label must be a string")
		}
		if err := json.Unmarshal(value, &label); err != nil {
			return nil, fmt.Errorf("invalid task label: %w", err)
		}
		labels = append(labels, label)
	}
	if _, err := RequiredOS(labels); err != nil {
		return nil, err
	}
	return labels, nil
}

// RequiredOS returns an empty string for unrestricted tasks. Duplicate and
// noncanonical OS labels are rejected, including duplicates of the same OS.
func RequiredOS(labels []string) (string, error) {
	required := ""
	for _, label := range labels {
		if len(label) < 3 || !strings.EqualFold(label[:3], "os:") {
			continue
		}
		os := label[3:]
		if label[:3] != "os:" || !valid(os) {
			return "", fmt.Errorf("unsupported OS label %q: use os:windows, os:linux or os:macos", label)
		}
		if required != "" {
			return "", fmt.Errorf("a task may carry at most one os: label")
		}
		required = os
	}
	return required, nil
}

// Matches fails closed for invalid labels and unknown hosts on restricted tasks.
func Matches(labels []string, hostOS string) bool {
	required, err := RequiredOS(labels)
	return err == nil && (required == "" || required == hostOS)
}

// LabelsWithOS returns a copy with all prior OS labels replaced or removed.
func LabelsWithOS(labels []string, os string) ([]string, error) {
	if os != "" && !valid(os) {
		return nil, fmt.Errorf("unsupported task OS %q", os)
	}
	result := make([]string, 0, len(labels)+1)
	for _, label := range labels {
		if len(label) >= 3 && strings.EqualFold(label[:3], "os:") {
			continue
		}
		result = append(result, label)
	}
	if os != "" {
		result = append(result, "os:"+os)
	}
	return result, nil
}

func valid(os string) bool { return os == "windows" || os == "linux" || os == "macos" }
