package channels

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestDiscordGatewayFullQueueResumesRejectedDispatchWithoutLoss(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const total = 300
	resumed := make(chan struct{})
	var resumedOnce sync.Once
	var connections atomic.Int64
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.CloseNow()
		connection := connections.Add(1)
		if err := wsjson.Write(ctx, conn, map[string]any{"op": 10, "d": map[string]int{"heartbeat_interval": 5000}}); err != nil {
			return
		}
		var handshake discordGatewayEnvelope
		for {
			if err := wsjson.Read(ctx, conn, &handshake); err != nil {
				return
			}
			if handshake.Op != 1 {
				break
			}
			if err := wsjson.Write(ctx, conn, map[string]any{"op": 11, "d": nil}); err != nil {
				return
			}
		}
		start := 1
		if connection == 1 {
			if handshake.Op != 2 {
				t.Errorf("initial handshake opcode=%d, want Identify", handshake.Op)
				return
			}
			if err := wsjson.Write(ctx, conn, map[string]any{"op": 0, "s": 1, "t": "READY", "d": map[string]any{
				"session_id": "backpressure-session", "resume_gateway_url": "ws" + strings.TrimPrefix(server.URL, "http"),
				"user": map[string]string{"id": "9999"},
			}}); err != nil {
				return
			}
		} else {
			var resume discordResumePayload
			if handshake.Op != 6 || json.Unmarshal(handshake.D, &resume) != nil || resume.Session != "backpressure-session" || resume.Sequence < 1 || resume.Sequence >= total+1 {
				t.Errorf("invalid backpressure resume: opcode=%d session=%q sequence=%d", handshake.Op, resume.Session, resume.Sequence)
				return
			}
			// READY is sequence 1. Message n has sequence n+1, so the first
			// unaccepted message ID is the last accepted sequence itself.
			start = int(resume.Sequence)
			resumedOnce.Do(func() { close(resumed) })
		}
		readerDone := make(chan struct{})
		go func() {
			defer close(readerDone)
			for {
				var event discordGatewayEnvelope
				if err := wsjson.Read(ctx, conn, &event); err != nil {
					return
				}
				if event.Op == 1 {
					if err := wsjson.Write(ctx, conn, map[string]any{"op": 11, "d": nil}); err != nil {
						return
					}
				}
			}
		}()
		defer func() {
			conn.CloseNow()
			<-readerDone
		}()
		for id := start; id <= total; id++ {
			if err := wsjson.Write(ctx, conn, map[string]any{"op": 0, "s": id + 1, "t": "MESSAGE_CREATE", "d": map[string]any{
				"id": strconv.Itoa(id), "channel_id": "111", "content": fmt.Sprintf("message %d", id),
				"author": map[string]any{"id": "7777", "username": "fixture", "bot": false},
			}}); err != nil {
				return // Queue saturation intentionally closes this connection.
			}
		}
		<-readerDone
	}))
	defer func() {
		cancel()
		server.Close()
	}()
	discord, err := NewDiscord(DiscordConfig{Token: "synthetic-token", GuildID: "100", GatewayURL: "ws" + strings.TrimPrefix(server.URL, "http")})
	if err != nil {
		t.Fatal(err)
	}
	if err := discord.RegisterRoom("111", nil); err != nil {
		t.Fatal(err)
	}
	sink := make(chan InboundEvent)
	runDone := make(chan error, 1)
	runStopped := make(chan struct{})
	go func() {
		defer close(runStopped)
		runDone <- discord.Run(ctx, sink)
	}()
	defer func() {
		cancel()
		<-runStopped
	}()
	// Do not consume until the blocked pipeline forces a resume. This fills
	// all bounded stages rather than merely testing a helper's full channel.
	select {
	case <-resumed:
	case err := <-runDone:
		t.Fatalf("gateway stopped before backpressure resume: %v", err)
	case <-ctx.Done():
		t.Fatal("blocked consumer never forced a gateway resume")
	}
	for id := 1; id <= total; id++ {
		select {
		case event := <-sink:
			if event.RoomID != "111" || event.Message.ID != strconv.Itoa(id) {
				t.Fatalf("replayed delivery at %d: %+v", id, event)
			}
		case err := <-runDone:
			t.Fatalf("gateway stopped at message %d: %v", id, err)
		case <-ctx.Done():
			t.Fatalf("delivery stalled at message %d", id)
		}
	}
	cancel()
	if err := <-runDone; err != nil {
		t.Fatalf("gateway shutdown: %v", err)
	}
}
