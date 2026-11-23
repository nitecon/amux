package storage

import (
	"context"
	"crypto/x509"
	"encoding/pem"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

func enrollSecurityFixture(t *testing.T) (*SecurityEnrollment, string, *x509.Certificate, EnrollmentRequest, EnrollmentResponse) {
	t.Helper()
	s, profile := enrollmentFixture(t)
	invitation, err := s.IssueInvitation(24181)
	if err != nil {
		t.Fatal(err)
	}
	request := EnrollmentRequest{RequestID: uuid.NewString(), Invitation: invitation.Invitation, CSRPEM: enrollmentCSR(t), InstanceID: uuid.NewString()}
	response, err := s.Enroll(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	block, _ := pem.Decode([]byte(response.CertificatePEM))
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatal(err)
	}
	return s, profile, certificate, request, response
}

func TestSecurityAuthorizationCurrentPendingPreviousAndRevoked(t *testing.T) {
	s, _, certificate, _, response := enrollSecurityFixture(t)
	ctx := context.Background()
	status, err := s.AuthorizeCertificate(ctx, certificate)
	if err != nil {
		t.Fatal(err)
	}
	if status.NodeID != response.NodeID || status.CurrentSerial != response.Serial || status.Role != "client" {
		t.Fatalf("unexpected authorized node: %+v", status)
	}
	var lastSeen int64
	if err = s.db.QueryRow("SELECT last_seen FROM nodes WHERE id=?", response.NodeID).Scan(&lastSeen); err != nil || lastSeen == 0 {
		t.Fatalf("authorize did not update last_seen: %d %v", lastSeen, err)
	}

	// A renewal pending credential is authorized before activation.
	if _, err = s.db.Exec("UPDATE nodes SET current_serial='old',pending_serial=? WHERE id=?", response.Serial, response.NodeID); err != nil {
		t.Fatal(err)
	}
	status, err = s.AuthorizeCertificate(ctx, certificate)
	if err != nil || status.PendingSerial == nil || *status.PendingSerial != response.Serial {
		t.Fatalf("pending credential rejected: %+v %v", status, err)
	}

	// Activation moves the previous certificate into a bounded overlap window.
	until := time.Now().Add(time.Minute).UnixMilli()
	if _, err = s.db.Exec("UPDATE nodes SET current_serial='new',pending_serial=NULL,previous_serial=?,previous_until=? WHERE id=?", response.Serial, until, response.NodeID); err != nil {
		t.Fatal(err)
	}
	status, err = s.AuthorizeCertificate(ctx, certificate)
	if err != nil || status.PreviousSerial == nil || status.PreviousUntil == nil {
		t.Fatalf("overlap credential rejected: %+v %v", status, err)
	}
	if _, err = s.db.Exec("UPDATE nodes SET previous_until=? WHERE id=?", time.Now().UnixMilli(), response.NodeID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AuthorizeCertificate(ctx, certificate); err == nil || !strings.Contains(err.Error(), "retired") {
		t.Fatalf("expired overlap accepted: %v", err)
	}

	if _, err = s.db.Exec("UPDATE nodes SET current_serial=?,previous_serial=NULL,previous_until=NULL,revoked=1 WHERE id=?", response.Serial, response.NodeID); err != nil {
		t.Fatal(err)
	}
	if _, err = s.AuthorizeCertificate(ctx, certificate); err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("revoked certificate accepted: %v", err)
	}
}

func TestSecurityAuthorizationRejectsUntrustedAndExpiredDurableCertificate(t *testing.T) {
	s, _, certificate, _, response := enrollSecurityFixture(t)
	other, _, otherCertificate, _, _ := enrollSecurityFixture(t)
	defer other.Close()
	if _, err := s.AuthorizeCertificate(context.Background(), otherCertificate); err == nil || !strings.Contains(err.Error(), "not trusted") {
		t.Fatalf("foreign profile certificate accepted: %v", err)
	}
	if _, err := s.db.Exec("UPDATE certificates SET expires_at=? WHERE serial=?", time.Now().Add(-time.Second).UnixMilli(), response.Serial); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AuthorizeCertificate(context.Background(), certificate); err == nil {
		t.Fatal("expired durable certificate was authorized")
	}
}

func TestEnrollmentAuthorizationSurvivesRestart(t *testing.T) {
	s, profile, certificate, request, response := enrollSecurityFixture(t)
	before, err := s.AuthorizeCertificate(context.Background(), certificate)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := OpenSecurityEnrollment(context.Background(), profile)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	// Enrollment's durable exact replay and durable authorization both survive a process restart.
	replayed, err := restarted.Enroll(context.Background(), request)
	if err != nil || replayed.Serial != response.Serial {
		t.Fatalf("issuance replay failed after restart: %+v %v", replayed, err)
	}
	after, err := restarted.AuthorizeCertificate(context.Background(), certificate)
	if err != nil {
		t.Fatal(err)
	}
	if after.NodeID != before.NodeID || after.CurrentSerial != before.CurrentSerial {
		t.Fatalf("authorization changed across restart: before=%+v after=%+v", before, after)
	}
}
