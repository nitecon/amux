package worksubmission

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/nitecon/amux/conversation"
)

// toolApprovalsKey stores user-granted tool approval rules in the shared
// preferences document as a list of patterns where "*" matches any text.
const toolApprovalsKey = "tool_approvals"

// shellControl lists characters that can chain, substitute or redirect shell
// commands. A request containing any of them never matches a rule, so
// "git grep *" cannot approve "git grep x; rm -rf ~".
const shellControl = ";&|<>`$\n\r"

// preferencesMu serializes read-modify-write updates of the shared preferences
// document within this process so concurrent saves do not discard each other.
var preferencesMu sync.Mutex

// LoadToolApprovals reads the saved tool approval rules. A missing file or key
// means no rules.
func LoadToolApprovals(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	root, err := readPreferencesRoot(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read tool approvals: %w", err)
	}
	raw, exists := root[toolApprovalsKey]
	if !exists {
		return nil, nil
	}
	var rules []string
	if err := json.Unmarshal(raw, &rules); err != nil {
		return nil, fmt.Errorf("tool approvals must be a list of strings: %w", err)
	}
	return rules, nil
}

// AddToolApproval saves one rule, preserving the other root fields. Only the
// rule's surrounding whitespace is trimmed; inner text stays literal. Saving an
// existing rule is a no-op.
func AddToolApproval(path, rule string) error {
	if path == "" {
		return errors.New("work preferences path is required")
	}
	rule = strings.TrimSpace(rule)
	if strings.Trim(rule, "* \t") == "" {
		return errors.New("tool approval rule must name a command or tool")
	}
	if strings.ContainsAny(rule, shellControl) {
		return errors.New("tool approval rule cannot contain shell control characters")
	}
	preferencesMu.Lock()
	defer preferencesMu.Unlock()
	rules, err := LoadToolApprovals(path)
	if err != nil {
		return fmt.Errorf("refusing to overwrite invalid tool approvals: %w", err)
	}
	if slices.Contains(rules, rule) {
		return nil
	}
	encoded, err := json.Marshal(append(rules, rule))
	if err != nil {
		return err
	}
	return writePreferencesKey(path, toolApprovalsKey, encoded)
}

// ToolApprovalSubject returns the text a rule is matched against: the shell
// command when the provider supplied one, otherwise the tool name. It is empty
// when the request names neither, and such requests are never auto-approved.
func ToolApprovalSubject(event conversation.Event) string {
	var data struct {
		ToolName string `json:"tool_name"` // Claude
		Input    struct {
			Command string `json:"command"`
		} `json:"input"`
		Command  json.RawMessage `json:"command"` // Codex
		ToolCall struct {        // ACP
			Name     string `json:"name"`
			RawInput struct {
				Command json.RawMessage `json:"command"`
			} `json:"rawInput"`
		} `json:"toolCall"`
	}
	if json.Unmarshal(event.Data, &data) != nil {
		return ""
	}
	for _, candidate := range []string{data.Input.Command, stringValue(data.Command), stringValue(data.ToolCall.RawInput.Command), data.ToolName, data.ToolCall.Name} {
		if strings.TrimSpace(candidate) != "" {
			return candidate
		}
	}
	return ""
}

// ToolApprovalOption returns the allow-once option to select when a saved rule
// matches the permission request, or "" when the user must decide.
func ToolApprovalOption(rules []string, event conversation.Event) string {
	if event.Kind != "permission" || !MatchToolApproval(rules, ToolApprovalSubject(event)) {
		return ""
	}
	for _, option := range event.Options {
		if option.Kind == "allow_once" {
			return option.ID
		}
	}
	return ""
}

// MatchToolApproval reports whether subject matches any rule. "*" matches any
// text and every other character, including whitespace, is literal.
func MatchToolApproval(rules []string, subject string) bool {
	if strings.TrimSpace(subject) == "" || strings.ContainsAny(subject, shellControl) {
		return false
	}
	for _, rule := range rules {
		if strings.TrimSpace(rule) == "" || strings.ContainsAny(rule, shellControl) {
			continue
		}
		pattern := "^" + strings.ReplaceAll(regexp.QuoteMeta(rule), `\*`, ".*") + "$"
		if regexp.MustCompile(pattern).MatchString(subject) {
			return true
		}
	}
	return false
}

func stringValue(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return ""
	}
	return text
}

// writePreferencesKey atomically replaces one root key in the shared
// preferences document. Callers hold preferencesMu across their full
// read-modify-write.
func writePreferencesKey(path, key string, value json.RawMessage) error {
	root, err := readPreferencesRoot(path)
	if errors.Is(err, os.ErrNotExist) {
		root = map[string]json.RawMessage{}
	} else if err != nil {
		return fmt.Errorf("read existing work preferences before update: %w", err)
	}
	root[key] = value
	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	return writePreferencesFile(path, data)
}
