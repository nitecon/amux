package gatewayclient

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/nitecon/amux/storage"
)

func TestSyncCheckoutsUsesRustCompatibleReplacementAndOverviewContract(t *testing.T) {
	origin := "https://github.com/example/project.git"
	var got struct {
		Hostname  string `json:"hostname"`
		Checkouts []struct {
			Project string  `json:"project"`
			Origin  *string `json:"origin"`
		} `json:"checkouts"`
	}
	var nilOriginEncoded bool
	requests := 0
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.URL.Path != "/v1/checkouts" || r.Header.Get("Authorization") != "Bearer test-key" {
			t.Fatalf("unexpected checkout request: %s %s auth=%q", r.Method, r.URL.Path, r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPut:
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatal(err)
			}
			var raw struct {
				Checkouts []map[string]json.RawMessage `json:"checkouts"`
			}
			if err := json.Unmarshal(body, &raw); err != nil {
				t.Fatal(err)
			}
			nilOriginEncoded = len(raw.Checkouts) == 2 && bytes.Equal(raw.Checkouts[1]["origin"], []byte("null"))
			_ = json.NewEncoder(w).Encode(map[string]bool{"ok": true})
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode([]storage.CheckoutOverview{{
				Ident:           "project",
				CanonicalRemote: &origin,
				Machines:        []storage.MachineCheckout{{Hostname: "other-host", Origin: &origin}},
			}})
		default:
			t.Fatalf("unexpected method %s", r.Method)
		}
	}))
	defer host.Close()
	client, err := New(host.URL, "test-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	items, err := client.SyncCheckouts(context.Background(), "this-host", []storage.CheckoutReportItem{{Project: "project", Origin: &origin}, {Project: "adhoc"}})
	if err != nil || requests != 2 {
		t.Fatalf("sync requests=%d err=%v", requests, err)
	}
	if got.Hostname != "this-host" || len(got.Checkouts) != 2 || got.Checkouts[0].Project != "project" || got.Checkouts[0].Origin == nil || *got.Checkouts[0].Origin != origin || !nilOriginEncoded {
		t.Fatalf("replacement report=%+v", got)
	}
	if len(items) != 1 || items[0].Ident != "project" || len(items[0].Machines) != 1 || items[0].Machines[0].Hostname != "other-host" || items[0].Machines[0].Origin == nil || *items[0].Machines[0].Origin != origin {
		t.Fatalf("checkout overview=%+v", items)
	}
}

func TestSyncCheckoutsRejectsEmptyHostnameBeforeRequest(t *testing.T) {
	client, err := New("http://localhost", "test-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.SyncCheckouts(context.Background(), "  ", nil); err == nil {
		t.Fatal("empty hostname accepted")
	}
}

func TestSyncCheckoutsNilInputEncodesEmptyArray(t *testing.T) {
	requests := 0
	host := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPut {
			var body struct {
				Checkouts []json.RawMessage `json:"checkouts"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Checkouts == nil || len(body.Checkouts) != 0 {
				t.Fatalf("empty checkout report encoded as %#v", body.Checkouts)
			}
			_, _ = io.WriteString(w, `{"ok":true}`)
			return
		}
		_, _ = io.WriteString(w, `[]`)
	}))
	defer host.Close()
	client, err := New(host.URL, "test-key", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.SyncCheckouts(context.Background(), "this-host", nil); err != nil {
		t.Fatal(err)
	}
	if requests != 2 {
		t.Fatalf("requests=%d, want PUT and GET", requests)
	}
}
