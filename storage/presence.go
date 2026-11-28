package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/identity/agent"
	projectid "github.com/nitecon/amux/identity/project"
)

const (
	presenceIdleAfter = int64(30_000)
	presenceGoneAfter = int64(90_000)
	presenceMaxAgents = 256
)

var (
	ErrPresenceInput    = errors.New("invalid presence report")
	ErrPresenceConflict = errors.New("presence identity is owned by another live source")
)

type PresenceReport struct {
	Source    string          `json:"source"`
	Sequence  int64           `json:"sequence"`
	ClientRun *string         `json:"client_run,omitempty"`
	OS        string          `json:"os,omitempty"`
	Hostname  *string         `json:"hostname,omitempty"`
	Agents    []PresenceAgent `json:"agents"`
}

type PresenceAgent struct {
	ProjectIdent       string       `json:"project_ident"`
	Origin             agent.Origin `json:"origin"`
	Hostname           *string      `json:"hostname,omitempty"`
	Goal               *string      `json:"goal,omitempty"`
	Generation         *string      `json:"generation,omitempty"`
	AgentID            *string      `json:"agent_id,omitempty"`
	Busy               bool         `json:"busy"`
	Draft              bool         `json:"draft"`
	AutoAssign         bool         `json:"auto_assign"`
	ReviewContextClear bool         `json:"review_context_clear"`
	ExecutionIdle      bool         `json:"execution_idle,omitempty"`
	LastActivityAt     *int64       `json:"last_activity_at,omitempty"`
}

type AgentPresence struct {
	PresenceAgent
	Source string `json:"source"`
	SeenAt int64  `json:"seen_at"`
	State  string `json:"state"`
}

func migratePresence(ctx context.Context, tx *sql.Tx) error {
	if _, err := tx.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS amux_presence_clock(id INTEGER PRIMARY KEY CHECK(id=1),observed_at INTEGER NOT NULL);
 INSERT OR IGNORE INTO amux_presence_clock(id,observed_at) VALUES(1,0);
 CREATE TABLE IF NOT EXISTS amux_presence_sources(source TEXT PRIMARY KEY,sequence INTEGER NOT NULL);
 CREATE TABLE IF NOT EXISTS amux_presence_agents(
  project_ident TEXT NOT NULL,
  instance_id TEXT NOT NULL,session_id TEXT NOT NULL,provider TEXT NOT NULL,os TEXT NOT NULL,source TEXT NOT NULL,
  sequence INTEGER NOT NULL,seen_at INTEGER NOT NULL,last_activity_at INTEGER,
  busy INTEGER NOT NULL CHECK(busy IN(0,1)),draft INTEGER NOT NULL CHECK(draft IN(0,1)),auto_assign INTEGER NOT NULL DEFAULT 0 CHECK(auto_assign IN(0,1)),
  state TEXT NOT NULL CHECK(state IN('unknown','busy','idle','gone')),
  PRIMARY KEY(project_ident,instance_id,session_id));
 CREATE INDEX IF NOT EXISTS amux_presence_source ON amux_presence_agents(source,state,seen_at);
 INSERT OR IGNORE INTO amux_go_migrations(version) VALUES(8);`); err != nil {
		return err
	}
	hasExecutionIdle, err := sqliteTableHasColumn(ctx, tx, "amux_presence_agents", "execution_idle")
	if err != nil || hasExecutionIdle {
		return err
	}
	_, err = tx.ExecContext(ctx, "ALTER TABLE amux_presence_agents ADD COLUMN execution_idle INTEGER NOT NULL DEFAULT 0 CHECK(execution_idle IN (0,1))")
	return err
}

func presenceState(busy, draft, executionIdle bool, activity *int64, now int64) string {
	if busy || draft {
		return "busy"
	}
	if executionIdle {
		return "idle"
	}
	if activity == nil {
		return "unknown"
	}
	if now-*activity >= presenceIdleAfter {
		return "idle"
	}
	return "busy"
}

func validatePresenceReport(report PresenceReport, now time.Time) (PresenceReport, error) {
	invalid := func(message string) (PresenceReport, error) {
		return PresenceReport{}, fmt.Errorf("%w: %s", ErrPresenceInput, message)
	}
	if !validRouterSource(report.Source) {
		return invalid("source must be 1-256 bytes with no surrounding whitespace or control characters")
	}
	if report.Sequence < 0 {
		return invalid("sequence must be non-negative")
	}
	if report.ClientRun != nil && !canonicalUUID(*report.ClientRun) {
		return invalid("client_run must be a canonical UUID")
	}
	if report.OS != "" {
		report.OS = strings.TrimSpace(report.OS)
		if report.OS != "linux" && report.OS != "macos" && report.OS != "windows" {
			return invalid("os must be linux, macos, or windows")
		}
	}
	if report.Hostname != nil {
		hostname, err := normalizePresenceHostname(report.Hostname)
		if err != nil {
			return invalid(err.Error())
		}
		report.Hostname = hostname
	}
	if len(report.Agents) > presenceMaxAgents {
		return invalid("too many agents")
	}
	report.Agents = append([]PresenceAgent(nil), report.Agents...)
	nowMS := now.UnixMilli()
	seen := make(map[string]struct{}, len(report.Agents))
	for i := range report.Agents {
		entry := &report.Agents[i]
		goal, err := NormalizeTaskGoal(entry.Goal)
		if err != nil {
			return invalid(err.Error())
		}
		entry.Goal = goal
		if entry.Generation != nil && !canonicalUUID(*entry.Generation) {
			return invalid("generation must be a canonical UUID")
		}
		if entry.AgentID != nil {
			id := strings.TrimSpace(*entry.AgentID)
			if id == "" {
				entry.AgentID = nil
			} else if len(id) > 128 || strings.IndexFunc(id, unicode.IsControl) >= 0 {
				return invalid("agent_id must be at most 128 UTF-8 bytes and contain no control characters")
			} else {
				entry.AgentID = &id
			}
		}
		if entry.Hostname != nil {
			entry.Hostname, err = normalizePresenceHostname(entry.Hostname)
			if err != nil {
				return invalid(err.Error())
			}
		}
		entry.ProjectIdent = projectid.NormalizeIdent(entry.ProjectIdent)
		if entry.ProjectIdent == "" {
			return invalid("project identifier is required")
		}
		origin, err := agent.NormalizeOrigin(&entry.Origin)
		if err != nil {
			return invalid(err.Error())
		}
		entry.Origin = *origin
		if entry.LastActivityAt != nil && (*entry.LastActivityAt < 0 || *entry.LastActivityAt > nowMS) {
			return invalid("last activity cannot be in the future or negative")
		}
		if _, ok := seen[entry.Origin.SessionID]; ok {
			return invalid("duplicate session")
		}
		seen[entry.Origin.SessionID] = struct{}{}
	}
	return report, nil
}

func normalizePresenceHostname(value *string) (*string, error) {
	if value == nil {
		return nil, nil
	}
	hostname := strings.TrimSpace(*value)
	if hostname == "" {
		return nil, nil
	}
	if len(hostname) > 128 || strings.IndexFunc(hostname, unicode.IsControl) >= 0 {
		return nil, errors.New("hostname must be at most 128 UTF-8 bytes and contain no control characters")
	}
	return &hostname, nil
}

// ReportPresence applies a complete, sequence-fenced snapshot for one source.
func (s *Store) ReportPresence(ctx context.Context, report PresenceReport, now time.Time) (bool, error) {
	return s.reportPresence(ctx, report, now, "")
}

// ReportPresenceWithPrincipal applies an authenticated complete snapshot. The
// principal is supplied by the trusted server authentication boundary; it is
// never read from the report payload.
func (s *Store) ReportPresenceWithPrincipal(ctx context.Context, report PresenceReport, now time.Time, principal string) (bool, error) {
	if !validPresencePrincipal(principal) {
		return false, fmt.Errorf("%w: invalid authenticated principal", ErrPresenceInput)
	}
	return s.reportPresence(ctx, report, now, principal)
}

func (s *Store) reportPresence(ctx context.Context, report PresenceReport, now time.Time, principal string) (bool, error) {
	report, err := validatePresenceReport(report, now)
	if err != nil {
		return false, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var lastSequence int64
	err = tx.QueryRowContext(ctx, "SELECT sequence FROM amux_presence_sources WHERE source=?", report.Source).Scan(&lastSequence)
	if err == nil && principal != "" {
		var existing sql.NullString
		if err := tx.QueryRowContext(ctx, "SELECT principal FROM amux_presence_sources WHERE source=?", report.Source).Scan(&existing); err != nil {
			return false, err
		}
		if existing.Valid && existing.String != principal {
			return false, ErrPresenceConflict
		}
	}
	if err == nil && report.Sequence <= lastSequence {
		return false, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	var storedClock int64
	if err := tx.QueryRowContext(ctx, "SELECT observed_at FROM amux_presence_clock WHERE id=1").Scan(&storedClock); err != nil {
		return false, err
	}
	effectiveNow := storedClock
	if now.UnixMilli() > effectiveNow {
		effectiveNow = now.UnixMilli()
	}
	for _, agent := range report.Agents {
		project, err := getProject(ctx, tx, agent.ProjectIdent)
		if err != nil {
			return false, err
		}
		if project == nil || project.ArchivedAt != nil {
			return false, fmt.Errorf("%w: project %q is missing or archived", ErrTaskNotFound, agent.ProjectIdent)
		}
		var owner, state, provider, osName string
		var seenAt int64
		err = tx.QueryRowContext(ctx, `SELECT source,state,seen_at FROM amux_presence_agents
		WHERE project_ident=? AND instance_id=? AND session_id=?`, agent.ProjectIdent, agent.Origin.InstanceID, agent.Origin.SessionID).Scan(&owner, &state, &seenAt)
		if err == nil {
			if err := tx.QueryRowContext(ctx, `SELECT provider,os FROM amux_presence_agents
				WHERE project_ident=? AND instance_id=? AND session_id=?`, agent.ProjectIdent, agent.Origin.InstanceID, agent.Origin.SessionID).Scan(&provider, &osName); err != nil {
				return false, err
			}
			if provider != agent.Origin.Provider || osName != agent.Origin.OS {
				return false, ErrPresenceConflict
			}
		}
		if err == nil && owner != report.Source && state != "gone" {
			if effectiveNow-seenAt < presenceGoneAfter {
				return false, ErrPresenceConflict
			}
		}
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
		// A live foreign source may not claim the same runtime identity under a
		// second instance or project. Session ownership is global.
		var foreignLive int
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM amux_presence_agents
			WHERE session_id=? AND source<>? AND state!='gone'
			AND (SELECT MAX(observed_at,?) FROM amux_presence_clock WHERE id=1)-seen_at<?)`,
			agent.Origin.SessionID, report.Source, now.UnixMilli(), presenceGoneAfter).Scan(&foreignLive); err != nil {
			return false, err
		}
		if foreignLive != 0 {
			return false, ErrPresenceConflict
		}
	}
	var observed int64
	if err := tx.QueryRowContext(ctx, "UPDATE amux_presence_clock SET observed_at=MAX(observed_at,?) WHERE id=1 RETURNING observed_at", now.UnixMilli()).Scan(&observed); err != nil {
		return false, err
	}
	var reportPrincipal any
	var principalSequence any
	if principal != "" {
		reportPrincipal = principal
		principalSequence = report.Sequence
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO amux_presence_sources(source,sequence,os,hostname,principal,principal_sequence) VALUES(?,?,?,?,?,?)
	 ON CONFLICT(source) DO UPDATE SET sequence=excluded.sequence,os=excluded.os,hostname=excluded.hostname,
	 principal=COALESCE(amux_presence_sources.principal,excluded.principal),
	 principal_sequence=COALESCE(excluded.principal_sequence,amux_presence_sources.principal_sequence)`, report.Source, report.Sequence, report.OS, nullableString(report.Hostname), reportPrincipal, principalSequence); err != nil {
		return false, err
	}
	listed := make(map[string]struct{}, len(report.Agents))
	for _, agent := range report.Agents {
		key := agent.ProjectIdent + "\x00" + agent.Origin.InstanceID + "\x00" + agent.Origin.SessionID
		listed[key] = struct{}{}
		identity := [3]string{agent.ProjectIdent, agent.Origin.InstanceID, agent.Origin.SessionID}
		before, err := readPresenceBefore(ctx, tx, identity)
		if err != nil {
			return false, err
		}
		var priorActivity sql.NullInt64
		err = tx.QueryRowContext(ctx, `SELECT last_activity_at FROM amux_presence_agents
		WHERE project_ident=? AND instance_id=? AND session_id=? AND source=?`, agent.ProjectIdent, agent.Origin.InstanceID, agent.Origin.SessionID, report.Source).Scan(&priorActivity)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return false, err
		}
		activity := agent.LastActivityAt
		if priorActivity.Valid && (activity == nil || priorActivity.Int64 > *activity) {
			value := priorActivity.Int64
			activity = &value
		}
		seenAt := now.UnixMilli()
		state := presenceState(agent.Busy, agent.Draft, agent.ExecutionIdle, activity, observed)
		if observed-seenAt >= presenceGoneAfter {
			state = "gone"
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO amux_presence_agents(project_ident,instance_id,session_id,provider,os,source,sequence,seen_at,last_activity_at,busy,draft,auto_assign,review_context_clear,state,hostname,goal,generation,agent_id,execution_idle,report_kind,event_agent_json,identity_seq,activity_seq,binding_required,binding_remote)
	 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'native',NULL,0,0,0,NULL) ON CONFLICT(project_ident,instance_id,session_id) DO UPDATE SET provider=excluded.provider,os=excluded.os,source=excluded.source,
 sequence=excluded.sequence,seen_at=MAX(amux_presence_agents.seen_at,excluded.seen_at),last_activity_at=excluded.last_activity_at,
	 busy=excluded.busy,draft=excluded.draft,auto_assign=excluded.auto_assign,review_context_clear=excluded.review_context_clear,state=excluded.state,hostname=excluded.hostname,
	 goal=excluded.goal,generation=excluded.generation,agent_id=excluded.agent_id,execution_idle=excluded.execution_idle,report_kind='native',event_agent_json=NULL,identity_seq=0,activity_seq=0,binding_required=0,binding_remote=NULL`, agent.ProjectIdent, agent.Origin.InstanceID, agent.Origin.SessionID, agent.Origin.Provider, agent.Origin.OS, report.Source, report.Sequence, seenAt, nullInt64(activity), agent.Busy, agent.Draft, agent.AutoAssign, agent.ReviewContextClear, state, nullableString(agent.Hostname), nullableString(agent.Goal), nullableString(agent.Generation), nullableString(agent.AgentID), agent.ExecutionIdle); err != nil {
			return false, err
		}
		wasLive := before.exists && before.state != "gone" && effectiveNow-before.seenAt < presenceGoneAfter
		online := state != "gone" && (!wasLive || before.source != report.Source || before.autoAssign != agent.AutoAssign || before.reviewContextClear != agent.ReviewContextClear || !sameOptionalString(before.goal, agent.Goal) || !sameOptionalString(before.generation, agent.Generation) || !sameOptionalString(before.agentID, agent.AgentID))
		if online {
			if err := capturePresenceTransition(ctx, tx, identity, "online", nil); err != nil {
				return false, err
			}
		}
		if before.exists && before.state != "gone" && state == "gone" {
			if _, err := tx.ExecContext(ctx, `UPDATE amux_presence_agents SET state='gone' WHERE project_ident=? AND instance_id=? AND session_id=?`, identity[0], identity[1], identity[2]); err != nil {
				return false, err
			}
			if err := capturePresenceTransition(ctx, tx, identity, "gone", nil); err != nil {
				return false, err
			}
		} else if state != "gone" && state != "unknown" && (!wasLive || before.state != state) {
			if err := capturePresenceTransition(ctx, tx, identity, state, nil); err != nil {
				return false, err
			}
		}
	}
	rows, err := tx.QueryContext(ctx, "SELECT project_ident,instance_id,session_id FROM amux_presence_agents WHERE source=? AND state!='gone'", report.Source)
	if err != nil {
		return false, err
	}
	var departed [][3]string
	for rows.Next() {
		var key [3]string
		if err := rows.Scan(&key[0], &key[1], &key[2]); err != nil {
			rows.Close()
			return false, err
		}
		if _, ok := listed[key[0]+"\x00"+key[1]+"\x00"+key[2]]; !ok {
			departed = append(departed, key)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	rows.Close()
	for _, key := range departed {
		if _, err := tx.ExecContext(ctx, `UPDATE amux_presence_agents SET state='gone' WHERE project_ident=? AND instance_id=? AND session_id=? AND source=?`, key[0], key[1], key[2], report.Source); err != nil {
			return false, err
		}
		if err := capturePresenceTransition(ctx, tx, key, "gone", nil); err != nil {
			return false, err
		}
	}
	if err := captureNativeHeartbeatIn(ctx, tx, report, now.UnixMilli()); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	_ = s.bus.Publish(eventbus.Event{Topic: eventbus.PresenceChanged})
	return true, nil
}

func nullInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func readPresence(ctx context.Context, q queryer, project string) ([]AgentPresence, error) {
	rows, err := q.QueryContext(ctx, `SELECT project_ident,instance_id,session_id,provider,os,source,seen_at,last_activity_at,busy,draft,auto_assign,review_context_clear,state,hostname,goal,generation,agent_id
 FROM amux_presence_agents WHERE project_ident=? ORDER BY COALESCE(last_activity_at,seen_at),session_id,instance_id`, projectid.NormalizeIdent(project))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]AgentPresence, 0)
	for rows.Next() {
		var item AgentPresence
		var activity sql.NullInt64
		var hostname, goal, generation, agentID sql.NullString
		if err := rows.Scan(&item.ProjectIdent, &item.Origin.InstanceID, &item.Origin.SessionID, &item.Origin.Provider, &item.Origin.OS, &item.Source, &item.SeenAt, &activity, &item.Busy, &item.Draft, &item.AutoAssign, &item.ReviewContextClear, &item.State, &hostname, &goal, &generation, &agentID); err != nil {
			return nil, err
		}
		if activity.Valid {
			item.LastActivityAt = &activity.Int64
		}
		if hostname.Valid {
			item.Hostname = &hostname.String
		}
		if goal.Valid {
			item.Goal = &goal.String
		}
		if generation.Valid {
			item.Generation = &generation.String
		}
		if agentID.Valid {
			item.AgentID = &agentID.String
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

// Presence returns the durable snapshot without deriving liveness from the Go clock.
func (s *Store) Presence(ctx context.Context, project string) ([]AgentPresence, error) {
	return readPresence(ctx, s.db, project)
}

// ReconcilePresence advances the persisted clock and materializes idle/gone transitions.
func (s *Store) ReconcilePresence(ctx context.Context, now time.Time) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var observed int64
	if err := tx.QueryRowContext(ctx, "UPDATE amux_presence_clock SET observed_at=MAX(observed_at,?) WHERE id=1 RETURNING observed_at", now.UnixMilli()).Scan(&observed); err != nil {
		return err
	}
	changed, err := reconcilePresenceIn(ctx, tx, observed, nil)
	if err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if changed {
		_ = s.bus.Publish(eventbus.Event{Topic: eventbus.PresenceChanged})
	}
	return nil
}

type presenceBefore struct {
	exists             bool
	source             string
	state              string
	seenAt             int64
	autoAssign         bool
	reviewContextClear bool
	goal               *string
	generation         *string
	agentID            *string
}

func readPresenceBefore(ctx context.Context, q queryer, key [3]string) (presenceBefore, error) {
	var before presenceBefore
	var goal, generation, agentID sql.NullString
	err := q.QueryRowContext(ctx, `SELECT source,state,seen_at,auto_assign,review_context_clear,goal,generation,agent_id FROM amux_presence_agents
		WHERE project_ident=? AND instance_id=? AND session_id=?`, key[0], key[1], key[2]).Scan(&before.source, &before.state, &before.seenAt, &before.autoAssign, &before.reviewContextClear, &goal, &generation, &agentID)
	if errors.Is(err, sql.ErrNoRows) {
		return before, nil
	}
	if err != nil {
		return before, err
	}
	before.exists = true
	if goal.Valid {
		before.goal = &goal.String
	}
	if generation.Valid {
		before.generation = &generation.String
	}
	if agentID.Valid {
		before.agentID = &agentID.String
	}
	return before, nil
}

func reconcilePresenceIn(ctx context.Context, tx *sql.Tx, observed int64, correlation *string) (bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT project_ident,instance_id,session_id,state,seen_at,busy,draft,last_activity_at,report_kind,execution_idle
		FROM amux_presence_agents WHERE state!='gone' ORDER BY project_ident,instance_id,session_id`)
	if err != nil {
		return false, err
	}
	type candidate struct {
		key           [3]string
		state         string
		seenAt        int64
		busy          bool
		draft         bool
		activity      sql.NullInt64
		reportKind    string
		executionIdle bool
	}
	var candidates []candidate
	for rows.Next() {
		var item candidate
		if err := rows.Scan(&item.key[0], &item.key[1], &item.key[2], &item.state, &item.seenAt, &item.busy, &item.draft, &item.activity, &item.reportKind, &item.executionIdle); err != nil {
			rows.Close()
			return false, err
		}
		candidates = append(candidates, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return false, err
	}
	if err := rows.Close(); err != nil {
		return false, err
	}
	changed := false
	for _, item := range candidates {
		next := "busy"
		if observed-item.seenAt >= presenceGoneAfter {
			next = "gone"
		} else if item.reportKind == "event" && !item.activity.Valid {
			// Raw event activity is unknown until an activity event supplies output.
		} else {
			var activity *int64
			if item.activity.Valid {
				activity = &item.activity.Int64
			}
			next = presenceState(item.busy, item.draft, item.reportKind == "native" && item.executionIdle, activity, observed)
		}
		if item.state == next {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE amux_presence_agents SET state=? WHERE project_ident=? AND instance_id=? AND session_id=?`, next, item.key[0], item.key[1], item.key[2]); err != nil {
			return false, err
		}
		if next != "unknown" {
			kind := next
			if err := capturePresenceTransition(ctx, tx, item.key, kind, correlation); err != nil {
				return false, err
			}
		}
		changed = true
	}
	return changed, nil
}
