package desktop

import (
	"context"
	"os"
	"runtime"
	"sort"
	"strings"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/widget"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/worker"
)

func isLocalConversationAgent(agent storage.AgentPresence, host string) bool {
	platform := runtime.GOOS
	if platform == "darwin" {
		platform = "macos"
	}
	return host != "" && agent.Hostname != nil && strings.EqualFold(*agent.Hostname, host) && agent.Origin.OS == platform
}

func sortConversationAgents(agents []storage.AgentPresence, host string) {
	sort.SliceStable(agents, func(i, j int) bool {
		a, b := agents[i], agents[j]
		if localA, localB := isLocalConversationAgent(a, host), isLocalConversationAgent(b, host); localA != localB {
			return localA
		}
		if a.Origin.OS != b.Origin.OS {
			return a.Origin.OS < b.Origin.OS
		}
		if (a.Hostname == nil) != (b.Hostname == nil) {
			return a.Hostname != nil
		}
		if a.Hostname != nil && b.Hostname != nil && *a.Hostname != *b.Hostname {
			return *a.Hostname < *b.Hostname
		}
		if a.Origin.Provider != b.Origin.Provider {
			return a.Origin.Provider < b.Origin.Provider
		}
		return a.Origin.SessionID < b.Origin.SessionID
	})
}

func (b *Browser) loadConversationAgents(project string, generation uint64) {
	if b.conversationAgents == nil {
		return
	}
	if project == "" {
		return
	}
	b.launch(func(ctx context.Context) {
		host, _ := os.Hostname()
		agents, err := b.client.Presence(ctx, project)
		sortConversationAgents(agents, host)
		b.dispatch(func() {
			if b.closing || b.selectedProject != project || b.projectConversationGeneration != generation {
				return
			}
			b.conversationAgents.RemoveAll()
			if err != nil {
				b.conversationAgents.Add(widget.NewLabel("Agent list unavailable"))
				return
			}
			heading := ""
			for _, agent := range agents {
				if agent.State == "gone" {
					continue
				}
				group := agent.Origin.OS
				local := isLocalConversationAgent(agent, host)
				if local {
					group = "This machine"
				}
				if group != heading {
					b.conversationAgents.Add(browserSectionHeading(group))
					heading = group
				}
				label := worker.AgentDisplayName(agent.Origin) + " · " + providerConversationLabel(agent.Origin.Provider)
				if local {
					button := widget.NewButton(label, func() {
						b.conversationSelection++
						b.openProviderConversation(agent.Origin.Provider)
					})
					button.Alignment = widget.ButtonAlignLeading
					b.conversationAgents.Add(button)
				} else {
					button := widget.NewButton(label, func() { b.openFleetAgentConversation(agent) })
					button.Alignment = widget.ButtonAlignLeading
					if agent.Hostname == nil {
						b.conversationAgents.Add(button)
					} else {
						host := widget.NewLabel(*agent.Hostname)
						host.Truncation = fyne.TextTruncateEllipsis
						b.conversationAgents.Add(container.NewVBox(button, host))
					}
				}
			}
			if len(b.conversationAgents.Objects) == 0 {
				b.conversationAgents.Add(widget.NewLabel("No online agents"))
			}
		})
	})
}

func (b *Browser) openFleetAgentConversation(agent storage.AgentPresence) {
	project := b.selectedProject
	if project == "" || agent.ProjectIdent != project {
		return
	}
	b.conversationSelection++
	selection := b.conversationSelection
	b.newConversationStatus.SetText("Opening remote conversation…")
	b.launch(func(ctx context.Context) {
		chat, err := b.client.OpenFleetConversation(ctx, project, agent.Origin.SessionID)
		b.dispatch(func() {
			if b.closing || b.selectedProject != project || b.mainView != "Conversations" || b.conversationSelection != selection {
				return
			}
			if err != nil {
				b.newConversationStatus.SetText("Open remote conversation: " + err.Error())
				return
			}
			chat.Conversation.FleetHostname = &chat.Hostname
			b.newConversationStatus.SetText("")
			b.openStandaloneConversation(chat.Conversation)
		})
	})
}
