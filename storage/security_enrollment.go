package storage

import (
	"context"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

const invitationLifetime = 10 * time.Minute
const enrollmentLeafLifetime = 30 * 24 * time.Hour
const securityCredentialOverlap = 7 * 24 * time.Hour

// EnrollmentRequest and EnrollmentResponse are the durable local enrollment contract.
type EnrollmentRequest struct {
	RequestID  string `json:"request_id"`
	Invitation string `json:"invitation"`
	CSRPEM     string `json:"csr_pem"`
	InstanceID string `json:"instance_id"`
}

// RenewalRequest binds a renewal idempotency key to the exact CSR PEM.
type RenewalRequest struct {
	RequestID string `json:"request_id"`
	CSRPEM    string `json:"csr_pem"`
}

type EnrollmentResponse struct {
	ClusterID      string `json:"cluster_id"`
	NodeID         string `json:"node_id"`
	CertificatePEM string `json:"certificate_pem"`
	RootPEM        string `json:"root_pem"`
	ExpiresAt      int64  `json:"expires_at"`
	Serial         string `json:"serial"`
}

type Invitation struct {
	Invitation string `json:"invitation"`
	ExpiresAt  int64  `json:"expires_at"`
}

// String keeps the peering key out of formatted diagnostic output.
func (i Invitation) String() string {
	return fmt.Sprintf("Invitation{Invitation:[redacted], ExpiresAt:%d}", i.ExpiresAt)
}

func (i Invitation) GoString() string { return i.String() }

type activeInvitation struct {
	id, raw  string
	deadline time.Time
}

// SecurityEnrollment provides in-process invitation and enrollment operations.
// The caller must hold LockSecurityProfile for the lifetime of this value.
type SecurityEnrollment struct {
	mu        sync.Mutex
	db        *sql.DB
	profile   string
	clusterID string
	rootPEM   []byte
	root      *x509.Certificate
	rootKey   *ecdsa.PrivateKey
	active    *activeInvitation
	closed    bool
}

func OpenSecurityEnrollment(ctx context.Context, profile string) (*SecurityEnrollment, error) {
	clusterID, err := EnsureSecurityInstallation(ctx, profile)
	if err != nil {
		return nil, err
	}
	if err := EnsureSecurityRoot(profile, clusterID); err != nil {
		return nil, err
	}
	root, rootKey, err := loadProfileRoot(profile)
	if err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(profile)
	if err != nil {
		return nil, err
	}
	// Rust records canonical paths in its local-client manifest. Resolve the
	// profile directory before comparing those paths with its boundary.
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return nil, err
	}
	dbPath := filepath.Join(abs, "security.sqlite")
	path := filepath.ToSlash(dbPath)
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "_pragma=foreign_keys%281%29&_pragma=journal_mode%28DELETE%29&_txlock=immediate"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err = db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS invitations(id TEXT PRIMARY KEY, verifier TEXT NOT NULL,created_at INTEGER NOT NULL,expires_at INTEGER NOT NULL,revoked INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS nodes(id TEXT PRIMARY KEY,instance_id TEXT NOT NULL UNIQUE,role TEXT NOT NULL,current_serial TEXT NOT NULL,previous_serial TEXT,previous_until INTEGER,pending_serial TEXT,revoked INTEGER NOT NULL DEFAULT 0,last_seen INTEGER);
CREATE TABLE IF NOT EXISTS certificates(serial TEXT PRIMARY KEY,node_id TEXT NOT NULL REFERENCES nodes(id),certificate_pem TEXT NOT NULL,created_at INTEGER NOT NULL,expires_at INTEGER NOT NULL,activated INTEGER NOT NULL DEFAULT 0);
CREATE TABLE IF NOT EXISTS issuance(request_id TEXT PRIMARY KEY,binding TEXT NOT NULL,response TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS consumers(id TEXT PRIMARY KEY,node_id TEXT NOT NULL REFERENCES nodes(id));
CREATE TABLE IF NOT EXISTS native_instances(id TEXT PRIMARY KEY,node_id TEXT NOT NULL REFERENCES nodes(id));
UPDATE invitations SET revoked=1;`); err != nil {
		db.Close()
		return nil, err
	}
	rootPEM, err := os.ReadFile(filepath.Join(abs, "root.pem"))
	if err != nil {
		db.Close()
		return nil, err
	}
	return &SecurityEnrollment{db: db, profile: abs, clusterID: clusterID, rootPEM: rootPEM, root: root, rootKey: rootKey}, nil
}

// Close releases this handle. The profile owner lock is managed by the caller.
func (s *SecurityEnrollment) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	s.active = nil
	return s.db.Close()
}

func (s *SecurityEnrollment) IssueInvitation(discoveryPort uint16) (Invitation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return Invitation{}, err
	}
	id, err := uuid.NewV7()
	if err != nil {
		return Invitation{}, err
	}
	secret := make([]byte, 32)
	if _, err = rand.Read(secret); err != nil {
		return Invitation{}, err
	}
	rootHash := sha256.Sum256(s.root.Raw)
	raw := fmt.Sprintf("amux1.%s.%s.%s.%s.%d", s.clusterID, hex.EncodeToString(rootHash[:]), id, hex.EncodeToString(secret), discoveryPort)
	now := time.Now().UnixMilli()
	expires := now + invitationLifetime.Milliseconds()
	tx, err := s.db.Begin()
	if err != nil {
		return Invitation{}, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE invitations SET revoked=1"); err != nil {
		return Invitation{}, err
	}
	if _, err = tx.Exec("INSERT INTO invitations(id,verifier,created_at,expires_at) VALUES(?,?,?,?)", id.String(), sha256Hex([]byte(raw)), now, expires); err != nil {
		return Invitation{}, err
	}
	if err = tx.Commit(); err != nil {
		return Invitation{}, err
	}
	s.active = &activeInvitation{id: id.String(), raw: raw, deadline: time.Now().Add(invitationLifetime)}
	return Invitation{Invitation: raw, ExpiresAt: expires}, nil
}

func (s *SecurityEnrollment) CopyInvitation() (Invitation, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return Invitation{}, err
	}
	if s.active == nil {
		return Invitation{}, PeeringKeyUnavailable
	}
	var expires int64
	var revoked int
	err := s.db.QueryRow("SELECT expires_at,revoked FROM invitations WHERE id=?", s.active.id).Scan(&expires, &revoked)
	if err != nil {
		return Invitation{}, err
	}
	if revoked != 0 || time.Now().After(s.active.deadline) || time.Now().UnixMilli() >= expires {
		s.active = nil
		_, err = s.db.Exec("UPDATE invitations SET revoked=1")
		if err != nil {
			return Invitation{}, err
		}
		return Invitation{}, PeeringKeyExpired
	}
	return Invitation{Invitation: s.active.raw, ExpiresAt: expires}, nil
}

func (s *SecurityEnrollment) RevokeInvitation() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return err
	}
	_, err := s.db.Exec("UPDATE invitations SET revoked=1")
	s.active = nil
	return err
}

func (s *SecurityEnrollment) Enroll(ctx context.Context, request EnrollmentRequest) (EnrollmentResponse, error) {
	if _, err := uuid.Parse(request.RequestID); err != nil {
		return EnrollmentResponse{}, err
	}
	if _, err := uuid.Parse(request.InstanceID); err != nil {
		return EnrollmentResponse{}, err
	}
	if len(request.CSRPEM) > 16384 {
		return EnrollmentResponse{}, fmt.Errorf("CSR exceeds limit")
	}
	binding := sha256Hex([]byte(request.Invitation + ":" + request.CSRPEM + ":" + request.InstanceID))
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return EnrollmentResponse{}, err
	}
	var stored, responseJSON string
	err := s.db.QueryRowContext(ctx, "SELECT binding,response FROM issuance WHERE request_id=?", request.RequestID).Scan(&stored, &responseJSON)
	if err == nil {
		if stored != binding {
			return EnrollmentResponse{}, EnrollmentConflict
		}
		var response EnrollmentResponse
		err = json.Unmarshal([]byte(responseJSON), &response)
		return response, err
	}
	if err != sql.ErrNoRows {
		return EnrollmentResponse{}, err
	}
	if s.active == nil {
		return EnrollmentResponse{}, PeeringKeyUnavailable
	}
	if time.Now().After(s.active.deadline) {
		return EnrollmentResponse{}, PeeringKeyExpired
	}
	var verifier string
	var expires int64
	var revoked int
	if err = s.db.QueryRow("SELECT verifier,expires_at,revoked FROM invitations WHERE id=?", s.active.id).Scan(&verifier, &expires, &revoked); err != nil {
		return EnrollmentResponse{}, err
	}
	if time.Now().UnixMilli() >= expires {
		return EnrollmentResponse{}, PeeringKeyExpired
	}
	if revoked != 0 || verifier != sha256Hex([]byte(request.Invitation)) {
		return EnrollmentResponse{}, PeeringKeyInvalid
	}
	csr, err := parseEnrollmentCSR(request.CSRPEM)
	if err != nil {
		return EnrollmentResponse{}, err
	}
	var existingID, role string
	var nodeRevoked int
	var certExpires sql.NullInt64
	err = s.db.QueryRow(`SELECT n.id,n.role,n.revoked,MAX(c.expires_at) FROM nodes n JOIN certificates c ON c.node_id=n.id WHERE n.instance_id=? GROUP BY n.id`, request.InstanceID).Scan(&existingID, &role, &nodeRevoked, &certExpires)
	existing := err == nil
	if err != nil && err != sql.ErrNoRows {
		return EnrollmentResponse{}, err
	}
	if existing && !(role == "client" && nodeRevoked == 0 && certExpires.Valid && certExpires.Int64 <= time.Now().UnixMilli()) {
		return EnrollmentResponse{}, InstallationAlreadyEnrolled
	}
	nodeID := existingID
	if !existing {
		u, e := uuid.NewV7()
		if e != nil {
			return EnrollmentResponse{}, e
		}
		nodeID = u.String()
	}
	certPEM, serial, until, err := s.signEnrollmentCSR(csr, nodeID)
	if err != nil {
		return EnrollmentResponse{}, err
	}
	if s.active == nil || time.Now().After(s.active.deadline) || time.Now().UnixMilli() >= expires {
		return EnrollmentResponse{}, PeeringKeyExpired
	}
	response := EnrollmentResponse{ClusterID: s.clusterID, NodeID: nodeID, CertificatePEM: certPEM, RootPEM: string(s.rootPEM), ExpiresAt: until, Serial: serial}
	encoded, err := json.Marshal(response)
	if err != nil {
		return EnrollmentResponse{}, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return EnrollmentResponse{}, err
	}
	defer tx.Rollback()
	if existing {
		_, err = tx.Exec("UPDATE nodes SET current_serial=?,previous_serial=NULL,previous_until=NULL,pending_serial=NULL WHERE id=?", serial, nodeID)
	} else {
		_, err = tx.Exec("INSERT INTO nodes(id,instance_id,role,current_serial) VALUES(?,?, 'client',?)", nodeID, request.InstanceID, serial)
	}
	if err != nil {
		return EnrollmentResponse{}, err
	}
	if _, err = tx.Exec("INSERT INTO certificates(serial,node_id,certificate_pem,created_at,expires_at) VALUES(?,?,?,?,?)", serial, nodeID, certPEM, time.Now().UnixMilli(), until); err != nil {
		return EnrollmentResponse{}, err
	}
	if _, err = tx.Exec("INSERT INTO issuance(request_id,binding,response) VALUES(?,?,?)", request.RequestID, binding, string(encoded)); err != nil {
		return EnrollmentResponse{}, err
	}
	if err = tx.Commit(); err != nil {
		return EnrollmentResponse{}, err
	}
	return response, nil
}

func (s *SecurityEnrollment) ready() error {
	if s.closed {
		return fmt.Errorf("security enrollment is closed")
	}
	return nil
}
func parseEnrollmentCSR(value string) (*x509.CertificateRequest, error) {
	block, _ := pem.Decode([]byte(value))
	if block == nil || block.Type != "CERTIFICATE REQUEST" {
		return nil, fmt.Errorf("invalid CSR")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("invalid CSR: %w", err)
	}
	if err = csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("invalid CSR signature")
	}
	if _, ok := csr.PublicKey.(*ecdsa.PublicKey); !ok {
		return nil, fmt.Errorf("CSR public key must be ECDSA")
	}
	return csr, nil
}
func (s *SecurityEnrollment) signEnrollmentCSR(csr *x509.CertificateRequest, nodeID string) (string, string, int64, error) {
	return s.signEnrollmentCSRAt(csr, nodeID, time.Now().UTC())
}

func (s *SecurityEnrollment) signEnrollmentCSRAt(csr *x509.CertificateRequest, nodeID string, now time.Time) (string, string, int64, error) {
	serialBytes := make([]byte, 16)
	if _, err := rand.Read(serialBytes); err != nil {
		return "", "", 0, err
	}
	serialBytes[0] &= 0x7f
	serialBytes[0] |= 0x01
	serialInt := new(big.Int).SetBytes(serialBytes)
	now = now.UTC()
	expires := now.UnixMilli()/1000*1000 + enrollmentLeafLifetime.Milliseconds()
	template := &x509.Certificate{SerialNumber: serialInt, Subject: pkix.Name{CommonName: nodeID}, NotBefore: now.Add(-5 * time.Minute), NotAfter: time.UnixMilli(expires), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, AuthorityKeyId: append([]byte(nil), s.root.SubjectKeyId...)}
	der, err := x509.CreateCertificate(rand.Reader, template, s.root, csr.PublicKey, s.rootKey)
	if err != nil {
		return "", "", 0, err
	}
	serial, err := certificateSerialCompat(der)
	if err != nil {
		return "", "", 0, err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), serial, expires, nil
}
func sha256Hex(value []byte) string { sum := sha256.Sum256(value); return hex.EncodeToString(sum[:]) }
