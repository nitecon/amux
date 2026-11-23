package worker

import (
	"context"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/google/uuid"
	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/server"
	"github.com/nitecon/amux/storage"
)

func TestPoolPresenceAdvertisesValidOSHostname(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	remote, err := storage.Open(ctx, filepath.Join(t.TempDir(), "remote.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer remote.Close()
	if err := remote.InsertProject(ctx, storage.Project{Ident: "hostname-test", ChannelName: "hostname-test", Kind: "adhoc"}); err != nil {
		t.Fatal(err)
	}
	handler, err := server.NewHandler(remote, "hostname-secret")
	if err != nil {
		t.Fatal(err)
	}
	host := httptest.NewServer(handler)
	defer host.Close()
	defer handler.(interface{ ShutdownStreams(context.Context) error }).ShutdownStreams(context.Background())
	client, err := gatewayclient.New(host.URL, "hostname-secret", nil)
	if err != nil {
		t.Fatal(err)
	}

	local, err := storage.Open(ctx, filepath.Join(t.TempDir(), "local.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer local.Close()
	osName := runtime.GOOS
	if osName == "darwin" {
		osName = "macos"
	}
	pool, err := local.CreateWorkerPool(ctx, storage.WorkerPool{
		ID: uuid.NewString(), Server: host.URL, Project: "hostname-test", Directory: t.TempDir(),
		Provider: "codex", OS: osName, SlotCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	slots, err := local.WorkerPoolSlots(ctx, pool.ID)
	if err != nil || len(slots) != 1 {
		t.Fatalf("slots=%+v err=%v", slots, err)
	}
	if err := poolPresence(ctx, PoolConfig{Store: local, Client: client, PoolID: pool.ID}, *pool, slots[0], false); err != nil {
		t.Fatal(err)
	}
	presence, err := remote.Presence(ctx, pool.Project)
	if err != nil || len(presence) != 1 {
		t.Fatalf("reported presence=%+v err=%v", presence, err)
	}
	hostname, hostErr := os.Hostname()
	hostname = strings.TrimSpace(hostname)
	valid := hostErr == nil && hostname != "" && len(hostname) <= 128 && strings.IndexFunc(hostname, unicode.IsControl) < 0
	if valid && (presence[0].Hostname == nil || *presence[0].Hostname != hostname) {
		t.Fatalf("advertised hostname=%v, want %q", presence[0].Hostname, hostname)
	}
	if !valid && presence[0].Hostname != nil {
		t.Fatalf("invalid OS hostname was advertised: %q", *presence[0].Hostname)
	}
}
