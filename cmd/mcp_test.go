package main

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/nitecon/amux/config"
)

func TestMCPCommandRejectsClientConfigurationFlags(t *testing.T) {
	for _, name := range []string{"config", "server", "enrolled-profile", "api-key-env", "work-preferences"} {
		t.Run(name, func(t *testing.T) {
			cmd := newMCPCommand(nil)
			cmd.Writer, cmd.ErrWriter = io.Discard, io.Discard
			if err := cmd.Run(context.Background(), []string{"amux", "--" + name, "example"}); err == nil {
				t.Fatalf("accepted removed flag --%s", name)
			}
		})
	}
}

func TestMCPCommandStopsCleanlyWhenInputIsIdle(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	if err := config.SaveClient(filepath.Join(home, ".amux", "config.yaml"), config.Client{
		GatewayEndpoint: "http://127.0.0.1:7913",
		APIKeyEnv:       "GATEWAY_API_KEY",
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	input, writer := io.Pipe()
	defer input.Close()
	defer writer.Close()
	done := make(chan error, 1)
	go func() {
		cmd := newMCPCommand(&sdk.IOTransport{Reader: input, Writer: nopWriteCloser{io.Discard}})
		done <- cmd.Run(ctx, []string{"amux"})
	}()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("MCP command did not stop")
	}
}

type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }
