package channels

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var _ Outbound = (*Discord)(nil)

const (
	testDiscordToken = "synthetic-secret-token"
	testGuildID      = "123456789012345678"
	testRoomID       = "223456789012345678"
	testParentID     = "323456789012345678"
	testMessageID    = "423456789012345678"
)

func newTestDiscord(t *testing.T, server *httptest.Server) *Discord {
	t.Helper()
	adapter, err := NewDiscord(DiscordConfig{
		Token: testDiscordToken, GuildID: testGuildID, CategoryID: testParentID,
		APIBaseURL: server.URL + "/api/v10",
	})
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func TestNewDiscordValidatesConfigWithoutRequests(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	adapter := newTestDiscord(t, server)
	if adapter.client.Timeout != discordRequestTimeout {
		t.Fatalf("default timeout=%v, want %v", adapter.client.Timeout, discordRequestTimeout)
	}
	if requests.Load() != 0 {
		t.Fatalf("constructor made %d provider requests", requests.Load())
	}

	for _, config := range []DiscordConfig{
		{GuildID: "0", Token: testDiscordToken},
		{GuildID: "18446744073709551616", Token: testDiscordToken},
		{GuildID: testGuildID, CategoryID: "-1", Token: testDiscordToken},
		{GuildID: testGuildID, Token: testDiscordToken, APIBaseURL: "http://example.com/api"},
	} {
		if _, err := NewDiscord(config); err == nil {
			t.Fatalf("NewDiscord(%+v) unexpectedly succeeded", config)
		}
	}
}

func TestDiscordEnsureRoomReusesExactProviderRoom(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertDiscordRequest(t, r, http.MethodGet, "/api/v10/guilds/"+testGuildID+"/channels")
		if _, err := io.WriteString(w, `[{"id":"`+testRoomID+`","name":"other"},{"id":"`+testParentID+`","name":"demo"}]`); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	adapter := newTestDiscord(t, server)

	room, err := adapter.EnsureRoom(context.Background(), "demo")
	if err != nil || room != testParentID {
		t.Fatalf("EnsureRoom()=(%q,%v), want %q", room, err, testParentID)
	}
}

func TestDiscordEnsureRoomCreatesTextChannelInConfiguredCategory(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			assertDiscordRequest(t, r, http.MethodGet, "/api/v10/guilds/"+testGuildID+"/channels")
			_, _ = io.WriteString(w, `[]`)
			return
		}
		assertDiscordRequest(t, r, http.MethodPost, "/api/v10/guilds/"+testGuildID+"/channels")
		var payload struct {
			Name     string  `json:"name"`
			Type     int     `json:"type"`
			ParentID *string `json:"parent_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload.Name != "demo" || payload.Type != 0 || payload.ParentID == nil || *payload.ParentID != testParentID {
			t.Errorf("create payload=%+v", payload)
		}
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"id":"`+testRoomID+`"}`)
	}))
	defer server.Close()
	adapter := newTestDiscord(t, server)

	room, err := adapter.EnsureRoom(context.Background(), "demo")
	if err != nil || room != testRoomID || calls != 2 {
		t.Fatalf("EnsureRoom()=(%q,%v), calls=%d", room, err, calls)
	}
}

func TestDiscordSendStructuredEmbedAndReplyReference(t *testing.T) {
	parentID := testParentID
	longTitle := strings.Repeat("é", discordEmbedTitle+1)
	longBody := strings.Repeat("🙂", discordEmbedBody+1)
	message := OutboundMessage{AgentID: "agent", Hostname: "host", Subject: longTitle, Body: longBody, EventAt: -1}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertDiscordRequest(t, r, http.MethodPost, "/api/v10/channels/"+testRoomID+"/messages")
		var payload struct {
			Embeds    []discordEmbed           `json:"embeds"`
			Reference *discordMessageReference `json:"message_reference"`
			Content   *string                  `json:"content"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload.Content != nil || len(payload.Embeds) != 1 || payload.Reference == nil || payload.Reference.MessageID != testParentID || payload.Reference.ChannelID != testRoomID {
			t.Fatalf("structured reply payload=%+v", payload)
		}
		embed := payload.Embeds[0]
		if len([]rune(embed.Title)) != discordEmbedTitle || !strings.HasSuffix(embed.Title, "…") || len([]rune(embed.Description)) != discordEmbedBody || !strings.HasSuffix(embed.Description, "…") {
			t.Errorf("embed truncation title=%d body=%d", len([]rune(embed.Title)), len([]rune(embed.Description)))
		}
		if embed.Color != discordEmbedColor || embed.Author.Name != "agent · host" || embed.Timestamp == nil || *embed.Timestamp != "1970-01-01T00:00:00Z" {
			t.Errorf("embed metadata=%+v", embed)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"id":"`+testMessageID+`"}`)
	}))
	defer server.Close()
	adapter := newTestDiscord(t, server)

	id, err := adapter.Send(context.Background(), testRoomID, &parentID, message)
	if err != nil || id != testMessageID {
		t.Fatalf("Send()=(%q,%v)", id, err)
	}
}

func TestDiscordSendTextUsesPlainContentAndOptionalReference(t *testing.T) {
	parentID := testParentID
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertDiscordRequest(t, r, http.MethodPost, "/api/v10/channels/"+testRoomID+"/messages")
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		var content string
		if err := json.Unmarshal(payload["content"], &content); err != nil || content != "human text" {
			t.Errorf("content=%q err=%v", content, err)
		}
		if _, ok := payload["embeds"]; ok {
			t.Error("text message included embeds")
		}
		var ref discordMessageReference
		if err := json.Unmarshal(payload["message_reference"], &ref); err != nil || ref.MessageID != testParentID || ref.ChannelID != testRoomID {
			t.Errorf("reference=%+v err=%v", ref, err)
		}
		_, _ = io.WriteString(w, `{"id":"`+testMessageID+`"}`)
	}))
	defer server.Close()
	adapter := newTestDiscord(t, server)

	id, err := adapter.SendText(context.Background(), testRoomID, &parentID, "human text")
	if err != nil || id != testMessageID {
		t.Fatalf("SendText()=(%q,%v)", id, err)
	}
}

func TestDiscordRejectsInvalidSnowflakesBeforeRequest(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, `{"id":"`+testMessageID+`"}`)
	}))
	defer server.Close()
	adapter := newTestDiscord(t, server)
	for _, room := range []string{"", "0", "-1", "18446744073709551616"} {
		if _, err := adapter.SendText(context.Background(), room, nil, "body"); err == nil {
			t.Errorf("SendText room %q succeeded", room)
		}
	}
	badParent := "0"
	if _, err := adapter.SendText(context.Background(), testRoomID, &badParent, "body"); err == nil {
		t.Error("SendText with zero parent succeeded")
	}
	if requests.Load() != 0 {
		t.Fatalf("invalid snowflakes made %d requests", requests.Load())
	}
}

func TestDiscordSnowflakeAliasesNormalizeLikeRust(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			assertDiscordRequest(t, r, http.MethodGet, "/api/v10/guilds/123/channels")
			_, _ = io.WriteString(w, `[{"id":"000789","name":"demo"}]`)
			return
		}
		assertDiscordRequest(t, r, http.MethodPost, "/api/v10/channels/789/messages")
		var payload struct {
			Reference discordMessageReference `json:"message_reference"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Error(err)
		}
		if payload.Reference.ChannelID != "789" || payload.Reference.MessageID != "1122" {
			t.Errorf("normalized reference=%+v", payload.Reference)
		}
		_, _ = io.WriteString(w, `{"id":"000423456789012345678"}`)
	}))
	defer server.Close()
	adapter, err := NewDiscord(DiscordConfig{
		Token: testDiscordToken, GuildID: "+00123", CategoryID: "000456", APIBaseURL: server.URL + "/api/v10",
	})
	if err != nil {
		t.Fatal(err)
	}
	room, err := adapter.EnsureRoom(context.Background(), "demo")
	if err != nil || room != "789" {
		t.Fatalf("EnsureRoom()=(%q,%v)", room, err)
	}
	parent := "+001122"
	id, err := adapter.SendText(context.Background(), "000789", &parent, "body")
	if err != nil || id != testMessageID {
		t.Fatalf("SendText()=(%q,%v), want normalized response %q", id, err, testMessageID)
	}
}

func TestDiscordErrorsAreBoundedAndWritesAreNotRetried(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		assertDiscordRequest(t, r, http.MethodPost, "/api/v10/channels/"+testRoomID+"/messages")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, "provider body must not be copied")
	}))
	defer server.Close()
	adapter := newTestDiscord(t, server)

	_, err := adapter.SendText(context.Background(), testRoomID, nil, "body")
	if err == nil || !strings.Contains(err.Error(), "HTTP 429") || strings.Contains(err.Error(), testDiscordToken) || strings.Contains(err.Error(), "provider body") {
		t.Fatalf("unexpected bounded API error: %v", err)
	}
	if requests.Load() != 1 {
		t.Fatalf("Discord POST attempts=%d, want 1", requests.Load())
	}
}

func TestDiscordRejectsRedirectWithoutForwardingToken(t *testing.T) {
	var destinationCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { destinationCalls.Add(1) }))
	defer destination.Close()
	var sourceCalls atomic.Int32
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sourceCalls.Add(1)
		if r.Header.Get("Authorization") != "Bot "+testDiscordToken {
			t.Errorf("source authorization=%q", r.Header.Get("Authorization"))
		}
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	defer source.Close()
	adapter := newTestDiscord(t, source)
	_, err := adapter.SendText(context.Background(), testRoomID, nil, "body")
	if err == nil || sourceCalls.Load() != 1 || destinationCalls.Load() != 0 {
		t.Fatalf("redirect result err=%v source=%d destination=%d", err, sourceCalls.Load(), destinationCalls.Load())
	}
}

func TestDiscordCancellationErrorsRemainInspectable(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		started <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-release:
		}
	}))
	defer func() {
		close(release)
		server.Close()
	}()
	adapter := newTestDiscord(t, server)

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := adapter.SendText(ctx, testRoomID, nil, "cancel")
		result <- err
	}()
	<-started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled request error=%v", err)
	}

	deadlineCtx, deadlineCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer deadlineCancel()
	if _, err := adapter.SendText(deadlineCtx, testRoomID, nil, "deadline"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("deadline request error=%v", err)
	}
}

func TestDiscordRejectsNullChannelListWithoutCreatingRoom(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = io.WriteString(w, "null")
	}))
	defer server.Close()
	adapter := newTestDiscord(t, server)
	if _, err := adapter.EnsureRoom(context.Background(), "demo"); err == nil {
		t.Fatal("EnsureRoom accepted null channel list")
	}
	if calls.Load() != 1 {
		t.Fatalf("null listing caused %d requests", calls.Load())
	}
}

func TestDiscordResponseLimitAndTransportCauses(t *testing.T) {
	t.Run("response limit", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, strings.Repeat("x", discordMaxResponse+1))
		}))
		defer server.Close()
		adapter := newTestDiscord(t, server)
		if _, err := adapter.EnsureRoom(context.Background(), "demo"); err == nil || !strings.Contains(err.Error(), "size limit") {
			t.Fatalf("oversized response error=%v", err)
		}
	})
	t.Run("response read error", func(t *testing.T) {
		want := errors.New("synthetic read failure")
		adapter, err := NewDiscord(DiscordConfig{
			Token: testDiscordToken, GuildID: testGuildID,
			APIBaseURL: "http://127.0.0.1:1234/api/v10",
			HTTPClient: &http.Client{Transport: roundTripperFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: errorReader{err: want}}, nil
			})},
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = adapter.SendText(context.Background(), testRoomID, nil, "body")
		if !errors.Is(err, want) {
			t.Fatalf("read error=%v", err)
		}
	})
}

func assertDiscordRequest(t *testing.T, r *http.Request, method, path string) {
	t.Helper()
	if r.Method != method || r.URL.Path != path {
		t.Errorf("request=%s %s, want %s %s", r.Method, r.URL.Path, method, path)
	}
	if got := r.Header.Get("Authorization"); got != "Bot "+testDiscordToken {
		t.Errorf("authorization=%q", got)
	}
	if got := r.Header.Get("User-Agent"); got != discordUserAgent {
		t.Errorf("User-Agent=%q", got)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

type errorReader struct{ err error }

func (r errorReader) Read([]byte) (int, error) { return 0, r.err }
func (r errorReader) Close() error             { return nil }
