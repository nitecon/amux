package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestRetireCredentialsDrainsPreviousOnlyWhenCountZeroOrExpired(t *testing.T) {
	ctx := context.Background()
	t.Run("drained", func(t *testing.T) {
		s, _, oldCertificate, _, enrolled := enrollSecurityFixture(t)
		pending, err := s.RenewCertificate(ctx, oldCertificate, RenewalRequest{RequestID: uuid.NewString(), CSRPEM: enrollmentCSR(t)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.ActivateCertificate(ctx, certificateFromEnrollmentResponse(t, pending)); err != nil {
			t.Fatal(err)
		}
		retireAt := time.Now()
		if _, err = s.db.Exec("UPDATE nodes SET previous_until=? WHERE id=?", retireAt.Add(time.Hour).UnixMilli(), enrolled.NodeID); err != nil {
			t.Fatal(err)
		}
		if err = s.RetireCredentials(ctx, map[string]int{enrolled.Serial: 2}, retireAt); err != nil {
			t.Fatal(err)
		}
		var previous string
		if err = s.db.QueryRow("SELECT previous_serial FROM nodes WHERE id=?", enrolled.NodeID).Scan(&previous); err != nil || previous != enrolled.Serial {
			t.Fatalf("active previous serial retired early: %q %v", previous, err)
		}
		if err = s.RetireCredentials(ctx, map[string]int{}, retireAt.Add(time.Minute)); err != nil {
			t.Fatal(err)
		}
		assertNoPreviousSecuritySerial(t, s, enrolled.NodeID)
		if err = s.RetireCredentials(ctx, nil, retireAt.Add(2*time.Minute)); err != nil {
			t.Fatalf("retirement retry should be idempotent: %v", err)
		}
		if _, err = s.AuthorizeCertificate(ctx, oldCertificate); err == nil {
			t.Fatal("drained previous certificate remained authorized")
		}
	})
	t.Run("deadline", func(t *testing.T) {
		s, _, oldCertificate, _, enrolled := enrollSecurityFixture(t)
		pending, err := s.RenewCertificate(ctx, oldCertificate, RenewalRequest{RequestID: uuid.NewString(), CSRPEM: enrollmentCSR(t)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.ActivateCertificate(ctx, certificateFromEnrollmentResponse(t, pending)); err != nil {
			t.Fatal(err)
		}
		retireAt := time.Now()
		if _, err = s.db.Exec("UPDATE nodes SET previous_until=? WHERE id=?", retireAt.Add(-time.Millisecond).UnixMilli(), enrolled.NodeID); err != nil {
			t.Fatal(err)
		}
		if err = s.RetireCredentials(ctx, map[string]int{enrolled.Serial: 4}, retireAt); err != nil {
			t.Fatal(err)
		}
		assertNoPreviousSecuritySerial(t, s, enrolled.NodeID)
	})
}

func assertNoPreviousSecuritySerial(t *testing.T, s *SecurityEnrollment, nodeID string) {
	t.Helper()
	var previous sql.NullString
	var until sql.NullInt64
	if err := s.db.QueryRow("SELECT previous_serial,previous_until FROM nodes WHERE id=?", nodeID).Scan(&previous, &until); err != nil {
		t.Fatal(err)
	}
	if previous.Valid || until.Valid {
		t.Fatalf("previous state remains: serial=%q until=%v", previous.String, until)
	}
}

func TestRetireCredentialsExpiresActiveInvitationAndIsIdempotent(t *testing.T) {
	s, _ := enrollmentFixture(t)
	invitation, err := s.IssueInvitation(24181)
	if err != nil {
		t.Fatal(err)
	}
	if copied, copyErr := s.CopyInvitation(); copyErr != nil || copied.Invitation != invitation.Invitation {
		t.Fatalf("active invitation was retired early: %+v %v", copied, copyErr)
	}
	expiredAt := time.Now().Add(time.Second)
	s.active.deadline = expiredAt.Add(-time.Millisecond)
	if err = s.RetireCredentials(context.Background(), nil, expiredAt); err != nil {
		t.Fatal(err)
	}
	if s.active != nil {
		t.Fatal("expired in-memory invitation was retained")
	}
	var revoked int
	parts := strings.Split(invitation.Invitation, ".")
	if len(parts) != 6 {
		t.Fatalf("unexpected invitation shape: %q", invitation.Invitation)
	}
	rowID := parts[3]
	if err = s.db.QueryRow("SELECT id FROM invitations WHERE revoked=1 AND id=?", rowID).Scan(&rowID); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow("SELECT revoked FROM invitations WHERE id=?", rowID).Scan(&revoked); err != nil || revoked != 1 {
		t.Fatalf("invitation was not durably revoked: %d %v", revoked, err)
	}
	if _, err = s.CopyInvitation(); err == nil {
		t.Fatal("expired invitation remained copyable")
	}
	if err = s.RetireCredentials(context.Background(), nil, expiredAt.Add(time.Second)); err != nil {
		t.Fatalf("expired invitation retry failed: %v", err)
	}
}

func TestRetireCredentialsRemovesExpiredLocalPreviousSafely(t *testing.T) {
	ctx := context.Background()
	s, profileDir := enrollmentFixture(t)
	instanceID := uuid.NewString()
	first, err := s.ProvisionLocal(ctx, "https://localhost:24182", instanceID)
	if err != nil {
		t.Fatal(err)
	}
	retiredAt := time.UnixMilli(first.ExpiresAt + 1)
	rotated, err := s.provisionLocalAt(ctx, "https://localhost:24182", instanceID, retiredAt)
	if err != nil {
		t.Fatal(err)
	}
	previousPath := filepath.Join(profileDir, "local-previous.json")
	if _, err = os.Stat(previousPath); err != nil {
		t.Fatal(err)
	}
	// Simulate interruption after the previous key was already removed.
	if err = os.Remove(first.PrivateKeyPEMPath); err != nil {
		t.Fatal(err)
	}
	if err = s.RetireCredentials(ctx, map[string]int{first.Serial: 4}, retiredAt); err != nil {
		t.Fatal(err)
	}
	for _, oldPath := range []string{first.PrivateKeyPEMPath, first.CertificatePEMPath, previousPath} {
		if _, err = os.Stat(oldPath); !os.IsNotExist(err) {
			t.Fatalf("expired local previous material remains at %s: %v", oldPath, err)
		}
	}
	for _, currentPath := range []string{rotated.PrivateKeyPEMPath, rotated.CertificatePEMPath} {
		if _, err = os.Stat(currentPath); err != nil {
			t.Fatalf("current local material was removed: %s: %v", currentPath, err)
		}
	}
	if err = s.RetireCredentials(ctx, nil, retiredAt.Add(time.Second)); err != nil {
		t.Fatalf("filesystem retirement retry failed: %v", err)
	}
}

func TestRetireCredentialsRejectsLocalPreviousPathEscape(t *testing.T) {
	ctx := context.Background()
	s, profileDir := enrollmentFixture(t)
	if _, err := s.ProvisionLocal(ctx, "https://localhost:24182", uuid.NewString()); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(filepath.Dir(profileDir), "retirement-sentinel")
	if err := os.WriteFile(outside, []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(outside)
	manifest := profilePreviousServerIdentity{Serial: "fixture", Certificate: outside, PrivateKey: filepath.Join(profileDir, "missing-key"), Until: time.Now().Add(-time.Second).UnixMilli()}
	data, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(profileDir, "local-previous.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err = s.RetireCredentials(ctx, nil, time.Now()); err == nil {
		t.Fatal("outside-profile credential path was accepted")
	}
	if _, err = os.Stat(outside); err != nil {
		t.Fatalf("retirement removed outside sentinel: %v", err)
	}
}
