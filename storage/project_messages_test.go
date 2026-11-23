package storage

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestGetUnconfirmedProjectMessagesMatchesRustUnreadPredicateAndPerAgentAck(t *testing.T) {
	ctx := context.Background()
	s := projectMessagesTestStore(t, "inbox", "other")
	first := insertProjectMessageFixture(t, s, "inbox", "user", "please handle this", false)
	agentStatus := insertProjectMessageFixture(t, s, "inbox", "agent", "agent status", false)
	second := insertProjectMessageFixture(t, s, "inbox", "user", "also this", false)
	otherProject := insertProjectMessageFixture(t, s, "other", "user", "other project", false)

	unread, err := s.GetUnconfirmedProjectMessages(ctx, "inbox", "agent-a")
	if err != nil || len(unread) != 2 || unread[0].ID != first || unread[1].ID != second {
		t.Fatalf("unread project messages=%+v err=%v", unread, err)
	}
	var registered int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM agents WHERE project_ident='inbox' AND agent_id='agent-a'").Scan(&registered); err != nil || registered != 1 {
		t.Fatalf("unread poll did not lazily register agent: rows=%d err=%v", registered, err)
	}
	if unread[0].Source != "user" || unread[1].Source != "user" || unread[0].ID == agentStatus || unread[1].ID == otherProject {
		t.Fatalf("unread predicate leaked a non-user agent message or another project: %+v", unread)
	}

	confirmed, err := s.ConfirmProjectMessage(ctx, "inbox", "agent-a", first)
	if err != nil || !confirmed {
		t.Fatalf("first per-agent confirmation=%t err=%v", confirmed, err)
	}
	confirmed, err = s.ConfirmProjectMessage(ctx, "inbox", "agent-a", first)
	if err != nil || confirmed {
		t.Fatalf("duplicate confirmation=%t err=%v, want idempotent false", confirmed, err)
	}
	var sharedConfirmed any
	if err := s.db.QueryRowContext(ctx, "SELECT confirmed_at FROM messages WHERE id=?", first).Scan(&sharedConfirmed); err != nil || sharedConfirmed != nil {
		t.Fatalf("per-agent ack modified shared confirmed_at=%v err=%v", sharedConfirmed, err)
	}
	for _, agentID := range []string{"agent-a", "agent-b"} {
		rows, err := s.GetUnconfirmedProjectMessages(ctx, "inbox", agentID)
		if err != nil {
			t.Fatal(err)
		}
		want := []int64{second}
		if agentID == "agent-b" {
			want = []int64{first, second}
		}
		if len(rows) != len(want) {
			t.Fatalf("agent %s unread=%+v want ids %v", agentID, rows, want)
		}
		for i, id := range want {
			if rows[i].ID != id {
				t.Fatalf("agent %s unread=%+v want ids %v", agentID, rows, want)
			}
		}
	}
}

func TestGetUnconfirmedProjectMessagesIncludesOnlyDeliverableSystemMessages(t *testing.T) {
	ctx := context.Background()
	s := projectMessagesTestStore(t, "inbox")
	hidden := insertProjectMessageFixture(t, s, "inbox", "system", "internal bookkeeping", false)
	deliverable := insertProjectMessageFixture(t, s, "inbox", "system", "delegated task created", true)

	unread, err := s.GetUnconfirmedProjectMessages(ctx, "inbox", "agent-a")
	if err != nil || len(unread) != 1 || unread[0].ID != deliverable || unread[0].Source != "system" || !unread[0].DeliverToAgents {
		t.Fatalf("deliverable-system unread=%+v err=%v", unread, err)
	}
	if unread[0].ID == hidden {
		t.Fatal("undeliverable system message was exposed")
	}
}

func TestProjectMessageFieldsAndUnreadUseSourceCompatibleNullables(t *testing.T) {
	ctx := context.Background()
	s := projectMessagesTestStore(t, "inbox")
	result, err := s.db.ExecContext(ctx, `INSERT INTO messages(project_ident,source,external_message_id,content,sent_at,confirmed_at,
		parent_message_id,agent_id,message_type,subject,hostname,event_at,deliver_to_agents,author_kind,resolved_at,resolved_by)
		VALUES('inbox','system','external-1','full record',11,12,13,'agent-a','reply','subject','host',14,1,'system',15,'agent:done')`)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	unread, err := s.GetUnconfirmedProjectMessages(ctx, "inbox", "reader")
	if err != nil || len(unread) != 1 {
		t.Fatalf("full message unread=%+v err=%v", unread, err)
	}
	message := unread[0]
	if message.ID != id || message.ProjectIdent != "inbox" || message.Source != "system" || message.Content != "full record" || message.SentAt != 11 || message.MessageType != "reply" || !message.DeliverToAgents {
		t.Fatalf("message required fields mismatch: %+v", message)
	}
	if message.ExternalMessageID == nil || *message.ExternalMessageID != "external-1" || message.ConfirmedAt == nil || *message.ConfirmedAt != 12 || message.ParentMessageID == nil || *message.ParentMessageID != 13 || message.AgentID == nil || *message.AgentID != "agent-a" || message.Subject == nil || *message.Subject != "subject" || message.Hostname == nil || *message.Hostname != "host" || message.EventAt == nil || *message.EventAt != 14 || message.AuthorKind == nil || *message.AuthorKind != "system" || message.ResolvedAt == nil || *message.ResolvedAt != 15 || message.ResolvedBy == nil || *message.ResolvedBy != "agent:done" {
		t.Fatalf("message optional fields mismatch: %+v", message)
	}
}

func TestConfirmProjectMessageRejectsCrossProjectMessageWithoutOrphanAck(t *testing.T) {
	ctx := context.Background()
	s := projectMessagesTestStore(t, "one", "two")
	one := insertProjectMessageFixture(t, s, "one", "user", "private to one", false)
	two := insertProjectMessageFixture(t, s, "two", "user", "private to two", false)
	if confirmed, err := s.ConfirmProjectMessage(ctx, "two", "reader", one); !errors.Is(err, ErrProjectMessageNotFound) || confirmed {
		t.Fatalf("cross-project confirmation=%t err=%v", confirmed, err)
	}
	var orphanCount int
	if err := s.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM agent_confirmations WHERE project_ident='two' AND message_id=?", one).Scan(&orphanCount); err != nil || orphanCount != 0 {
		t.Fatalf("cross-project ack rows=%d err=%v", orphanCount, err)
	}
	if confirmed, err := s.ConfirmProjectMessage(ctx, "two", "reader", two); err != nil || !confirmed {
		t.Fatalf("same-project confirmation=%t err=%v", confirmed, err)
	}
	oneUnread, err := s.GetUnconfirmedProjectMessages(ctx, "one", "reader")
	if err != nil || len(oneUnread) != 1 || oneUnread[0].ID != one {
		t.Fatalf("other project inbox was affected: %+v err=%v", oneUnread, err)
	}
}

func TestProjectAgentsMigrationPreservesExistingSchemaAndInboxReadDoesNotChangeSchema(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "messages.db")
	s, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertProject(ctx, fixtureProject("inbox")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `ALTER TABLE agents ADD COLUMN legacy_marker TEXT;
		CREATE INDEX fixture_agents_legacy ON agents(legacy_marker);
		INSERT INTO agents(project_ident,agent_id,registered_at,legacy_marker) VALUES('inbox','old-agent',7,'retained');
		DELETE FROM amux_go_migrations WHERE version=35`); err != nil {
		t.Fatal(err)
	}
	beforeDDL, err := projectMessageSchema(ctx, s.db)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	afterMigrationDDL, err := projectMessageSchema(ctx, reopened.db)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeDDL, afterMigrationDDL) {
		t.Fatalf("startup migration changed pre-existing schema: before=%v after=%v", beforeDDL, afterMigrationDDL)
	}
	var version int
	if err := reopened.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM amux_go_migrations WHERE version=35").Scan(&version); err != nil || version != 1 {
		t.Fatalf("migration 35 not recorded: count=%d err=%v", version, err)
	}
	var retained string
	if err := reopened.db.QueryRowContext(ctx, "SELECT legacy_marker FROM agents WHERE project_ident='inbox' AND agent_id='old-agent'").Scan(&retained); err != nil || retained != "retained" {
		t.Fatalf("legacy agent schema/data not preserved: marker=%q err=%v", retained, err)
	}

	beforeReadDDL, err := projectMessageSchema(ctx, reopened.db)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reopened.GetUnconfirmedProjectMessages(ctx, "inbox", "native-session"); err != nil {
		t.Fatal(err)
	}
	afterReadDDL, err := projectMessageSchema(ctx, reopened.db)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(beforeReadDDL, afterReadDDL) {
		t.Fatalf("inbox read mutated schema: before=%v after=%v", beforeReadDDL, afterReadDDL)
	}
}

func projectMessagesTestStore(t *testing.T, projectsToAdd ...string) *Store {
	t.Helper()
	s := testStore(t)
	for _, ident := range projectsToAdd {
		if err := s.InsertProject(context.Background(), fixtureProject(ident)); err != nil {
			t.Fatal(err)
		}
	}
	return s
}

func insertProjectMessageFixture(t *testing.T, s *Store, project, source, content string, deliver bool) int64 {
	t.Helper()
	result, err := s.db.ExecContext(context.Background(), `INSERT INTO messages(project_ident,source,content,sent_at,deliver_to_agents)
		VALUES(?,?,?,?,?)`, project, source, content, time.Now().UnixMilli(), deliver)
	if err != nil {
		t.Fatal(err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func projectMessageSchema(ctx context.Context, db *sql.DB) (map[string]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT type,name,sql FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type,name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	schema := make(map[string]string)
	for rows.Next() {
		var kind, name string
		var statement sql.NullString
		if err := rows.Scan(&kind, &name, &statement); err != nil {
			return nil, err
		}
		schema[kind+":"+name] = statement.String
	}
	return schema, rows.Err()
}
