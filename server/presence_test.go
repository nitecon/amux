package server

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/nitecon/amux/storage"
)

func TestPresenceRejectsMalformedSnapshots(t *testing.T) {
	s, err := storage.Open(context.Background(), filepath.Join(t.TempDir(), "presence.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h, err := NewHandler(s, "key")
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`null`, `[]`, `{}`, `{"source":"host","sequence":1,"agents":[],"unknown":true}`, `{"source":"host","sequence":1,"agents":[]} {}`, strings.Repeat(" ", 1<<20) + `{}`} {
		r := httptest.NewRequest("POST", "/v1/worker-presence", strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer key")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("malformed snapshot status=%d body=%s", w.Code, w.Body.String())
		}
	}
}
