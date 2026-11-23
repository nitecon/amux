package storage

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/nitecon/amux/router"
)

func TestAssignmentDeliveredReceiptConsumesOfferWithoutChangingClaim(t *testing.T) {
	s := taskStore(t)
	actor := taskActor()
	task := ownedInProgressTask(t, s, actor)
	uid := addFeedbackOffer(t, s, task, actor, "worker:captured")
	event := router.Event{UID: uuid.NewString(), ID: "agent.notify_result", Version: 1, Source: "worker:captured", EmittedAt: task.UpdatedAt + 1, Correlation: &uid,
		Payload: []byte(`{"target":"` + actor.SessionID + `","status":"delivered"}`)}
	for range 2 {
		if err := s.ReconcileRouterAssignmentFeedback(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	current, err := s.Task(context.Background(), task.ProjectIdent, task.ID)
	if err != nil || current.Task.Status != "in_progress" || current.Task.UpdatedAt != task.UpdatedAt || current.Task.OwnerOrigin == nil || *current.Task.OwnerOrigin != *actor {
		t.Fatalf("delivered receipt changed claim: %+v %v", current, err)
	}
	assertEqual(t, s.db, "SELECT COUNT(*) FROM assignment_offers", int64(0))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM amux_assignment_offer_claims", int64(0))
	assertEqual(t, s.db, "SELECT COUNT(*) FROM assignment_refusals", int64(0))
}
