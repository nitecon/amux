package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
)

// SecurityDiscovery contains the public trust material advertised to enrolling clients.
type SecurityDiscovery struct {
	Version    uint32 `json:"version"`
	ClusterID  string `json:"cluster_id"`
	RootSHA256 string `json:"root_sha256"`
	RootPEM    string `json:"root_pem"`
	SecurePort uint16 `json:"secure_port"`
}

func (s *SecurityEnrollment) Discovery(securePort uint16) SecurityDiscovery {
	return SecurityDiscovery{
		Version: 1, ClusterID: s.clusterID, RootSHA256: sha256Hex(s.root.Raw),
		RootPEM: string(s.rootPEM), SecurePort: securePort,
	}
}

// IssuanceResponse retrieves the durable enrollment receipt by request ID.
func (s *SecurityEnrollment) IssuanceResponse(ctx context.Context, requestID string) (EnrollmentResponse, bool, error) {
	if _, err := uuid.Parse(requestID); err != nil {
		return EnrollmentResponse{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return EnrollmentResponse{}, false, err
	}
	var encoded string
	err := s.db.QueryRowContext(ctx, "SELECT response FROM issuance WHERE request_id=?", requestID).Scan(&encoded)
	if errors.Is(err, sql.ErrNoRows) {
		return EnrollmentResponse{}, false, nil
	}
	if err != nil {
		return EnrollmentResponse{}, false, err
	}
	var response EnrollmentResponse
	if err := json.Unmarshal([]byte(encoded), &response); err != nil {
		return EnrollmentResponse{}, false, err
	}
	return response, true, nil
}

// BindInstance associates a canonical runtime instance with its authenticated node.
func (s *SecurityEnrollment) BindInstance(ctx context.Context, nodeID, instanceID string) error {
	if !canonicalUUID(instanceID) || instanceID == uuid.Nil.String() {
		return errors.New("invalid native instance")
	}
	return s.bindPeerID(ctx, "native_instances", nodeID, instanceID, "native instance belongs to another node")
}

// OwnsNativeInstance verifies a previously registered instance for a node.
func (s *SecurityEnrollment) OwnsNativeInstance(ctx context.Context, nodeID, instanceID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return false, err
	}
	var owns bool
	err := s.db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM native_instances WHERE id=? AND node_id=?)`, instanceID, nodeID).Scan(&owns)
	return owns, err
}

// BindConsumer associates a durable stream consumer with its authenticated node.
func (s *SecurityEnrollment) BindConsumer(ctx context.Context, nodeID, consumerID string) error {
	if len(consumerID) == 0 || len(consumerID) > 128 {
		return errors.New("invalid consumer")
	}
	return s.bindPeerID(ctx, "consumers", nodeID, consumerID, "consumer belongs to another node")
}

// RevokeNode makes all certificates belonging to the node ineligible for authorization.
func (s *SecurityEnrollment) RevokeNode(ctx context.Context, nodeID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return err
	}
	result, err := s.db.ExecContext(ctx, "UPDATE nodes SET revoked=1 WHERE id=?", nodeID)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return errors.New("node not found")
	}
	return nil
}

// table is a fixed schema name supplied by BindInstance or BindConsumer.
func (s *SecurityEnrollment) bindPeerID(ctx context.Context, table, nodeID, id, conflict string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return err
	}
	if _, err := s.db.ExecContext(ctx, "INSERT INTO "+table+"(id,node_id) VALUES(?,?) ON CONFLICT(id) DO NOTHING", id, nodeID); err != nil {
		return err
	}
	var owner string
	if err := s.db.QueryRowContext(ctx, "SELECT node_id FROM "+table+" WHERE id=?", id).Scan(&owner); err != nil {
		return err
	}
	if owner != nodeID {
		return errors.New(conflict)
	}
	return nil
}
