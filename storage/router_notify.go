package storage

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/nitecon/amux/plugins"
	"github.com/nitecon/amux/router"
)

// routerNewAssignment matches the Rust notifier's retained-history check.
// Expired history fails closed because the task's prior assignment cycle can
// no longer be ruled out.
func routerNewAssignment(ctx context.Context, q queryer, task json.RawMessage) (bool, error) {
	var fields map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(task))
	decoder.UseNumber()
	if err := decoder.Decode(&fields); err != nil {
		return false, fmt.Errorf("decode notification task: %w", err)
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != nil && err != io.EOF {
		return false, fmt.Errorf("decode notification task: %w", err)
	} else if err == nil {
		return false, fmt.Errorf("decode notification task: multiple JSON values")
	}
	if fields == nil {
		return false, fmt.Errorf("decode notification task: expected object")
	}
	var taskID string
	if err := json.Unmarshal(fields["id"], &taskID); err != nil || taskID == "" {
		return false, nil
	}
	var labels []json.RawMessage
	if raw := fields["labels"]; len(raw) > 0 && !isJSONNull(raw) {
		_ = json.Unmarshal(raw, &labels)
	}
	for _, raw := range labels {
		var label string
		if json.Unmarshal(raw, &label) == nil && label == "task-review" {
			return false, nil
		}
	}
	var isNew bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM task_events
		WHERE json_extract(task_json,'$.id')=? AND kind='task_created'
		  AND json_extract(task_json,'$.status')='todo'
	) AND NOT EXISTS(
		SELECT 1 FROM task_events
		WHERE json_extract(task_json,'$.id')=?
		  AND (kind IN ('task_unclaimed','task_completed')
		       OR json_extract(task_json,'$.status')!='todo'
		       OR json_extract(comment_json,'$.author_type')='agent')
	)`, taskID, taskID).Scan(&isNew)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return isNew, err
}

// emitRouterNotificationIn adds immutable host snapshots before appending the
// notification to the caller's transaction. The caller owns commit and wakeup.
func emitRouterNotificationIn(ctx context.Context, tx *sql.Tx, registry *router.Registry, pluginID, causeUID string, payload json.RawMessage) (router.EmitResult, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(payload, &envelope); err != nil || envelope == nil {
		if err == nil {
			err = fmt.Errorf("expected notification object")
		}
		return router.EmitResult{}, fmt.Errorf("decode notification payload: %w", err)
	}
	var project string
	if err := json.Unmarshal(envelope["project"], &project); err != nil {
		return router.EmitResult{}, fmt.Errorf("decode notification project: %w", err)
	}
	var event map[string]json.RawMessage
	if err := json.Unmarshal(envelope["event"], &event); err != nil || event == nil {
		if err == nil {
			err = fmt.Errorf("expected event object")
		}
		return router.EmitResult{}, fmt.Errorf("decode notification event: %w", err)
	}
	var task map[string]json.RawMessage
	if err := json.Unmarshal(event["task"], &task); err != nil || task == nil {
		if err == nil {
			err = fmt.Errorf("expected task object")
		}
		return router.EmitResult{}, fmt.Errorf("decode notification task: %w", err)
	}
	var ownerOrigin, senderOrigin json.RawMessage
	if len(task["owner_origin"]) > 0 {
		ownerOrigin = task["owner_origin"]
	}
	if len(event["origin"]) > 0 {
		senderOrigin = event["origin"]
	}
	ownerHost, err := registeredNotificationHost(ctx, tx, project, ownerOrigin)
	if err != nil {
		return router.EmitResult{}, fmt.Errorf("lookup notification owner host: %w", err)
	}
	var mode, senderName string
	_ = json.Unmarshal(envelope["mode"], &mode)
	_ = json.Unmarshal(event["sender_name"], &senderName)
	var senderHost *string
	if isJSONNull(senderOrigin) && (mode == "assigned" || senderName == "amux") {
		senderName = "amux"
		if hostname, hostErr := os.Hostname(); hostErr == nil && hostname != "" {
			senderHost = &hostname
		}
		encoded, _ := json.Marshal(senderName)
		event["sender_name"] = encoded
	} else {
		senderHost, err = registeredNotificationHost(ctx, tx, project, senderOrigin)
		if err != nil {
			return router.EmitResult{}, fmt.Errorf("lookup notification sender host: %w", err)
		}
	}
	event["sender_hostname"], _ = json.Marshal(senderHost)
	event["owner_hostname"], _ = json.Marshal(ownerHost)
	envelope["event"], err = json.Marshal(event)
	if err != nil {
		return router.EmitResult{}, fmt.Errorf("encode notification event: %w", err)
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		return router.EmitResult{}, fmt.Errorf("encode notification payload: %w", err)
	}
	correlation := causeUID
	return emitRouterEventIn(ctx, tx, registry, router.EmitRequest{
		ID: plugins.NotifyEventID, Version: 1, Payload: encoded,
		Source: "plugin:" + pluginID, Correlation: &correlation,
	})
}

type notificationHostCandidate struct {
	host     string
	instance string
	project  string
	seen     int64
}

const (
	registeredHostNativeTable   = "amux_presence_agents"
	registeredHostRetainedTable = "task_target_presence"
)

// registeredNotificationHost resolves only explicitly stored host metadata.
// Matching precedence is exact instance, same project, then latest seen.
func registeredNotificationHost(ctx context.Context, q queryer, project string, origin json.RawMessage) (*string, error) {
	if len(origin) == 0 || isJSONNull(origin) {
		return nil, nil
	}
	var identity map[string]json.RawMessage
	if err := json.Unmarshal(origin, &identity); err != nil {
		return nil, fmt.Errorf("decode notification origin: %w", err)
	}
	if identity == nil {
		return nil, nil
	}
	var sessionID, instanceID string
	if json.Unmarshal(identity["session_id"], &sessionID) != nil || sessionID == "" {
		return nil, nil
	}
	_ = json.Unmarshal(identity["instance_id"], &instanceID)
	var best *notificationHostCandidate
	for _, table := range []string{registeredHostNativeTable, registeredHostRetainedTable} {
		candidate, err := rankedRegisteredHostCandidate(ctx, q, table, sessionID, instanceID, project)
		if err != nil {
			return nil, err
		}
		if candidate == nil {
			continue
		}
		if best == nil || notificationHostPreferred(*candidate, *best, project, instanceID) {
			best = candidate
		}
	}
	if best == nil {
		return nil, nil
	}
	host := best.host
	return &host, nil
}

// rankedRegisteredHostCandidate reads the best nonblank hostname from one
// supported registration table. The switch keeps the interpolated identifier
// internal and fixed.
func rankedRegisteredHostCandidate(ctx context.Context, q queryer, table, sessionID, instanceID, project string) (*notificationHostCandidate, error) {
	var tableName string
	switch table {
	case registeredHostNativeTable:
		tableName = registeredHostNativeTable
	case registeredHostRetainedTable:
		tableName = registeredHostRetainedTable
	default:
		return nil, fmt.Errorf("unsupported registered host table %q", table)
	}
	present, err := sqliteTableHasColumn(ctx, q, tableName, "hostname")
	if err != nil {
		return nil, err
	}
	if !present {
		return nil, nil
	}
	var candidate notificationHostCandidate
	err = q.QueryRowContext(ctx, "SELECT project_ident,instance_id,hostname,seen_at FROM "+tableName+" WHERE session_id=? AND hostname IS NOT NULL AND trim(hostname)!='' ORDER BY (instance_id=?) DESC,(project_ident=?) DESC,seen_at DESC LIMIT 1", sessionID, instanceID, project).Scan(&candidate.project, &candidate.instance, &candidate.host, &candidate.seen)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s host snapshots: %w", tableName, err)
	}
	return &candidate, nil
}

func notificationHostPreferred(candidate, current notificationHostCandidate, project, instance string) bool {
	if (candidate.instance == instance) != (current.instance == instance) {
		return candidate.instance == instance
	}
	if (candidate.project == project) != (current.project == project) {
		return candidate.project == project
	}
	return candidate.seen > current.seen
}

func sqliteTableHasColumn(ctx context.Context, q queryer, table, column string) (bool, error) {
	var exists bool
	err := q.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM pragma_table_info(?) WHERE name=?)", table, column).Scan(&exists)
	return exists, err
}

func isJSONNull(raw json.RawMessage) bool {
	return len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
