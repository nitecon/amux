package storage

import (
	"context"
	"path/filepath"
	"testing"
)

func TestLivePresenceSnapshotKeepsClockWhenBindingsHideAllAgents(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "presence-binding-clock.db"))
	if _, err := s.db.ExecContext(ctx, `UPDATE projects SET canonical_remote='github.com/current/project' WHERE ident='presence'`); err != nil {
		t.Fatal(err)
	}
	const suppliedNow = int64(1_800_000_000_000)
	const persistedClock = suppliedNow + 30_000
	if _, err := s.db.ExecContext(ctx, `UPDATE amux_presence_clock SET observed_at=? WHERE id=1`, persistedClock); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO amux_presence_agents
		(project_ident,instance_id,session_id,provider,os,source,sequence,seen_at,last_activity_at,busy,draft,auto_assign,review_context_clear,state,
		report_kind,event_agent_json,identity_seq,activity_seq,binding_required,binding_remote)
		VALUES('presence','instance','session','codex','linux','raw:source',1,?,NULL,0,0,0,0,'unknown','event','{"session_id":"session"}',1,0,1,'github.com/old/project')`, persistedClock); err != nil {
		t.Fatal(err)
	}

	agents, observed, err := livePresenceSnapshot(ctx, s.db, suppliedNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 0 {
		t.Fatalf("binding-mismatched agents visible: %+v", agents)
	}
	if observed != persistedClock {
		t.Fatalf("observed clock=%d, want persisted clock %d", observed, persistedClock)
	}
}

func TestLivePresenceSnapshotNormalizesOnlyBindingProjectLookup(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "presence-binding-alias.db"))
	if _, err := s.db.ExecContext(ctx, `UPDATE projects SET canonical_remote='github.com/current/project' WHERE ident='presence'`); err != nil {
		t.Fatal(err)
	}
	const now = int64(1_800_000_000_000)
	if _, err := s.db.ExecContext(ctx, `UPDATE amux_presence_clock SET observed_at=? WHERE id=1`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO amux_presence_agents
		(project_ident,instance_id,session_id,provider,os,source,sequence,seen_at,last_activity_at,busy,draft,auto_assign,review_context_clear,state,
		report_kind,event_agent_json,identity_seq,activity_seq,binding_required,binding_remote)
		VALUES(' Presence ','instance','session','codex','linux','raw:source',1,?,NULL,0,0,0,0,'unknown','event','{"session_id":"session"}',1,0,1,'github.com/current/project')`, now); err != nil {
		t.Fatal(err)
	}

	agents, _, err := livePresenceSnapshot(ctx, s.db, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(agents) != 1 || agents[0].ProjectIdent != " Presence " {
		t.Fatalf("raw project identity was not preserved for matched binding: %+v", agents)
	}
}
