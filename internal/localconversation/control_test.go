package localconversation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/providers"
	"github.com/nitecon/amux/storage"
)

type approvalTestProvider struct {
	done     chan struct{}
	approved chan struct{}
	sink     conversation.Sink
}

func (p *approvalTestProvider) Done() <-chan struct{} { return p.done }
func (*approvalTestProvider) Err() error              { return nil }
func (p *approvalTestProvider) Close() error          { close(p.done); return nil }
func (p *approvalTestProvider) ResolvePermission(ctx context.Context, request, option string) error {
	if request == "request" && option == "allow" {
		close(p.approved)
	}
	return nil
}
func (p *approvalTestProvider) Prompt(ctx context.Context, _ string) (string, error) {
	if err := p.sink(ctx, conversation.Event{Kind: "permission", RequestID: "request", Options: []conversation.PermissionOption{{ID: "allow", Name: "Allow"}}}); err != nil {
		return "", err
	}
	select {
	case <-p.approved:
		return "", p.sink(ctx, conversation.Event{Kind: "text_delta", MessageID: "answer", Text: "Approved output"})
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestManagedChatSurvivesViewerDisconnectAndResolvesApproval(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	root := t.TempDir()
	provider := &approvalTestProvider{done: make(chan struct{}), approved: make(chan struct{})}
	mux := http.NewServeMux()
	stop := RegisterControl(mux, ctx, root, func(string) string { return root }, func(_ context.Context, _ storage.WorkerRun, sink conversation.Sink) (providers.Session, error) {
		provider.sink = sink
		return provider, nil
	}, nil)
	defer stop()
	server := httptest.NewServer(mux)
	defer server.Close()
	config := filepath.Join(root, "config.yml")
	if err := os.WriteFile(config+".control", []byte(strings.TrimPrefix(server.URL, "http://")+"\ntest-token\n"), 0600); err != nil {
		t.Fatal(err)
	}
	remote := Remote{ConfigPath: config}
	info, err := remote.Create(ctx, "demo", "codex", "", providers.Preferences{})
	if err != nil {
		t.Fatal(err)
	}
	follow := func(viewCtx context.Context, after int64) (<-chan gatewayclient.Frame, <-chan error) {
		frames, streamErr := make(chan gatewayclient.Frame), make(chan error, 1)
		go func() {
			streamErr <- remote.Stream(viewCtx, "demo", info.ID, gatewayclient.Cursor{Events: after}, func(frame gatewayclient.Frame) error {
				select {
				case frames <- frame:
					return nil
				case <-viewCtx.Done():
					return viewCtx.Err()
				}
			})
		}()
		return frames, streamErr
	}
	waitFrame := func(frames <-chan gatewayclient.Frame, errs <-chan error, kind string) gatewayclient.Frame {
		t.Helper()
		for {
			select {
			case err := <-errs:
				t.Fatalf("stream ended before %s: %v", kind, err)
			case frame := <-frames:
				if frame.Type == kind || frame.Event != nil && frame.Event.Event.Kind == kind {
					return frame
				}
			case <-ctx.Done():
				t.Fatalf("did not receive %s", kind)
			}
		}
	}
	view, closeView := context.WithCancel(ctx)
	defer closeView()
	frames, errs := follow(view, 0)
	waitFrame(frames, errs, "local_ready")
	if err := remote.Enqueue(ctx, "demo", info.ID, "prompt", conversation.Command{Kind: "prompt", Text: "Original question"}); err != nil {
		t.Fatal(err)
	}
	permission := waitFrame(frames, errs, "permission").Event
	closeView()
	<-errs
	select {
	case <-provider.done:
		t.Fatal("provider stopped with the viewer")
	default:
	}
	approvalWatch, closeApprovalWatch := context.WithCancel(ctx)
	defer closeApprovalWatch()
	snapshots := make(chan []Approval, 4)
	go func() {
		_ = remote.WatchApprovals(approvalWatch, func(approvals []Approval) error {
			select {
			case snapshots <- approvals:
				return nil
			case <-approvalWatch.Done():
				return approvalWatch.Err()
			}
		})
	}()
	select {
	case pending := <-snapshots:
		if len(pending) != 1 || pending[0].Event.ID != permission.ID {
			t.Fatalf("reconnected approval stream returned %+v", pending)
		}
	case <-ctx.Done():
		t.Fatal("approval stream did not replay pending approval")
	}
	reopened, closeReopened := context.WithCancel(ctx)
	defer closeReopened()
	frames, errs = follow(reopened, permission.Sequence-1)
	restored := waitFrame(frames, errs, "permission").Event
	if restored.ID != permission.ID {
		t.Fatal("approval event identity changed")
	}
	if err := remote.Enqueue(ctx, "demo", info.ID, "approval", conversation.Command{Kind: "permission", PermissionEventID: restored.ID, OptionID: "allow"}); err != nil {
		t.Fatal(err)
	}
	waitFrame(frames, errs, "turn_completed")
	select {
	case pending := <-snapshots:
		if len(pending) != 0 {
			t.Fatalf("resolved approval stayed pending: %+v", pending)
		}
	case <-ctx.Done():
		t.Fatal("approval stream did not clear resolved approval")
	}
	text, err := os.ReadFile(filepath.Join(root, "demo", info.ID+".md"))
	if err != nil || !strings.Contains(string(text), "> Original question") || !strings.Contains(string(text), "> Approved output") {
		t.Fatalf("unexpected transcript %q, %v", text, err)
	}
}

func TestLocalConversationPopulatesAgentOriginOnCreateAndLegacyFallback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	root := t.TempDir()
	created, err := Create(root, "demo", root, "claude", providers.Preferences{})
	if err != nil {
		t.Fatal(err)
	}
	if created.Owner.SessionID == "" || created.Owner.Provider != "claude" || created.Owner.OS == "" {
		t.Fatalf("created local conversation missing owner identity: %+v", created.Owner)
	}
	var opened storage.WorkerRun
	s, err := Open(root, created, func(_ context.Context, run storage.WorkerRun, _ conversation.Sink) (providers.Session, error) {
		opened = run
		return &approvalTestProvider{done: make(chan struct{}), approved: make(chan struct{})}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	s.KeepAlive(ctx)
	if _, err := s.connect(ctx); err != nil {
		t.Fatal(err)
	}
	s.Stop()
	if opened.Owner.SessionID != created.Owner.SessionID || opened.Owner.InstanceID == "" || opened.Owner.Provider != "claude" || opened.Owner.OS != created.Owner.OS {
		t.Fatalf("connected run had incomplete owner identity: %+v", opened.Owner)
	}

	// Pre-existing local conversation without a persisted owner falls back to its conversation ID.
	legacyID := "018f0000-0000-7000-8000-000000000099"
	legacy := Header{Info: storage.Conversation{ID: legacyID, ProjectIdent: "demo", Provider: "codex"}, Directory: root}
	if err := writeMarkdownChat(filepath.Join(root, "demo", legacyID+".md"), legacy); err != nil {
		t.Fatal(err)
	}
	var legacyOpened storage.WorkerRun
	ls, err := Open(root, legacy.Info, func(_ context.Context, run storage.WorkerRun, _ conversation.Sink) (providers.Session, error) {
		legacyOpened = run
		return &approvalTestProvider{done: make(chan struct{}), approved: make(chan struct{})}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	ls.KeepAlive(ctx)
	if _, err := ls.connect(ctx); err != nil {
		t.Fatal(err)
	}
	ls.Stop()
	if legacyOpened.Owner.SessionID != legacyID || legacyOpened.Owner.InstanceID == "" || legacyOpened.Owner.Provider != "codex" || legacyOpened.Owner.OS == "" {
		t.Fatalf("legacy local conversation owner fallback failed: %+v", legacyOpened.Owner)
	}
}
