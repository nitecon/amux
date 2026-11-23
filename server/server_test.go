package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nitecon/amux/storage"
)

func TestProjectAPIBearerAuthAndDurability(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "server.db")
	store, err := storage.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.InsertProject(ctx, storage.Project{Ident: "demo", ChannelName: "none", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	handler, err := NewHandler(store, "private-test-key")
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		method, path, body, key string
		status                  int
	}{
		{"GET", "/v1/users", "", "", 401},
		{"GET", "/v1/users", "", "private-test-key", 200},
		{"GET", "/v1/users/me", "", "private-test-key", 404},
		{"GET", "/v1/execution/runs", "", "", 401},
		{"GET", "/v1/execution/runs", "", "private-test-key", 200},
		{"POST", "/v1/projects/demo/archive", "", "", 401},
		{"POST", "/v1/projects/demo/archive", "", "wrong", 401},
		{"POST", "/v1/projects/demo/archive", "", "private-test-key", 200},
		{"POST", "/v1/projects/missing/archive", "", "private-test-key", 404},
		{"PATCH", "/v1/projects/demo/repo", `{"namespace":"owner","repo_name":"name"}`, "private-test-key", 200},
		{"PATCH", "/v1/projects/demo/repo", `{"namespace":"owner"}`, "private-test-key", 400},
		{"PATCH", "/v1/projects/demo/repo", `null`, "private-test-key", 400},
	} {
		req := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("x-gateway-user-role", "admin") // Never grants authentication.
		if test.key != "" {
			req.Header.Set("Authorization", "Bearer "+test.key)
		}
		res := httptest.NewRecorder()
		handler.ServeHTTP(res, req)
		if res.Code != test.status {
			t.Fatalf("%s %s => %d %s", test.method, test.path, res.Code, res.Body.String())
		}
		if strings.Contains(res.Body.String(), "private-test-key") {
			t.Fatal("credential leaked")
		}
	}
	p, err := store.Project(ctx, "demo")
	if err != nil || p.ArchivedAt == nil || p.RepoFullName == nil || *p.RepoFullName != "owner/name" {
		t.Fatalf("API did not persist: %+v, %v", p, err)
	}
	req := httptest.NewRequest("POST", "/v1/projects/demo/restore", nil)
	req.Header.Set("Authorization", "Bearer private-test-key")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != 200 {
		t.Fatal(res.Code)
	}
	p, err = store.Project(ctx, "demo")
	if err != nil || p.ArchivedAt != nil {
		t.Fatalf("restore: %+v, %v", p, err)
	}
}

func TestServeStopsWithoutDesktop(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, listener, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ready") }), time.Second)
	}()
	client := &http.Client{Timeout: time.Second}
	res, err := client.Get("http://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(res.Body)
	res.Body.Close()
	if err != nil || string(body) != "ready" {
		t.Fatalf("response %q, %v", body, err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("server did not stop")
	}
	if conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second); err == nil {
		conn.Close()
		t.Fatal("listener remained open")
	}
}

func TestHandlerRejectsMissingDependencies(t *testing.T) {
	if _, err := NewHandler(nil, "key"); err == nil {
		t.Fatal("nil store accepted")
	}
	s, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := NewHandler(s, ""); err == nil {
		t.Fatal("empty credential accepted")
	}
}

func TestShutdownDeadlineClosesActiveRequest(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	exited := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- Serve(ctx, listener, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { close(entered); <-r.Context().Done(); close(exited) }), 20*time.Millisecond)
	}()
	client := &http.Client{Timeout: 2 * time.Second}
	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		res, err := client.Get("http://" + listener.Addr().String())
		if err == nil {
			res.Body.Close()
		}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("request did not enter")
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("shutdown timeout was hidden")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shutdown hung")
	}
	select {
	case <-exited:
	case <-time.After(time.Second):
		t.Fatal("handler context not canceled")
	}
	<-requestDone
}

// Ported from src/server.rs::profile_lock: one authority per canonical DB path.
func TestProfileLockIsExclusiveAndReleases(t *testing.T) {
	path := filepath.Join(t.TempDir(), "database.db")
	first, err := LockProfile(path)
	if err != nil {
		t.Fatal(err)
	}
	if second, err := LockProfile(path); err == nil {
		second.Close()
		first.Close()
		t.Fatal("two servers acquired the same profile")
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := LockProfile(path)
	if err != nil {
		t.Fatal("closed lock was not released:", err)
	}
	next.Close()
}
