package storage

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestPresenceObservationMigrationImportsRetainedAndLegacyRows(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "observation-import.db"))
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO projects(ident,channel_name,room_id,created_at,kind)
		VALUES('rust-project','fixture','',1,'repo');
		CREATE TABLE presence_clock(id INTEGER PRIMARY KEY CHECK(id=1),observed_at INTEGER NOT NULL);
		INSERT INTO presence_clock VALUES(1,900);
		CREATE TABLE presence_sources(source TEXT PRIMARY KEY,heartbeat_seq INTEGER NOT NULL);
		INSERT INTO presence_sources VALUES('rust-source',77),('native-source',66);
		INSERT INTO amux_presence_sources(source,sequence,heartbeat_uid,heartbeat_seen_at,os,hostname)
			VALUES('native-source',66,'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb',700,'windows','Native-Host');
		INSERT INTO router_events(seq,uid,id,version,payload,source,emitted_at)
			VALUES(77,'aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa','client.heartbeat',1,
			'{"os":"linux","hostname":"Rust-Host","agents":[]}','rust-source',800);
		INSERT INTO router_events(seq,uid,id,version,payload,source,emitted_at)
			VALUES(66,'cccccccc-cccc-4ccc-8ccc-cccccccccccc','client.heartbeat',1,
			'{"os":"linux","hostname":"Wrong-Host","agents":[]}','native-source',600);
		CREATE TABLE presence_agents(source TEXT NOT NULL,agent_json TEXT NOT NULL,seen_at INTEGER NOT NULL,
			output_at INTEGER,draft INTEGER NOT NULL,state TEXT NOT NULL,PRIMARY KEY(source,agent_json));
		INSERT INTO task_target_presence(project_ident,instance_id,session_id,origin_json,generation,seen_at,
			canonical_remote,display_name,hostname,advertised,source,agent_json,report_seen_at,output_at,draft,state,
			identity_seq,activity_seq)
			VALUES('rust-project','inst','directory-session','{}','gen',12,'','Manual target','manual-host',1,
			'directory-source','{"project":"rust-project","instance_id":"inst","session_id":"directory-session","provider":"codex","os":"linux","busy":false,"draft":true}',120,110,1,'busy',41,42);
		INSERT INTO presence_agents VALUES('legacy-source',
			'{"project":"rust-project","instance_id":"old-inst","session_id":"legacy-session","provider":"codex","os":"linux","busy":true,"draft":false}',130,125,1,'busy');
		INSERT INTO task_target_presence(project_ident,instance_id,session_id,origin_json,generation,seen_at,
			canonical_remote,display_name,hostname,advertised,source,agent_json,report_seen_at,output_at,draft,state,
			identity_seq,activity_seq)
			VALUES('missing-project','bad-inst','unresolved-session','{}','',0,'','',NULL,0,'bad-source',
			'{"project":"missing-project","instance_id":"bad-inst","session_id":"unresolved-session","provider":"codex","os":"linux","canonical_remote":"github.com/example/missing"}',140,NULL,0,'unknown',3,0);
		DELETE FROM amux_go_migrations WHERE version=27`); err != nil {
		t.Fatal(err)
	}
	runPresenceObservationMigration(t, ctx, s)

	assertEqual(t, s.db, `SELECT json_extract(event_agent_json,'$.session_id') FROM amux_presence_agents
		WHERE project_ident='rust-project' AND session_id='directory-session'`, "directory-session")
	assertEqual(t, s.db, `SELECT last_activity_at FROM amux_presence_agents WHERE session_id='directory-session'`, int64(110))
	assertEqual(t, s.db, `SELECT draft FROM amux_presence_agents WHERE session_id='directory-session'`, int64(1))
	assertEqual(t, s.db, `SELECT activity_seq FROM amux_presence_agents WHERE session_id='directory-session'`, int64(42))
	assertEqual(t, s.db, `SELECT output_at FROM task_target_presence WHERE session_id='directory-session'`, int64(110))
	assertEqual(t, s.db, `SELECT draft FROM task_target_presence WHERE session_id='directory-session'`, int64(1))
	assertEqual(t, s.db, `SELECT identity_seq FROM task_target_presence WHERE session_id='directory-session'`, int64(41))
	assertEqual(t, s.db, `SELECT activity_seq FROM task_target_presence WHERE session_id='directory-session'`, int64(42))
	assertEqual(t, s.db, `SELECT advertised FROM task_target_presence WHERE session_id='directory-session'`, int64(1))
	assertEqual(t, s.db, `SELECT display_name FROM task_target_presence WHERE session_id='directory-session'`, "Manual target")
	assertEqual(t, s.db, `SELECT hostname FROM task_target_presence WHERE session_id='directory-session'`, "manual-host")
	assertEqual(t, s.db, `SELECT report_kind FROM amux_presence_agents WHERE session_id='legacy-session'`, "event")
	assertEqual(t, s.db, `SELECT identity_seq FROM amux_presence_agents WHERE session_id='legacy-session'`, int64(0))
	assertEqual(t, s.db, `SELECT last_activity_at FROM amux_presence_agents WHERE session_id='legacy-session'`, int64(125))
	assertEqual(t, s.db, `SELECT draft FROM amux_presence_agents WHERE session_id='legacy-session'`, int64(1))
	assertEqual(t, s.db, `SELECT COUNT(*) FROM amux_presence_agents WHERE session_id='unresolved-session'`, int64(0))
	assertEqual(t, s.db, `SELECT heartbeat_seq FROM rawpresence_sources WHERE source='rust-source'`, int64(77))
	assertEqual(t, s.db, `SELECT COUNT(*) FROM amux_presence_sources WHERE source='rust-source'`, int64(0))
	assertEqual(t, s.db, `SELECT heartbeat_uid FROM rawpresence_sources WHERE source='rust-source'`, "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
	assertEqual(t, s.db, `SELECT heartbeat_seen_at FROM rawpresence_sources WHERE source='rust-source'`, int64(800))
	assertEqual(t, s.db, `SELECT os FROM rawpresence_sources WHERE source='rust-source'`, "linux")
	assertEqual(t, s.db, `SELECT hostname FROM rawpresence_sources WHERE source='rust-source'`, "Rust-Host")
	assertEqual(t, s.db, `SELECT heartbeat_uid FROM amux_presence_sources WHERE source='native-source'`, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
	assertEqual(t, s.db, `SELECT os FROM amux_presence_sources WHERE source='native-source'`, "windows")
	assertEqual(t, s.db, `SELECT hostname FROM amux_presence_sources WHERE source='native-source'`, "Native-Host")
	assertEqual(t, s.db, `SELECT observed_at FROM amux_presence_clock WHERE id=1`, int64(900))
	assertEqual(t, s.db, `SELECT COUNT(*) FROM presence_agents`, int64(1))
	assertEqual(t, s.db, `SELECT heartbeat_seq FROM presence_sources WHERE source='rust-source'`, int64(77))
	assertEqual(t, s.db, `SELECT observed_at FROM presence_clock WHERE id=1`, int64(900))
	assertEqual(t, s.db, `SELECT COUNT(*) FROM task_target_presence WHERE session_id='unresolved-session'`, int64(1))

	if _, err := s.db.ExecContext(ctx, `DELETE FROM amux_presence_agents WHERE session_id='legacy-session'`); err != nil {
		t.Fatal(err)
	}
	runPresenceObservationMigration(t, ctx, s)
	assertEqual(t, s.db, `SELECT COUNT(*) FROM amux_presence_agents WHERE session_id='legacy-session'`, int64(0))
	if _, err := s.db.ExecContext(ctx, `DELETE FROM rawpresence_sources WHERE source='rust-source'`); err != nil {
		t.Fatal(err)
	}
	runPresenceObservationMigration(t, ctx, s)
	assertEqual(t, s.db, `SELECT COUNT(*) FROM rawpresence_sources WHERE source='rust-source'`, int64(0))
	if _, err := s.db.ExecContext(ctx, `DELETE FROM projects WHERE ident='rust-project'`); err != nil {
		t.Fatal(err)
	}
	assertEqual(t, s.db, `SELECT COUNT(*) FROM amux_presence_agents WHERE project_ident='rust-project'`, int64(0))
}

func TestPresenceObservationMigrationNativeRowsWin(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "observation-native.db"))
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO projects(ident,channel_name,room_id,created_at) VALUES('known','fixture','',1);
		INSERT INTO amux_presence_agents(project_ident,instance_id,session_id,provider,os,source,sequence,seen_at,
			busy,draft,auto_assign,review_context_clear,state,report_kind,event_agent_json,identity_seq,activity_seq)
			VALUES('known','inst','same','codex','linux','native-source',8,500,1,0,1,1,'busy','native','native-json',9,10);
		CREATE TABLE presence_agents(source TEXT,agent_json TEXT,seen_at INTEGER,output_at INTEGER,draft INTEGER,state TEXT);
		INSERT INTO presence_agents VALUES('old-source',
			'{"project":"known","instance_id":"inst","session_id":"same","provider":"codex","os":"linux"}',1,2,0,'idle');
		DELETE FROM amux_go_migrations WHERE version=27`); err != nil {
		t.Fatal(err)
	}
	runPresenceObservationMigration(t, ctx, s)
	assertEqual(t, s.db, `SELECT source FROM amux_presence_agents WHERE session_id='same'`, "native-source")
	assertEqual(t, s.db, `SELECT event_agent_json FROM amux_presence_agents WHERE session_id='same'`, "native-json")
	assertEqual(t, s.db, `SELECT auto_assign FROM amux_presence_agents WHERE session_id='same'`, int64(1))
	assertEqual(t, s.db, `SELECT identity_seq FROM amux_presence_agents WHERE session_id='same'`, int64(9))
}

func TestPresenceObservationMigrationSkipsEntireNativeOwnedSource(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "observation-native-source.db"))
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO projects(ident,channel_name,room_id,created_at) VALUES('rust-project','fixture','',1);
		INSERT INTO amux_presence_sources(source,sequence,heartbeat_uid,heartbeat_seen_at,os,hostname)
			VALUES('native-source',8,'bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb',500,'linux','Native-Host');
		INSERT INTO task_target_presence(project_ident,instance_id,session_id,origin_json,generation,seen_at,
			canonical_remote,display_name,hostname,advertised,source,agent_json,report_seen_at,output_at,draft,state,
			identity_seq,activity_seq)
			VALUES('rust-project','inst','omitted-session','{}','gen',12,'','Manual target','manual-host',1,
			'native-source','{"project":"rust-project","instance_id":"inst","session_id":"omitted-session","provider":"codex","os":"linux","busy":false,"draft":false}',450,420,0,'idle',6,7);
		DELETE FROM amux_go_migrations WHERE version=27`); err != nil {
		t.Fatal(err)
	}
	runPresenceObservationMigration(t, ctx, s)
	assertEqual(t, s.db, `SELECT COUNT(*) FROM amux_presence_agents WHERE source='native-source' AND report_kind='event'`, int64(0))
	assertEqual(t, s.db, `SELECT COUNT(*) FROM amux_presence_agents WHERE session_id='omitted-session'`, int64(0))
	assertEqual(t, s.db, `SELECT source FROM task_target_presence WHERE session_id='omitted-session'`, "native-source")
	assertEqual(t, s.db, `SELECT report_seen_at FROM task_target_presence WHERE session_id='omitted-session'`, int64(450))
	assertEqual(t, s.db, `SELECT display_name FROM task_target_presence WHERE session_id='omitted-session'`, "Manual target")
	assertEqual(t, s.db, `SELECT heartbeat_uid FROM amux_presence_sources WHERE source='native-source'`, "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb")
}

func TestPresenceObservationMigrationImportsEmptyV1IdentityVerbatim(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "observation-empty-identity.db"))
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `CREATE TABLE presence_agents(source TEXT,agent_json TEXT,seen_at INTEGER,
		output_at INTEGER,draft INTEGER,state TEXT);
		INSERT INTO task_target_presence(project_ident,instance_id,session_id,origin_json,generation,seen_at,
			canonical_remote,advertised,source,agent_json,report_seen_at,output_at,draft,state,identity_seq,activity_seq)
			VALUES('','','','{}','',0,'',0,'legacy-source',
			'{"project":"","instance_id":"","session_id":"","provider":"","os":""}',30,25,0,'unknown',0,0);
		INSERT INTO presence_agents VALUES('legacy-source',
			'{"project":"","instance_id":"","session_id":"","provider":"","os":""}',31,99,1,'busy');
		DELETE FROM amux_go_migrations WHERE version=27`); err != nil {
		t.Fatal(err)
	}
	runPresenceObservationMigration(t, ctx, s)
	assertEqual(t, s.db, `SELECT COUNT(*) FROM amux_presence_agents WHERE project_ident='' AND instance_id='' AND session_id=''`, int64(1))
	assertEqual(t, s.db, `SELECT last_activity_at FROM amux_presence_agents WHERE project_ident='' AND instance_id='' AND session_id=''`, int64(25))
	assertEqual(t, s.db, `SELECT auto_assign+review_context_clear FROM amux_presence_agents WHERE project_ident='' AND instance_id='' AND session_id=''`, int64(0))
	assertEqual(t, s.db, `SELECT COUNT(*) FROM projects WHERE ident=''`, int64(0))
	assertEqual(t, s.db, `SELECT COUNT(*) FROM task_target_presence WHERE project_ident='' AND instance_id='' AND session_id=''`, int64(1))
}

func TestPresenceObservationMigrationFailureRollsBackImportAndLedger(t *testing.T) {
	ctx := context.Background()
	s := presenceStore(t, filepath.Join(t.TempDir(), "observation-rollback.db"))
	defer s.Close()
	if _, err := s.db.ExecContext(ctx, `INSERT INTO projects(ident,channel_name,room_id,created_at) VALUES('known','fixture','',1);
		INSERT INTO task_target_presence(project_ident,instance_id,session_id,origin_json,generation,seen_at,
			canonical_remote,advertised,source,agent_json,report_seen_at,output_at,draft,state,identity_seq,activity_seq)
			VALUES('known','inst','session','{}','g',0,'',1,'old-source',
			'{"project":"known","instance_id":"inst","session_id":"session","provider":"codex","os":"linux"}',20,10,1,'busy',4,5);
		DELETE FROM amux_go_migrations WHERE version=27;
		CREATE TRIGGER fail_observation_restore BEFORE UPDATE OF last_activity_at ON amux_presence_agents
			BEGIN SELECT RAISE(ABORT,'injected migration failure'); END`); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := migratePresenceObservations(ctx, tx); err == nil {
		t.Fatal("migration unexpectedly succeeded")
	}
	if err := tx.Rollback(); err != nil && err != sql.ErrTxDone {
		t.Fatal(err)
	}
	assertEqual(t, s.db, `SELECT COUNT(*) FROM amux_presence_agents WHERE session_id='session'`, int64(0))
	assertEqual(t, s.db, `SELECT COUNT(*) FROM amux_go_migrations WHERE version=27`, int64(0))
}

func runPresenceObservationMigration(t *testing.T, ctx context.Context, s *Store) {
	t.Helper()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := migratePresenceObservations(ctx, tx); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
}
