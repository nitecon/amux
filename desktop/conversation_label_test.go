package desktop

import (
	"testing"
	"time"

	"github.com/nitecon/amux/identity/agent"
	"github.com/nitecon/amux/storage"
)

func TestConversationLabelShowsMachineAgentProviderAndTime(t *testing.T) {
	created := time.Date(2026, 11, 25, 21, 4, 0, 0, time.Local)
	owner := agent.Origin{SessionID: "session-a", Provider: "claude"}
	c := storage.Conversation{ID: "13599c49-0000", Provider: "claude", Owner: owner, CreatedAt: created.UnixMilli()}
	name := agent.Name("session-a")

	if got, want := conversationLabel(c), name+" (Claude) @ Nov 25, 2026 21:04"; got != want {
		t.Fatalf("unknown host label = %q, want %q", got, want)
	}
	host := "Defcon2"
	c.OwnerHostname = &host
	if got, want := conversationLabel(c), "Defcon2 - "+name+" (Claude) @ Nov 25, 2026 21:04"; got != want {
		t.Fatalf("owner host label = %q, want %q", got, want)
	}
	fleet := "Fleet1"
	c.FleetHostname = &fleet
	if got, want := conversationLabel(c), "Fleet1 - "+name+" (Claude) @ Nov 25, 2026 21:04"; got != want {
		t.Fatalf("fleet host label = %q, want %q", got, want)
	}
}
