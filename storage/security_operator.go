package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// SecurityServerIdentityStatus is the non-secret TLS identity captured when a
// profile-managed listener starts. Live certificate rotation is not reflected
// in this value; the listener's startup TLS configuration remains authoritative.
type SecurityServerIdentityStatus struct {
	ClusterID         string   `json:"cluster_id"`
	Names             []string `json:"server_names"`
	Serial            string   `json:"server_serial"`
	ExpiresAt         int64    `json:"server_expires_at"`
	PreviousSerial    *string  `json:"previous_server_serial"`
	PreviousExpiresAt *int64   `json:"previous_server_until"`
}

// ServerIdentityStatus snapshots public identity metadata for the already
// configured profile TLS listener. Call after EnsureProfileServerTLS at startup.
func (s *SecurityEnrollment) ServerIdentityStatus(ctx context.Context) (SecurityServerIdentityStatus, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return SecurityServerIdentityStatus{}, err
	}
	if err := ctx.Err(); err != nil {
		return SecurityServerIdentityStatus{}, err
	}
	data, err := os.ReadFile(filepath.Join(s.profile, "server-identity.json"))
	if err != nil {
		return SecurityServerIdentityStatus{}, err
	}
	var identity profileServerIdentity
	if err := json.Unmarshal(data, &identity); err != nil || identity.Serial == "" || identity.ExpiresAt <= 0 {
		return SecurityServerIdentityStatus{}, fmt.Errorf("invalid profile server identity metadata")
	}
	status := SecurityServerIdentityStatus{
		ClusterID: s.clusterID, Names: append([]string(nil), identity.Names...),
		Serial: identity.Serial, ExpiresAt: identity.ExpiresAt,
	}
	if identity.Previous != nil {
		status.PreviousSerial = &identity.Previous.Serial
		status.PreviousExpiresAt = &identity.Previous.Until
	}
	return status, nil
}

// SecurityNodes returns the current durable node table for the local operator
// status endpoint, ordered by node ID.
func (s *SecurityEnrollment) SecurityNodes(ctx context.Context) ([]AuthorizedSecurityNode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT id,instance_id,role,current_serial,previous_serial,previous_until,pending_serial,revoked,last_seen FROM nodes ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	nodes := make([]AuthorizedSecurityNode, 0)
	for rows.Next() {
		var node AuthorizedSecurityNode
		var previousSerial, pendingSerial sql.NullString
		var previousUntil, lastSeen sql.NullInt64
		if err := rows.Scan(&node.NodeID, &node.InstanceID, &node.Role, &node.CurrentSerial, &previousSerial, &previousUntil, &pendingSerial, &node.Revoked, &lastSeen); err != nil {
			return nil, err
		}
		if previousSerial.Valid {
			node.PreviousSerial = &previousSerial.String
		}
		if previousUntil.Valid {
			node.PreviousUntil = &previousUntil.Int64
		}
		if pendingSerial.Valid {
			node.PendingSerial = &pendingSerial.String
		}
		if lastSeen.Valid {
			node.LastSeen = &lastSeen.Int64
		}
		nodes = append(nodes, node)
	}
	return nodes, rows.Err()
}

// BeginShutdown invalidates outstanding invitations before the listener drains.
func (s *SecurityEnrollment) BeginShutdown(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return s.RevokeInvitation()
}
