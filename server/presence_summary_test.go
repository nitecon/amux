package server

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/nitecon/amux/storage"
)

func TestPresenceSummaryRoute(t *testing.T) {
	ctx := context.Background()
	s, err := storage.Open(ctx, filepath.Join(t.TempDir(), "presence-summary.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	handler, err := NewHandler(s, "key")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("GET", "/v1/presence/summary", nil)
	req.Header.Set("Authorization", "Bearer key")
	res := httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	if res.Code != 200 {
		t.Fatalf("summary status=%d body=%s", res.Code, res.Body.String())
	}
	var got map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"machines": float64(0), "os": []any{}, "agents": float64(0), "active": float64(0), "idle": float64(0)}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("summary=%v want=%v", got, want)
	}
	req = httptest.NewRequest("GET", "/v1/presence/summary?hostname=local-machine", nil)
	req.Header.Set("Authorization", "Bearer key")
	res = httptest.NewRecorder()
	handler.ServeHTTP(res, req)
	var summary storage.PresenceSummary
	if res.Code != 200 || json.Unmarshal(res.Body.Bytes(), &summary) != nil || summary.ThisMachine == nil || *summary.ThisMachine != (storage.MachineAgentCounts{}) {
		t.Fatalf("summary with hostname status=%d body=%s", res.Code, res.Body)
	}
}
