package gatewayclient

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/nitecon/amux/storage"
)

// WatchTaskEvents consumes one durable task subscription. The callback must
// persist work before returning its receipt status. The gateway resumes from
// its last recorded receipt when the same consumer reconnects.
func (c *Client) WatchTaskEvents(ctx context.Context, consumer string, initialCursor *int64, handle func(storage.TaskEvent) (string, error), onSubscribe func(int64) error) error {
	if consumer == "" || handle == nil {
		return fmt.Errorf("task subscription requires a consumer and event handler")
	}
	u, err := url.Parse(c.base + "/v1/tasks/stream")
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
		if credentialErr := manifestError(err); credentialErr != nil {
			return credentialErr
		}
		if response != nil {
			return &HTTPError{Status: response.StatusCode}
		}
		return err
	}
	defer conn.CloseNow()
	conn.SetReadLimit(1 << 20)
	write := func(frame any) error {
		deadline, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		return wsjson.Write(deadline, conn, frame)
	}
	subscription := map[string]any{"type": "subscribe", "protocol_version": 1, "consumer_id": consumer}
	if initialCursor != nil {
		subscription["after_event_id"] = *initialCursor
	}
	if err := write(subscription); err != nil {
		return err
	}
	var subscribed bool
	for {
		var frame struct {
			Type       string             `json:"type"`
			Version    int                `json:"protocol_version"`
			Cursor     *int64             `json:"cursor"`
			ConsumerID string             `json:"consumer_id"`
			Event      *storage.TaskEvent `json:"event"`
			Message    string             `json:"message"`
		}
		readCtx, stop := context.WithTimeout(ctx, 65*time.Second)
		err := wsjson.Read(readCtx, conn, &frame)
		stop()
		if err != nil {
			return err
		}
		switch frame.Type {
		case "subscribed":
			if subscribed || frame.Version != 1 || frame.ConsumerID != consumer || frame.Cursor == nil || *frame.Cursor < 0 {
				return fmt.Errorf("invalid task subscription acknowledgment")
			}
			subscribed = true
			if onSubscribe != nil {
				if err := onSubscribe(*frame.Cursor); err != nil {
					return err
				}
			}
		case "heartbeat":
			if err := write(map[string]string{"type": "heartbeat"}); err != nil {
				return err
			}
		case "event":
			if !subscribed || frame.Event == nil || frame.Event.ID <= 0 {
				return fmt.Errorf("invalid task stream event")
			}
			status, err := handle(*frame.Event)
			if err != nil {
				return err
			}
			if err := write(map[string]any{"type": "ack", "event_id": frame.Event.ID, "status": status}); err != nil {
				return err
			}
		case "recorded", "heartbeat_ack":
		case "error":
			return fmt.Errorf("task stream rejected request: %s", frame.Message)
		default:
			return fmt.Errorf("unsupported task stream frame %q", frame.Type)
		}
	}
}
