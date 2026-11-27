package main

import (
	"bufio"
	"bytes"
	"context"
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
	"github.com/urfave/cli/v3"
)

func TestServerConfigIsLoadedWithCommandLineOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("database: unused.db\nmessage-retention-days: 10\n"), 0600); err != nil {
		t.Fatal(err)
	}
	command := newCommand()
	// Replacing the startup action isolates config binding from the listener.
	command.Command("server").Action = func(_ context.Context, server *cli.Command) error {
		if err := config.InitServer(server.String("config"), server); err != nil {
			return err
		}
		if settings := config.Get().Server; settings.Database != "unused.db" || settings.RetentionDays != 15 {
			t.Fatalf("database = %q, retention = %d", settings.Database, settings.RetentionDays)
		}
		if err := os.WriteFile(path, []byte("database: replacement.db\nmessage-retention-days: 20\n"), 0600); err != nil {
			return err
		}
		if err := config.InitServer(path, server); err != nil {
			return err
		}
		if settings := config.Get().Server; settings.Database != "replacement.db" || settings.RetentionDays != 15 {
			t.Fatalf("reloaded settings = %+v", settings)
		}
		if err := os.WriteFile(path, []byte("database: replacement.db\nprofile: conflicting\n"), 0600); err != nil {
			return err
		}
		if err := config.InitServer(path, server); err == nil {
			t.Fatal("accepted conflicting server settings")
		}
		if settings := config.Get().Server; settings.Database != "replacement.db" || settings.Profile != "" {
			t.Fatalf("failed reload replaced snapshot: %+v", settings)
		}
		settings := config.Get().Server
		settings.Database = "saved.db"
		if err := config.SaveServer(path, settings); err != nil {
			return err
		}
		saved, err := config.ReadServer(path, server)
		if err != nil {
			return err
		}
		if saved.Database != "saved.db" || config.Get().Server.Database != "saved.db" {
			t.Fatalf("server save did not update file and snapshot: %+v", saved)
		}
		return nil
	}
	if err := command.Run(context.Background(), []string{"amux", "server", "--config", path, "--message-retention-days", "15"}); err != nil {
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
	cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestServerProcessHelper$", "--", "server", "--database", filepath.Join(t.TempDir(), "server.db"), "--listen", "127.0.0.1:0")
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
	if err := wsjson.Write(ctx, stream, map[string]any{"type": "subscribe", "consumer_id": uuid.NewString(), "events": []string{"task.created"}}); err != nil {
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
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("server shutdown: %v: %s", err, stderr.String())
	}
}
