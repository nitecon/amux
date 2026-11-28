package main

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/google/uuid"
	"github.com/nitecon/amux/config"
	"github.com/nitecon/amux/controllers"
	"github.com/nitecon/amux/fism"
	"github.com/nitecon/amux/storage"
	"github.com/urfave/cli/v3"
)

func TestServerConfigIsLoadedFromYAML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("database: unused.db\nmessage-retention-days: 10\n"), 0600); err != nil {
		t.Fatal(err)
	}
	command := newCommand()
	// Replacing the startup action isolates config binding from the listener.
	command.Command("server").Action = func(_ context.Context, server *cli.Command) error {
		if err := config.LoadServer(server.String("config")); err != nil {
			return err
		}
		if settings := config.GetServer(); settings.Database != "unused.db" || settings.RetentionDays != 10 {
			t.Fatalf("database = %q, retention = %d", settings.Database, settings.RetentionDays)
		}
		if err := os.WriteFile(path, []byte("database: replacement.db\nmessage-retention-days: 20\n"), 0600); err != nil {
			return err
		}
		if err := config.LoadServer(path); err != nil {
			return err
		}
		if settings := config.GetServer(); settings.Database != "replacement.db" || settings.RetentionDays != 20 {
			t.Fatalf("reloaded settings = %+v", settings)
		}
		if err := os.WriteFile(path, []byte("database: replacement.db\nprofile: conflicting\n"), 0600); err != nil {
			return err
		}
		if err := config.LoadServer(path); err == nil {
			t.Fatal("accepted conflicting server settings")
		}
		if settings := config.GetServer(); settings.Database != "replacement.db" || settings.Profile != "" {
			t.Fatalf("failed reload replaced snapshot: %+v", settings)
		}
		settings := config.GetServer()
		settings.Database = "saved.db"
		settings.RetentionDays = 0
		if err := config.SaveServer(path, settings); err != nil {
			return err
		}
		if live := config.GetServer(); live.Database != "replacement.db" {
			t.Fatalf("file save changed the active server settings: %+v", live)
		}
		if err := config.LoadServer(path); err != nil {
			return err
		}
		if live := config.GetServer(); live.Database != "saved.db" || live.RetentionDays != 0 {
			t.Fatalf("server reload did not apply saved settings: %+v", live)
		}
		return nil
	}
	if err := command.Run(context.Background(), []string{"amux", "server", "--config", path}); err != nil {
		t.Fatal(err)
	}
}

func TestServerProcessHelper(t *testing.T) {
	if os.Getenv("AMUX_GO_SERVER_TEST") != "1" {
		return
	}
	for i, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"amux"}, os.Args[i+1:]...)
			main()
			return
		}
	}
	os.Exit(2)
}

func TestServerRunsWithoutDisplayAndStopsOnSignal(t *testing.T) {
	if os.PathSeparator == '\\' {
		t.Skip("Windows shutdown uses service control rather than POSIX interrupt")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	dir := t.TempDir()
	path := filepath.Join(dir, "server.yml")
	settings := config.DefaultServer()
	settings.Database = filepath.Join(dir, "server.db")
	settings.Listen = "127.0.0.1:0"
	if err := config.SaveServer(path, settings); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestServerProcessHelper$", "--", "server", "--config", path)
	cmd.Env = append(os.Environ(), "AMUX_GO_SERVER_TEST=1", "GATEWAY_API_KEY=process-test-key", "DISPLAY=", "WAYLAND_DISPLAY=", "DBUS_SESSION_BUS_ADDRESS=", "HOME="+t.TempDir())
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()
	ready := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stdout)
		if scanner.Scan() {
			ready <- scanner.Text()
		} else {
			ready <- ""
		}
		io.Copy(io.Discard, stdout)
	}()
	var line string
	select {
	case line = <-ready:
	case <-ctx.Done():
		t.Fatal("server did not report readiness")
	}
	address, ok := strings.CutPrefix(line, "listening ")
	if !ok {
		t.Fatalf("server startup failed: %q", line)
	}
	client := &http.Client{Timeout: time.Second}
	req, err := http.NewRequest("POST", "http://"+address+"/v1/projects/missing/archive", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer process-test-key")
	res, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != 404 {
		t.Fatalf("server response %d", res.StatusCode)
	}
	stream, _, err := websocket.Dial(ctx, "ws://"+address+"/v1/events/stream", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer process-test-key"}}})
	if err != nil {
		t.Fatalf("production router stream: %v", err)
	}
	defer stream.CloseNow()
	consumerID := uuid.NewString()
	if err := wsjson.Write(ctx, stream, map[string]any{"type": "subscribe", "consumer_id": consumerID, "events": []string{"task.created", controllers.GlobalNotifyEventID}}); err != nil {
		t.Fatal(err)
	}
	var subscribed struct {
		Type                   string `json:"type"`
		ServerRestartReminders bool   `json:"server_restart_reminders"`
	}
	if err := wsjson.Read(ctx, stream, &subscribed); err != nil || subscribed.Type != "subscribed" || !subscribed.ServerRestartReminders {
		t.Fatalf("full builtin attachment: %+v %v", subscribed, err)
	}
	if err := wsjson.Write(ctx, stream, map[string]any{"type": "emit", "id": "client.heartbeat", "version": 1, "payload": map[string]any{"agents": []any{}}}); err != nil {
		t.Fatal(err)
	}
	var emitted struct {
		Type string `json:"type"`
		UID  string `json:"uid"`
	}
	if err := wsjson.Read(ctx, stream, &emitted); err != nil || emitted.Type != "emitted" || emitted.UID == "" {
		t.Fatalf("production remote emit: %+v %v", emitted, err)
	}
	if err := wsjson.Write(ctx, stream, map[string]any{"type": "emit", "id": "clock.tick", "version": 1, "payload": json.RawMessage(`{}`)}); err != nil {
		t.Fatal(err)
	}
	var forbidden struct {
		Type string `json:"type"`
	}
	if err := wsjson.Read(ctx, stream, &forbidden); err != nil || forbidden.Type != "error" {
		t.Fatalf("remote clock bypass: %+v %v", forbidden, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+address+"/v1/notifications/global", strings.NewReader(`{"message":"Process smoke check"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer process-test-key")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("publish global notification: HTTP %d", response.StatusCode)
	}
	var wire struct {
		Type  string     `json:"type"`
		Event fism.Event `json:"event"`
	}
	if err := wsjson.Read(ctx, stream, &wire); err != nil || wire.Type != "event" || wire.Event.ID != controllers.GlobalNotifyEventID {
		t.Fatalf("global notification: %+v %v", wire, err)
	}
	stream.CloseNow()
	replacement, _, err := websocket.Dial(ctx, "ws://"+address+"/v1/events/stream", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer process-test-key"}}})
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.CloseNow()
	if err := wsjson.Write(ctx, replacement, map[string]any{"type": "subscribe", "consumer_id": consumerID, "events": []string{controllers.GlobalNotifyEventID}}); err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Read(ctx, replacement, &subscribed); err != nil || subscribed.Type != "subscribed" {
		t.Fatalf("replacement subscribe: %+v %v", subscribed, err)
	}
	original := wire.Event
	wire.Event = fism.Event{}
	if err := wsjson.Read(ctx, replacement, &wire); err != nil || wire.Type != "event" || wire.Event.UID != original.UID || wire.Event.Seq != original.Seq {
		t.Fatalf("unacknowledged notice did not replay: %+v %v", wire, err)
	}
	if err := wsjson.Write(ctx, replacement, map[string]any{"type": "ack", "seq": original.Seq}); err != nil {
		t.Fatal(err)
	}
	if err := wsjson.Read(ctx, replacement, &wire); err != nil || wire.Type != "acked" {
		t.Fatalf("notification acknowledgment: %+v %v", wire, err)
	}
	reader, err := sql.Open("sqlite", settings.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		var completedAt int64
		err := reader.QueryRowContext(ctx, "SELECT completed_at FROM retention_runs WHERE id=1").Scan(&completedAt)
		if err == nil && completedAt > 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("startup retention did not commit its outcome: %v", err)
		case <-tick.C:
		}
	}
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("server shutdown: %v: %s", err, stderr.String())
	}
	journal, err := storage.Open(ctx, settings.Database)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	events, err := journal.RouterEvents(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.ID == controllers.RetentionStartupID {
			t.Fatal("startup retention was persisted as transition work")
		}
	}
	cursor, err := journal.OpenRemoteConsumer(ctx, consumerID, uuid.NewString(), nil)
	if err != nil || cursor < original.Seq {
		t.Fatalf("reopened notification cursor = %d, want at least %d (%v)", cursor, original.Seq, err)
	}
}
