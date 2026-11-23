package desktop

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

func (b *Browser) refreshConversationLaunchControls() {
	if b.conversationProvider == nil || b.conversationModel == nil || b.conversationPersona == nil {
		return
	}
	provider := b.conversationProvider.Selected
	prefs := b.conversationLaunchPreferences(provider)
	b.updatingConversationLaunch = true
	if b.conversationModel.Text != prefs.Model {
		b.conversationModel.SetText(prefs.Model)
	}
	if b.conversationPersona.Text != prefs.Persona {
		b.conversationPersona.SetText(prefs.Persona)
	}
	b.updatingConversationLaunch = false
	if provider == "antigravity" {
		b.conversationModel.Disable()
		b.conversationPersona.Disable()
		b.conversationLaunchNote.SetText("Antigravity does not expose model or persona settings through the ACP controls used here.")
	} else if provider == "codex" {
		b.conversationModel.Enable()
		b.conversationPersona.Disable()
		b.conversationLaunchNote.SetText("Codex app-server supports a model override here; its launch interface does not expose agent persona selection.")
	} else {
		b.conversationModel.Enable()
		b.conversationPersona.Enable()
		b.conversationLaunchNote.SetText("Claude uses its model and agent launch options.")
	}
	b.conversationLaunchNote.Refresh()
}

func (b *Browser) saveCurrentConversationLaunchPreferences(model, persona string) {
	if b.conversationProvider == nil || b.updatingConversationLaunch {
		return
	}
	b.saveConversationLaunchPreferences(b.conversationProvider.Selected, ConversationLaunchPreferences{Model: model, Persona: persona})
}

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
	model, persona := strings.TrimSpace(prefs.Model), strings.TrimSpace(prefs.Persona)
	for _, value := range []string{model, persona} {
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
		return command, nil
	case "codex":
		command := []string{"codex", "app-server", "--listen", "stdio://"}
		if model != "" {
			command = append(command, "--config", "model="+strconv.Quote(model))
		}
		if persona != "" {
			return nil, fmt.Errorf("Codex app-server does not expose an agent persona option")
		}
		return command, nil
	case "antigravity":
		if model != "" || persona != "" {
			return nil, fmt.Errorf("this ACP provider does not expose model or persona preferences")
		}
		return nil, nil
	default:
		return nil, fmt.Errorf("unsupported provider %q", provider)
	}
}
