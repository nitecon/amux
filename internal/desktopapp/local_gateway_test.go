package desktopapp

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/internal/gatewayprocess"
	"github.com/nitecon/amux/storage"
)

func TestLocalGatewayServesProjectsAndPersistsTasks(t *testing.T) {
	cli := os.Getenv("AMUX_GO_TEST_BINARY")
	if cli == "" {
		t.Skip("set AMUX_GO_TEST_BINARY to the built amux executable")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	db := filepath.Join(t.TempDir(), "application.sqlite")
	store, err := storage.Open(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	err = store.InsertProject(ctx, storage.Project{Ident: "desktop-demo", ChannelName: "internal", Kind: "adhoc"})
	store.Close()
	if err != nil {
		t.Fatal(err)
	}

	start := func() (*gatewayclient.Client, func()) {
		t.Helper()
		const key = "synthetic-desktop-local-key"
		t.Setenv("AMUX_GO_TEST_GATEWAY_KEY", key)
		endpoint, stop, err := gatewayprocess.StartHTTP(ctx, cli, db, "127.0.0.1:0", "AMUX_GO_TEST_GATEWAY_KEY", os.Stderr)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(stop)
		client, err := gatewayclient.New(endpoint, key, nil)
		if err != nil {
			t.Fatal(err)
		}
		return client, stop
	}
	client, stop := start()
	projects, err := client.Projects(ctx)
	if err != nil || len(projects) != 1 || projects[0].Ident != "desktop-demo" {
		t.Fatalf("projects=%+v err=%v", projects, err)
	}
	task, err := client.CreateTask(ctx, "desktop-demo", gatewayclient.TaskCreateInput{Title: "Review local startup"})
	if err != nil {
		t.Fatal(err)
	}
	stop()

	client, _ = start()
	detail, err := client.TaskDetail(ctx, "desktop-demo", task.ID)
	if err != nil || detail == nil || detail.Task.Title != task.Title {
		t.Fatalf("task detail after restart=%+v err=%v", detail, err)
	}
}
