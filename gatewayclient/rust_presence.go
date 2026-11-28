package gatewayclient

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/nitecon/amux/identity/agent"
)

type RustPresenceAgent struct {
	agent.Origin
	Name            string  `json:"name"`
	Project         string  `json:"project"`
	Generation      string  `json:"generation"`
	CanonicalRemote *string `json:"canonical_remote"`
	Goal            *string `json:"goal"`
	ReviewClear     bool    `json:"review_context_clear"`
	Busy            bool    `json:"-"`
}

// ReportRustPresence publishes one complete roster through the Rust gateway's
// event router. The publisher does not subscribe to assignment events here.
func (c *Client) ReportRustPresence(ctx context.Context, run, hostname, os string, agents []RustPresenceAgent) error {
	u, err := url.Parse(c.base + "/v1/events/stream")
	if err != nil {
		return err
	}
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	conn, response, err := websocket.Dial(ctx, u.String(), &websocket.DialOptions{HTTPClient: c.http, HTTPHeader: c.headers(nil)})
	if err != nil {
		if response != nil {
			return &HTTPError{Status: response.StatusCode}
		}
		return err
	}
	defer conn.CloseNow()
	emit := func(id string, payload any) error {
		if err := wsjson.Write(ctx, conn, map[string]any{"type": "emit", "id": id, "version": 1, "payload": payload}); err != nil {
			return err
		}
		var response struct {
			Type string `json:"type"`
		}
		if err := wsjson.Read(ctx, conn, &response); err != nil {
			return err
		}
		if response.Type != "emitted" {
			return fmt.Errorf("gateway rejected %s presence event", id)
		}
		return nil
	}
	if err := emit("client.heartbeat", map[string]any{"client_run": run, "hostname": hostname, "os": os, "agents": agents}); err != nil {
		return err
	}
	for _, agent := range agents {
		at := time.Now()
		if !agent.Busy {
			at = at.Add(-31 * time.Second)
		}
		if err := emit("terminal.activity", map[string]any{"session_id": agent.SessionID, "instance_id": agent.InstanceID, "output_at": at.UnixMilli(), "draft": agent.Busy}); err != nil {
			return err
		}
	}
	return nil
}
