package worker

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/storage"
)

func TestRunPoolRejectsChangedCheckoutWithoutLosingActiveWork(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	remote, local, client, host, pool, slot := poolRegistrationFixture(t, ctx, "github.com/example/native-pool", true, http.DefaultTransport)
	defer remote.Close()
	defer local.Close()
	defer host.Close()
	task, err := remote.InsertTask(ctx, storage.TaskInsert{Project: pool.Project, Title: "Saved active work", Reporter: "fixture"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := remote.TransitionTask(ctx, pool.Project, task.ID, "in_progress", &slot.Owner); err != nil {
		t.Fatal(err)
	}
	conv, err := remote.CreateConversation(ctx, pool.Project, task.ID, &slot.Owner)
	if err != nil {
		t.Fatal(err)
	}
	run := storage.WorkerRun{ID: uuid.NewString(), Server: host.URL, Project: pool.Project, ConversationID: conv.ID, Directory: pool.Directory, Owner: slot.Owner}
	if err := local.AttachWorkerPoolRun(ctx, pool.ID, slot.ID, run); err != nil {
		t.Fatal(err)
	}
	before, err := local.WorkerRun(ctx, run.ID)
	if err != nil {
		t.Fatal(err)
	}
	config := PoolConfig{Store: local, Client: client, PoolID: pool.ID, ClientRun: uuid.NewString()}
	if err := poolPresence(ctx, config, *pool, slot, true); err != nil {
		t.Fatal(err)
	}
	registered, err := remote.Targets(ctx, pool.Project, time.Now())
	if err != nil || len(registered) != 1 {
		t.Fatalf("initial routing lease: %+v %v", registered, err)
	}
	// A checkout can change while the worker is offline. Resuming its journal
	// must validate local identity before starting a provider on that checkout.
	runGit(t, pool.Directory, "remote", "set-url", "origin", "git@github.com:example/replacement.git")
	var opened atomic.Int32
	config.Open = func(context.Context, storage.WorkerRun, conversation.Sink) (Session, error) {
		opened.Add(1)
		return nil, errors.New("unexpected provider launch")
	}
	err = RunPool(ctx, config)
	var response *gatewayclient.HTTPError
	if !errors.As(err, &response) || response.Status != http.StatusConflict || opened.Load() != 0 {
		t.Fatalf("changed checkout: error=%v opened=%d", err, opened.Load())
	}
	after, err := local.WorkerRun(ctx, run.ID)
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("registration rejection changed recovery journal: before=%+v after=%+v error=%v", before, after, err)
	}
	detail, err := remote.Task(ctx, pool.Project, task.ID)
	if err != nil || detail.Task.Status != "in_progress" || detail.Task.OwnerOrigin == nil || *detail.Task.OwnerOrigin != slot.Owner {
		t.Fatalf("registration rejection released active work: %+v %v", detail, err)
	}
	if agents, err := remote.Presence(ctx, pool.Project); err != nil || len(agents) != 1 || agents[0].State != "gone" {
		t.Fatalf("failed resume left stale availability: %+v %v", agents, err)
	}
	if targets, err := remote.Targets(ctx, pool.Project, time.Now()); err != nil || len(targets) != 1 || !reflect.DeepEqual(targets, registered) {
		t.Fatalf("wrong checkout renewed manual routing lease: before=%+v after=%+v error=%v", registered, targets, err)
	}
}

func TestPoolMachineSummaryKeepsOneHostAcrossSlotsAndShutdown(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	osName, hostname := poolMachineMetadata()
	if hostname == nil {
		t.Skip("host has no valid hostname for machine deduplication")
	}
	remote, local, client, host, first, firstSlot := poolRegistrationFixture(t, ctx, "", false, http.DefaultTransport)
	defer remote.Close()
	defer local.Close()
	defer host.Close()
	secondConfig := *first
	secondConfig.ID = uuid.NewString()
	second, err := local.CreateWorkerPool(ctx, secondConfig)
	if err != nil {
		t.Fatal(err)
	}
	secondSlots, err := local.WorkerPoolSlots(ctx, second.ID)
	if err != nil || len(secondSlots) != 1 {
		t.Fatalf("second pool: %+v %v", secondSlots, err)
	}
	firstConfig := PoolConfig{Store: local, Client: client, PoolID: first.ID, ClientRun: uuid.NewString()}
	secondRuntime := PoolConfig{Store: local, Client: client, PoolID: second.ID, ClientRun: uuid.NewString()}
	if err := poolPresence(ctx, firstConfig, *first, firstSlot, false); err != nil {
		t.Fatal(err)
	}
	if err := poolPresence(ctx, secondRuntime, *second, secondSlots[0], false); err != nil {
		t.Fatal(err)
	}
	summary, err := remote.PresenceSummary(ctx, time.Now())
	if err != nil || summary.Machines != 1 || summary.Agents != 2 || !reflect.DeepEqual(summary.OS, []string{osName}) {
		t.Fatalf("pool sources split one host: %+v %v", summary, err)
	}
	omitPoolSlot(firstConfig, *first, firstSlot)
	omitPoolSlot(secondRuntime, *second, secondSlots[0])
	summary, err = remote.PresenceSummary(ctx, time.Now())
	if err != nil || summary.Machines != 1 || summary.Agents != 0 || summary.Active != 0 || !reflect.DeepEqual(summary.OS, []string{osName}) {
		t.Fatalf("empty shutdown reports lost host identity: %+v %v", summary, err)
	}
}
