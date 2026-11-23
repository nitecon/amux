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
	"encoding/pem"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func enrollmentFixture(t *testing.T) (*SecurityEnrollment, string) {
	t.Helper()
	profile := t.TempDir()
	s, err := OpenSecurityEnrollment(context.Background(), profile)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, profile
}

func TestPairingOriginNormalizesDefaultPortForIPv4AndIPv6(t *testing.T) {
	for _, host := range []string{"10.0.1.101", "[2001:db8::1]"} {
		want := "https://" + host + ":24182"
		for _, input := range []string{"https://" + host, want, want + "/"} {
			origin, err := pairingBaseURL(input)
			if err != nil || origin.String() != want {
				t.Fatalf("pairing origin for %q = %v, %v; want %q", input, origin, err, want)
			}
		}
	}
}

func enrollmentCSR(t *testing.T) string {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "fixture client"}}, key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
}

func TestSecurityEnrollmentIssuanceReplayAndReplacement(t *testing.T) {
	s, profile := enrollmentFixture(t)
	old, err := s.IssueInvitation(24181)
	if err != nil {
		t.Fatal(err)
	}
	current, err := s.IssueInvitation(24182)
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"%v", "%+v", "%#v"} {
		if strings.Contains(fmt.Sprintf(format, current), current.Invitation) {
			t.Fatalf("diagnostic format %s exposed the invitation", format)
		}
	}
	if current.ExpiresAt-time.Now().UnixMilli() > invitationLifetime.Milliseconds() || current.ExpiresAt <= time.Now().UnixMilli() {
		t.Fatalf("unexpected invitation expiry %d", current.ExpiresAt)
	}
	if _, err = s.Enroll(context.Background(), EnrollmentRequest{RequestID: uuid.NewString(), Invitation: old.Invitation, CSRPEM: enrollmentCSR(t), InstanceID: uuid.NewString()}); err == nil || !strings.Contains(err.Error(), "invalid") {
		t.Fatalf("replaced invitation should fail, got %v", err)
	}
	csr := enrollmentCSR(t)
	request := EnrollmentRequest{RequestID: uuid.NewString(), Invitation: current.Invitation, CSRPEM: csr, InstanceID: uuid.NewString()}
	response, err := s.Enroll(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	replay, err := s.Enroll(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	if replay.Serial != response.Serial || replay.CertificatePEM != response.CertificatePEM {
		t.Fatal("exact replay did not return the durable response")
	}
	changed := request
	changed.CSRPEM = enrollmentCSR(t)
	if _, err = s.Enroll(context.Background(), changed); err == nil || !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("same request ID with changed proof should conflict, got %v", err)
	}
	block, _ := pem.Decode([]byte(response.CertificatePEM))
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	if cert.Subject.CommonName != response.NodeID || cert.KeyUsage != x509.KeyUsageDigitalSignature || len(cert.ExtKeyUsage) != 1 || cert.ExtKeyUsage[0] != x509.ExtKeyUsageClientAuth {
		t.Fatalf("unexpected enrolled certificate: %+v", cert)
	}
	if len(cert.AuthorityKeyId) == 0 || !bytes.Equal(cert.AuthorityKeyId, s.root.SubjectKeyId) {
		t.Fatal("enrolled client leaf did not identify the installation CA")
	}
	if _, err = cert.Verify(x509.VerifyOptions{Roots: func() *x509.CertPool {
		pool := x509.NewCertPool()
		rootPEM := s.rootPEM
		pool.AppendCertsFromPEM(rootPEM)
		return pool
	}(), KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}); err != nil {
		t.Fatalf("client certificate chain failed: %v", err)
	}
	if response.Serial == "" {
		t.Fatal("response serial missing")
	}
	var verifier string
	if err := s.db.QueryRow("SELECT verifier FROM invitations WHERE revoked=0").Scan(&verifier); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(verifier, current.Invitation) {
		t.Fatal("invitation plaintext persisted in verifier")
	}
	if _, err = s.Enroll(context.Background(), EnrollmentRequest{RequestID: uuid.NewString(), Invitation: current.Invitation, CSRPEM: enrollmentCSR(t), InstanceID: request.InstanceID}); err == nil || !strings.Contains(err.Error(), "already_enrolled") {
		t.Fatalf("duplicate instance enrollment should be rejected, got %v", err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenSecurityEnrollment(context.Background(), profile)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if _, err := restarted.Enroll(context.Background(), EnrollmentRequest{RequestID: uuid.NewString(), Invitation: current.Invitation, CSRPEM: enrollmentCSR(t), InstanceID: uuid.NewString()}); err == nil {
		t.Fatalf("restart should invalidate the outstanding invitation, got %v", err)
	}
	replayedAfterRestart, err := restarted.Enroll(context.Background(), request)
	if err != nil {
		t.Fatalf("durable replay after restart failed: %v", err)
	}
	if replayedAfterRestart.Serial != response.Serial || replayedAfterRestart.CertificatePEM != response.CertificatePEM {
		t.Fatal("restart replay changed the original response")
	}
}

func TestSecurityEnrollmentRevocationExpirationAndRestartLegacyRows(t *testing.T) {
	profile := t.TempDir()
	clusterID, err := EnsureSecurityInstallation(context.Background(), profile)
	if err != nil {
		t.Fatal(err)
	}
	if err = EnsureSecurityRoot(profile, clusterID); err != nil {
		t.Fatal(err)
	}
	// A Rust-compatible profile may already contain legacy installation and invitation rows.
	legacy, err := OpenSecurityEnrollment(context.Background(), profile)
	if err != nil {
		t.Fatal(err)
	}
	inv, err := legacy.IssueInvitation(24181)
	if err != nil {
		t.Fatal(err)
	}
	legacyID := strings.Split(inv.Invitation, ".")[3]
	if err = legacy.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSecurityEnrollment(context.Background(), profile)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	var revoked int
	if err = reopened.db.QueryRow("SELECT revoked FROM invitations WHERE id=?", legacyID).Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	if revoked != 1 {
		t.Fatal("restart must revoke persisted invitations")
	}
	if _, err = reopened.CopyInvitation(); err == nil || !strings.Contains(err.Error(), "unavailable") {
		t.Fatalf("restart should clear active invitation: %v", err)
	}
	_, err = reopened.IssueInvitation(24181)
	if err != nil {
		t.Fatal(err)
	}
	if err = reopened.RevokeInvitation(); err != nil {
		t.Fatal(err)
	}
	if _, err = reopened.CopyInvitation(); err == nil {
		t.Fatal("revoked invitation should not be copied")
	}
	// Backdate the stored deadline without sleeping; expiration must revoke it.
	expired, err := reopened.IssueInvitation(24181)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = reopened.db.Exec("UPDATE invitations SET expires_at=? WHERE id=?", time.Now().Add(-time.Second).UnixMilli(), strings.Split(expired.Invitation, ".")[3]); err != nil {
		t.Fatal(err)
	}
	if _, err = reopened.CopyInvitation(); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired invitation accepted: %v", err)
	}
	var expiredRevoked int
	if err = reopened.db.QueryRow("SELECT revoked FROM invitations WHERE id=?", strings.Split(expired.Invitation, ".")[3]).Scan(&expiredRevoked); err != nil {
		t.Fatal(err)
	}
	if expiredRevoked != 1 {
		t.Fatal("expiration should revoke invitation rows")
	}
}

func TestSecurityEnrollmentRejectsInvalidCSRSignature(t *testing.T) {
	s, _ := enrollmentFixture(t)
	inv, err := s.IssueInvitation(24181)
	if err != nil {
		t.Fatal(err)
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: "bad"}}, key)
	if err != nil {
		t.Fatal(err)
	}
	der[len(der)-1] ^= 1
	badPEM := string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
	request := EnrollmentRequest{RequestID: uuid.NewString(), Invitation: inv.Invitation, CSRPEM: badPEM, InstanceID: uuid.NewString()}
	if _, err = s.Enroll(context.Background(), request); err == nil {
		t.Fatal("invalid CSR signature accepted")
	}
	s.active.deadline = time.Now().Add(-time.Second)
	der[len(der)-1] ^= 1
	request.CSRPEM = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}))
	if _, err = s.Enroll(context.Background(), request); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("corrected proof accepted after monotonic expiry: %v", err)
	}
}

func TestSecurityEnrollmentSchemaPreservesExistingRows(t *testing.T) {
	profile := t.TempDir()
	clusterID := uuid.NewString()
	db, err := sql.Open("sqlite", filepath.Join(profile, "security.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE installation(id INTEGER PRIMARY KEY CHECK(id=1),cluster_id TEXT NOT NULL);
INSERT INTO installation(id,cluster_id) VALUES(1,?);
CREATE TABLE invitations(id TEXT PRIMARY KEY, verifier TEXT NOT NULL,created_at INTEGER NOT NULL,expires_at INTEGER NOT NULL,revoked INTEGER NOT NULL DEFAULT 0);
INSERT INTO invitations(id,verifier,created_at,expires_at,revoked) VALUES('legacy-row','fixture-verifier',1,2,0);
CREATE TABLE nodes(id TEXT PRIMARY KEY,instance_id TEXT NOT NULL UNIQUE,role TEXT NOT NULL,current_serial TEXT NOT NULL,previous_serial TEXT,previous_until INTEGER,pending_serial TEXT,revoked INTEGER NOT NULL DEFAULT 0,last_seen INTEGER);
CREATE TABLE certificates(serial TEXT PRIMARY KEY,node_id TEXT NOT NULL REFERENCES nodes(id),certificate_pem TEXT NOT NULL,created_at INTEGER NOT NULL,expires_at INTEGER NOT NULL,activated INTEGER NOT NULL DEFAULT 0);
CREATE TABLE issuance(request_id TEXT PRIMARY KEY,binding TEXT NOT NULL,response TEXT NOT NULL);
INSERT INTO issuance(request_id,binding,response) VALUES('legacy-request','legacy-binding','{}');
CREATE TABLE consumers(id TEXT PRIMARY KEY,node_id TEXT NOT NULL REFERENCES nodes(id));
CREATE TABLE native_instances(id TEXT PRIMARY KEY,node_id TEXT NOT NULL REFERENCES nodes(id));`, clusterID)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.Close(); err != nil {
		t.Fatal(err)
	}
	if err = EnsureSecurityRoot(profile, clusterID); err != nil {
		t.Fatal(err)
	}
	s, err := OpenSecurityEnrollment(context.Background(), profile)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	gotCluster, err := EnsureSecurityInstallation(context.Background(), profile)
	if err != nil || gotCluster != clusterID {
		t.Fatalf("legacy installation identity changed: %q %v", gotCluster, err)
	}
	var revoked int
	if err = s.db.QueryRow("SELECT revoked FROM invitations WHERE id='legacy-row'").Scan(&revoked); err != nil {
		t.Fatal(err)
	}
	if revoked != 1 {
		t.Fatal("startup must revoke, not delete, legacy invitation rows")
	}
	var binding string
	if err = s.db.QueryRow("SELECT binding FROM issuance WHERE request_id='legacy-request'").Scan(&binding); err != nil {
		t.Fatal(err)
	}
	if binding != "legacy-binding" {
		t.Fatal("legacy replay row was not preserved")
	}
}
