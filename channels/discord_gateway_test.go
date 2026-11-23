package channels

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestDiscordGatewayIdentifyHeartbeatAndLiveMessage(t *testing.T) {
	serverErrors := make(chan error, 1)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			serverErrors <- err
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := gatewayTestWrite(conn, map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 60000}}); err != nil {
			serverErrors <- err
			return
		}
		identify, err := gatewayTestRead(ctx, conn)
		if err != nil {
			serverErrors <- err
			return
		}
		var payload discordIdentifyPayload
		if identify.Op != 2 || json.Unmarshal(identify.D, &payload) != nil || payload.Intents != discordGatewayIntents || payload.Properties.OS != runtime.GOOS {
			serverErrors <- fmt.Errorf("unexpected identify payload: op=%d data=%s", identify.Op, identify.D)
			return
		}
		if err := gatewayTestWrite(conn, map[string]any{"op": 0, "s": 1, "t": "READY", "d": map[string]any{
			"session_id": "session-1", "resume_gateway_url": gatewayTestURL(server.URL), "user": map[string]any{"id": "999"},
		}}); err != nil {
			serverErrors <- err
			return
		}
		if err := gatewayTestWrite(conn, map[string]any{"op": 1, "d": nil}); err != nil {
			serverErrors <- err
			return
		}
		heartbeat, err := gatewayTestRead(ctx, conn)
		if err != nil {
			serverErrors <- err
			return
		}
		var sequence int64
		if heartbeat.Op != 1 || json.Unmarshal(heartbeat.D, &sequence) != nil || sequence != 1 {
			serverErrors <- fmt.Errorf("unexpected immediate heartbeat: op=%d data=%s", heartbeat.Op, heartbeat.D)
			return
		}
		if err := gatewayTestWrite(conn, map[string]any{"op": 11, "d": nil}); err != nil {
			serverErrors <- err
			return
		}
		if err := gatewayTestWrite(conn, map[string]any{"op": 0, "s": 2, "t": "MESSAGE_CREATE", "d": map[string]any{
			"id": "222", "channel_id": "123", "content": "hello", "author": map[string]any{"id": "456", "username": "alice"},
		}}); err != nil {
			serverErrors <- err
		}
	}))
	defer server.Close()

	discord, err := NewDiscord(DiscordConfig{Token: "test-token", GuildID: "123", GatewayURL: gatewayTestURL(server.URL)})
	if err != nil {
		t.Fatal(err)
	}
	if err := discord.RegisterRoom("123", nil); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	sink := make(chan InboundEvent, 1)
	runDone := make(chan error, 1)
	go func() { runDone <- discord.Run(ctx, sink) }()
	select {
	case err := <-serverErrors:
		cancel()
		t.Fatal(err)
	case event := <-sink:
		if event.ChannelName != "discord" || event.RoomID != "123" || event.Message.ID != "222" || event.Message.Content != "hello" || event.Message.Sender != "alice" || event.Message.AuthorKind != "human" {
			cancel()
			t.Fatalf("unexpected inbound event: %#v", event)
		}
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("timed out waiting for Discord inbound event")
	}
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Discord Gateway Run did not stop after cancellation")
	}
	select {
	case err := <-serverErrors:
		if err != nil {
			t.Fatal(err)
		}
	default:
	}
}

func TestDiscordGatewayServerHeartbeatDoesNotShortenScheduledACKWindow(t *testing.T) {
	acks := make(chan struct{}, 1)
	serverResult := make(chan error, 1)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			serverResult <- err
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		first, err := gatewayTestRead(ctx, conn)
		if err != nil {
			serverResult <- err
			return
		}
		if first.Op != 1 {
			serverResult <- fmt.Errorf("first client event op = %d, want requested heartbeat", first.Op)
			return
		}
		time.Sleep(150 * time.Millisecond)
		second, err := gatewayTestRead(ctx, conn)
		if err != nil {
			serverResult <- fmt.Errorf("scheduled heartbeat not sent: %w", err)
			return
		}
		if second.Op != 1 {
			serverResult <- fmt.Errorf("second client event op = %d, want scheduled heartbeat", second.Op)
			return
		}
		acks <- struct{}{}
		serverResult <- nil
	}))
	defer server.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, gatewayTestURL(server.URL), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	requests := make(chan struct{}, 1)
	requests <- struct{}{}
	sequence := &atomic.Int64{}
	heartbeatDone := make(chan error, 1)
	go func() {
		heartbeatDone <- runDiscordHeartbeatsWithFirstDelay(ctx, conn, time.Second, sequence, requests, acks, 100*time.Millisecond)
	}()
	select {
	case err := <-serverResult:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("server did not observe delayed scheduled heartbeat")
	}
	cancel()
	select {
	case err := <-heartbeatDone:
		if err != nil {
			t.Fatalf("heartbeat loop failed after ACK: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("heartbeat loop did not stop")
	}
}

func TestDiscordGatewayServerHeartbeatACKHasFullDeadline(t *testing.T) {
	serverDone := make(chan struct{})
	firstHeartbeat := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer close(serverDone)
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			if _, _, err := conn.Read(r.Context()); err != nil {
				return
			}
			select {
			case firstHeartbeat <- struct{}{}:
			default:
			}
		}
	}))
	defer server.Close()

	ctx, cancel := context.WithCancel(context.Background())
	conn, _, err := websocket.Dial(ctx, gatewayTestURL(server.URL), nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	defer conn.CloseNow()
	defer cancel()
	requests := make(chan struct{}, 1)
	requests <- struct{}{}
	heartbeatDone := make(chan error, 1)
	go func() {
		heartbeatDone <- runDiscordHeartbeatsWithFirstDelay(ctx, conn, 500*time.Millisecond, &atomic.Int64{}, requests, make(chan struct{}), 400*time.Millisecond)
	}()
	select {
	case <-firstHeartbeat:
	case <-time.After(time.Second):
		t.Fatal("server did not observe the requested heartbeat")
	}
	select {
	case err := <-heartbeatDone:
		if err == nil || !strings.Contains(err.Error(), "ACK missing") {
			t.Fatalf("heartbeat error = %v, want missing ACK for server-requested heartbeat", err)
		}
	case <-time.After(750 * time.Millisecond):
		t.Fatal("server-requested heartbeat had no ACK deadline")
	}
	_ = conn.CloseNow()
	select {
	case <-serverDone:
	case <-time.After(time.Second):
		t.Fatal("websocket server did not finish after heartbeat close")
	}
}

func TestDiscordGatewayFatalCloseBeforeHello(t *testing.T) {
	for _, code := range []websocket.StatusCode{4004, 4010, 4014} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					return
				}
				defer conn.CloseNow()
				_ = conn.Close(code, "authentication failed")
			}))
			defer server.Close()

			gatewayURL := "ws" + strings.TrimPrefix(server.URL, "http")
			discord, err := NewDiscord(DiscordConfig{Token: "test-token", GuildID: "123", GatewayURL: gatewayURL})
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			err = discord.Run(ctx, make(chan InboundEvent))
			if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("fatal code %d", code)) {
				t.Fatalf("Run error = %v, want fatal close code %d", err, code)
			}
		})
	}
}

func TestDiscordGatewayOpcode7ResumesLastAcceptedSequence(t *testing.T) {
	var connections atomic.Int32
	resumed := make(chan discordResumePayload, 1)
	serverErrors := make(chan error, 1)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			serverErrors <- err
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		index := connections.Add(1)
		if err := gatewayTestWrite(conn, map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 60000}}); err != nil {
			serverErrors <- err
			return
		}
		clientEvent, err := gatewayTestRead(ctx, conn)
		if err != nil {
			serverErrors <- err
			return
		}
		if index == 1 {
			if clientEvent.Op != 2 {
				serverErrors <- fmt.Errorf("first connection op = %d, want Identify", clientEvent.Op)
				return
			}
			if err := gatewayTestWrite(conn, map[string]any{"op": 0, "s": 7, "t": "READY", "d": map[string]any{
				"session_id": "resume-session", "resume_gateway_url": gatewayTestURL(server.URL), "user": map[string]any{"id": "999"},
			}}); err != nil {
				serverErrors <- err
				return
			}
			if err := gatewayTestWrite(conn, map[string]any{"op": 7, "d": nil}); err != nil {
				serverErrors <- err
			}
			return
		}
		var payload discordResumePayload
		if clientEvent.Op != 6 || json.Unmarshal(clientEvent.D, &payload) != nil {
			serverErrors <- fmt.Errorf("second connection did not resume: op=%d data=%s", clientEvent.Op, clientEvent.D)
			return
		}
		resumed <- payload
		_ = conn.Close(websocket.StatusCode(4004), "test complete")
	}))
	defer server.Close()

	discord, err := NewDiscord(DiscordConfig{Token: "test-token", GuildID: "123", GatewayURL: gatewayTestURL(server.URL)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = discord.Run(ctx, make(chan InboundEvent))
	if err == nil || !strings.Contains(err.Error(), "fatal code 4004") {
		t.Fatalf("Run error = %v, want fatal close after resume", err)
	}
	select {
	case payload := <-resumed:
		if payload.Session != "resume-session" || payload.Sequence != 7 || payload.Token != "test-token" {
			t.Fatalf("unexpected resume payload: %#v", payload)
		}
	case serverErr := <-serverErrors:
		t.Fatal(serverErr)
	case <-time.After(time.Second):
		t.Fatal("server did not observe resume")
	}
	select {
	case serverErr := <-serverErrors:
		t.Fatal(serverErr)
	default:
	}
}

func TestDiscordGatewayInvalidSessionDisposition(t *testing.T) {
	for _, tc := range []struct {
		name         string
		resumable    bool
		connectionOp int
		wantNext     discordGatewayNext
		wantCleared  bool
	}{
		{name: "resume", resumable: true, connectionOp: 6, wantNext: gatewayNextResume},
		{name: "identify", resumable: false, connectionOp: 2, wantNext: gatewayNextIdentify, wantCleared: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			serverResult := make(chan error, 1)
			var server *httptest.Server
			server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := websocket.Accept(w, r, nil)
				if err != nil {
					serverResult <- err
					return
				}
				defer conn.CloseNow()
				ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
				defer cancel()
				if err := gatewayTestWrite(conn, map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 60000}}); err != nil {
					serverResult <- err
					return
				}
				event, err := gatewayTestRead(ctx, conn)
				if err != nil {
					serverResult <- err
					return
				}
				if event.Op != tc.connectionOp {
					serverResult <- fmt.Errorf("client op = %d, want %d", event.Op, tc.connectionOp)
					return
				}
				if err := gatewayTestWrite(conn, map[string]any{"op": 9, "d": tc.resumable}); err != nil {
					serverResult <- err
					return
				}
				serverResult <- nil
			}))
			defer server.Close()

			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			conn, _, err := websocket.Dial(ctx, gatewayTestURL(server.URL), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.CloseNow()
			session := newDiscordGatewaySession()
			session.id = "session"
			session.resumeURL = gatewayTestURL(server.URL)
			session.sequence.Store(12)
			next, delay, err := (&Discord{token: "test-token"}).runGatewayConnection(ctx, conn, session, make(chan discordGatewayWork, 1), tc.connectionOp == 6, func() {})
			if err != nil {
				t.Fatal(err)
			}
			if next != tc.wantNext || delay < time.Second || delay > 5*time.Second {
				t.Fatalf("next=%v delay=%v, want next=%v and 1-5s delay", next, delay, tc.wantNext)
			}
			if cleared := !session.canResume(); cleared != tc.wantCleared {
				t.Fatalf("session cleared=%v, want %v", cleared, tc.wantCleared)
			}
			if err := <-serverResult; err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDiscordGatewayMissingHeartbeatACKReconnectsByResume(t *testing.T) {
	var connections atomic.Int32
	serverErrors := make(chan error, 2)
	resumed := make(chan discordResumePayload, 1)
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			serverErrors <- err
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 4*time.Second)
		defer cancel()
		index := connections.Add(1)
		if err := gatewayTestWrite(conn, map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 80}}); err != nil {
			serverErrors <- err
			return
		}
		if index == 1 {
			if _, err := gatewayTestReadUntilOp(ctx, conn, 2); err != nil {
				serverErrors <- err
				return
			}
			if err := gatewayTestWrite(conn, map[string]any{"op": 0, "s": 41, "t": "READY", "d": map[string]any{
				"session_id": "missed-ack", "resume_gateway_url": gatewayTestURL(server.URL), "user": map[string]any{"id": "999"},
			}}); err != nil {
				serverErrors <- err
				return
			}
			for {
				if _, err := gatewayTestRead(ctx, conn); err != nil {
					return // No ACK: the client should reconnect after its heartbeat deadline.
				}
			}
		}
		event, err := gatewayTestReadUntilOp(ctx, conn, 6)
		if err != nil {
			serverErrors <- err
			return
		}
		var payload discordResumePayload
		if err := json.Unmarshal(event.D, &payload); err != nil {
			serverErrors <- err
			return
		}
		resumed <- payload
		_ = conn.Close(websocket.StatusCode(4004), "test complete")
	}))
	defer server.Close()

	discord, err := NewDiscord(DiscordConfig{Token: "test-token", GuildID: "123", GatewayURL: gatewayTestURL(server.URL)})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err = discord.Run(ctx, make(chan InboundEvent))
	if err == nil || !strings.Contains(err.Error(), "fatal code 4004") {
		t.Fatalf("Run error = %v, want fatal close after resumed reconnect", err)
	}
	select {
	case payload := <-resumed:
		if payload.Session != "missed-ack" || payload.Sequence != 41 {
			t.Fatalf("unexpected resumed session after missed ACK: %#v", payload)
		}
	case serverErr := <-serverErrors:
		t.Fatal(serverErr)
	case <-time.After(time.Second):
		t.Fatal("missing heartbeat ACK did not lead to a resume")
	}
}

func TestDiscordGatewaySessionStartLimitAndCancellation(t *testing.T) {
	discord := &Discord{gatewayURL: "ws://localhost/gateway"}
	info := discordGatewayInfo{url: discord.gatewayURL, limited: true, remaining: 0, resetAt: time.Now().Add(20 * time.Millisecond)}
	updated, err := discord.prepareIdentify(context.Background(), info, new(time.Time))
	if err == nil || !strings.Contains(err.Error(), "session start limit exhausted") || updated.remaining != 0 {
		t.Fatalf("prepareIdentify = (%#v, %v), want exhausted session limit", updated, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	info.resetAt = time.Now().Add(time.Hour)
	_, err = discord.prepareIdentify(ctx, info, new(time.Time))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("prepareIdentify cancellation error = %v, want context.Canceled", err)
	}
}

func TestDiscordGatewayReadyBackfillDoesNotBlockHeartbeats(t *testing.T) {
	releaseBackfill := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseBackfill) }) }
	serverErrors := make(chan error, 2)
	heartbeatSeen := make(chan struct{})
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v10/channels/123/messages" {
			if r.URL.Query().Get("after") != "100" || r.URL.Query().Get("limit") != "100" {
				serverErrors <- fmt.Errorf("unexpected backfill query: %s", r.URL.RawQuery)
				release()
				return
			}
			select {
			case <-r.Context().Done():
				return
			case <-releaseBackfill:
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`[{"id":"111","channel_id":"123","content":"from backfill","author":{"id":"456","username":"alice"}}]`))
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			serverErrors <- err
			return
		}
		defer conn.CloseNow()
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if err := gatewayTestWrite(conn, map[string]any{"op": 10, "d": map[string]any{"heartbeat_interval": 60000}}); err != nil {
			serverErrors <- err
			return
		}
		if _, err := gatewayTestReadUntilOp(ctx, conn, 2); err != nil {
			serverErrors <- err
			return
		}
		if err := gatewayTestWrite(conn, map[string]any{"op": 0, "s": 1, "t": "READY", "d": map[string]any{
			"session_id": "backfill", "resume_gateway_url": gatewayTestURL(server.URL), "user": map[string]any{"id": "999"},
		}}); err != nil {
			serverErrors <- err
			return
		}
		if err := gatewayTestWrite(conn, map[string]any{"op": 1, "d": nil}); err != nil {
			serverErrors <- err
			return
		}
		if _, err := gatewayTestReadUntilOp(ctx, conn, 1); err != nil {
			serverErrors <- err
			return
		}
		close(heartbeatSeen)
		release()
		<-ctx.Done()
	}))
	defer server.Close()
	defer release()

	apiBase := server.URL + "/api/v10"
	gatewayURL := gatewayTestURL(server.URL) + "/gateway"
	discord, err := NewDiscord(DiscordConfig{Token: "test-token", GuildID: "123", APIBaseURL: apiBase, GatewayURL: gatewayURL})
	if err != nil {
		t.Fatal(err)
	}
	cursor := "100"
	if err := discord.RegisterRoom("123", &cursor); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	sink := make(chan InboundEvent, 1)
	runDone := make(chan error, 1)
	go func() { runDone <- discord.Run(ctx, sink) }()
	select {
	case err := <-serverErrors:
		cancel()
		t.Fatal(err)
	case event := <-sink:
		if event.RoomID != "123" || event.Message.ID != "111" || event.Message.Content != "from backfill" {
			cancel()
			t.Fatalf("unexpected backfill event: %#v", event)
		}
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("Ready did not deliver the registered room backfill")
	}
	select {
	case <-heartbeatSeen:
	case <-time.After(time.Second):
		t.Fatal("heartbeat was not handled while REST backfill was blocked")
	}
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Discord Gateway did not stop after cancellation")
	}
}

func gatewayTestReadUntilOp(ctx context.Context, conn *websocket.Conn, op int) (discordGatewayTestClientEvent, error) {
	for {
		event, err := gatewayTestRead(ctx, conn)
		if err != nil {
			return discordGatewayTestClientEvent{}, err
		}
		if event.Op == op {
			return event, nil
		}
	}
}

func gatewayTestURL(httpURL string) string {
	return "ws" + strings.TrimPrefix(httpURL, "http")
}

func gatewayTestWrite(conn *websocket.Conn, value any) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return conn.Write(ctx, websocket.MessageText, data)
}

type discordGatewayTestClientEvent struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
}

func gatewayTestRead(ctx context.Context, conn *websocket.Conn) (discordGatewayTestClientEvent, error) {
	frameType, data, err := conn.Read(ctx)
	if err != nil {
		return discordGatewayTestClientEvent{}, err
	}
	if frameType != websocket.MessageText {
		return discordGatewayTestClientEvent{}, fmt.Errorf("received non-text client frame")
	}
	var event discordGatewayTestClientEvent
	if err := json.Unmarshal(data, &event); err != nil {
		return discordGatewayTestClientEvent{}, err
	}
	return event, nil
}
