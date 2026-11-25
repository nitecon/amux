package worker

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/worksubmission"
)

type approvalRecorder struct {
	Session
	resolved []string
}

func (s *approvalRecorder) ResolvePermission(_ context.Context, requestID, optionID string) error {
	s.resolved = append(s.resolved, requestID+"/"+optionID)
	return nil
}

func TestToolApprovalsResolveMatchingRequestsAndForwardOthers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "work-preferences.json")
	if err := worksubmission.AddToolApproval(path, "git grep *"); err != nil {
		t.Fatal(err)
	}
	var forwarded []string
	approvals := &toolApprovals{path: path}
	sink := approvals.sink(func(_ context.Context, event conversation.Event) error {
		forwarded = append(forwarded, event.RequestID)
		return nil
	})
	var session Session = &approvalRecorder{}
	approvals.session.Store(&session)
	options := []conversation.PermissionOption{{ID: "allow-once", Kind: "allow_once"}, {ID: "deny", Kind: "reject_once"}}
	request := func(id, command string) conversation.Event {
		data, _ := json.Marshal(map[string]any{"tool_name": "Bash", "input": map[string]string{"command": command}})
		return conversation.Event{Kind: "permission", RequestID: id, Data: data, Options: options}
	}
	for _, event := range []conversation.Event{request("1", "git grep -i foo"), request("2", "git log"), request("3", "git grep x; rm -rf ~")} {
		if err := sink(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	if got := session.(*approvalRecorder).resolved; len(got) != 1 || got[0] != "1/allow-once" {
		t.Fatalf("resolved = %q", got)
	}
	if len(forwarded) != 2 || forwarded[0] != "2" || forwarded[1] != "3" {
		t.Fatalf("forwarded = %q", forwarded)
	}
}
