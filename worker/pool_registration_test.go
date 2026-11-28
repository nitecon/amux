package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/identity/agent"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

type registrationTrace struct {
	path         string
	origin       agent.Origin
	registration struct {
		Generation      string  `json:"generation"`
		CanonicalRemote *string `json:"canonical_remote"`
		Hostname        *string `json:"hostname"`
	}
	report storage.PresenceReport
}

type loseRegistrationAck struct {
	base  http.RoundTripper
	calls atomic.Int32
	trace chan registrationTrace
}

func (t *loseRegistrationAck) RoundTrip(request *http.Request) (*http.Response, error) {
	trace := registrationTrace{path: request.URL.Path}
	trace.origin = agent.Origin{
		SessionID: request.Header.Get("X-Agent-Session-Id"), InstanceID: request.Header.Get("X-Agent-Instance-Id"),
		Provider: request.Header.Get("X-Agent-Provider"), OS: request.Header.Get("X-Agent-OS"),
	}
	if request.GetBody != nil {
		body, err := request.GetBody()
		if err != nil {
			return nil, err
		}
		defer body.Close()
		if strings.HasSuffix(trace.path, "/task-targets") {
			err = json.NewDecoder(body).Decode(&trace.registration)
		} else if strings.HasSuffix(trace.path, "/worker-presence") {
			err = json.NewDecoder(body).Decode(&trace.report)
		}
		if err != nil {
			return nil, err
		}
	}
	t.trace <- trace
	response, err := t.base.RoundTrip(request)
	if err == nil && strings.HasSuffix(trace.path, "/task-targets") && t.calls.Add(1) == 1 {
		response.Body.Close()
		return nil, errors.New("registration acknowledgement lost")
	}
	return response, err
}

func TestPoolPresenceRegistersAdHocAndMatchingRepositoryTargets(t *testing.T) {
	for _, test := range []struct {
		name      string
		withGit   bool
		canonical string
	}{
		{name: "ad hoc"},
		{name: "matching repository", withGit: true, canonical: "github.com/example/native-pool"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			remote, local, client, host, pool, slot := poolRegistrationFixture(t, ctx, test.canonical, test.withGit, nil)
			defer remote.Close()
			defer local.Close()
			defer host.Close()
			defer host.Config.Handler.(interface{ ShutdownStreams(context.Context) error }).ShutdownStreams(context.Background())

			if err := poolPresence(ctx, PoolConfig{Store: local, Client: client, PoolID: pool.ID}, *pool, slot, false); err != nil {
				t.Fatal(err)
			}
			targets, err := remote.Targets(ctx, pool.Project, time.Now().UTC())
			if err != nil || len(targets) != 1 {
				t.Fatalf("registered targets=%+v err=%v", targets, err)
			}
			if targets[0].Target.Origin != slot.Owner || targets[0].Target.Generation != slot.Owner.InstanceID {
				t.Fatalf("registered owner/generation=%+v, want owner=%+v generation=%s", targets[0].Target, slot.Owner, slot.Owner.InstanceID)
			}
			if targets[0].Hostname == nil || strings.TrimSpace(*targets[0].Hostname) == "" {
				t.Fatalf("registration omitted valid hostname: %+v", targets[0])
			}
			if test.canonical == "" && targets[0].CanonicalRemote != nil {
				t.Fatalf("ad hoc target invented a repository binding: %+v", targets[0])
			}
			if test.canonical != "" && (targets[0].CanonicalRemote == nil || *targets[0].CanonicalRemote != test.canonical) {
				t.Fatalf("target binding=%v, want %q", targets[0].CanonicalRemote, test.canonical)
			}
			presence, err := remote.Presence(ctx, pool.Project)
			if err != nil || len(presence) != 1 || presence[0].AutoAssign != true {
				t.Fatalf("presence=%+v err=%v", presence, err)
			}
		})
	}
}

func TestPoolPresenceRejectsWrongLocalRepositoryBeforeAdvertising(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	const serverRemote = "github.com/example/server-repository"
	remote, local, client, host, pool, slot := poolRegistrationFixture(t, ctx, serverRemote, true, nil)
	defer remote.Close()
	defer local.Close()
	defer host.Close()
	defer host.Config.Handler.(interface{ ShutdownStreams(context.Context) error }).ShutdownStreams(context.Background())

	if err := poolPresence(ctx, PoolConfig{Store: local, Client: client, PoolID: pool.ID}, *pool, slot, false); err == nil {
		t.Fatal("wrong local repository was accepted")
	}
	presence, err := remote.Presence(ctx, pool.Project)
	if err != nil || len(presence) != 0 {
		t.Fatalf("wrong binding advertised activity: presence=%+v err=%v", presence, err)
	}
	targets, err := remote.Targets(ctx, pool.Project, time.Now().UTC())
	if err != nil || len(targets) != 0 {
		t.Fatalf("wrong binding registered a target: targets=%+v err=%v", targets, err)
	}
	updated, err := local.WorkerPoolSlots(ctx, pool.ID)
	if err != nil || len(updated) != 1 || updated[0].Owner != slot.Owner || updated[0].RunID != slot.RunID {
		t.Fatalf("binding failure changed durable slot ownership: slots=%+v err=%v", updated, err)
	}
}

func TestPoolPresenceRetriesLostRegistrationAckWithCurrentOwnerAndStableLifetime(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	trace := make(chan registrationTrace, 8)
	transport := &loseRegistrationAck{base: http.DefaultTransport, trace: trace}
	remote, local, client, host, pool, slot := poolRegistrationFixture(t, ctx, "", false, transport)
	defer remote.Close()
	defer local.Close()
	defer host.Close()
	defer host.Config.Handler.(interface{ ShutdownStreams(context.Context) error }).ShutdownStreams(context.Background())

	task, err := remote.InsertTask(ctx, storage.TaskInsert{Project: pool.Project, Title: "owner generation", Reporter: "test"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	conversation, err := remote.CreateConversation(ctx, pool.Project, task.ID, &slot.Owner)
	if err != nil {
		t.Fatal(err)
	}
	run := storage.WorkerRun{ID: uuid.NewString(), Server: host.URL, Project: pool.Project, ConversationID: conversation.ID, Directory: pool.Directory, Owner: slot.Owner}
	if err := local.AttachWorkerPoolRun(ctx, pool.ID, slot.ID, run); err != nil {
		t.Fatal(err)
	}
	prepared, err := local.PrepareWorkerInstance(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.ActivateWorkerInstance(ctx, run.ID, prepared.PendingInstanceID); err != nil {
		t.Fatal(err)
	}
	current, err := poolSlot(ctx, local, pool.ID, slot.ID)
	if err != nil {
		t.Fatal(err)
	}
	currentRun, err := local.WorkerRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	currentOwner := currentRun.Owner
	clientRun := uuid.NewString()
	if err := poolPresence(ctx, PoolConfig{Store: local, Client: client, PoolID: pool.ID, ClientRun: clientRun}, *pool, current, true); err != nil {
		t.Fatal(err)
	}
	if transport.calls.Load() != 2 {
		t.Fatalf("registration attempts=%d, want lost acknowledgement and idempotent retry", transport.calls.Load())
	}
	var requests []registrationTrace
	for len(requests) < 3 {
		select {
		case request := <-trace:
			requests = append(requests, request)
		case <-ctx.Done():
			t.Fatalf("captured %d requests before timeout", len(requests))
		}
	}
	first, retry, report := requests[0], requests[1], requests[2]
	if first.path != retry.path || !strings.HasSuffix(first.path, "/task-targets") || first.origin != retry.origin || !reflect.DeepEqual(first.registration, retry.registration) {
		t.Fatalf("registration retry changed its identity: first=%+v retry=%+v", first, retry)
	}
	if first.origin != currentOwner || first.registration.Generation != currentOwner.InstanceID {
		t.Fatalf("registration used stale owner generation: origin=%+v generation=%q want=%+v", first.origin, first.registration.Generation, currentOwner)
	}
	if !strings.HasSuffix(report.path, "/worker-presence") || report.report.Sequence != current.ReportSequence+1 || report.report.ClientRun == nil || *report.report.ClientRun != clientRun {
		t.Fatalf("presence lost persisted sequence/client lifetime: %+v", report)
	}
	machineOS, hostname := poolMachineMetadata()
	if report.report.OS != machineOS || hostname == nil || report.report.Hostname == nil || *report.report.Hostname != *hostname {
		t.Fatalf("presence omitted host envelope: %+v", report.report)
	}
	updated, err := local.WorkerPoolSlots(ctx, pool.ID)
	if err != nil || len(updated) != 1 || updated[0].Owner != currentOwner || updated[0].RunID != run.ID {
		t.Fatalf("registration retry changed durable run ownership: %+v err=%v", updated, err)
	}
	registered, err := remote.Targets(ctx, pool.Project, time.Now().UTC())
	if err != nil || len(registered) != 1 || registered[0].Target.Origin != currentOwner || registered[0].Target.Generation != currentOwner.InstanceID {
		t.Fatalf("registered target=%+v err=%v", registered, err)
	}
	omitPoolSlot(PoolConfig{Store: local, Client: client, PoolID: pool.ID, ClientRun: clientRun}, *pool, current)
	shutdown := <-trace
	if !strings.HasSuffix(shutdown.path, "/worker-presence") || len(shutdown.report.Agents) != 0 || shutdown.report.OS != machineOS || hostname == nil || shutdown.report.Hostname == nil || *shutdown.report.Hostname != *hostname {
		t.Fatalf("shutdown did not send an empty inventory with machine metadata: %+v", shutdown)
	}
}

func poolRegistrationFixture(t *testing.T, ctx context.Context, boundRemote string, localRepo bool, transport http.RoundTripper) (*storage.Store, *storage.Store, *gatewayclient.Client, *httptest.Server, *storage.WorkerPool, storage.WorkerPoolSlot) {
	t.Helper()
	remote, err := storage.Open(ctx, filepath.Join(t.TempDir(), "remote.db"))
	if err != nil {
		t.Fatal(err)
	}
	project := storage.Project{Ident: "pool-registration", ChannelName: "pool-registration", Kind: "adhoc"}
	if boundRemote != "" {
		project.Kind = "repo"
		project.CanonicalRemote = &boundRemote
	}
	if err := remote.InsertProject(ctx, project); err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewHandler(remote, "pool-registration-secret")
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(handler)
	client, err := gatewayclient.New(host.URL, "pool-registration-secret", &http.Client{Transport: transport})
	if err != nil {
		t.Fatal(err)
	}
	local, err := storage.Open(ctx, filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	if localRepo {
		runGit(t, directory, "init", "-q")
		runGit(t, directory, "remote", "add", "origin", "git@github.com:example/native-pool.git")
	}
	osName := runtime.GOOS
	if osName == "darwin" {
		osName = "macos"
	}
	pool, err := local.CreateWorkerPool(ctx, storage.WorkerPool{
		ID: uuid.NewString(), Server: host.URL, Project: project.Ident, Directory: directory,
		Provider: "codex", OS: osName, SlotCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	slots, err := local.WorkerPoolSlots(ctx, pool.ID)
	if err != nil || len(slots) != 1 {
		t.Fatalf("pool slots=%+v err=%v", slots, err)
	}
	return remote, local, client, host, pool, slots[0]
}

func runGit(t *testing.T, directory string, args ...string) {
	t.Helper()
	command := exec.Command("git", append([]string{"-C", directory}, args...)...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v: %s", args, err, output)
	}
}
