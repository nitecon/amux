package storage

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
)

const localRenewBefore = enrollmentLeafLifetime / 3

// LocalSecurityProfile is serialized with the Rust local-client.json field names.
type LocalSecurityProfile struct {
	Version            uint32 `json:"version"`
	ClusterID          string `json:"cluster_id"`
	NodeID             string `json:"node_id"`
	InstanceID         string `json:"instance_id"`
	URL                string `json:"url"`
	RootPEMPath        string `json:"root_pem_path"`
	CertificatePEMPath string `json:"certificate_pem_path"`
	PrivateKeyPEMPath  string `json:"private_key_pem_path"`
	Serial             string `json:"serial"`
	ExpiresAt          int64  `json:"expires_at"`
}

// ProvisionLocal creates or reuses this profile's local administrator identity.
// The caller must hold the profile owner lock for the handle's lifetime.
func (s *SecurityEnrollment) ProvisionLocal(ctx context.Context, url, instanceID string) (LocalSecurityProfile, error) {
	return s.provisionLocalAt(ctx, url, instanceID, time.Now().UTC())
}

func (s *SecurityEnrollment) provisionLocalAt(ctx context.Context, url, instanceID string, now time.Time) (LocalSecurityProfile, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return LocalSecurityProfile{}, err
	}
	if strings.TrimSpace(instanceID) == "" {
		return LocalSecurityProfile{}, fmt.Errorf("local instance ID is required")
	}
	manifestPath := filepath.Join(s.profile, "local-client.json")
	exists, err := regularProfileFile(manifestPath)
	if err != nil {
		return LocalSecurityProfile{}, err
	}
	if exists {
		profile, certificate, certificatePEM, err := s.loadLocalProfile(manifestPath, instanceID)
		if err != nil {
			return LocalSecurityProfile{}, err
		}
		previous, err := s.loadLocalPrevious()
		if err != nil {
			return LocalSecurityProfile{}, err
		}
		if err := s.reconcileLocalProfile(ctx, profile, certificatePEM, previous); err != nil {
			return LocalSecurityProfile{}, err
		}
		if profile.ExpiresAt-now.UnixMilli() <= localRenewBefore.Milliseconds() {
			return s.rotateLocalProfile(ctx, manifestPath, profile, certificate, now)
		}
		if profile.URL != url {
			profile.URL = url
			if err := saveLocalProfile(manifestPath, profile); err != nil {
				return LocalSecurityProfile{}, err
			}
		}
		return profile, nil
	}
	return s.createLocalProfile(ctx, manifestPath, url, instanceID, now)
}

func (s *SecurityEnrollment) createLocalProfile(ctx context.Context, manifestPath, url, instanceID string, now time.Time) (LocalSecurityProfile, error) {
	var existing string
	err := s.db.QueryRowContext(ctx, "SELECT id FROM nodes WHERE instance_id=?", instanceID).Scan(&existing)
	if err == nil {
		return LocalSecurityProfile{}, fmt.Errorf("local instance already has a security node without its profile")
	}
	if err != sql.ErrNoRows {
		return LocalSecurityProfile{}, err
	}
	nodeID, err := uuid.NewV7()
	if err != nil {
		return LocalSecurityProfile{}, err
	}
	identity, err := generateLocalClientIdentity()
	if err != nil {
		return LocalSecurityProfile{}, err
	}
	certificatePEM, serial, expiresAt, err := s.signEnrollmentCSRAt(identity.csr, nodeID.String(), now)
	if err != nil {
		return LocalSecurityProfile{}, err
	}
	generation := nodeID.String()
	profile, err := s.localProfilePaths(generation, nodeID.String(), instanceID, url, serial, expiresAt)
	if err != nil {
		return LocalSecurityProfile{}, err
	}
	if err = s.writeLocalRoot(); err != nil {
		return LocalSecurityProfile{}, err
	}
	if err = s.writeLocalLeaf(profile, identity.privateKeyPEM, certificatePEM); err != nil {
		return LocalSecurityProfile{}, err
	}
	if err = saveLocalProfile(manifestPath, profile); err != nil {
		return LocalSecurityProfile{}, err
	}
	if err = s.reconcileLocalProfile(ctx, profile, certificatePEM, nil); err != nil {
		return LocalSecurityProfile{}, err
	}
	return profile, nil
}

func (s *SecurityEnrollment) rotateLocalProfile(ctx context.Context, manifestPath string, profile LocalSecurityProfile, oldCertificate *x509.Certificate, now time.Time) (LocalSecurityProfile, error) {
	identity, err := generateLocalClientIdentity()
	if err != nil {
		return LocalSecurityProfile{}, err
	}
	certificatePEM, serial, expiresAt, err := s.signEnrollmentCSRAt(identity.csr, profile.NodeID, now)
	if err != nil {
		return LocalSecurityProfile{}, err
	}
	generation, err := uuid.NewV7()
	if err != nil {
		return LocalSecurityProfile{}, err
	}
	updated, err := s.localProfilePaths(generation.String(), profile.NodeID, profile.InstanceID, profile.URL, serial, expiresAt)
	if err != nil {
		return LocalSecurityProfile{}, err
	}
	updated.RootPEMPath = profile.RootPEMPath
	if err = s.writeLocalLeaf(updated, identity.privateKeyPEM, certificatePEM); err != nil {
		return LocalSecurityProfile{}, err
	}
	until := now.Add(securityCredentialOverlap).UnixMilli()
	if until > oldCertificate.NotAfter.UnixMilli() {
		until = oldCertificate.NotAfter.UnixMilli()
	}
	previous := profilePreviousServerIdentity{Serial: profile.Serial, Certificate: profile.CertificatePEMPath, PrivateKey: profile.PrivateKeyPEMPath, Until: until}
	if err = saveLocalPrevious(filepath.Join(s.profile, "local-previous.json"), previous); err != nil {
		return LocalSecurityProfile{}, err
	}
	if err = saveLocalProfile(manifestPath, updated); err != nil {
		return LocalSecurityProfile{}, err
	}
	if err = s.reconcileLocalProfile(ctx, updated, certificatePEM, &previous); err != nil {
		return LocalSecurityProfile{}, err
	}
	return updated, nil
}

type localClientIdentity struct {
	privateKeyPEM []byte
	csr           *x509.CertificateRequest
}

func generateLocalClientIdentity() (localClientIdentity, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return localClientIdentity{}, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return localClientIdentity{}, err
	}
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{}}, key)
	if err != nil {
		return localClientIdentity{}, err
	}
	csr, err := x509.ParseCertificateRequest(csrDER)
	if err != nil {
		return localClientIdentity{}, err
	}
	return localClientIdentity{privateKeyPEM: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), csr: csr}, nil
}

func (s *SecurityEnrollment) localProfilePaths(generation, nodeID, instanceID, url, serial string, expiresAt int64) (LocalSecurityProfile, error) {
	rootPath, err := filepath.Abs(filepath.Join(s.profile, "local-root.pem"))
	if err != nil {
		return LocalSecurityProfile{}, err
	}
	certificatePath, err := filepath.Abs(filepath.Join(s.profile, "local-"+generation+".pem"))
	if err != nil {
		return LocalSecurityProfile{}, err
	}
	privateKeyPath, err := filepath.Abs(filepath.Join(s.profile, "local-"+generation+"-key.pem"))
	if err != nil {
		return LocalSecurityProfile{}, err
	}
	return LocalSecurityProfile{Version: 1, ClusterID: s.clusterID, NodeID: nodeID, InstanceID: instanceID, URL: url, RootPEMPath: rootPath, CertificatePEMPath: certificatePath, PrivateKeyPEMPath: privateKeyPath, Serial: serial, ExpiresAt: expiresAt}, nil
}

func (s *SecurityEnrollment) writeLocalRoot() error {
	path := filepath.Join(s.profile, "local-root.pem")
	exists, err := regularProfileFile(path)
	if err != nil {
		return err
	}
	if exists {
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		block, _ := pem.Decode(data)
		if block == nil || block.Type != "CERTIFICATE" {
			return fmt.Errorf("invalid local root certificate")
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil || !bytes.Equal(certificate.Raw, s.root.Raw) {
			return fmt.Errorf("local root certificate does not match profile")
		}
		return nil
	}
	return replacePrivateFile(path, s.rootPEM)
}

func (s *SecurityEnrollment) writeLocalLeaf(profile LocalSecurityProfile, keyPEM []byte, certificatePEM string) error {
	protected, err := protectProfileRootKey(keyPEM)
	if err != nil {
		return err
	}
	if err = replacePrivateFile(profile.PrivateKeyPEMPath, protected); err != nil {
		return err
	}
	return replacePrivateFile(profile.CertificatePEMPath, []byte(certificatePEM))
}

func saveLocalProfile(path string, profile LocalSecurityProfile) error {
	data, err := json.MarshalIndent(profile, "", "  ")
	if err != nil {
		return err
	}
	return replacePrivateFile(path, data)
}

func saveLocalPrevious(path string, previous profilePreviousServerIdentity) error {
	data, err := json.MarshalIndent(previous, "", "  ")
	if err != nil {
		return err
	}
	return replacePrivateFile(path, data)
}

func (s *SecurityEnrollment) loadLocalPrevious() (*profilePreviousServerIdentity, error) {
	path := filepath.Join(s.profile, "local-previous.json")
	exists, err := regularProfileFile(path)
	if err != nil || !exists {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var previous profilePreviousServerIdentity
	if err = json.Unmarshal(data, &previous); err != nil {
		return nil, fmt.Errorf("invalid previous local identity manifest: %w", err)
	}
	return &previous, nil
}

func (s *SecurityEnrollment) loadLocalProfile(path, instanceID string) (LocalSecurityProfile, *x509.Certificate, string, error) {
	var profile LocalSecurityProfile
	data, err := os.ReadFile(path)
	if err != nil {
		return profile, nil, "", err
	}
	if err = json.Unmarshal(data, &profile); err != nil {
		return profile, nil, "", err
	}
	if profile.Version != 1 || profile.ClusterID != s.clusterID || profile.NodeID == "" || profile.Serial == "" || profile.InstanceID != instanceID {
		return profile, nil, "", fmt.Errorf("local profile binding changed or is invalid")
	}
	rootPath, err := confinedProfilePath(s.profile, profile.RootPEMPath, true)
	if err != nil {
		return profile, nil, "", err
	}
	rootPEM, err := os.ReadFile(rootPath)
	if err != nil {
		return profile, nil, "", err
	}
	rootBlock, _ := pem.Decode(rootPEM)
	if rootBlock == nil || rootBlock.Type != "CERTIFICATE" {
		return profile, nil, "", fmt.Errorf("invalid local root certificate")
	}
	localRoot, err := x509.ParseCertificate(rootBlock.Bytes)
	if err != nil || !bytes.Equal(localRoot.Raw, s.root.Raw) {
		return profile, nil, "", fmt.Errorf("local root certificate does not match profile")
	}
	certificatePath, err := confinedProfilePath(s.profile, profile.CertificatePEMPath, true)
	if err != nil {
		return profile, nil, "", err
	}
	certificatePEMBytes, err := os.ReadFile(certificatePath)
	if err != nil {
		return profile, nil, "", err
	}
	certificateBlock, _ := pem.Decode(certificatePEMBytes)
	if certificateBlock == nil || certificateBlock.Type != "CERTIFICATE" {
		return profile, nil, "", fmt.Errorf("invalid local client certificate")
	}
	certificate, err := x509.ParseCertificate(certificateBlock.Bytes)
	if err != nil {
		return profile, nil, "", err
	}
	serial, err := certificateSerialCompat(certificate.Raw)
	if err != nil || serial != profile.Serial || certificate.Subject.CommonName != profile.NodeID || certificate.NotAfter.UnixMilli() != profile.ExpiresAt || !validLocalClientCertificate(certificate, s.root) {
		return profile, nil, "", fmt.Errorf("local client certificate does not match profile")
	}
	keyPath, err := confinedProfilePath(s.profile, profile.PrivateKeyPEMPath, true)
	if err != nil {
		return profile, nil, "", err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return profile, nil, "", err
	}
	keyPEM, err = unprotectProfileRootKey(keyPEM)
	if err != nil {
		return profile, nil, "", err
	}
	key, err := parseProfileRootKey(keyPEM)
	if err != nil {
		return profile, nil, "", fmt.Errorf("invalid local client private key")
	}
	publicKey, ok := certificate.PublicKey.(*ecdsa.PublicKey)
	if !ok || publicKey.X.Cmp(key.X) != 0 || publicKey.Y.Cmp(key.Y) != 0 {
		return profile, nil, "", fmt.Errorf("local client certificate and key do not match")
	}
	return profile, certificate, string(certificatePEMBytes), nil
}

func validLocalClientCertificate(certificate, root *x509.Certificate) bool {
	if certificate.IsCA || certificate.KeyUsage&x509.KeyUsageDigitalSignature == 0 || !containsExtKeyUsage(certificate.ExtKeyUsage, x509.ExtKeyUsageClientAuth) {
		return false
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)
	_, err := certificate.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, CurrentTime: certificate.NotBefore.Add(time.Second)})
	return err == nil
}

func (s *SecurityEnrollment) reconcileLocalProfile(ctx context.Context, profile LocalSecurityProfile, certificatePEM string, previous *profilePreviousServerIdentity) error {
	var nodeID, instanceID, role, currentSerial string
	var revoked bool
	err := s.db.QueryRowContext(ctx, "SELECT id,instance_id,role,current_serial,revoked FROM nodes WHERE id=?", profile.NodeID).Scan(&nodeID, &instanceID, &role, &currentSerial, &revoked)
	if err == sql.ErrNoRows {
		if previous != nil {
			return fmt.Errorf("local administrator rotation has no durable node row")
		}
		var conflict string
		if lookupErr := s.db.QueryRowContext(ctx, "SELECT id FROM nodes WHERE instance_id=?", profile.InstanceID).Scan(&conflict); lookupErr == nil {
			return fmt.Errorf("local instance is bound to another security node")
		} else if lookupErr != sql.ErrNoRows {
			return lookupErr
		}
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		if _, err = tx.ExecContext(ctx, "INSERT INTO nodes(id,instance_id,role,current_serial) VALUES(?,?,'administrator',?)", profile.NodeID, profile.InstanceID, profile.Serial); err != nil {
			return err
		}
		if _, err = tx.ExecContext(ctx, "INSERT INTO certificates(serial,node_id,certificate_pem,created_at,expires_at,activated) VALUES(?,?,?,?,?,1)", profile.Serial, profile.NodeID, certificatePEM, time.Now().UnixMilli(), profile.ExpiresAt); err != nil {
			return err
		}
		return tx.Commit()
	}
	if err != nil {
		return err
	}
	if instanceID != profile.InstanceID || role != "administrator" || revoked {
		return fmt.Errorf("local administrator identity is unavailable or conflicting")
	}
	if currentSerial == profile.Serial {
		var storedPEM string
		var expires int64
		if err := s.db.QueryRowContext(ctx, "SELECT certificate_pem,expires_at FROM certificates WHERE serial=? AND node_id=?", profile.Serial, profile.NodeID).Scan(&storedPEM, &expires); err != nil {
			return fmt.Errorf("local administrator certificate row is missing: %w", err)
		}
		if storedPEM != certificatePEM || expires != profile.ExpiresAt {
			return fmt.Errorf("local administrator certificate row conflicts with profile")
		}
		return nil
	}
	if previous == nil || previous.Serial != currentSerial {
		return fmt.Errorf("local administrator credential conflicts with durable profile")
	}
	previousCertificatePEM, previousCertificate, err := s.validateLocalPrevious(*previous, profile.NodeID)
	if err != nil {
		return err
	}
	var storedPrevious string
	var previousExpires int64
	if err = s.db.QueryRowContext(ctx, "SELECT certificate_pem,expires_at FROM certificates WHERE serial=? AND node_id=?", previous.Serial, profile.NodeID).Scan(&storedPrevious, &previousExpires); err != nil {
		return fmt.Errorf("previous local administrator certificate row is missing: %w", err)
	}
	if storedPrevious != previousCertificatePEM || previousExpires != previousCertificate.NotAfter.UnixMilli() || previous.Until > previousExpires {
		return fmt.Errorf("previous local administrator certificate conflicts with durable profile")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.ExecContext(ctx, "INSERT INTO certificates(serial,node_id,certificate_pem,created_at,expires_at,activated) VALUES(?,?,?,?,?,1)", profile.Serial, profile.NodeID, certificatePEM, time.Now().UnixMilli(), profile.ExpiresAt); err != nil {
		return err
	}
	newCertificateBlock, _ := pem.Decode([]byte(certificatePEM))
	if newCertificateBlock == nil || newCertificateBlock.Type != "CERTIFICATE" {
		return fmt.Errorf("invalid replacement local certificate")
	}
	newCertificate, err := x509.ParseCertificate(newCertificateBlock.Bytes)
	if err != nil {
		return err
	}
	previousUntil := newCertificate.NotBefore.Add(5*time.Minute + securityCredentialOverlap).UnixMilli()
	result, err := tx.ExecContext(ctx, "UPDATE nodes SET previous_serial=current_serial,previous_until=?,current_serial=?,pending_serial=NULL WHERE id=? AND current_serial=? AND revoked=0", previousUntil, profile.Serial, profile.NodeID, currentSerial)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("local administrator identity changed during rotation")
	}
	return tx.Commit()
}

func (s *SecurityEnrollment) validateLocalPrevious(previous profilePreviousServerIdentity, nodeID string) (string, *x509.Certificate, error) {
	certificatePath, err := confinedProfilePath(s.profile, previous.Certificate, true)
	if err != nil {
		return "", nil, err
	}
	certificatePEM, err := os.ReadFile(certificatePath)
	if err != nil {
		return "", nil, err
	}
	block, _ := pem.Decode(certificatePEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return "", nil, fmt.Errorf("invalid previous local certificate")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return "", nil, err
	}
	serial, err := certificateSerialCompat(certificate.Raw)
	if err != nil || serial != previous.Serial || certificate.Subject.CommonName != nodeID || !validLocalClientCertificate(certificate, s.root) {
		return "", nil, fmt.Errorf("previous local certificate does not match manifest")
	}
	keyPath, err := confinedProfilePath(s.profile, previous.PrivateKey, true)
	if err != nil {
		return "", nil, err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return "", nil, err
	}
	keyPEM, err = unprotectProfileRootKey(keyPEM)
	if err != nil {
		return "", nil, err
	}
	key, err := parseProfileRootKey(keyPEM)
	if err != nil {
		return "", nil, err
	}
	publicKey, ok := certificate.PublicKey.(*ecdsa.PublicKey)
	if !ok || publicKey.X.Cmp(key.X) != 0 || publicKey.Y.Cmp(key.Y) != 0 {
		return "", nil, fmt.Errorf("previous local certificate and key do not match")
	}
	return string(certificatePEM), certificate, nil
}
