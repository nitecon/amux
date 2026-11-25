package desktop

import (
	"context"

	"github.com/nitecon/amux/storage"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/widget"
	"github.com/nitecon/amux/internal/localconversation"
)

type localChat = localconversation.Session

var (
	createMarkdownChat      = localconversation.Create
	listMarkdownChats       = localconversation.List
	openMarkdownChat        = localconversation.Open
	readMarkdownChat        = localconversation.Read
	localProviderExecutable = localconversation.ProviderExecutable
)

func (b *Browser) createLocalConversation(project, directory, provider, replace string) {
	prefs := b.conversationLaunchPreferences(provider)
	_, err := providerLaunchCommand(provider, prefs)
	if err != nil {
		b.newConversationStatus.SetText(err.Error())
		return
	}
	if directory == "" && b.clientConfigPath == "" {
		for _, item := range b.projectItems {
			if item.Ident == project {
				directory = b.projectCheckout(item).Directory
				break
			}
		}
	}
	if directory == "" && b.clientConfigPath == "" {
		b.showLocalChatOpenError("Check out this project to start a local chat.")
		return
	}
	key := chatKey(project, provider)
	if b.openingDefaults[key] {
		return
	}
	if b.openingDefaults == nil {
		b.openingDefaults = make(map[string]bool)
	}
	b.openingDefaults[key] = true
	selection, workspace := b.conversationSelection, b.activeWorkspaceID
	if !b.launch(func(ctx context.Context) {
		var info storage.Conversation
		var err error
		if b.clientConfigPath == "" {
			info, err = createMarkdownChat(b.localConversationsDirectory, project, directory, provider, prefs)
		} else {
			info, err = (localconversation.Remote{ConfigPath: b.clientConfigPath}).Create(ctx, project, provider, replace, prefs)
		}
		b.dispatch(func() {
			delete(b.openingDefaults, key)
			if err == nil && replace != "" {
				if local := b.localChatSessions[chatKey(project, replace)]; local != nil {
					go local.Stop()
				}
			}
			if project != b.selectedProject || workspace != b.activeWorkspaceID {
				return
			}
			if err != nil {
				if selection == b.conversationSelection && b.selection.ProjectConversation == "" {
					b.showLocalChatOpenError("Create local conversation: " + err.Error())
				} else {
					b.newConversationStatus.SetText("Create local conversation: " + err.Error())
				}
				return
			}
			b.newConversationStatus.SetText("")
			if b.mainView == "Conversations" && selection == b.conversationSelection {
				b.openStandaloneConversation(info)
			}
			b.loadProjectConversations()
		})
	}) {
		delete(b.openingDefaults, key)
	}
}

func (b *Browser) showLocalChatOpenError(message string) {
	b.standaloneChatHost.Objects = []fyne.CanvasObject{widget.NewLabel(message)}
	b.standaloneChatHost.Refresh()
}
