package storage

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/nitecon/amux/channels"
	"github.com/nitecon/amux/plugins"
	"github.com/nitecon/amux/router"
)

type inboundSourceFaultEmitter struct {
	*Store
	failed     chan string
	lostAckUID string
}

func (e *inboundSourceFaultEmitter) EmitRouterEvent(ctx context.Context, registry *router.Registry, request router.EmitRequest) (router.EmitResult, error) {
	result, err := e.Store.EmitRouterEvent(ctx, registry, request)
	if err != nil {
		select {
		case e.failed <- request.UID:
		default:
		}
		return result, err
	}
	if result.Stored && e.lostAckUID == "" {
		e.lostAckUID = request.UID
		return router.EmitResult{}, errors.New("fixture committed acknowledgement lost")
	}
	return result, nil
}

func TestInboundSourceStartupBacklogSurvivesPersistenceFailureAndLostCommitAck(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "inbound-source.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	registry, err := router.NewRegistry([]router.EventDefinition{plugins.InboundDefinition()}, []router.PluginDefinition{
		plugins.InboundPlugin(store).Definition,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `CREATE TRIGGER fail_inbound_source BEFORE INSERT ON router_events
		BEGIN SELECT RAISE(ABORT,'fixture ingress journal failure'); END`); err != nil {
		t.Fatal(err)
	}
	emitter := &inboundSourceFaultEmitter{Store: store, failed: make(chan string, 1)}
	input := make(chan channels.InboundEvent, 256)
	sourceDone := make(chan error, 1)
	sourceStopped := make(chan struct{})
	go func() {
		defer close(sourceStopped)
		sourceDone <- plugins.RunInboundSource(ctx, emitter, registry, input)
	}()
	producerDone := make(chan struct{})
	go func() {
		defer close(producerDone)
		defer close(input)
		for i := range 300 {
			select {
			case <-ctx.Done():
				return
			case input <- channels.InboundEvent{ChannelName: "discord", RoomID: "room", Message: channels.InboundMessage{
				ID: fmt.Sprintf("%03d", i), Content: "hello", AuthorKind: "human",
			}}:
			}
		}
	}()
	defer func() {
		cancel()
		<-producerDone
		<-sourceStopped
	}()
	var failedUID string
	select {
	case failedUID = <-emitter.failed:
	case <-ctx.Done():
		t.Fatal("source never attempted durable acceptance")
	}
	deadline := time.Now().Add(5 * time.Second)
	for len(input) != cap(input) {
		if time.Now().After(deadline) {
			t.Fatal("failed persistence did not fill the bounded startup queue")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-producerDone:
		t.Fatal("producer bypassed persistence backpressure")
	default:
	}
	assertEqual(t, store.db, "SELECT COUNT(*) FROM router_events WHERE id='channel.message_received'", int64(0))
	if _, err := store.db.ExecContext(ctx, "DROP TRIGGER fail_inbound_source"); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-sourceDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("source did not drain the startup backlog")
	}
	<-producerDone
	if emitter.lostAckUID == "" || emitter.lostAckUID != failedUID {
		t.Fatalf("logical input UID changed across failure/commit/replay: failed=%q committed=%q", failedUID, emitter.lostAckUID)
	}
	assertEqual(t, store.db, "SELECT COUNT(*) FROM router_events WHERE id='channel.message_received'", int64(300))
	rows, err := store.db.QueryContext(ctx, `SELECT json_extract(payload,'$.message.id') FROM router_events
		WHERE id='channel.message_received' ORDER BY seq`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for i := range 300 {
		var id string
		if !rows.Next() {
			t.Fatalf("durable prefix ended at message %d", i)
		}
		if err := rows.Scan(&id); err != nil || id != fmt.Sprintf("%03d", i) {
			t.Fatalf("durable source order at %d: id=%q err=%v", i, id, err)
		}
	}
	if rows.Next() || rows.Err() != nil {
		t.Fatalf("unexpected extra event or query failure: %v", rows.Err())
	}
}
