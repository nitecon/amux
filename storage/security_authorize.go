package storage

import (
	"context"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/auth/authn"
	"github.com/nitecon/amux/auth/authz"
)

// AuthorizedSecurityNode is the durable node state associated with a validated
// client certificate. Callers must obtain the certificate from a verified TLS
// peer or pass it through SecurityEnrollment.AuthorizeCertificate, which verifies
// its chain and client-auth usage against the profile root.
type AuthorizedSecurityNode struct {
	NodeID         string  `json:"node_id"`
	InstanceID     string  `json:"instance_id"`
	Role           string  `json:"role"`
	CurrentSerial  string  `json:"current_serial"`
	PreviousSerial *string `json:"previous_serial"`
	PreviousUntil  *int64  `json:"previous_until"`
	PendingSerial  *string `json:"pending_serial"`
	Revoked        bool    `json:"revoked"`
	LastSeen       *int64  `json:"last_seen"`
}

type securityQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

// AuthorizeCertificate verifies the supplied certificate identity against this
// profile's CA and checks its durable current/pending/overlap authorization.
func (s *SecurityEnrollment) AuthorizeCertificate(ctx context.Context, certificate *x509.Certificate) (AuthorizedSecurityNode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return AuthorizedSecurityNode{}, err
	}
	node, _, _, err := s.authorizeCertificateLocked(ctx, certificate, time.Now())
	return node, err
}

func (s *SecurityEnrollment) authorizeCertificateLocked(ctx context.Context, certificate *x509.Certificate, now time.Time) (AuthorizedSecurityNode, *x509.Certificate, string, error) {
	certificate, err := authn.VerifyClientCertificate(certificate, s.root, now)
	if err != nil {
		return AuthorizedSecurityNode{}, nil, "", err
	}
	serial, err := certificateSerialCompat(certificate.Raw)
	if err != nil {
		return AuthorizedSecurityNode{}, nil, "", err
	}
	node, err := authorizeSecuritySerial(ctx, s.db, serial, now.UnixMilli())
	return node, certificate, serial, err
}

func authorizeSecuritySerial(ctx context.Context, db securityQueryer, serial string, now int64) (AuthorizedSecurityNode, error) {
	node, err := loadAuthorizedSecurityNode(ctx, db, serial, now)
	if err != nil {
		return AuthorizedSecurityNode{}, err
	}
	if node.LastSeen == nil || *node.LastSeen < now-60_000 {
		if _, err := db.ExecContext(ctx, "UPDATE nodes SET last_seen=? WHERE id=?", now, node.NodeID); err != nil {
			return AuthorizedSecurityNode{}, err
		}
	}
	return node, nil
}

func loadAuthorizedSecurityNode(ctx context.Context, db securityQueryer, serial string, now int64) (AuthorizedSecurityNode, error) {
	var node AuthorizedSecurityNode
	var previousSerial, pendingSerial sql.NullString
	var previousUntil, lastSeen sql.NullInt64
	err := db.QueryRowContext(ctx, `SELECT n.id,n.instance_id,n.role,n.current_serial,n.previous_serial,n.previous_until,n.pending_serial,n.revoked,n.last_seen
FROM nodes n JOIN certificates c ON c.node_id=n.id
WHERE c.serial=? AND c.expires_at>?`, serial, now).Scan(
		&node.NodeID, &node.InstanceID, &node.Role, &node.CurrentSerial,
		&previousSerial, &previousUntil, &pendingSerial, &node.Revoked, &lastSeen,
	)
	if err != nil {
		return AuthorizedSecurityNode{}, err
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
	if err := authz.CheckCertificateSerial(authz.CertificateState{
		CurrentSerial: node.CurrentSerial, PendingSerial: node.PendingSerial, PreviousSerial: node.PreviousSerial,
		PreviousUntil: node.PreviousUntil, Revoked: node.Revoked,
	}, serial, now); err != nil {
		return AuthorizedSecurityNode{}, err
	}
	return node, nil
}

// RenewCertificate creates a pending client certificate for the node identified
// by the actual authenticated peer leaf. Exact request replays are returned
// before renewal-state guards, matching Rust's durable issuance contract.
func (s *SecurityEnrollment) RenewCertificate(ctx context.Context, presented *x509.Certificate, request RenewalRequest) (EnrollmentResponse, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return EnrollmentResponse{}, err
	}
	node, _, _, err := s.authorizeCertificateLocked(ctx, presented, time.Now())
	if err != nil {
		return EnrollmentResponse{}, err
	}
	if _, err := uuid.Parse(request.RequestID); err != nil {
		return EnrollmentResponse{}, err
	}
	binding := sha256Hex([]byte("renew:" + node.NodeID + ":" + request.CSRPEM))
	var stored, responseJSON string
	err = s.db.QueryRowContext(ctx, "SELECT binding,response FROM issuance WHERE request_id=?", request.RequestID).Scan(&stored, &responseJSON)
	if err == nil {
		if stored != binding {
			return EnrollmentResponse{}, fmt.Errorf("request ID already bound")
		}
		var response EnrollmentResponse
		err = json.Unmarshal([]byte(responseJSON), &response)
		return response, err
	}
	if err != sql.ErrNoRows {
		return EnrollmentResponse{}, err
	}
	if node.PendingSerial != nil {
		return EnrollmentResponse{}, fmt.Errorf("renewal pending; activate or recover it first")
	}
	if node.PreviousSerial != nil {
		return EnrollmentResponse{}, fmt.Errorf("previous credential has not retired")
	}
	if len(request.CSRPEM) > 16_384 {
		return EnrollmentResponse{}, fmt.Errorf("CSR exceeds limit")
	}
	csr, err := parseEnrollmentCSR(request.CSRPEM)
	if err != nil {
		return EnrollmentResponse{}, err
	}
	certificatePEM, serial, expires, err := s.signEnrollmentCSR(csr, node.NodeID)
	if err != nil {
		return EnrollmentResponse{}, err
	}
	response := EnrollmentResponse{ClusterID: s.clusterID, NodeID: node.NodeID, CertificatePEM: certificatePEM, RootPEM: string(s.rootPEM), ExpiresAt: expires, Serial: serial}
	encoded, err := json.Marshal(response)
	if err != nil {
		return EnrollmentResponse{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return EnrollmentResponse{}, err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "INSERT INTO certificates(serial,node_id,certificate_pem,created_at,expires_at,activated) VALUES(?,?,?,?,?,0)", serial, node.NodeID, certificatePEM, time.Now().UnixMilli(), expires); err != nil {
		return EnrollmentResponse{}, err
	}
	result, err := tx.ExecContext(ctx, "UPDATE nodes SET pending_serial=? WHERE id=? AND revoked=0 AND current_serial=? AND pending_serial IS NULL AND previous_serial IS NULL", serial, node.NodeID, node.CurrentSerial)
	if err != nil {
		return EnrollmentResponse{}, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return EnrollmentResponse{}, err
	}
	if changed != 1 {
		return EnrollmentResponse{}, fmt.Errorf("credential state changed during renewal")
	}
	if _, err = tx.ExecContext(ctx, "INSERT INTO issuance(request_id,binding,response) VALUES(?,?,?)", request.RequestID, binding, string(encoded)); err != nil {
		return EnrollmentResponse{}, err
	}
	if err = tx.Commit(); err != nil {
		return EnrollmentResponse{}, err
	}
	return response, nil
}

// ActivateCertificate activates the actual authenticated leaf. If it is the
// pending credential, the current leaf becomes the previous credential for the
// bounded overlap period; current and previous leaves remain idempotent.
func (s *SecurityEnrollment) ActivateCertificate(ctx context.Context, presented *x509.Certificate) (AuthorizedSecurityNode, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return AuthorizedSecurityNode{}, err
	}
	_, certificate, serial, err := s.authorizeCertificateLocked(ctx, presented, time.Now())
	if err != nil {
		return AuthorizedSecurityNode{}, err
	}
	now := time.Now().UnixMilli()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return AuthorizedSecurityNode{}, err
	}
	defer tx.Rollback()
	node, err := loadAuthorizedSecurityNode(ctx, tx, serial, now)
	if err != nil {
		return AuthorizedSecurityNode{}, err
	}
	if node.PendingSerial != nil && *node.PendingSerial == serial {
		if node.PreviousSerial != nil {
			return AuthorizedSecurityNode{}, fmt.Errorf("previous credential has not retired")
		}
		var oldExpiry int64
		if err = tx.QueryRowContext(ctx, "SELECT expires_at FROM certificates WHERE serial=?", node.CurrentSerial).Scan(&oldExpiry); err != nil {
			return AuthorizedSecurityNode{}, err
		}
		previousUntil := now + securityCredentialOverlap.Milliseconds()
		if oldExpiry < previousUntil {
			previousUntil = oldExpiry
		}
		result, updateErr := tx.ExecContext(ctx, "UPDATE nodes SET previous_serial=current_serial,previous_until=?,current_serial=?,pending_serial=NULL WHERE id=? AND current_serial=? AND pending_serial=? AND previous_serial IS NULL AND revoked=0", previousUntil, serial, node.NodeID, node.CurrentSerial, serial)
		if updateErr != nil {
			return AuthorizedSecurityNode{}, updateErr
		}
		changed, updateErr := result.RowsAffected()
		if updateErr != nil {
			return AuthorizedSecurityNode{}, updateErr
		}
		if changed != 1 {
			return AuthorizedSecurityNode{}, fmt.Errorf("credential state changed during activation")
		}
	}
	if _, err = tx.ExecContext(ctx, "UPDATE certificates SET activated=1 WHERE serial=?", serial); err != nil {
		return AuthorizedSecurityNode{}, err
	}
	if err = tx.Commit(); err != nil {
		return AuthorizedSecurityNode{}, err
	}
	status, _, _, err := s.authorizeCertificateLocked(ctx, certificate, time.Now())
	return status, err
}
