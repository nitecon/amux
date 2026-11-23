package storage

import (
	"context"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestProvisionLocalColdStartAndRustProfileRestart(t *testing.T) {
	ctx := context.Background()
	profileDir := t.TempDir()
	s, err := OpenSecurityEnrollment(ctx, profileDir)
	if err != nil {
		t.Fatal(err)
	}
	instanceID := uuid.NewString()
	first, err := s.ProvisionLocal(ctx, "https://localhost:24182", instanceID)
	if err != nil {
		t.Fatal(err)
	}
	if first.Version != 1 || first.NodeID == "" || first.InstanceID != instanceID || first.ClusterID != s.clusterID {
		t.Fatalf("bad local profile: %+v", first)
	}
	if filepath.Base(first.RootPEMPath) != "local-root.pem" || filepath.Base(first.CertificatePEMPath) != "local-"+first.NodeID+".pem" || filepath.Base(first.PrivateKeyPEMPath) != "local-"+first.NodeID+"-key.pem" {
		t.Fatalf("unexpected Rust-compatible paths: %+v", first)
	}
	manifest, err := os.ReadFile(filepath.Join(profileDir, "local-client.json"))
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err = json.Unmarshal(manifest, &wire); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"version", "cluster_id", "node_id", "instance_id", "url", "root_pem_path", "certificate_pem_path", "private_key_pem_path", "serial", "expires_at"} {
		if _, ok := wire[key]; !ok {
			t.Fatalf("Rust local profile field %q missing", key)
		}
	}
	status, err := s.AuthorizeCertificate(ctx, localCertificate(t, first))
	if err != nil || status.NodeID != first.NodeID || status.Role != "administrator" {
		t.Fatalf("local administrator was not durable: %+v %v", status, err)
	}
	var activated int
	if err = s.db.QueryRow("SELECT activated FROM certificates WHERE serial=?", first.Serial).Scan(&activated); err != nil || activated != 1 {
		t.Fatalf("local certificate was not activated: %d %v", activated, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}

	restarted, err := OpenSecurityEnrollment(ctx, profileDir)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	reused, err := restarted.ProvisionLocal(ctx, "https://localhost:24182", instanceID)
	if err != nil {
		t.Fatal(err)
	}
	if reused.NodeID != first.NodeID || reused.Serial != first.Serial {
		t.Fatal("cold restart replaced the local administrator")
	}
	updatedURL, err := restarted.ProvisionLocal(ctx, "https://127.0.0.1:24182", instanceID)
	if err != nil || updatedURL.URL != "https://127.0.0.1:24182" || updatedURL.Serial != first.Serial {
		t.Fatalf("URL update changed identity: %+v %v", updatedURL, err)
	}
	if _, err = restarted.ProvisionLocal(ctx, updatedURL.URL, uuid.NewString()); err == nil {
		t.Fatal("changed instance binding was accepted")
	}
}

func TestProvisionLocalExpiredRotationAndRetryAfterSQLiteFailure(t *testing.T) {
	ctx := context.Background()
	profileDir := t.TempDir()
	s, err := OpenSecurityEnrollment(ctx, profileDir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	instanceID := uuid.NewString()
	first, err := s.ProvisionLocal(ctx, "https://localhost:24182", instanceID)
	if err != nil {
		t.Fatal(err)
	}
	rotateAt := time.UnixMilli(first.ExpiresAt + 1)
	if _, err = s.db.Exec(`CREATE TRIGGER fail_local_rotation BEFORE UPDATE OF current_serial ON nodes BEGIN SELECT RAISE(FAIL,'fixture update failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.provisionLocalAt(ctx, "https://localhost:24182", instanceID, rotateAt); err == nil {
		t.Fatal("fixture database failure was ignored")
	}
	if _, err = s.db.Exec("DROP TRIGGER fail_local_rotation"); err != nil {
		t.Fatal(err)
	}
	// The new Rust-compatible local-client manifest and previous manifest were
	// published before the interrupted database update; retry completes it.
	rotated, err := s.ProvisionLocal(ctx, "https://localhost:24182", instanceID)
	if err != nil {
		t.Fatalf("rotation retry failed: %v", err)
	}
	if rotated.NodeID != first.NodeID || rotated.Serial == first.Serial {
		t.Fatalf("rotation did not preserve node identity: old=%+v new=%+v", first, rotated)
	}
	var current, previous string
	var previousUntil int64
	if err = s.db.QueryRow("SELECT current_serial,previous_serial,previous_until FROM nodes WHERE id=?", first.NodeID).Scan(&current, &previous, &previousUntil); err != nil {
		t.Fatal(err)
	}
	if current != rotated.Serial || previous != first.Serial || previousUntil <= first.ExpiresAt {
		t.Fatalf("unexpected recovered rotation state current=%q previous=%q until=%d", current, previous, previousUntil)
	}
	data, err := os.ReadFile(filepath.Join(profileDir, "local-previous.json"))
	if err != nil {
		t.Fatal(err)
	}
	var prior profilePreviousServerIdentity
	if err = json.Unmarshal(data, &prior); err != nil {
		t.Fatal(err)
	}
	if prior.Serial != first.Serial || prior.Certificate != first.CertificatePEMPath || prior.PrivateKey != first.PrivateKeyPEMPath || prior.Until != first.ExpiresAt {
		t.Fatalf("previous manifest is not Rust-compatible: %+v", prior)
	}
}

func TestProvisionLocalColdStartRetryAfterSQLiteFailure(t *testing.T) {
	ctx := context.Background()
	profileDir := t.TempDir()
	s, err := OpenSecurityEnrollment(ctx, profileDir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	instanceID := uuid.NewString()
	if _, err = s.db.Exec(`CREATE TRIGGER fail_local_insert BEFORE INSERT ON nodes BEGIN SELECT RAISE(FAIL,'fixture insert failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ProvisionLocal(ctx, "https://localhost:24182", instanceID); err == nil {
		t.Fatal("fixture database failure was ignored")
	}
	if _, err = s.db.Exec("DROP TRIGGER fail_local_insert"); err != nil {
		t.Fatal(err)
	}
	var profile LocalSecurityProfile
	data, err := os.ReadFile(filepath.Join(profileDir, "local-client.json"))
	if err != nil {
		t.Fatalf("manifest should be published for retry: %v", err)
	}
	if err = json.Unmarshal(data, &profile); err != nil {
		t.Fatal(err)
	}
	recovered, err := s.ProvisionLocal(ctx, "https://localhost:24182", instanceID)
	if err != nil {
		t.Fatalf("cold-start reconciliation failed: %v", err)
	}
	if recovered.NodeID != profile.NodeID || recovered.Serial != profile.Serial {
		t.Fatal("retry generated a second local identity")
	}
	if _, err = s.AuthorizeCertificate(ctx, localCertificate(t, recovered)); err != nil {
		t.Fatalf("recovered local cert not authorized: %v", err)
	}
}

func TestProvisionLocalFailsClosedOnRoleAndInstanceConflict(t *testing.T) {
	ctx := context.Background()
	profileDir := t.TempDir()
	s, err := OpenSecurityEnrollment(ctx, profileDir)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	instanceID := uuid.NewString()
	profile, err := s.ProvisionLocal(ctx, "https://localhost:24182", instanceID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec("UPDATE nodes SET role='client' WHERE id=?", profile.NodeID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ProvisionLocal(ctx, "https://localhost:24182", instanceID); err == nil {
		t.Fatal("non-administrator role was accepted")
	}
	if _, err = s.db.Exec("UPDATE nodes SET role='administrator',instance_id=? WHERE id=?", uuid.NewString(), profile.NodeID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.ProvisionLocal(ctx, "https://localhost:24182", instanceID); err == nil {
		t.Fatal("conflicting instance binding was accepted")
	}
}

func localCertificate(t *testing.T, profile LocalSecurityProfile) *x509.Certificate {
	t.Helper()
	data, err := os.ReadFile(profile.CertificatePEMPath)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode(data)
	if block == nil {
		t.Fatal("local certificate PEM missing")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}
