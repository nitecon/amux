package desktop

import (
	"testing"
	"time"

	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/worker"
)

func TestConversationLabelShowsMachineAgentProviderAndTime(t *testing.T) {
	created := time.Date(2026, 11, 25, 21, 4, 0, 0, time.Local)
	owner := storage.AgentOrigin{SessionID: "session-a", Provider: "claude"}
	c := storage.Conversation{ID: "13599c49-0000", Provider: "claude", Owner: owner, CreatedAt: created.UnixMilli()}
	name := worker.AgentName("session-a")

	if got, want := conversationLabel(c, nil), name+" (Claude) @ Nov 25, 2026 21:04"; got != want {
		t.Fatalf("unknown host label = %q, want %q", got, want)
	}
	other, host := "Other", "Defcon2"
	comments := []storage.TaskComment{
		{Hostname: &other, Origin: &storage.AgentOrigin{SessionID: "session-b"}},
		{Hostname: &host, Origin: &owner},
	}
	if got, want := conversationLabel(c, comments), "Defcon2 - "+name+" (Claude) @ Nov 25, 2026 21:04"; got != want {
		t.Fatalf("comment host label = %q, want %q", got, want)
	}
	fleet := "Fleet1"
	c.FleetHostname = &fleet
	if got, want := conversationLabel(c, comments), "Fleet1 - "+name+" (Claude) @ Nov 25, 2026 21:04"; got != want {
		t.Fatalf("fleet host label = %q, want %q", got, want)
	}
}
