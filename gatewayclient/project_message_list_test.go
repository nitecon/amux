package gatewayclient

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
)

func TestListProjectMessageThreadsPreservesOptionalQueryAndValidatesResult(t *testing.T) {
	var requests atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet || r.URL.Path != "/v1/projects/demo/messages" {
			t.Errorf("request=%s %s", r.Method, r.URL.Path)
		}
		query := r.URL.Query()
		if query.Get("state") != "all" || query.Get("kind") != "human,agent" || query.Get("agent_id") != "" || !hasQueryValue(query, "agent_id") || query.Get("limit") != "0" || query.Get("offset") != "9" {
			t.Errorf("query=%v", query)
		}
		if r.Header.Get("X-Agent-Session-Id") != "" {
			t.Errorf("message list unexpectedly used agent identity: %v", r.Header)
		}
		_, _ = fmt.Fprint(w, projectMessageListFixture(12))
	}))
	defer host.Close()
	client, err := New(host.URL, "message-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	agent := ""
	limit := uint64(0)
	result, err := client.ListProjectMessageThreads(context.Background(), "DEMO", ProjectMessageListQuery{
		State: "all", Kinds: []string{"human", "agent"}, AgentID: &agent, Limit: &limit, Offset: 9,
	})
	if err != nil || len(result.Threads) != 1 || result.Threads[0].ID != 41 || result.Counts.HumanOpen != 2 || requests.Load() != 1 {
		t.Fatalf("result=%+v err=%v requests=%d", result, err, requests.Load())
	}
}

func TestListProjectMessageThreadsAllowsOmittedDefaultAndActivityTies(t *testing.T) {
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			t.Errorf("default query should be omitted, got %q", r.URL.RawQuery)
		}
		_, _ = fmt.Fprint(w, `{"threads":[`+projectMessageThreadFixture(41, 12)+`,`+projectMessageThreadFixture(42, 12)+`],"counts":{"human_open":0,"agent_open":0,"alerts_open":0,"system_open":0,"resolved":0}}`)
	}))
	defer host.Close()
	client, err := New(host.URL, "message-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.ListProjectMessageThreads(context.Background(), "demo", ProjectMessageListQuery{})
	if err != nil || len(result.Threads) != 2 {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestListProjectMessageThreadsAllowsSourceCompatibleEmptyAuthorKind(t *testing.T) {
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		thread := projectMessageThreadFixtureWithOverrides(41, 12, `"author_kind":"human"`, `"author_kind":""`)
		_, _ = fmt.Fprint(w, `{"threads":[`+thread+`],"counts":{"human_open":0,"agent_open":0,"alerts_open":0,"system_open":0,"resolved":0}}`)
	}))
	defer host.Close()
	client, err := New(host.URL, "message-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.ListProjectMessageThreads(context.Background(), "demo", ProjectMessageListQuery{})
	if err != nil || len(result.Threads) != 1 || result.Threads[0].AuthorKind != "" {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestListProjectMessageThreadsRejectsMalformedResponses(t *testing.T) {
	bodies := []string{
		`{"threads":null,"counts":{"human_open":0,"agent_open":0,"alerts_open":0,"system_open":0,"resolved":0}}`,
		`{"threads":[],"counts":null}`,
		`{"threads":[],"counts":{"human_open":0,"agent_open":0,"alerts_open":0,"system_open":0}}`,
		`{"threads":[],"counts":{"human_open":"0","agent_open":0,"alerts_open":0,"system_open":0,"resolved":0}}`,
		`{"threads":[],"counts":{"human_open":null,"agent_open":0,"alerts_open":0,"system_open":0,"resolved":0}}`,
		`{"threads":[{"id":0}],"counts":{"human_open":0,"agent_open":0,"alerts_open":0,"system_open":0,"resolved":0}}`,
		`{"threads":[` + projectMessageThreadFixtureWithOverrides(41, 12, `"preview":"question"`, `"preview":null`) + `],"counts":{"human_open":0,"agent_open":0,"alerts_open":0,"system_open":0,"resolved":0}}`,
		`{"threads":[` + projectMessageThreadFixtureWithOverrides(41, 12, `"agent_id":null`, `"agent_id":12`) + `],"counts":{"human_open":0,"agent_open":0,"alerts_open":0,"system_open":0,"resolved":0}}`,
		`{"threads":[` + projectMessageThreadFixtureWithState(41, 12, "unknown") + `],"counts":{"human_open":0,"agent_open":0,"alerts_open":0,"system_open":0,"resolved":0}}`,
		`{"threads":[` + projectMessageThreadFixture(41, 11) + `,` + projectMessageThreadFixture(42, 12) + `],"counts":{"human_open":0,"agent_open":0,"alerts_open":0,"system_open":0,"resolved":0}}`,
	}
	for _, body := range bodies {
		t.Run(body, func(t *testing.T) {
			host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, body) }))
			defer host.Close()
			client, err := New(host.URL, "message-key", nil)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.ListProjectMessageThreads(context.Background(), "demo", ProjectMessageListQuery{}); err == nil {
				t.Fatalf("accepted malformed response: %s", body)
			}
		})
	}
}

func TestListProjectMessageThreadsRejectsEmptyProjectWithoutHTTP(t *testing.T) {
	var requests atomic.Int32
	host := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { requests.Add(1) }))
	defer host.Close()
	client, err := New(host.URL, "message-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.ListProjectMessageThreads(context.Background(), " ", ProjectMessageListQuery{}); err == nil || requests.Load() != 0 {
		t.Fatalf("empty project err=%v requests=%d", err, requests.Load())
	}
}

func hasQueryValue(query url.Values, key string) bool {
	_, ok := query[key]
	return ok
}

func projectMessageListFixture(lastActivity int64) string {
	return `{"threads":[` + projectMessageThreadFixture(41, lastActivity) + `],"counts":{"human_open":2,"agent_open":1,"alerts_open":0,"system_open":0,"resolved":1}}`
}

func projectMessageThreadFixture(id int64, activity int64) string {
	return projectMessageThreadFixtureWithState(id, activity, "open")
}

func projectMessageThreadFixtureWithState(id int64, activity int64, state string) string {
	return fmt.Sprintf(`{"id":%d,"project_ident":"demo","source":"user","author_kind":"human","agent_id":null,"hostname":null,"subject":null,"preview":"question","sent_at":10,"last_activity_at":%d,"reply_count":0,"ack_count":0,"state":%q,"resolved_at":null,"resolved_by":null}`, id, activity, state)
}

func projectMessageThreadFixtureWithOverrides(id int64, activity int64, original, replacement string) string {
	thread := projectMessageThreadFixture(id, activity)
	return strings.Replace(thread, original, replacement, 1)
}
