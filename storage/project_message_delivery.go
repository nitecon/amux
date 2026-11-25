package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/router"
)

const (
	projectInboxRecipientBatch = 32
	projectInboxTickBudget     = 256
	projectInboxPromptLimit    = 64*1024 - 1
)

type projectInboxRecipient struct {
	project, instance, session, provider, os, source string
	sequence                                         int64
	generation                                       sql.NullString
}

// ReconcileProjectInbox turns unread project messages into durable prompts
// for the exact live native session that owns a taskless project conversation. The
// message confirmation and command are committed together, so replayed ticks
// cannot lose or duplicate an inbox item.
func (s *Store) ReconcileProjectInbox(ctx context.Context, event router.Event) error {
	if event.ID != "clock.tick" || event.Version != 1 || !canonicalUUID(event.UID) {
		return fmt.Errorf("project inbox reconciliation requires a clock.tick v1 event with a canonical UID")
	}
	now, err := nativeDeliveryNow(ctx, s.db, event.EmittedAt)
	if err != nil {
		return err
	}
	recipients, err := s.projectInboxRecipients(ctx, now)
	if err != nil {
		return err
	}
	if len(recipients) == 0 {
		return nil
	}
	start := int(event.Seq % int64(len(recipients)))
	if start < 0 {
		start += len(recipients)
	}
	var failures []error
	wake := false
	remaining := projectInboxTickBudget
	for i := 0; i < len(recipients) && remaining > 0; i++ {
		recipient := recipients[(start+i)%len(recipients)]
		quota := remaining / (len(recipients) - i)
		if quota > projectInboxRecipientBatch {
			quota = projectInboxRecipientBatch
		}
		if quota < 1 {
			continue
		}
		delivered, err := s.deliverProjectInboxRecipient(ctx, recipient, now, quota)
		remaining -= quota
		if delivered {
			wake = true
		}
		if err != nil {
			failures = append(failures, fmt.Errorf("deliver project inbox for %s/%s: %w", recipient.project, recipient.session, err))
		}
	}
	if wake {
		s.wakeConversations()
	}
	return errors.Join(failures...)
}

func (s *Store) projectInboxRecipients(ctx context.Context, now int64) ([]projectInboxRecipient, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT a.project_ident,a.instance_id,a.session_id,a.provider,a.os,a.source,a.sequence,a.generation
		FROM amux_presence_agents a JOIN projects p ON p.ident=lower(trim(a.project_ident))
		WHERE a.report_kind='native' AND a.state!='gone' AND ?-a.seen_at<?
		AND EXISTS(SELECT 1 FROM messages m WHERE m.project_ident=a.project_ident
		 AND (m.source='user' OR m.deliver_to_agents=1)
		 AND EXISTS(SELECT 1 FROM amux_conversations c WHERE c.project_ident=a.project_ident AND c.task_id IS NULL
		   AND json_extract(c.owner_json,'$.session_id')=a.session_id
		   AND json_extract(c.owner_json,'$.instance_id')=a.instance_id
		   AND COALESCE(m.event_at,m.sent_at)>=c.created_at)
		 AND NOT EXISTS(SELECT 1 FROM agent_confirmations c WHERE c.agent_id=a.session_id AND c.project_ident=m.project_ident AND c.message_id=m.id))
		ORDER BY a.project_ident,a.session_id,a.instance_id`, now, presenceGoneAfter)
	if err != nil {
		return nil, err
	}
	var all []projectInboxRecipient
	for rows.Next() {
		var r projectInboxRecipient
		if err := rows.Scan(&r.project, &r.instance, &r.session, &r.provider, &r.os, &r.source, &r.sequence, &r.generation); err != nil {
			rows.Close()
			return nil, err
		}
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	// A stable session advertised by multiple simultaneously live instances is
	// ambiguous; do not deliver to either until the presence snapshot resolves it.
	counts := make(map[string]int)
	key := func(r projectInboxRecipient) string { return r.project + "\x00" + r.session }
	for _, r := range all {
		counts[key(r)]++
	}
	out := make([]projectInboxRecipient, 0, len(all))
	for _, r := range all {
		if counts[key(r)] == 1 {
			out = append(out, r)
		}
	}
	return out, nil
}

func (s *Store) deliverProjectInboxRecipient(ctx context.Context, r projectInboxRecipient, now int64, limit int) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var source, provider, osName, state, reportKind string
	var sequence, seenAt int64
	var draft, bindingRequired int
	var generation, bindingRemote sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT source,sequence,seen_at,state,draft,provider,os,report_kind,generation,binding_required,binding_remote
		FROM amux_presence_agents WHERE project_ident=? AND instance_id=? AND session_id=?`, r.project, r.instance, r.session).
		Scan(&source, &sequence, &seenAt, &state, &draft, &provider, &osName, &reportKind, &generation, &bindingRequired, &bindingRemote)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	now, err = nativeDeliveryNow(ctx, tx, time.Now().UTC().UnixMilli())
	if err != nil {
		return false, err
	}
	if source != r.source || sequence != r.sequence || provider != r.provider || osName != r.os || state == "gone" || reportKind != "native" || draft != 0 || now-seenAt >= presenceGoneAfter || !sameNullableGeneration(generation, optionalSQLString(r.generation)) {
		return false, nil
	}
	var liveInstances int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM amux_presence_agents WHERE project_ident=? AND session_id=?
		AND report_kind='native' AND state!='gone' AND ?-seen_at<?`, r.project, r.session, now, presenceGoneAfter).Scan(&liveInstances); err != nil {
		return false, err
	}
	if liveInstances != 1 {
		return false, nil
	}
	var bindingOK int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM projects p WHERE p.ident=lower(trim(?)) AND
		(?=0 OR p.canonical_remote=COALESCE(?,'') OR (p.kind='adhoc' AND p.canonical_remote IS NULL AND COALESCE(?,'')='')))`,
		r.project, bindingRequired, optionalSQLString(bindingRemote), optionalSQLString(bindingRemote)).Scan(&bindingOK); err != nil {
		return false, err
	}
	if bindingOK == 0 {
		return false, nil
	}
	principal, err := nativeSourcePrincipal(ctx, tx, source, sequence)
	if err != nil {
		return false, err
	}
	if !validPresencePrincipal(principal) {
		return false, nil
	}
	conversationID, createdAt, ok, err := uniqueProjectInboxConversation(ctx, tx, r)
	if err != nil || !ok {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO agents(project_ident,agent_id,registered_at) VALUES(?,?,?)`, r.project, r.session, now); err != nil {
		return false, err
	}
	var queued int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM amux_conversation_commands WHERE conversation_id=? AND state IN('queued','claimed') AND kind='prompt'`, conversationID).Scan(&queued); err != nil {
		return false, err
	}
	capacity := 128 - queued
	if capacity <= 0 {
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return false, nil
	}
	if limit > capacity {
		limit = capacity
	}
	messages, err := readUnconfirmedProjectMessages(ctx, tx, r.project, r.session, limit, &createdAt)
	if err != nil {
		return false, err
	}
	for _, message := range messages {
		text := renderProjectInboxPrompt(message)
		commandID := projectInboxCommandID(r.project, r.session, message.ID)
		if _, err := enqueueConversationCommandIn(ctx, tx, r.project, conversationID, commandID, conversation.Command{Kind: "prompt", Text: text}); err != nil {
			return false, err
		}
		result, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO agent_confirmations(agent_id,project_ident,message_id,confirmed_at) VALUES(?,?,?,?)`, r.session, r.project, message.ID, now)
		if err != nil {
			return false, err
		}
		if n, _ := result.RowsAffected(); n != 1 {
			return false, fmt.Errorf("project inbox message %d was confirmed concurrently", message.ID)
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return len(messages) != 0, nil
}

func uniqueProjectInboxConversation(ctx context.Context, q queryer, r projectInboxRecipient) (string, int64, bool, error) {
	rows, err := q.QueryContext(ctx, `SELECT id,created_at FROM amux_conversations WHERE project_ident=? AND task_id IS NULL AND
		json_extract(owner_json,'$.session_id')=? AND json_extract(owner_json,'$.instance_id')=? AND
		json_extract(owner_json,'$.provider')=? AND json_extract(owner_json,'$.os')=? ORDER BY id LIMIT 2`,
		r.project, r.session, r.instance, r.provider, r.os)
	if err != nil {
		return "", 0, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return "", 0, false, rows.Err()
	}
	var id string
	var createdAt int64
	if err := rows.Scan(&id, &createdAt); err != nil {
		return "", 0, false, err
	}
	if rows.Next() {
		return "", 0, false, nil
	}
	return id, createdAt, true, rows.Err()
}

func projectInboxCommandID(project, session string, messageID int64) string {
	name := "amux:project-inbox:" + project + "\x00" + session + "\x00" + strconv.FormatInt(messageID, 10)
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte(name)).String()
}

func renderProjectInboxPrompt(m ProjectMessage) string {
	var b strings.Builder
	b.WriteString("[amux project message]\n")
	metadata := []struct{ name, value string }{
		{"project", m.ProjectIdent}, {"message_id", strconv.FormatInt(m.ID, 10)}, {"source", m.Source},
		{"message_type", m.MessageType}, {"author_kind", stringValue(m.AuthorKind)}, {"agent_id", stringValue(m.AgentID)},
		{"subject", stringValue(m.Subject)}, {"hostname", stringValue(m.Hostname)},
		{"sent_at", strconv.FormatInt(m.SentAt, 10)}, {"event_at", optionalInt64Text(m.EventAt)},
	}
	for _, item := range metadata {
		if item.value == "" {
			continue
		}
		b.WriteString(item.name)
		b.WriteString(": ")
		b.WriteString(projectInboxOneLine(item.value, 512))
		b.WriteByte('\n')
	}
	b.WriteString("\nmessage:\n")
	prefix := b.String()
	body := strings.ToValidUTF8(strings.ReplaceAll(m.Content, "\x00", "\\0"), "�")
	text := prefix + body
	if len(text) <= projectInboxPromptLimit {
		return text
	}
	cue := "\n[message truncated; call project_message_thread with id " + strconv.FormatInt(m.ID, 10) + "]"
	budget := projectInboxPromptLimit - len(prefix) - len(cue)
	if budget < 0 {
		budget = 0
	}
	if budget > len(body) {
		budget = len(body)
	}
	for budget > 0 && !utf8.RuneStart(body[budget]) {
		budget--
	}
	return prefix + body[:budget] + cue
}

func projectInboxOneLine(value string, max int) string {
	value = strings.ToValidUTF8(value, "�")
	var b strings.Builder
	for _, r := range value {
		if unicode.IsControl(r) {
			r = ' '
		}
		if b.Len()+utf8.RuneLen(r) > max {
			break
		}
		b.WriteRune(r)
	}
	return strings.TrimSpace(strings.Join(strings.Fields(b.String()), " "))
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func optionalInt64Text(value *int64) string {
	if value == nil {
		return ""
	}
	return strconv.FormatInt(*value, 10)
}
