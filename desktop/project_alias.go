package desktop

import "strings"

func (b *Browser) projectSearchAliases() map[string][]string {
	aliases := make(map[string][]string)
	for _, workspaceID := range b.workspaceIDs {
		selection := b.prefs.Selections[workspaceSelectionKey(b.prefKey, workspaceID)]
		if workspaceID == b.activeWorkspaceID {
			selection = b.selection
		}
		name := strings.TrimSpace(selection.WorkspaceName)
		if selection.Project != "" && name != "" {
			aliases[selection.Project] = append(aliases[selection.Project], name)
		}
	}
	return aliases
}
