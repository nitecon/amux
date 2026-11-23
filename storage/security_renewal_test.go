package storage

import (
	"context"
	"crypto/x509"
	"database/sql"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func certificateFromEnrollmentResponse(t *testing.T, response EnrollmentResponse) *x509.Certificate {
	t.Helper()
	block, _ := pem.Decode([]byte(response.CertificatePEM))
	if block == nil {
		t.Fatal("certificate PEM missing")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return certificate
}

func TestSecurityRenewalReplayPendingAndActivationOverlap(t *testing.T) {
	ctx := context.Background()
	s, _, originalCertificate, _, original := enrollSecurityFixture(t)
	request := RenewalRequest{RequestID: uuid.NewString(), CSRPEM: enrollmentCSR(t)}
	pending, err := s.RenewCertificate(ctx, originalCertificate, request)
	if err != nil {
		t.Fatal(err)
	}
	if pending.NodeID != original.NodeID || pending.Serial == original.Serial {
		t.Fatalf("renewal response mismatch: %+v", pending)
	}
	pendingCertificate := certificateFromEnrollmentResponse(t, pending)
	var activated int
	var pendingSerial sql.NullString
	if err = s.db.QueryRow("SELECT activated FROM certificates WHERE serial=?", pending.Serial).Scan(&activated); err != nil || activated != 0 {
		t.Fatalf("pending certificate should not be activated: %d %v", activated, err)
	}
	if err = s.db.QueryRow("SELECT pending_serial FROM nodes WHERE id=?", original.NodeID).Scan(&pendingSerial); err != nil || !pendingSerial.Valid || pendingSerial.String != pending.Serial {
		t.Fatalf("pending serial was not recorded: %+v %v", pendingSerial, err)
	}
	status, err := s.AuthorizeCertificate(ctx, pendingCertificate)
	if err != nil || status.PendingSerial == nil || *status.PendingSerial != pending.Serial {
		t.Fatalf("pending leaf was not accepted as actual identity: %+v %v", status, err)
	}

	// Exact replay is checked before pending/previous transition guards.
	replayed, err := s.RenewCertificate(ctx, pendingCertificate, request)
	if err != nil || replayed.Serial != pending.Serial || replayed.CertificatePEM != pending.CertificatePEM {
		t.Fatalf("pending renewal replay changed response: %+v %v", replayed, err)
	}
	changed := request
	changed.CSRPEM = enrollmentCSR(t)
	if _, err = s.RenewCertificate(ctx, originalCertificate, changed); err == nil || !strings.Contains(err.Error(), "bound") {
		t.Fatalf("rebound request ID accepted: %v", err)
	}
	if _, err = s.RenewCertificate(ctx, pendingCertificate, RenewalRequest{RequestID: uuid.NewString(), CSRPEM: enrollmentCSR(t)}); err == nil || !strings.Contains(err.Error(), "renewal pending") {
		t.Fatalf("second pending renewal accepted: %v", err)
	}

	activatedNode, err := s.ActivateCertificate(ctx, pendingCertificate)
	if err != nil {
		t.Fatal(err)
	}
	if activatedNode.CurrentSerial != pending.Serial || activatedNode.PreviousSerial == nil || *activatedNode.PreviousSerial != original.Serial || activatedNode.PendingSerial != nil {
		t.Fatalf("activation state mismatch: %+v", activatedNode)
	}
	wantPreviousUntil := time.Now().Add(securityCredentialOverlap).UnixMilli()
	if wantPreviousUntil > original.ExpiresAt {
		wantPreviousUntil = original.ExpiresAt
	}
	if activatedNode.PreviousUntil == nil || *activatedNode.PreviousUntil > wantPreviousUntil || wantPreviousUntil-*activatedNode.PreviousUntil > 1000 {
		t.Fatalf("previous overlap not capped by old expiry: got %v want about %d", activatedNode.PreviousUntil, wantPreviousUntil)
	}
	if err = s.db.QueryRow("SELECT activated FROM certificates WHERE serial=?", pending.Serial).Scan(&activated); err != nil || activated != 1 {
		t.Fatalf("activation bit not committed: %d %v", activated, err)
	}
	if _, err = s.AuthorizeCertificate(ctx, originalCertificate); err != nil {
		t.Fatalf("previous leaf should be accepted during overlap: %v", err)
	}
	if _, err = s.RenewCertificate(ctx, originalCertificate, RenewalRequest{RequestID: uuid.NewString(), CSRPEM: enrollmentCSR(t)}); err == nil || !strings.Contains(err.Error(), "previous credential") {
		t.Fatalf("previous leaf renewal should respect overlap guard: %v", err)
	}
	// The same request still replays while the node is in previous-credential state.
	replayed, err = s.RenewCertificate(ctx, originalCertificate, request)
	if err != nil || replayed.Serial != pending.Serial {
		t.Fatalf("replay after activation failed: %+v %v", replayed, err)
	}
	if _, err = s.ActivateCertificate(ctx, pendingCertificate); err != nil {
		t.Fatalf("activation should be idempotent for current leaf: %v", err)
	}
}

func TestSecurityConcurrentActivationWaitsForPreviousDrain(t *testing.T) {
	ctx := context.Background()
	s, _, old, _, original := enrollSecurityFixture(t)
	request := RenewalRequest{RequestID: uuid.NewString(), CSRPEM: enrollmentCSR(t)}
	issued, err := s.RenewCertificate(ctx, old, request)
	if err != nil {
		t.Fatal(err)
	}
	certificate := certificateFromEnrollmentResponse(t, issued)
	type activation struct {
		node AuthorizedSecurityNode
		err  error
	}
	start := make(chan struct{})
	results := make(chan activation, 8)
	for range cap(results) {
		go func() {
			<-start
			node, err := s.ActivateCertificate(ctx, certificate)
			results <- activation{node: node, err: err}
		}()
	}
	close(start)
	var until int64
	for range cap(results) {
		result := <-results
		node := result.node
		if result.err != nil || node.CurrentSerial != issued.Serial ||
			node.PreviousSerial == nil || *node.PreviousSerial != original.Serial || node.PreviousUntil == nil {
			t.Errorf("activation lost the preceding generation: %+v %v", node, result.err)
			continue
		}
		if until != 0 && *node.PreviousUntil != until {
			t.Error("idempotent activation changed the overlap deadline")
		}
		until = *node.PreviousUntil
	}
	if replay, err := s.RenewCertificate(ctx, certificate, request); err != nil || replay.Serial != issued.Serial {
		t.Fatalf("activation lost the renewal receipt: %v", err)
	}
	if err := s.RetireCredentials(ctx, map[string]int{original.Serial: 1}, time.Now()); err != nil {
		t.Fatal(err)
	}
	next := RenewalRequest{RequestID: uuid.NewString(), CSRPEM: enrollmentCSR(t)}
	if _, err := s.RenewCertificate(ctx, certificate, next); err == nil {
		t.Fatal("new renewal accepted before previous connection drained")
	}
	node, err := s.AuthorizeCertificate(ctx, certificate)
	if err != nil || node.PendingSerial != nil || node.PreviousUntil == nil || *node.PreviousUntil != until || node.PreviousSerial == nil || *node.PreviousSerial != original.Serial {
		t.Fatalf("rejected renewal modified durable state: %+v %v", node, err)
	}
	if err := s.RetireCredentials(ctx, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenewCertificate(ctx, certificate, next); err != nil {
		t.Fatalf("new renewal rejected after previous connection drained: %v", err)
	}
}

func TestSecurityActivationRejectsOverlapAndCapsOldExpiry(t *testing.T) {
	ctx := context.Background()
	t.Run("previous not retired", func(t *testing.T) {
		s, _, oldCertificate, _, original := enrollSecurityFixture(t)
		pending, err := s.RenewCertificate(ctx, oldCertificate, RenewalRequest{RequestID: uuid.NewString(), CSRPEM: enrollmentCSR(t)})
		if err != nil {
			t.Fatal(err)
		}
		pendingCertificate := certificateFromEnrollmentResponse(t, pending)
		if _, err = s.db.Exec("UPDATE nodes SET previous_serial='retained',previous_until=? WHERE id=?", time.Now().Add(time.Minute).UnixMilli(), original.NodeID); err != nil {
			t.Fatal(err)
		}
		if _, err = s.ActivateCertificate(ctx, pendingCertificate); err == nil || !strings.Contains(err.Error(), "previous credential") {
			t.Fatalf("activation over existing overlap accepted: %v", err)
		}
		var activated int
		if err = s.db.QueryRow("SELECT activated FROM certificates WHERE serial=?", pending.Serial).Scan(&activated); err != nil || activated != 0 {
			t.Fatalf("failed activation changed the certificate: %d %v", activated, err)
		}
	})
	t.Run("old certificate expiry caps overlap", func(t *testing.T) {
		s, _, oldCertificate, _, original := enrollSecurityFixture(t)
		pending, err := s.RenewCertificate(ctx, oldCertificate, RenewalRequest{RequestID: uuid.NewString(), CSRPEM: enrollmentCSR(t)})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = s.db.Exec("UPDATE certificates SET expires_at=? WHERE serial=?", time.Now().Add(-time.Second).UnixMilli(), original.Serial); err != nil {
			t.Fatal(err)
		}
		pendingCertificate := certificateFromEnrollmentResponse(t, pending)
		if _, err = s.ActivateCertificate(ctx, pendingCertificate); err != nil {
			t.Fatal(err)
		}
		var until int64
		if err = s.db.QueryRow("SELECT previous_until FROM nodes WHERE id=?", original.NodeID).Scan(&until); err != nil {
			t.Fatal(err)
		}
		var oldExpiry int64
		if err = s.db.QueryRow("SELECT expires_at FROM certificates WHERE serial=?", original.Serial).Scan(&oldExpiry); err != nil {
			t.Fatal(err)
		}
		if until != oldExpiry {
			t.Fatalf("overlap was not capped: until=%d expiry=%d", until, oldExpiry)
		}
		if _, err = s.AuthorizeCertificate(ctx, oldCertificate); err == nil {
			t.Fatal("expired old certificate remained authorized")
		}
	})
}

func TestSecurityRenewalAndActivationRejectForeignOrClosedCertificate(t *testing.T) {
	ctx := context.Background()
	s, _, certificate, _, _ := enrollSecurityFixture(t)
	foreign, _, foreignCertificate, _, _ := enrollSecurityFixture(t)
	defer foreign.Close()
	request := RenewalRequest{RequestID: uuid.NewString(), CSRPEM: enrollmentCSR(t)}
	if _, err := s.RenewCertificate(ctx, foreignCertificate, request); err == nil {
		t.Fatal("foreign certificate renewed a node")
	}
	if _, err := s.ActivateCertificate(ctx, foreignCertificate); err == nil {
		t.Fatal("foreign certificate activated a node")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RenewCertificate(ctx, certificate, request); err == nil {
		t.Fatal("closed security store renewed a certificate")
	}
	if _, err := s.ActivateCertificate(ctx, certificate); err == nil {
		t.Fatal("closed security store activated a certificate")
	}
	if _, err := s.AuthorizeCertificate(ctx, certificate); err == nil {
		t.Fatal("closed security store authorized a certificate")
	}
}
