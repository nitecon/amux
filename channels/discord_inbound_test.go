package channels

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestDiscordInboundRoomRegistrationKeepsFirstCursor(t *testing.T) {
	d := &Discord{}
	cursor := "000456"
	if err := d.RegisterRoom("+00123", &cursor); err != nil {
		t.Fatal(err)
	}
	if err := d.RegisterRoom("123", nil); err != nil {
		t.Fatal(err)
	}
	if got := d.inbound.rooms["123"]; got == nil || *got != cursor {
		t.Fatalf("registered cursor=%v, want retained original %q", got, cursor)
	}
	invalidCursor := "invalid"
	if err := d.RegisterRoom("124", &invalidCursor); err != nil {
		t.Fatal(err)
	}
	if got := d.inbound.rooms["124"]; got == nil || *got != invalidCursor {
		t.Fatalf("invalid source cursor was not retained for Ready-time skip: %v", got)
	}
	if err := d.RegisterRoom("0", nil); err == nil {
		t.Fatal("zero room ID was accepted")
	}
}

func TestDiscordFetchSincePreservesOrderFiltersSelfAndRendersMessages(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/api/v10/channels/123/messages" {
			t.Errorf("request=%s %s", r.Method, r.URL.String())
		}
		if r.URL.Query().Get("after") != "456" || r.URL.Query().Get("limit") != "100" {
			t.Errorf("query=%v", r.URL.Query())
		}
		_, _ = io.WriteString(w, `[
			{"id":"700","channel_id":"123","content":"self","author":{"id":"999","username":"self","bot":true}},
			{"id":"701","channel_id":"123","content":" body ","author":{"id":"888","username":"other-bot","bot":true},"embeds":[{"title":" Alert ","description":" details ","fields":[{"name":"severity","value":"high"}]}]},
			{"id":"702","channel_id":"123","content":"hook","author":{"id":"887","username":"webhook","bot":true},"webhook_id":"1234"}
		]`)
	}))
	defer server.Close()
	d := newTestDiscord(t, server)
	d.setDiscordBotID("+000999")
	after := "+000456"
	messages, err := d.FetchSince(context.Background(), "000123", &after)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || len(messages) != 2 {
		t.Fatalf("fetch calls=%d messages=%+v", calls.Load(), messages)
	}
	if messages[0] != (InboundMessage{ID: "701", Content: "body\n\nAlert\ndetails\nseverity: high", Sender: "other-bot", AuthorKind: "bot"}) {
		t.Fatalf("adapted bot message=%+v", messages[0])
	}
	if messages[1] != (InboundMessage{ID: "702", Content: "hook", Sender: "webhook", AuthorKind: "webhook"}) {
		t.Fatalf("adapted webhook message=%+v", messages[1])
	}
}

// Ports discord.rs::discord_message_content_uses_embed_when_body_is_empty.
func TestDiscordInboundContentUsesEmbedWhenBodyEmpty(t *testing.T) {
	message := discordInboundAPIMessage{Embeds: []discordInboundEmbed{{
		Author: &struct {
			Name string `json:"name"`
		}{Name: "Alertmanager"},
		Title:       "FIRING KubeDaemonSetMisScheduled",
		Description: "1 Pods of DaemonSet rook-ceph/nodeplugin are running where they are not supposed to run.",
		Fields: []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		}{{Name: "severity", Value: "warning"}},
		Footer: &struct {
			Text string `json:"text"`
		}{Text: "cluster monitoring"},
	}}}
	got := discordInboundContent("", message.Embeds)
	want := "Alertmanager\nFIRING KubeDaemonSetMisScheduled\n1 Pods of DaemonSet rook-ceph/nodeplugin are running where they are not supposed to run.\nseverity: warning\ncluster monitoring"
	if got != want {
		t.Fatalf("content=%q, want %q", got, want)
	}
}

// Ports discord.rs::discord_message_content_preserves_body_and_embed.
func TestDiscordInboundContentPreservesBodyAndEmbed(t *testing.T) {
	embeds := []discordInboundEmbed{{Title: "Alert details", Description: "failure rate is 8.333%"}}
	if got, want := discordInboundContent("[FIRING]", embeds), "[FIRING]\n\nAlert details\nfailure rate is 8.333%"; got != want {
		t.Fatalf("content=%q, want %q", got, want)
	}
}

func TestDiscordBackfillSkipsInvalidCursorsAndContinuesAfterRoomError(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case "/api/v10/channels/101/messages":
			w.WriteHeader(http.StatusInternalServerError)
		case "/api/v10/channels/102/messages":
			if r.URL.Query().Get("after") != "600" {
				t.Errorf("cursor query=%v", r.URL.Query())
			}
			_, _ = io.WriteString(w, `[{"id":"700","channel_id":"102","content":"ready","author":{"id":"800","username":"reader"}}]`)
		default:
			t.Errorf("unexpected backfill request %s", r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	d := newTestDiscord(t, server)
	first, skipped, second := "500", "not-a-snowflake", "000600"
	for room, cursor := range map[string]*string{"101": &first, "103": &skipped, "102": &second, "104": nil} {
		if err := d.RegisterRoom(room, cursor); err != nil {
			t.Fatal(err)
		}
	}
	sink := make(chan InboundEvent, 4)
	if err := d.backfillDiscordRooms(context.Background(), sink); err != nil {
		t.Fatal(err)
	}
	close(sink)
	if calls.Load() != 2 {
		t.Fatalf("backfill calls=%d, want only valid cursors", calls.Load())
	}
	var events []InboundEvent
	for event := range sink {
		events = append(events, event)
	}
	if len(events) != 1 || events[0].ChannelName != "discord" || events[0].RoomID != "102" || events[0].Message.ID != "700" {
		t.Fatalf("backfill events=%+v", events)
	}
}

func TestDiscordBackfillCancellationUnblocksSink(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `[{"id":"700","channel_id":"102","content":"ready","author":{"id":"800","username":"reader"}}]`)
	}))
	defer server.Close()
	d := newTestDiscord(t, server)
	cursor := "600"
	if err := d.RegisterRoom("102", &cursor); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		done <- d.backfillDiscordRooms(ctx, make(chan InboundEvent))
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("backfill cancellation error=%v", err)
	}
}

// Includes discord.rs::gateway_authored_filter_only_matches_self.
func TestDiscordDispatchFiltersSelfForwardsKnownRoomsAndRepliesOnlyToBotMention(t *testing.T) {
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/v10/channels/200/messages" {
			t.Errorf("notice request=%s %s", r.Method, r.URL.String())
		}
		posts.Add(1)
		var body struct {
			Content *string `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Content == nil || *body.Content != discordUnknownRoomNotice {
			t.Errorf("unknown-room reply=%+v err=%v", body, err)
		}
		_, _ = io.WriteString(w, `{"id":"901"}`)
	}))
	defer server.Close()
	d := newTestDiscord(t, server)
	d.setDiscordBotID("900")
	if err := d.RegisterRoom("100", nil); err != nil {
		t.Fatal(err)
	}
	sink := make(chan InboundEvent, 3)
	known := func(authorID, content string) json.RawMessage {
		return json.RawMessage(fmt.Sprintf(`{"id":"%s","channel_id":"100","content":%q,"author":{"id":%q,"username":"sender"}}`, authorID, content, authorID))
	}
	if err := d.dispatchDiscordMessage(context.Background(), sink, known("900", "self")); err != nil {
		t.Fatal(err)
	}
	if err := d.dispatchDiscordMessage(context.Background(), sink, known("901", "hello")); err != nil {
		t.Fatal(err)
	}
	if err := d.dispatchDiscordMessage(context.Background(), sink, json.RawMessage(`{"id":"902","channel_id":"200","content":"arbitrary text","author":{"id":"901","username":"human"},"mentions":[{"id":"900","username":"bot"}]}`)); err != nil {
		t.Fatal(err)
	}
	if err := d.dispatchDiscordMessage(context.Background(), sink, json.RawMessage(`{"id":"903","channel_id":"201","content":"no mention","author":{"id":"901","username":"human"}}`)); err != nil {
		t.Fatal(err)
	}
	if len(sink) != 1 || posts.Load() != 1 {
		t.Fatalf("queued=%d unknown-room replies=%d", len(sink), posts.Load())
	}
	event := <-sink
	if event.ChannelName != "discord" || event.RoomID != "100" || event.Message.ID != "901" || event.Message.Content != "hello" || event.Message.AuthorKind != "human" {
		t.Fatalf("known room event=%+v", event)
	}
}

func TestDiscordGatewayAuthoredFilterMatchesOnlyKnownSelfID(t *testing.T) {
	message := discordInboundAPIMessage{
		ID:        "700",
		ChannelID: "123",
		Author:    discordInboundAuthor{ID: "42", Username: "author"},
	}
	for _, tc := range []struct {
		name     string
		authorID string
		botID    string
		wantOwn  bool
	}{
		{name: "self", authorID: "42", botID: "42", wantOwn: true},
		{name: "other author", authorID: "7", botID: "42", wantOwn: false},
		{name: "unknown bot ID", authorID: "42", botID: "", wantOwn: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			message.Author.ID = tc.authorID
			got, own, err := adaptDiscordInboundMessage(message, tc.botID)
			if err != nil {
				t.Fatal(err)
			}
			if own != tc.wantOwn {
				t.Fatalf("own=%v, want %v (adapted=%+v)", own, tc.wantOwn, got)
			}
		})
	}
}

func TestDiscordDispatchKnownRoomEnqueueHonorsCancellation(t *testing.T) {
	d := &Discord{}
	if err := d.RegisterRoom("100", nil); err != nil {
		t.Fatal(err)
	}
	raw := json.RawMessage(`{"id":"901","channel_id":"100","content":"hello","author":{"id":"801","username":"sender"}}`)
	ctx, cancel := context.WithCancel(context.Background())
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		done <- d.dispatchDiscordMessage(ctx, make(chan InboundEvent), raw)
	}()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("dispatch cancellation error=%v", err)
	}
}

func TestDiscordFetchSinceWithoutCursorRequestsLatestHundred(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if query.Get("limit") != "100" || query.Has("after") {
			t.Errorf("latest fetch query=%v", query)
		}
		_, _ = io.WriteString(w, `[]`)
	}))
	defer server.Close()
	d := newTestDiscord(t, server)
	messages, err := d.FetchSince(context.Background(), "123", nil)
	if err != nil || len(messages) != 0 {
		t.Fatalf("latest fetch=%+v err=%v", messages, err)
	}
}
