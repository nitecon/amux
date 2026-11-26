package worker

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/worksubmission"
)

type approvalRecorder struct {
	Session
	resolved   []string
	resolveErr error
}

func (s *approvalRecorder) ResolvePermission(_ context.Context, requestID, optionID string) error {
	s.resolved = append(s.resolved, requestID+"/"+optionID)
	return s.resolveErr
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

func TestOpenWithToolApprovalsUsesPermissionCWDAndFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "work-preferences.json")
	root := filepath.Join(t.TempDir(), "repo")
	other := filepath.Join(t.TempDir(), "other")
	if err := worksubmission.AddDirectoryToolApproval(path, root, "git log *"); err != nil {
		t.Fatal(err)
	}
	requestData, err := json.Marshal(map[string]any{"command": []string{"git", "log", "-3"}, "cwd": root})
	if err != nil {
		t.Fatal(err)
	}
	request := conversation.Event{Kind: "permission", RequestID: "cwd", Data: requestData, Options: []conversation.PermissionOption{{ID: "allow", Kind: "allow_once"}}}
	var sink conversation.Sink
	session := &approvalRecorder{}
	forwarded := 0
	open := OpenWithToolApprovals(func(_ context.Context, _ storage.WorkerRun, got conversation.Sink) (Session, error) {
		sink = got
		return session, nil
	}, path)
	run := storage.WorkerRun{Directory: other}
	if _, err := open(context.Background(), run, func(context.Context, conversation.Event) error { forwarded++; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := sink(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(session.resolved) != 1 {
		t.Fatalf("scoped request not approved: %v", session.resolved)
	}
	// A request whose actual cwd differs from the saved scope must be forwarded.
	request.RequestID = "mismatch"
	request.Data, err = json.Marshal(map[string]any{"command": []string{"git", "log", "-3"}, "cwd": other})
	if err != nil {
		t.Fatal(err)
	}
	if err := sink(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(session.resolved) != 1 || forwarded != 1 {
		t.Fatalf("mismatched cwd forwarded=%v resolved=%v", forwarded, session.resolved)
	}
	// Rules are loaded for every permission event, so adding one after Open takes effect.
	if err := worksubmission.AddDirectoryToolApproval(path, other, "git log *"); err != nil {
		t.Fatal(err)
	}
	request.RequestID = "added-after-open"
	if err := sink(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if len(session.resolved) != 2 {
		t.Fatalf("new rule was not applied after open: %v", session.resolved)
	}
	// A failed ResolvePermission must retain the normal user decision path.
	session.resolveErr = errors.New("provider rejected resolution")
	request.RequestID = "resolve-error"
	if err := sink(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if forwarded != 2 || len(session.resolved) != 3 {
		t.Fatalf("failed resolution forwarded=%v resolved=%v", forwarded, session.resolved)
	}
	// When cwd is omitted, the saved run directory scopes the decision.
	session.resolveErr = nil
	request.RequestID = "fallback"
	request.Data, err = json.Marshal(map[string]any{"command": []string{"git", "log", "-3"}})
	if err != nil {
		t.Fatal(err)
	}
	run.Directory = root
	if _, err := open(context.Background(), run, func(context.Context, conversation.Event) error { forwarded++; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := sink(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if forwarded != 2 || len(session.resolved) != 4 {
		t.Fatalf("fallback forwarded=%v resolved=%v", forwarded, session.resolved)
	}
}
