package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/nitecon/amux/fism"
)

// A target filter expresses interest. Authority comes from the enrolled
// instance binding and the captured recipient or authenticated native report.
func remoteNotificationAllowed(ctx context.Context, tx *sql.Tx, event fism.Event, principal string, enrollment *SecurityEnrollment) (bool, error) {
	if principal == "" || enrollment == nil {
		return false, nil
	}

	var captured sql.NullString
	var capturedReceiver string
	err := tx.QueryRowContext(ctx, `SELECT principal,receiver_json FROM amux_native_delivery_receipts WHERE notify_uid=?`, event.UID).Scan(&captured, &capturedReceiver)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if captured.Valid {
		receiver, err := decodeNativeReceiver(json.RawMessage(capturedReceiver))
		if err != nil || receiver == nil || receiver.InstanceID == "" || captured.String != principal {
			return false, nil
		}
		return enrollment.OwnsNativeInstance(ctx, principal, receiver.InstanceID)
	}

	var notice struct {
		Target   string          `json:"target"`
		Project  string          `json:"project"`
		Receiver json.RawMessage `json:"receiver"`
		Event    struct {
			Task json.RawMessage `json:"task"`
		} `json:"event"`
	}
	if json.Unmarshal(event.Payload, &notice) != nil || notice.Target == "" || notice.Project == "" {
		return false, nil
	}
	receiver, err := decodeNativeReceiver(notice.Receiver)
	if err != nil {
		return false, nil
	}
	if receiver == nil {
		owner := notificationTaskOwner(notice.Event.Task)
		if owner == nil || owner.SessionID != notice.Target {
			return false, nil
		}
		receiver = &nativeReceiver{InstanceID: owner.InstanceID, Provider: owner.Provider, OS: owner.OS}
	}
	if receiver.InstanceID == "" {
		return false, nil
	}
	owns, err := enrollment.OwnsNativeInstance(ctx, principal, receiver.InstanceID)
	if err != nil || !owns {
		return false, err
	}

	var generation string
	if receiver.Generation != nil {
		generation = *receiver.Generation
	}
	var allowed bool
	err = tx.QueryRowContext(ctx, `SELECT EXISTS(
		SELECT 1 FROM amux_presence_agents a JOIN amux_presence_sources s ON s.source=a.source
		WHERE a.project_ident=? AND a.session_id=? AND a.instance_id=? AND a.report_kind='native'
		AND s.sequence=a.sequence AND s.principal_sequence=a.sequence AND s.principal=?
		AND (?='' OR a.provider=?) AND (?='' OR a.os=?) AND (?='' OR a.generation=?))`,
		notice.Project, notice.Target, receiver.InstanceID, principal,
		receiver.Provider, receiver.Provider, receiver.OS, receiver.OS, generation, generation).Scan(&allowed)
	return allowed, err
}
