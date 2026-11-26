package worksubmission

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"github.com/nitecon/amux/conversation"
)

// toolApprovalsKey stores user-granted tool approval rules in the shared
// preferences document as a list of patterns where "*" matches any text.
const toolApprovalsKey = "tool_approvals"
const directoryToolApprovalsKey = "directory_tool_approvals"

type DirectoryToolApproval struct {
	Directory string   `json:"directory"`
	Rules     []string `json:"rules"`
}

// shellControl lists characters that can chain, substitute or redirect shell
// commands. A request containing any of them never matches a rule, so
// "git grep *" cannot approve "git grep x; rm -rf ~".
const shellControl = ";&|<>`$\n\r"

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
	return decodeToolApprovals(root)
}

func decodeToolApprovals(root map[string]json.RawMessage) ([]string, error) {
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

// LoadDirectoryToolApprovals returns global rules and rules scoped to the exact
// cleaned absolute directory. Relative directories never authorize anything.
func LoadDirectoryToolApprovals(path, directory string) ([]string, error) {
	global, err := LoadToolApprovals(path)
	if err != nil {
		return nil, err
	}
	canonical, ok := canonicalApprovalDirectory(directory)
	if !ok || path == "" {
		return global, nil
	}
	root, err := readPreferencesRoot(path)
	if errors.Is(err, os.ErrNotExist) {
		return global, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read directory tool approvals: %w", err)
	}
	raw, exists := root[directoryToolApprovalsKey]
	if !exists {
		return global, nil
	}
	var entries []DirectoryToolApproval
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, fmt.Errorf("directory tool approvals must be a list: %w", err)
	}
	for _, entry := range entries {
		if entry.Directory == canonical {
			global = append(global, entry.Rules...)
		}
	}
	return global, nil
}

// AddDirectoryToolApproval saves a rule for one cleaned absolute directory.
func AddDirectoryToolApproval(path, directory, rule string) error {
	canonical, ok := canonicalApprovalDirectory(directory)
	if !ok {
		return errors.New("tool approval directory must be absolute")
	}
	return updateToolApproval(path, rule, directoryToolApprovalsKey, func(root map[string]json.RawMessage, _ []string, rule string) (any, bool, error) {
		var entries []DirectoryToolApproval
		if raw, exists := root[directoryToolApprovalsKey]; exists {
			if err := json.Unmarshal(raw, &entries); err != nil {
				return nil, false, fmt.Errorf("refusing to overwrite invalid directory tool approvals: %w", err)
			}
		}
		for i := range entries {
			if entries[i].Directory == canonical {
				if slices.Contains(entries[i].Rules, rule) {
					return nil, false, nil
				}
				entries[i].Rules = append(entries[i].Rules, rule)
				return entries, true, nil
			}
		}
		entries = append(entries, DirectoryToolApproval{Directory: canonical, Rules: []string{rule}})
		return entries, true, nil
	})
}

func canonicalApprovalDirectory(directory string) (string, bool) {
	if directory == "" || !filepath.IsAbs(directory) {
		return "", false
	}
	return filepath.Clean(directory), true
}

func validateToolApprovalRule(rule string) error {
	rule = strings.TrimSpace(rule)
	if strings.Trim(rule, "* \t") == "" {
		return errors.New("tool approval rule must name a command or tool")
	}
	if strings.ContainsAny(rule, shellControl) {
		return errors.New("tool approval rule cannot contain shell control characters")
	}
	return nil
}

// AddToolApproval saves one rule, preserving the other root fields. Only the
// rule's surrounding whitespace is trimmed; inner text stays literal. Saving an
// existing rule is a no-op.
func AddToolApproval(path, rule string) error {
	return updateToolApproval(path, rule, toolApprovalsKey, func(_ map[string]json.RawMessage, rules []string, rule string) (any, bool, error) {
		if slices.Contains(rules, rule) {
			return nil, false, nil
		}
		return append(rules, rule), true, nil
	})
}

// updateToolApproval validates path and rule, locks the shared preferences
// document, verifies existing tool approvals, and lets mutate compute the
// replacement value for key before atomically writing the preferences root.
func updateToolApproval(path, rule, key string, mutate func(root map[string]json.RawMessage, rules []string, rule string) (any, bool, error)) error {
	if path == "" {
		return errors.New("work preferences path is required")
	}
	rule = strings.TrimSpace(rule)
	if err := validateToolApprovalRule(rule); err != nil {
		return err
	}
	unlock, err := lockPreferences(path)
	if err != nil {
		return err
	}
	defer unlock()
	root, err := readPreferencesRoot(path)
	if errors.Is(err, os.ErrNotExist) {
		root = map[string]json.RawMessage{}
	} else if err != nil {
		return fmt.Errorf("read existing work preferences before update: %w", err)
	}
	rules, err := decodeToolApprovals(root)
	if err != nil {
		return fmt.Errorf("refusing to overwrite invalid tool approvals: %w", err)
	}
	value, changed, err := mutate(root, rules, rule)
	if err != nil || !changed {
		return err
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	root[key] = encoded
	return writePreferencesRoot(path, root)
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
	for _, candidate := range []string{data.Input.Command, stringValue(data.Command), commandValue(data.Command), stringValue(data.ToolCall.RawInput.Command), commandValue(data.ToolCall.RawInput.Command), data.ToolName, data.ToolCall.Name} {
		if strings.TrimSpace(candidate) != "" {
			return candidate
		}
	}
	return ""
}

func commandValue(raw json.RawMessage) string {
	var command []string
	if json.Unmarshal(raw, &command) == nil {
		return strings.Join(command, " ")
	}
	return ""
}

// ToolApprovalDirectory returns the permission request's actual working
// directory, falling back to the saved run directory when the provider omits it.
func ToolApprovalDirectory(event conversation.Event, fallback string) string {
	var data struct {
		Cwd   string `json:"cwd"`
		Input struct {
			Cwd string `json:"cwd"`
		} `json:"input"`
		ToolCall struct {
			RawInput struct {
				Cwd string `json:"cwd"`
			} `json:"rawInput"`
		} `json:"toolCall"`
	}
	if json.Unmarshal(event.Data, &data) == nil {
		for _, candidate := range []string{data.Cwd, data.Input.Cwd, data.ToolCall.RawInput.Cwd} {
			if candidate != "" {
				if canonical, ok := canonicalApprovalDirectory(candidate); ok {
					return canonical
				}
				return ""
			}
		}
	}
	if canonical, ok := canonicalApprovalDirectory(fallback); ok {
		return canonical
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

// ToolApprovalRuleDraft returns subject as a rule that matches only itself.
// Request text such as "git grep 'a*b'" keeps its "*" literal by escaping it
// as "\*" (and "\" as "\\"), so only a "*" the user types becomes a wildcard.
func ToolApprovalRuleDraft(subject string) string {
	return strings.NewReplacer(`\`, `\\`, `*`, `\*`).Replace(subject)
}

// MatchToolApproval reports whether subject matches any rule. An unescaped "*"
// matches any text, "\*" and "\\" match a literal "*" and "\", and every
// other character, including whitespace, is literal.
func MatchToolApproval(rules []string, subject string) bool {
	if strings.TrimSpace(subject) == "" || strings.ContainsAny(subject, shellControl) {
		return false
	}
	for _, rule := range rules {
		if strings.TrimSpace(rule) == "" || strings.ContainsAny(rule, shellControl) {
			continue
		}
		if regexp.MustCompile(rulePattern(rule)).MatchString(subject) {
			return true
		}
	}
	return false
}

func rulePattern(rule string) string {
	var pattern strings.Builder
	pattern.WriteString("^")
	for i := 0; i < len(rule); i++ {
		switch {
		case rule[i] == '\\' && i+1 < len(rule) && (rule[i+1] == '\\' || rule[i+1] == '*'):
			i++
			pattern.WriteString(regexp.QuoteMeta(rule[i : i+1]))
		case rule[i] == '*':
			pattern.WriteString(".*")
		default:
			pattern.WriteString(regexp.QuoteMeta(rule[i : i+1]))
		}
	}
	pattern.WriteString("$")
	return pattern.String()
}

func stringValue(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) != nil {
		return ""
	}
	return text
}

// writePreferencesKey atomically replaces one root key in the shared
// preferences document. Callers hold lockPreferences across their full
// read-modify-write.
func writePreferencesKey(path, key string, value json.RawMessage) error {
	root, err := readPreferencesRoot(path)
	if errors.Is(err, os.ErrNotExist) {
		root = map[string]json.RawMessage{}
	} else if err != nil {
		return fmt.Errorf("read existing work preferences before update: %w", err)
	}
	root[key] = value
	return writePreferencesRoot(path, root)
}

func writePreferencesRoot(path string, root map[string]json.RawMessage) error {
	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	return writePreferencesFile(path, data)
}
