package storage

import (
	"context"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestSecurityPeerReceiptAndInstanceBindingPersist(t *testing.T) {
	s, profile, _, request, enrolled := enrollSecurityFixture(t)
	ctx := context.Background()
	invitation, err := s.CopyInvitation()
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.Enroll(ctx, EnrollmentRequest{
		RequestID: uuid.NewString(), InstanceID: uuid.NewString(),
		Invitation: invitation.Invitation, CSRPEM: enrollmentCSR(t),
	})
	if err != nil {
		t.Fatal(err)
	}
	instance := uuid.NewString()
	if err := s.BindInstance(ctx, enrolled.NodeID, instance); err != nil {
		t.Fatal(err)
	}
	const consumer = "fixture-consumer"
	if err := s.BindConsumer(ctx, enrolled.NodeID, consumer); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSecurityEnrollment(ctx, profile)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	got, found, err := reopened.IssuanceResponse(ctx, request.RequestID)
	if err != nil || !found || got != enrolled {
		t.Fatalf("durable enrollment receipt changed: found=%t err=%v", found, err)
	}
	if _, found, err := reopened.IssuanceResponse(ctx, uuid.NewString()); found || err != nil {
		t.Fatalf("missing receipt: found=%t err=%v", found, err)
	}
	if _, _, err := reopened.IssuanceResponse(ctx, "invalid"); err == nil {
		t.Fatal("malformed request ID accepted")
	}
	if err := reopened.BindInstance(ctx, other.NodeID, instance); err == nil {
		t.Fatal("instance moved to another node")
	}
	if err := reopened.BindInstance(ctx, enrolled.NodeID, instance); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{uuid.Nil.String(), "{" + instance + "}"} {
		if err := reopened.BindInstance(ctx, enrolled.NodeID, invalid); err == nil {
			t.Fatalf("invalid instance accepted: %q", invalid)
		}
	}
	if err := reopened.BindConsumer(ctx, other.NodeID, consumer); err == nil {
		t.Fatal("consumer moved to another node")
	}
	if err := reopened.BindConsumer(ctx, enrolled.NodeID, consumer); err != nil {
		t.Fatal(err)
	}
	for _, invalid := range []string{"", strings.Repeat("a", 129)} {
		if err := reopened.BindConsumer(ctx, enrolled.NodeID, invalid); err == nil {
			t.Fatalf("invalid consumer accepted: %q", invalid)
		}
	}
}
