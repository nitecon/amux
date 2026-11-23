package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/nitecon/amux/storage"
)

func TestProjectShortcutLinksHTTPCRUDScopeAndValidation(t *testing.T) {
	store, handler := taskAPI(t)
	if err := store.InsertProject(context.Background(), storage.Project{Ident: "other", ChannelName: "other", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(handler)
	defer srv.Close()
	requestWithContentType := func(method, path, body, contentType string) (int, []byte, string) {
		t.Helper()
		req, err := http.NewRequest(method, srv.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer task-key")
		if contentType != "" {
			req.Header.Set("Content-Type", contentType)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		data, err := io.ReadAll(res.Body)
		if err != nil {
			t.Fatal(err)
		}
		return res.StatusCode, data, res.Header.Get("Content-Type")
	}
	request := func(method, path, body string) (int, []byte) {
		contentType := ""
		if body != "" {
			contentType = "application/json"
		}
		status, data, _ := requestWithContentType(method, path, body, contentType)
		return status, data
	}
	base := "/v1/projects/tasks/links"
	status, body := request(http.MethodGet, base, "")
	if status != http.StatusOK || string(body) != "[]\n" {
		t.Fatalf("empty list status=%d body=%s", status, body)
	}
	status, body = request(http.MethodPost, base, `{"label":" Build ","url":" https://ci.example.test ","extra":true}`)
	if status != http.StatusOK {
		t.Fatalf("add status=%d body=%s", status, body)
	}
	var link storage.ProjectLink
	if err := json.Unmarshal(body, &link); err != nil {
		t.Fatal(err)
	}
	if link.ProjectIdent != "tasks" || link.Label != "Build" || link.URL != "https://ci.example.test" || link.Rank != 1 {
		t.Fatalf("created link=%+v", link)
	}
	for _, tc := range []struct {
		name        string
		body        string
		contentType string
		status      int
	}{
		{"missing content type", `{"label":"x","url":"https://x"}`, "", http.StatusUnsupportedMediaType},
		{"wrong content type", `{"label":"x","url":"https://x"}`, "text/plain", http.StatusUnsupportedMediaType},
		{"malformed JSON", `{`, "application/json", http.StatusBadRequest},
		{"missing label", `{"url":"https://x"}`, "application/json", http.StatusUnprocessableEntity},
		{"missing URL", `{"label":"x"}`, "application/json", http.StatusUnprocessableEntity},
		{"wrong field type", `{"label":5,"url":"https://x"}`, "application/json", http.StatusUnprocessableEntity},
		{"null field", `{"label":null,"url":"https://x"}`, "application/json", http.StatusUnprocessableEntity},
		{"null object", `null`, "application/json", http.StatusUnprocessableEntity},
		{"trailing JSON value", `{"label":"x","url":"https://x"}{}`, "application/json", http.StatusBadRequest},
		{"body over Axum default limit", strings.Repeat(" ", (2<<20)+1), "application/json", http.StatusRequestEntityTooLarge},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body, responseType := requestWithContentType(http.MethodPost, base, tc.body, tc.contentType)
			if status != tc.status {
				t.Fatalf("status=%d want=%d body=%s", status, tc.status, body)
			}
			if !strings.HasPrefix(responseType, "text/plain") {
				t.Fatalf("rejection content type=%q body=%q", responseType, body)
			}
		})
	}
	status, body, _ = requestWithContentType(http.MethodPost, base, `{"label":"x","url":"https://x"}`, "application/vnd.amux+json")
	if status != http.StatusOK {
		t.Fatalf("JSON suffix content type status=%d body=%s", status, body)
	}
	status, body = request(http.MethodPost, base, `{"label":"bad","url":"ftp://invalid"}`)
	if status != http.StatusBadRequest || !strings.Contains(string(body), "url must start with http:// or https://") {
		t.Fatalf("bad URL status=%d body=%s", status, body)
	}
	status, body = request(http.MethodPost, "/v1/projects/missing/links", `{"label":"x","url":"https://x"}`)
	if status != http.StatusBadRequest || !strings.Contains(string(body), "project 'missing' not found") {
		t.Fatalf("missing project status=%d body=%s", status, body)
	}
	status, body = request(http.MethodPost, "/v1/projects/missing/links", `{"label":"","url":"ftp://invalid"}`)
	if status != http.StatusBadRequest || !strings.Contains(string(body), "project 'missing' not found") {
		t.Fatalf("missing project did not precede validation: status=%d body=%s", status, body)
	}
	status, body = request(http.MethodGet, base, "")
	if status != http.StatusOK || !strings.Contains(string(body), `"id":`+jsonNumber(link.ID)) {
		t.Fatalf("list status=%d body=%s", status, body)
	}
	status, body = request(http.MethodDelete, "/v1/projects/other/links/"+jsonNumber(link.ID), "")
	if status != http.StatusOK || !strings.Contains(string(body), `"changed":false`) {
		t.Fatalf("cross-project delete status=%d body=%s", status, body)
	}
	status, body = request(http.MethodDelete, base+"/"+jsonNumber(link.ID), "")
	if status != http.StatusOK || !strings.Contains(string(body), `"changed":true`) {
		t.Fatalf("delete status=%d body=%s", status, body)
	}
	status, body = request(http.MethodDelete, base+"/not-an-id", "")
	if status != http.StatusBadRequest {
		t.Fatalf("invalid id status=%d body=%s", status, body)
	}
}

func jsonNumber(value int64) string { return strconv.FormatInt(value, 10) }
