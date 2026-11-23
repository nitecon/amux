package storage

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
	"time"

	"github.com/nitecon/amux/plugins"
	"github.com/nitecon/amux/router"
)

func TestNotificationSnapshotsNativeRegisteredHostsAfterExecutionReplacement(t *testing.T) {
	s := presenceStore(t, filepath.Join(t.TempDir(), "notify-native.db"))
	ctx := context.Background()
	now := time.Now().UTC()
	sender := presenceAgent("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	owner := presenceAgent("cccccccc-cccc-4ccc-8ccc-cccccccccccc", "dddddddd-dddd-4ddd-8ddd-dddddddddddd")
	senderHost, ownerHost, replacementHost := "OriginalSender", "OwnerMachine", "ReplacementSender"
	sender.Hostname, owner.Hostname = &senderHost, &ownerHost
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "native", Sequence: 1, Agents: []PresenceAgent{sender, owner}}, now); err != nil || !ok {
		t.Fatalf("initial native reports: %v %v", ok, err)
	}
	replacement := sender
	replacement.Origin.InstanceID = "eeeeeeee-eeee-4eee-8eee-eeeeeeeeeeee"
	replacement.Hostname = &replacementHost
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "native", Sequence: 2, Agents: []PresenceAgent{replacement, owner}}, now.Add(time.Second)); err != nil || !ok {
		t.Fatalf("replacement native report: %v %v", ok, err)
	}
	registry, err := router.NewRegistry(plugins.NotificationEventDefinitions(), []router.PluginDefinition{{ID: "observer", Subscriptions: []router.Subscription{{Event: plugins.NotifyEventID, MinVersion: 1, MaxVersion: 1}}}})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{
		"target": owner.Origin.SessionID, "project": "another-project", "mode": "recipient", "require_idle": false,
		"event": map[string]any{"kind": "task_commented", "origin": sender.Origin,
			"task": map[string]any{"id": "fixture", "hostname": "CreationMachine", "owner_origin": owner.Origin, "rank": json.Number("9007199254740993")}},
	})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	const cause = "ffffffff-ffff-4fff-8fff-ffffffffffff"
	emitted, err := emitRouterNotificationIn(ctx, tx, registry, "notifier", cause, payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	// Later native reports can change display metadata, never the saved notice.
	owner.Hostname = &replacementHost
	if ok, err := s.ReportPresence(ctx, PresenceReport{Source: "native", Sequence: 3, Agents: []PresenceAgent{replacement, owner}}, now.Add(2*time.Second)); err != nil || !ok {
		t.Fatalf("later report: %v %v", ok, err)
	}
	events, err := s.RouterEvents(ctx, 0, 20)
	if err != nil || len(events) != 1 {
		t.Fatalf("saved notification: %+v %v", events, err)
	}
	event := events[0]
	if event.UID != emitted.Event.UID || event.Source != "plugin:notifier" || event.Correlation == nil || *event.Correlation != cause {
		t.Fatalf("notification provenance: %+v", event)
	}
	var snapshot struct {
		Event struct {
			Sender string `json:"sender_hostname"`
			Owner  string `json:"owner_hostname"`
			Task   struct {
				Host string      `json:"hostname"`
				Rank json.Number `json:"rank"`
			} `json:"task"`
		} `json:"event"`
	}
	if err := json.Unmarshal(event.Payload, &snapshot); err != nil {
		t.Fatal(err)
	}
	if snapshot.Event.Sender != senderHost || snapshot.Event.Owner != ownerHost || snapshot.Event.Task.Host != "CreationMachine" || snapshot.Event.Task.Rank.String() != "9007199254740993" {
		t.Fatalf("host/numeric snapshot changed: %s", event.Payload)
	}
}
