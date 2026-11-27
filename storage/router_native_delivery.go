package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/plugins"
	"github.com/nitecon/amux/router"
	"github.com/nitecon/amux/taskplatform"
)

const nativeDeliveryMaxAgeMS int64 = 5 * 60_000

// migrateNativeDelivery is migration 32. Adoption fields intentionally have
// no task foreign key: task mutation can invalidate the offer while the
// already-created initial prompt remains a durable effect.
func migrateNativeDelivery(ctx context.Context, tx *sql.Tx) error {
	_, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS amux_native_delivery_receipts(
	 notify_uid TEXT PRIMARY KEY, project TEXT NOT NULL, task_id TEXT NOT NULL,
	 receiver_json TEXT NOT NULL, source TEXT, principal TEXT,
	 result_principal TEXT, stale_offer INTEGER NOT NULL DEFAULT 0 CHECK(stale_offer IN(0,1)),
	 actual_offer INTEGER NOT NULL DEFAULT 0 CHECK(actual_offer IN(0,1)),
	 expires_at INTEGER NOT NULL, adopted INTEGER NOT NULL DEFAULT 0 CHECK(adopted IN(0,1)),
	 run_id TEXT, conversation_id TEXT, prompt_id TEXT,
	 command_id TEXT, result_source TEXT, status TEXT NOT NULL DEFAULT 'pending'
	 CHECK(status IN('pending','completed','skipped')),
	 created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
	);
	CREATE INDEX IF NOT EXISTS idx_native_delivery_open ON amux_native_delivery_receipts(status,expires_at);
	INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(32);`)
	return err
}

// captureNativeDeliveryAdoptionIn links an assignment offer to its original
// atomic adoption effect before a later worker Resume may replace task owner.
// No matching, still-valid offer is a normal no-op.
func captureNativeDeliveryAdoptionIn(ctx context.Context, tx *sql.Tx, task *Task, owner AgentOrigin, runID, conversationID, promptID string) error {
	if task == nil || task.ID == "" || !canonicalUUID(runID) || !canonicalUUID(conversationID) || !canonicalUUID(promptID) {
		return fmt.Errorf("invalid native delivery adoption context")
	}
	normalizedOwner, err := NormalizeOrigin(&owner)
	if err != nil || normalizedOwner == nil {
		return fmt.Errorf("invalid native delivery adoption owner: %w", err)
	}
	owner = *normalizedOwner
	ownerJSON, err := json.Marshal(owner)
	if err != nil {
		return err
	}
	var uid, project, session string
	var reporterSource sql.NullString
	var originJSON sql.NullString
	var principal sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT o.notify_uid,o.project,o.session_id,o.reporter_source,c.origin_json,c.reporter_principal
	FROM assignment_offers o JOIN amux_assignment_offer_claims c ON c.notify_uid=o.notify_uid
	 WHERE o.task_id=? ORDER BY o.notify_uid DESC LIMIT 1`, task.ID).Scan(&uid, &project, &session, &reporterSource, &originJSON, &principal)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	var offered AgentOrigin
	if !originJSON.Valid || json.Unmarshal([]byte(originJSON.String), &offered) != nil {
		return nil
	}
	normalized, err := NormalizeOrigin(&offered)
	if err != nil || normalized == nil || *normalized != owner || session != owner.SessionID || project != task.ProjectIdent {
		return nil
	}
	var current int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM tasks WHERE id=? AND project_ident=? AND status IN ('todo','in_progress','stalled')
	 AND owner_origin_json=json(?) )`, task.ID, project, string(ownerJSON)).Scan(&current); err != nil {
		return err
	}
	if current == 0 {
		return nil
	}
	var nativeSource sql.NullString
	var nativeSequence sql.NullInt64
	var nativeGeneration sql.NullString
	var nativeSeen sql.NullInt64
	var nativeGoal sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT source,sequence,generation,seen_at,goal FROM amux_presence_agents WHERE project_ident=? AND instance_id=? AND session_id=?
	 AND provider=? AND os=? AND report_kind='native' AND state!='gone'`, project, owner.InstanceID, owner.SessionID, owner.Provider, owner.OS).Scan(&nativeSource, &nativeSequence, &nativeGeneration, &nativeSeen, &nativeGoal)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	nativePrincipal := ""
	if err == nil && nativeSource.Valid && nativeSequence.Valid {
		clockNow, clockErr := nativeDeliveryNow(ctx, tx, time.Now().UTC().UnixMilli())
		if clockErr != nil {
			return clockErr
		}
		if clockNow-nativeSeen.Int64 >= presenceGoneAfter {
			return nil
		}
		nativePrincipal, err = nativeSourcePrincipal(ctx, tx, nativeSource.String, nativeSequence.Int64)
		if err != nil {
			return err
		}
	}
	var promptExists int
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM amux_assignment_runs r JOIN amux_conversation_commands c
	 ON c.conversation_id=r.conversation_id AND c.id=r.prompt_id WHERE r.run_id=? AND r.task_id=?
	 AND r.session_id=? AND r.conversation_id=? AND r.prompt_id=? AND c.sequence=1 AND c.kind='prompt')`, runID, task.ID, owner.SessionID, conversationID, promptID).Scan(&promptExists); err != nil {
		return err
	}
	if promptExists == 0 {
		return fmt.Errorf("native adoption is missing its durable initial prompt")
	}
	receiver := nativeReceiver{SessionID: owner.SessionID, InstanceID: owner.InstanceID, Provider: owner.Provider, OS: owner.OS, Generation: optionalSQLString(nativeGeneration), Goal: optionalSQLString(nativeGoal)}
	receiverJSON, err := json.Marshal(receiver)
	if err != nil {
		return err
	}
	var emittedAt sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT emitted_at FROM router_events WHERE uid=?`, uid).Scan(&emittedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	now, err := nativeDeliveryNow(ctx, tx, time.Now().UTC().UnixMilli())
	if err != nil {
		return err
	}
	if !emittedAt.Valid {
		return nil
	}
	expiresAt := nativeDeliveryDeadline(emittedAt.Int64)
	var resultSource *string
	if reporterSource.Valid && reporterSource.String != "" && validPrincipal(principal) != nil {
		resultSource = &reporterSource.String
	}
	if resultSource == nil {
		resultSource = stringPointer("plugin:native-delivery")
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO amux_native_delivery_receipts
	 (notify_uid,project,task_id,receiver_json,source,principal,result_principal,expires_at,adopted,actual_offer,run_id,conversation_id,prompt_id,command_id,result_source,status,created_at,updated_at)
	 VALUES(?,?,?,?,?,?,?, ?,1,1,?,?,?,?,?,'pending',?,?)
	 ON CONFLICT(notify_uid) DO UPDATE SET adopted=1,run_id=excluded.run_id,
	 conversation_id=excluded.conversation_id,prompt_id=excluded.prompt_id,command_id=excluded.command_id,
	 result_source=excluded.result_source,result_principal=excluded.result_principal,
	 updated_at=excluded.updated_at WHERE amux_native_delivery_receipts.status='pending'
	 AND amux_native_delivery_receipts.project=excluded.project AND amux_native_delivery_receipts.task_id=excluded.task_id
	 AND amux_native_delivery_receipts.receiver_json=excluded.receiver_json
	 AND amux_native_delivery_receipts.source IS excluded.source AND amux_native_delivery_receipts.principal IS excluded.principal`, uid, project, task.ID, string(receiverJSON), nullableString(stringPointer(nativeSource.String)), nullableString(stringPointer(nativePrincipal)), nullableString(validPrincipal(principal)), expiresAt, runID, conversationID, promptID, promptID, nullableString(resultSource), now, now)
	return err
}

func validPrincipal(value sql.NullString) *string {
	if !value.Valid || !validPresencePrincipal(value.String) {
		return nil
	}
	return &value.String
}

func nativeDeliveryNow(ctx context.Context, q queryer, wall int64) (int64, error) {
	var observed sql.NullInt64
	if err := q.QueryRowContext(ctx, `SELECT observed_at FROM amux_presence_clock WHERE id=1`).Scan(&observed); err != nil {
		return 0, err
	}
	if !observed.Valid {
		return 0, fmt.Errorf("presence clock has no persisted observation")
	}
	if observed.Int64 > wall {
		return observed.Int64, nil
	}
	return wall, nil
}

func nativeDeliveryDeadline(emittedAt int64) int64 {
	if emittedAt > int64(^uint64(0)>>1)-nativeDeliveryMaxAgeMS {
		return int64(^uint64(0) >> 1)
	}
	return emittedAt + nativeDeliveryMaxAgeMS
}

func validateNativeDeliveryBinding(ctx context.Context, q queryer, i nativeNoticeIdentity, now int64) (bool, bool, error) {
	if i.receiver.SessionID == "" || i.receiver.InstanceID == "" || i.source == "" {
		return false, false, nil
	}
	rows, err := q.QueryContext(ctx, `SELECT instance_id,provider,os,source,sequence,generation FROM amux_presence_agents
	 WHERE project_ident=? AND session_id=? AND report_kind='native' AND state!='gone' AND ?-seen_at<?`, i.project, i.receiver.SessionID, now, presenceGoneAfter)
	if err != nil {
		return false, false, err
	}
	defer rows.Close()
	count, exact := 0, false
	var sequence int64
	var source string
	for rows.Next() {
		var instance, provider, osName, rowSource string
		var seq int64
		var generation sql.NullString
		if err = rows.Scan(&instance, &provider, &osName, &rowSource, &seq, &generation); err != nil {
			return false, false, err
		}
		count++
		if instance == i.receiver.InstanceID && provider == i.receiver.Provider && osName == i.receiver.OS && rowSource == i.source && sameNullableGeneration(generation, i.receiver.Generation) {
			exact = true
			sequence = seq
			source = rowSource
		}
	}
	if err = rows.Err(); err != nil {
		return false, false, err
	}
	if count == 0 {
		return false, false, nil
	}
	if !exact {
		return false, true, nil
	}
	principal, err := nativeSourcePrincipal(ctx, q, source, sequence)
	if err != nil {
		return false, false, err
	}
	if principal != i.principal {
		return false, true, nil
	}
	return principal != "", false, nil
}

func sameNullableGeneration(generation sql.NullString, frozen *string) bool {
	if frozen == nil {
		return !generation.Valid
	}
	return generation.Valid && generation.String == *frozen
}

func validateNativeAdoptionProof(ctx context.Context, q queryer, i nativeNoticeIdentity) (bool, error) {
	if !canonicalUUID(i.runID) || !canonicalUUID(i.conversationID) || !canonicalUUID(i.promptID) || i.commandID != i.promptID {
		return false, nil
	}
	var found int
	err := q.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM amux_conversations c JOIN tasks t ON t.id=c.task_id
	 JOIN amux_conversation_commands p ON p.conversation_id=c.id WHERE c.id=? AND c.project_ident=? AND c.task_id=?
	 AND t.project_ident=? AND json_extract(c.owner_json,'$.session_id')=?
	 AND json_extract(c.owner_json,'$.provider')=? AND json_extract(c.owner_json,'$.os')=?
	 AND p.id=? AND p.sequence=1 AND p.kind='prompt')`, i.conversationID, i.project, i.taskID, i.project,
		i.receiver.SessionID, i.receiver.Provider, i.receiver.OS, i.promptID).Scan(&found)
	return found != 0, err
}

// ReconcileNativeDelivery freezes the chosen receiver once and atomically
// queues any prompt, records the result, and emits the correlated result event.
// pending asks the durable router to retry the same event.
func (s *Store) ReconcileNativeDelivery(ctx context.Context, registry *router.Registry, event router.Event) (bool, error) {
	if event.ID != plugins.NotifyEventID || event.Version != 1 || !canonicalUUID(event.UID) || registry == nil {
		return false, fmt.Errorf("native delivery requires a valid agent.notify v1 event and registry")
	}
	var notice nativeNotice
	if err := json.Unmarshal(event.Payload, &notice); err != nil {
		return false, fmt.Errorf("invalid agent.notify payload: %w", err)
	}
	if err := plugins.NotificationEventDefinitions()[0].Validate(event.Payload); err != nil {
		return false, err
	}
	notice.TaskID = notificationTaskID(notice.Event.Task)
	notice.unsupported = strings.TrimSpace(notice.Target) == "" || strings.TrimSpace(notice.Project) == "" || notice.TaskID == ""
	notice.receiver, _ = decodeNativeReceiver(notice.Receiver)
	notice.reminder = rawBool(notice.Reminder)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	now, err := nativeDeliveryNow(ctx, tx, time.Now().UTC().UnixMilli())
	if err != nil {
		return false, err
	}
	identity, err := freezeNativeNotice(ctx, tx, event, notice, now)
	if err != nil {
		return false, err
	}
	if identity.status != "pending" {
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return false, nil
	}
	if identity.staleOffer {
		if err := completeNativeNotice(ctx, tx, registry, event, identity, "skipped", "stale-assignment-offer", now); err != nil {
			return false, err
		}
		if err := tx.Commit(); err != nil {
			return false, err
		}
		_ = s.bus.Publish(eventbus.Event{Topic: eventbus.RouterChanged})
		return false, nil
	}
	if notice.unsupported {
		if err := completeNativeNotice(ctx, tx, registry, event, identity, "skipped", "unsupported-native-context", now); err != nil {
			return false, err
		}
		if err := tx.Commit(); err != nil {
			return false, err
		}
		_ = s.bus.Publish(eventbus.Event{Topic: eventbus.RouterChanged})
		return false, nil
	}
	if identity.adopted {
		valid, err := validateNativeAdoptionProof(ctx, tx, identity)
		if err != nil {
			return false, err
		}
		if !valid {
			return false, fmt.Errorf("adopted notification has no durable initial-prompt proof")
		}
		if err := completeNativeNotice(ctx, tx, registry, event, identity, "delivered", "adopted", now); err != nil {
			return false, err
		}
		if err := tx.Commit(); err != nil {
			return false, err
		}
		_ = s.bus.Publish(eventbus.Event{Topic: eventbus.RouterChanged})
		return false, nil
	}
	if now >= identity.expiresAt {
		if err := completeNativeNotice(ctx, tx, registry, event, identity, "skipped", "expired", now); err != nil {
			return false, err
		}
		if err := tx.Commit(); err != nil {
			return false, err
		}
		_ = s.bus.Publish(eventbus.Event{Topic: eventbus.RouterChanged})
		return false, nil
	}
	if !identity.frozen {
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return true, nil
	}
	if identity.actualOffer {
		// An actual offer without its atomic adoption proof must never be treated
		// as an ordinary reminder or create a second prompt.
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return true, nil
	}
	if notice.Mode == "assigned" && notice.reminder {
		current, err := readTask(ctx, tx, identity.project, identity.taskID)
		if err != nil && !errors.Is(err, ErrTaskNotFound) {
			return false, err
		}
		ownerMatches := current != nil && (current.Status == "in_progress" || current.Status == "stalled") && current.OwnerOrigin != nil && *current.OwnerOrigin == identity.receiver.origin()
		if !ownerMatches {
			if err := completeNativeNotice(ctx, tx, registry, event, identity, "skipped", "task-owner-changed", now); err != nil {
				return false, err
			}
			if err := tx.Commit(); err != nil {
				return false, err
			}
			_ = s.bus.Publish(eventbus.Event{Topic: eventbus.RouterChanged})
			return false, nil
		}
	}
	bound, replaced, err := validateNativeDeliveryBinding(ctx, tx, identity, now)
	if err != nil {
		return false, err
	}
	if replaced {
		if err := completeNativeNotice(ctx, tx, registry, event, identity, "skipped", "receiver-rebound", now); err != nil {
			return false, err
		}
		if err := tx.Commit(); err != nil {
			return false, err
		}
		_ = s.bus.Publish(eventbus.Event{Topic: eventbus.RouterChanged})
		return false, nil
	}
	if !bound {
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return true, nil
	}
	if captured, ok := capturedRequesterResult(notice, identity); notice.RequireIdle && ok {
		current, err := readTask(ctx, tx, identity.project, identity.taskID)
		if err != nil && !errors.Is(err, ErrTaskNotFound) {
			return false, err
		}
		if !currentTaskRequesterMatches(current, identity, captured) {
			if err := completeNativeNotice(ctx, tx, registry, event, identity, "skipped", "requester-subscription-changed", now); err != nil {
				return false, err
			}
			if err := tx.Commit(); err != nil {
				return false, err
			}
			_ = s.bus.Publish(eventbus.Event{Topic: eventbus.RouterChanged})
			return false, nil
		}
	}
	conversationID, ready, err := nativeNoticeConversation(ctx, tx, identity, notice)
	if err != nil {
		return false, err
	}
	if !ready {
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return true, nil
	}
	if notice.RequireIdle {
		var draft, prompts int
		if err := tx.QueryRowContext(ctx, `SELECT p.draft,(SELECT COUNT(*) FROM amux_conversation_commands c
		 WHERE c.conversation_id=? AND c.kind='prompt' AND c.state IN('queued','claimed'))
		 FROM amux_presence_agents p WHERE p.project_ident=? AND p.instance_id=? AND p.session_id=? AND p.source=? AND p.report_kind='native'`, conversationID, identity.project, identity.receiver.InstanceID, identity.receiver.SessionID, identity.source).Scan(&draft, &prompts); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return true, tx.Commit()
			}
			return false, err
		}
		if draft != 0 || prompts != 0 {
			if err := tx.Commit(); err != nil {
				return false, err
			}
			return true, nil
		}
	}
	text, err := renderNativeNotification(event.Payload)
	if err != nil {
		return false, err
	}
	commandID := event.UID
	command, err := enqueueConversationCommandIn(ctx, tx, identity.project, conversationID, commandID, conversation.Command{Kind: "prompt", Text: text})
	if err != nil {
		return false, err
	}
	identity.commandID, identity.conversationID = command.ID, conversationID
	if err := completeNativeNotice(ctx, tx, registry, event, identity, "delivered", "queued", now); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	s.wakeConversations()
	_ = s.bus.Publish(eventbus.Event{Topic: eventbus.RouterChanged})
	return false, nil
}

type nativeNotice struct {
	Target      string          `json:"target"`
	Project     string          `json:"project"`
	Mode        string          `json:"mode"`
	RequireIdle bool            `json:"require_idle"`
	Reminder    json.RawMessage `json:"reminder"`
	Receiver    json.RawMessage `json:"receiver"`
	TaskID      string          `json:"-"`
	reminder    bool
	receiver    *nativeReceiver
	unsupported bool
	Event       struct {
		Task    json.RawMessage `json:"task"`
		Comment json.RawMessage `json:"comment"`
		Kind    string          `json:"kind"`
	} `json:"event"`
}

func rawBool(raw json.RawMessage) bool { var value bool; _ = json.Unmarshal(raw, &value); return value }

func notificationTaskID(raw json.RawMessage) string {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return ""
	}
	var id string
	if err := json.Unmarshal(fields["id"], &id); err != nil || id == "" {
		return ""
	}
	return id
}

func notificationTaskOwner(raw json.RawMessage) *AgentOrigin {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return nil
	}
	var owner AgentOrigin
	if json.Unmarshal(fields["owner_origin"], &owner) != nil {
		return nil
	}
	normalized, err := NormalizeOrigin(&owner)
	if err != nil {
		return nil
	}
	return normalized
}

func capturedRequesterResult(notice nativeNotice, identity nativeNoticeIdentity) (RegisteredTargetIdentity, bool) {
	if !isNotifierResult(notice.Event.Kind) || notice.Mode != "recipient" {
		return RegisteredTargetIdentity{}, false
	}
	var task struct {
		ID          string          `json:"id"`
		Project     string          `json:"project_ident"`
		Requester   json.RawMessage `json:"requester"`
		OwnerOrigin json.RawMessage `json:"owner_origin"`
	}
	if json.Unmarshal(notice.Event.Task, &task) != nil || task.ID != identity.taskID || task.Project != identity.project || len(task.Requester) == 0 || isJSONNull(task.Requester) {
		return RegisteredTargetIdentity{}, false
	}
	var requester RegisteredTargetIdentity
	if json.Unmarshal(task.Requester, &requester) != nil || !canonicalUUID(requester.Generation) || requester.Generation == "00000000-0000-0000-0000-000000000000" {
		return RegisteredTargetIdentity{}, false
	}
	origin, err := NormalizeOrigin(&requester.Origin)
	if err != nil || origin == nil || origin.SessionID != notice.Target {
		return RegisteredTargetIdentity{}, false
	}
	if owner := notificationTaskOwner(notice.Event.Task); owner != nil && *owner == *origin {
		return RegisteredTargetIdentity{}, false
	}
	requester.Origin = *origin
	return requester, true
}

func currentTaskRequesterMatches(task *Task, identity nativeNoticeIdentity, captured RegisteredTargetIdentity) bool {
	if task == nil || task.ProjectIdent != identity.project || task.ID != identity.taskID || captured.Origin != identity.receiver.origin() ||
		identity.receiver.Generation == nil || captured.Generation != *identity.receiver.Generation || !taskplatform.Matches(task.Labels, identity.receiver.OS) {
		return false
	}
	if task.Goal != nil && (identity.receiver.Goal == nil || *task.Goal != *identity.receiver.Goal) {
		return false
	}
	if len(task.Requester) == 0 || isJSONNull(task.Requester) {
		return false
	}
	var current RegisteredTargetIdentity
	if json.Unmarshal(task.Requester, &current) != nil || current != captured || !canonicalUUID(current.Generation) || current.Generation == "00000000-0000-0000-0000-000000000000" {
		return false
	}
	origin, err := NormalizeOrigin(&current.Origin)
	return err == nil && origin != nil && *origin == current.Origin
}

func decodeNativeReceiver(raw json.RawMessage) (*nativeReceiver, error) {
	if len(raw) == 0 || isJSONNull(raw) {
		return nil, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
		return nil, fmt.Errorf("receiver must be an object")
	}
	r := &nativeReceiver{}
	_ = json.Unmarshal(fields["session_id"], &r.SessionID)
	_ = json.Unmarshal(fields["instance_id"], &r.InstanceID)
	_ = json.Unmarshal(fields["provider"], &r.Provider)
	_ = json.Unmarshal(fields["os"], &r.OS)
	var generation string
	if json.Unmarshal(fields["generation"], &generation) == nil && generation != "" {
		r.Generation = &generation
	}
	return r, nil
}

type nativeReceiver struct {
	SessionID  string  `json:"session_id"`
	InstanceID string  `json:"instance_id"`
	Provider   string  `json:"provider"`
	OS         string  `json:"os"`
	Generation *string `json:"generation,omitempty"`
	Goal       *string `json:"goal,omitempty"`
}

func (r nativeReceiver) origin() AgentOrigin {
	return AgentOrigin{SessionID: r.SessionID, InstanceID: r.InstanceID, Provider: r.Provider, OS: r.OS}
}

type nativeNoticeIdentity struct {
	uid, project, taskID, source, principal, resultSource, resultPrincipal, status, conversationID, commandID string
	runID, promptID                                                                                           string
	receiver                                                                                                  nativeReceiver
	expiresAt                                                                                                 int64
	actualOffer, adopted, staleOffer, frozen                                                                  bool
}

func freezeNativeNotice(ctx context.Context, tx *sql.Tx, event router.Event, notice nativeNotice, now int64) (nativeNoticeIdentity, error) {
	i := nativeNoticeIdentity{uid: event.UID, project: notice.Project, taskID: notice.TaskID, status: "pending", expiresAt: nativeDeliveryDeadline(event.EmittedAt)}
	var receiverJSON, source, principal, resultSource, resultPrincipal, status sql.NullString
	var conversationID, commandID, runID, promptID sql.NullString
	var adopted, actualOffer, staleOffer int
	err := tx.QueryRowContext(ctx, `SELECT receiver_json,source,principal,result_source,result_principal,status,adopted,actual_offer,stale_offer,expires_at,conversation_id,command_id,run_id,prompt_id FROM amux_native_delivery_receipts WHERE notify_uid=?`, event.UID).Scan(&receiverJSON, &source, &principal, &resultSource, &resultPrincipal, &status, &adopted, &actualOffer, &staleOffer, &i.expiresAt, &conversationID, &commandID, &runID, &promptID)
	if err == nil {
		if err = json.Unmarshal([]byte(receiverJSON.String), &i.receiver); err != nil {
			return i, err
		}
		i.source, i.principal, i.resultSource, i.resultPrincipal, i.status = source.String, principal.String, resultSource.String, resultPrincipal.String, status.String
		i.conversationID, i.commandID = conversationID.String, commandID.String
		i.runID, i.promptID = runID.String, promptID.String
		i.adopted = adopted != 0
		i.actualOffer = actualOffer != 0
		i.staleOffer = staleOffer != 0
		i.frozen = true
		return i, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return i, err
	}
	if notice.unsupported {
		i.receiver = nativeReceiver{SessionID: notice.Target}
		i.frozen = true
		raw, _ := json.Marshal(i.receiver)
		_, err = tx.ExecContext(ctx, `INSERT INTO amux_native_delivery_receipts(notify_uid,project,task_id,receiver_json,stale_offer,actual_offer,expires_at,status,created_at,updated_at)
		 VALUES(?,?,?, ?,0,0,?,'pending',?,?)`, i.uid, i.project, i.taskID, string(raw), i.expiresAt, now, now)
		return i, err
	}
	i.actualOffer = event.Source == "plugin:assignment" && notice.Mode == "assigned"
	if i.actualOffer {
		i.frozen = true
		var taskID, project, session string
		var originJSON, reporterSource, reporterPrincipal sql.NullString
		err = tx.QueryRowContext(ctx, `SELECT o.task_id,o.project,o.session_id,c.origin_json,o.reporter_source,c.reporter_principal
		 FROM assignment_offers o JOIN amux_assignment_offer_claims c USING(notify_uid) WHERE o.notify_uid=?`, event.UID).Scan(&taskID, &project, &session, &originJSON, &reporterSource, &reporterPrincipal)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return i, err
		}
		if err != nil || taskID != i.taskID || project != i.project || session != notice.Target {
			i.staleOffer = true
			i.receiver.SessionID = notice.Target
		} else {
			var captured AgentOrigin
			if !originJSON.Valid || json.Unmarshal([]byte(originJSON.String), &captured) != nil {
				i.staleOffer = true
			} else {
				normalized, normalizeErr := NormalizeOrigin(&captured)
				taskOwner := notificationTaskOwner(notice.Event.Task)
				if normalizeErr != nil || normalized == nil || normalized.SessionID != session || taskOwner == nil || *taskOwner != *normalized {
					i.staleOffer = true
				} else {
					i.receiver = nativeReceiver{SessionID: normalized.SessionID, InstanceID: normalized.InstanceID, Provider: normalized.Provider, OS: normalized.OS}
				}
			}
			if reporterSource.Valid && validRouterSource(reporterSource.String) && validPrincipal(reporterPrincipal) != nil {
				i.resultSource = reporterSource.String
				i.resultPrincipal = reporterPrincipal.String
			}
		}
	} else if notice.receiver != nil {
		i.receiver = *notice.receiver
		i.frozen = true
	} else {
		// Periodic reminders name the task owner in the immutable task snapshot.
		if notice.Mode == "assigned" && notice.reminder {
			o := notificationTaskOwner(notice.Event.Task)
			if o == nil {
				i.staleOffer = true
				i.frozen = true
			} else {
				i.receiver = nativeReceiver{SessionID: o.SessionID, InstanceID: o.InstanceID, Provider: o.Provider, OS: o.OS}
				i.frozen = true
			}
		} else {
			i.receiver.SessionID = notice.Target
		}
	}
	if i.receiver.SessionID == "" {
		i.receiver.SessionID = notice.Target
	}
	var matches []struct {
		project, instance, provider, os, source string
		sequence, seen                          int64
		generation                              sql.NullString
		goal                                    sql.NullString
		draft                                   int
		state                                   string
	}
	if !i.staleOffer {
		rows, err := tx.QueryContext(ctx, `SELECT project_ident,instance_id,provider,os,source,sequence,seen_at,generation,draft,state,goal
	 FROM amux_presence_agents WHERE project_ident=? AND session_id=? AND report_kind='native' AND state!='gone'
	 AND ?-seen_at<? AND (?='' OR instance_id=?) ORDER BY seen_at DESC`, notice.Project, i.receiver.SessionID, now, presenceGoneAfter, i.receiver.InstanceID, i.receiver.InstanceID)
		if err != nil {
			return i, err
		}
		for rows.Next() {
			var x struct {
				project, instance, provider, os, source string
				sequence, seen                          int64
				generation                              sql.NullString
				goal                                    sql.NullString
				draft                                   int
				state                                   string
			}
			if err = rows.Scan(&x.project, &x.instance, &x.provider, &x.os, &x.source, &x.sequence, &x.seen, &x.generation, &x.draft, &x.state, &x.goal); err != nil {
				rows.Close()
				return i, err
			}
			matches = append(matches, x)
		}
		if err = rows.Err(); err != nil {
			rows.Close()
			return i, err
		}
		rows.Close()
	}
	if len(matches) == 1 {
		x := matches[0]
		if i.receiver.Provider != "" && i.receiver.Provider != x.provider || i.receiver.OS != "" && i.receiver.OS != x.os || i.receiver.Generation != nil && (!x.generation.Valid || *i.receiver.Generation != x.generation.String) {
			return i, fmt.Errorf("notification receiver does not match its current native binding")
		}
		i.receiver = nativeReceiver{SessionID: i.receiver.SessionID, InstanceID: x.instance, Provider: x.provider, OS: x.os, Generation: optionalSQLString(x.generation), Goal: optionalSQLString(x.goal)}
		i.frozen = true
		i.source = x.source
		p, e := nativeSourcePrincipal(ctx, tx, x.source, x.sequence)
		if e != nil {
			return i, e
		}
		i.principal = p
		if p != "" {
			i.resultSource = x.source
			i.resultPrincipal = p
		}
	}
	if len(matches) > 1 {
		i.frozen = false
	}
	if !i.frozen && now >= i.expiresAt {
		i.frozen = true
	}
	if !i.frozen {
		return i, nil
	}
	raw, _ := json.Marshal(i.receiver)
	_, err = tx.ExecContext(ctx, `INSERT INTO amux_native_delivery_receipts(notify_uid,project,task_id,receiver_json,source,principal,result_principal,stale_offer,actual_offer,expires_at,result_source,status,created_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,'pending',?,?) ON CONFLICT(notify_uid) DO NOTHING`, i.uid, i.project, i.taskID, string(raw), nullableString(stringPointer(i.source)), nullableString(stringPointer(i.principal)), nullableString(stringPointer(i.resultPrincipal)), i.staleOffer, i.actualOffer, i.expiresAt, nullableString(stringPointer(i.resultSource)), now, now)
	return i, err
}

func stringPointer(v string) *string {
	if v == "" {
		return nil
	}
	return &v
}

func nativeNoticeConversation(ctx context.Context, tx *sql.Tx, i nativeNoticeIdentity, n nativeNotice) (string, bool, error) {
	if i.receiver.InstanceID == "" {
		return "", false, nil
	}
	base := `SELECT id FROM amux_conversations WHERE project_ident=? AND json_extract(owner_json,'$.session_id')=?
	 AND json_extract(owner_json,'$.instance_id')=? AND json_extract(owner_json,'$.provider')=? AND json_extract(owner_json,'$.os')=?`
	args := []any{i.project, i.receiver.SessionID, i.receiver.InstanceID, i.receiver.Provider, i.receiver.OS}
	if n.TaskID != "" {
		ids, err := conversationIDs(ctx, tx, base+` AND task_id=? ORDER BY id LIMIT 2`, append(args, n.TaskID)...)
		if err != nil {
			return "", false, err
		}
		if len(ids) == 1 {
			return ids[0], true, nil
		}
		if len(ids) > 1 {
			return "", false, nil
		}
	}
	ids, err := conversationIDs(ctx, tx, base+` ORDER BY id LIMIT 2`, args...)
	if err != nil {
		return "", false, err
	}
	if len(ids) != 1 {
		return "", false, nil
	}
	return ids[0], true, nil
}

func conversationIDs(ctx context.Context, tx *sql.Tx, query string, args ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ids := []string{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func completeNativeNotice(ctx context.Context, tx *sql.Tx, registry *router.Registry, event router.Event, i nativeNoticeIdentity, status, reason string, now int64) error {
	resultSource := i.resultSource
	if resultSource == "" || i.resultPrincipal == "" {
		resultSource = "plugin:native-delivery"
	}
	payload, _ := json.Marshal(map[string]any{"target": i.receiver.SessionID, "status": status, "reason": reason})
	correlation := event.UID
	emitted, err := emitRouterEventIn(ctx, tx, registry, router.EmitRequest{ID: plugins.NotifyResultEventID, Version: 1, Source: resultSource, Payload: payload, Correlation: &correlation})
	if err != nil {
		return err
	}
	if !emitted.Stored && !emitted.Replayed {
		return fmt.Errorf("native notification result has no durable subscribers")
	}
	receiptStatus := status
	if status == "delivered" {
		receiptStatus = "completed"
	}
	_, err = tx.ExecContext(ctx, `UPDATE amux_native_delivery_receipts SET status=?,conversation_id=COALESCE(NULLIF(? ,''),conversation_id),command_id=COALESCE(NULLIF(? ,''),command_id),result_source=?,result_principal=?,updated_at=? WHERE notify_uid=? AND status='pending'`, receiptStatus, i.conversationID, i.commandID, resultSource, i.resultPrincipal, now, event.UID)
	return err
}
