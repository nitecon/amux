package desktop

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

func conversationLaunchKey(workspace, provider string) string {
	return workspace + "\x00provider\x00" + provider
}

func (b *Browser) conversationLaunchPreferences(provider string) ConversationLaunchPreferences {
	return b.prefs.ConversationLaunch[conversationLaunchKey(workspaceSelectionKey(b.prefKey, b.activeWorkspaceID), provider)]
}

func (b *Browser) saveConversationLaunchPreferences(provider string, prefs ConversationLaunchPreferences) {
	if b.prefs.ConversationLaunch == nil {
		b.prefs.ConversationLaunch = make(map[string]ConversationLaunchPreferences)
	}
	b.prefs.ConversationLaunch[conversationLaunchKey(workspaceSelectionKey(b.prefKey, b.activeWorkspaceID), provider)] = prefs
	b.persist()
	if b.workerPanel != nil {
		b.workerPanel.refreshLaunchSummary()
	}
}

// providerLaunchCommand returns only arguments supported by the provider's
// current native launcher. ACP providers expose configuration through ACP
// options, which this client does not negotiate yet.
func providerLaunchCommand(provider string, prefs ConversationLaunchPreferences) ([]string, error) {
	model, persona, effort := strings.TrimSpace(prefs.Model), strings.TrimSpace(prefs.Persona), strings.TrimSpace(prefs.Effort)
	for _, value := range []string{model, persona, effort} {
		if len(value) > 4096 || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return nil, fmt.Errorf("provider launch preference is too long or contains control characters")
		}
	}
	switch provider {
	case "claude":
		command := []string{"claude"}
		if model != "" {
			command = append(command, "--model", model)
		}
		if persona != "" {
			command = append(command, "--agent", persona)
		}
		if effort != "" {
			command = append(command, "--effort", effort)
		}
		return command, nil
	case "codex":
		command := []string{"codex", "app-server", "--listen", "stdio://"}
		if model != "" {
			command = append(command, "--config", "model="+strconv.Quote(model))
		}
		if effort != "" {
			command = append(command, "--config", "model_reasoning_effort="+strconv.Quote(effort))
		}
		if persona != "" {
			return nil, fmt.Errorf("Codex app-server does not expose an agent persona option")
		}
		return command, nil
	case "antigravity":
		if model != "" || persona != "" || effort != "" {
			return nil, fmt.Errorf("this ACP provider does not expose model or persona preferences")
		}
		return nil, nil
	default:
		return nil, fmt.Errorf("unsupported provider %q", provider)
	}
}
