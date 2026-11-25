package desktop

import "github.com/nitecon/amux/internal/localconversation"

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

var providerLaunchCommand = localconversation.LaunchCommand
