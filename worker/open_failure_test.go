package worker

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/providers/acp"
	"github.com/nitecon/amux/providers/claude"
	"github.com/nitecon/amux/providers/codex"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

// Provider Start functions return concrete pointers. Converting a nil pointer
// and startup error to Session must not make worker cleanup call its methods.
func TestRunDoesNotOwnFailedProviderSession(t *testing.T) {
	for _, provider := range []string{"antigravity", "codex", "claude"} {
		t.Run(provider, func(t *testing.T) {
			ctx := context.Background()
			remote, err := storage.Open(ctx, filepath.Join(t.TempDir(), "remote.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer remote.Close()
			if err := remote.InsertProject(ctx, storage.Project{Ident: "demo", ChannelName: "demo", Kind: "adhoc"}); err != nil {
				t.Fatal(err)
			}
			task, err := remote.InsertTask(ctx, storage.TaskInsert{Project: "demo", Title: "Startup failure", Reporter: "user"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			actor := storage.AgentOrigin{SessionID: uuid.NewString(), InstanceID: uuid.NewString(), Provider: provider, OS: "linux"}
			bound, err := remote.CreateConversation(ctx, "demo", task.ID, &actor)
			if err != nil {
				t.Fatal(err)
			}
			handler, err := server.NewHandler(remote, "fixture")
			if err != nil {
				t.Fatal(err)
			}
			host := httptest.NewServer(handler)
			defer host.Close()
			client, err := gatewayclient.New(host.URL, "fixture", nil)
			if err != nil {
				t.Fatal(err)
			}
			local, err := storage.Open(ctx, filepath.Join(t.TempDir(), "local.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer local.Close()
			run := storage.WorkerRun{ID: uuid.NewString(), Server: client.Endpoint(), Project: "demo", ConversationID: bound.ID, Directory: t.TempDir(), Owner: actor}
			if err := local.SaveWorkerRun(ctx, run); err != nil {
				t.Fatal(err)
			}
			startupErr := errors.New("provider startup failed")
			opener := func(context.Context, storage.WorkerRun, conversation.Sink) (Session, error) {
				switch provider {
				case "antigravity":
					var session *acp.Session
					return session, startupErr
				case "codex":
					var session *codex.Session
					return session, startupErr
				default:
					var session *claude.Session
					return session, startupErr
				}
			}
			if err := Run(ctx, Config{Store: local, Client: client, RunID: run.ID, Open: opener}); !errors.Is(err, startupErr) {
				t.Fatalf("startup error replaced: %v", err)
			}
			release, err := local.AcquireWorkerRun(run.ID)
			if err != nil {
				t.Fatalf("failed start retained run lease: %v", err)
			}
			release()
		})
	}
}
