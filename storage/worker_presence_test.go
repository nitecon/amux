package storage

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
)

func TestNextWorkerRunReportPersistsMonotonicSequence(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "worker-presence.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	run := WorkerRun{ID: uuid.NewString(), Server: "http://127.0.0.1:7913", Project: "demo", ConversationID: uuid.NewString(), Directory: t.TempDir(), Owner: *taskActor()}
	if err = s.SaveWorkerRun(ctx, run); err != nil {
		t.Fatal(err)
	}
	first, err := s.NextWorkerRunReport(ctx, run.ID)
	if err != nil || first.PresenceSequence != 1 {
		t.Fatalf("first report=%+v err=%v", first, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	second, err := s.NextWorkerRunReport(ctx, run.ID)
	if err != nil || second.PresenceSequence != 2 {
		t.Fatalf("second report=%+v err=%v", second, err)
	}
	loaded, err := s.WorkerRun(ctx, run.ID)
	if err != nil || loaded.PresenceSequence != 2 {
		t.Fatalf("loaded run=%+v err=%v", loaded, err)
	}
}
