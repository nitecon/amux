package gatewayclient

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

func TestPresenceRoundTripAndReplayFence(t *testing.T) {
	ctx := context.Background()
	s, err := storage.Open(ctx, filepath.Join(t.TempDir(), "presence.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.InsertProject(ctx, storage.Project{Ident: "demo", ChannelName: "demo", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewHandler(s, "test-key")
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(handler)
	defer host.Close()
	c, err := New(host.URL, "test-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	activity := time.Now().Add(-time.Minute).UnixMilli()
	hostname := "WorkerMachine"
	goal, generation, alias := "selected-goal", uuid.NewString(), "named-agent"
	report := storage.PresenceReport{Source: "native-host", Sequence: 1, Agents: []storage.PresenceAgent{{
		ProjectIdent: "demo", Origin: storage.AgentOrigin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: "codex", OS: "linux"}, LastActivityAt: &activity, Hostname: &hostname, Goal: &goal, Generation: &generation, AgentID: &alias,
	}}}
	if accepted, err := c.ReportPresence(ctx, report); err != nil || !accepted {
		t.Fatalf("report accepted=%v err=%v", accepted, err)
	}
	replayed := report
	replayed.Agents = append([]storage.PresenceAgent(nil), report.Agents...)
	replacementHost := "ReplayMustNotReplaceHost"
	replayed.Agents[0].Hostname = &replacementHost
	replacementGeneration := uuid.NewString()
	replayed.Agents[0].Generation = &replacementGeneration
	if accepted, err := c.ReportPresence(ctx, replayed); err != nil || accepted {
		t.Fatalf("replay accepted=%v err=%v", accepted, err)
	}
	if err := s.ReconcilePresence(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	agents, err := c.Presence(ctx, "demo")
	if err != nil || len(agents) != 1 || agents[0].State != "idle" {
		t.Fatalf("presence=%+v err=%v", agents, err)
	}
	if agents[0].Hostname == nil || *agents[0].Hostname != hostname {
		t.Fatalf("hostname round trip/replay fence: %+v", agents[0])
	}
	if agents[0].Goal == nil || *agents[0].Goal != goal || agents[0].Generation == nil || *agents[0].Generation != generation || agents[0].AgentID == nil || *agents[0].AgentID != alias {
		t.Fatalf("routing metadata round trip/replay fence: %+v", agents[0])
	}
	other := report
	other.Source = "other-host"
	if _, err := c.ReportPresence(ctx, other); err == nil {
		t.Fatal("conflicting source accepted")
	} else {
		var response *HTTPError
		if !errors.As(err, &response) || response.Status != 409 {
			t.Fatalf("conflict: %v", err)
		}
	}
	bad, err := New(host.URL, "wrong-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bad.Presence(ctx, "demo"); err == nil {
		t.Fatal("unauthenticated read accepted")
	}
	report.Sequence++
	report.Agents = nil
	if accepted, err := c.ReportPresence(ctx, report); err != nil || !accepted {
		t.Fatalf("omission accepted=%v err=%v", accepted, err)
	}
	if err := s.ReconcilePresence(ctx, time.Now()); err != nil {
		t.Fatal(err)
	}
	agents, err = c.Presence(ctx, "demo")
	if err != nil || len(agents) != 1 || agents[0].State != "gone" {
		t.Fatalf("omission=%+v err=%v", agents, err)
	}
}
