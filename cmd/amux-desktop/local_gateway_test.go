package main

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/nitecon/amux/gatewayclient"
	"github.com/nitecon/amux/storage"
)

func TestLocalGatewayServesProjectsAndPersistsTasks(t *testing.T) {
	ctx := context.Background()
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
		endpoint, stop, err := startLocalGateway(ctx, db, "127.0.0.1:0", key)
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
