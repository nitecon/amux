package gatewayclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/nitecon/amux/controllers"
)

// WatchGlobalNotifications resumes one durable subscription. Acknowledgments
// are sent only after each callback succeeds. The caller owns reconnect policy.
func (c *Client) WatchGlobalNotifications(ctx context.Context, consumerID string, consume func(uid, message string) error) error {
	if consume == nil {
		return fmt.Errorf("notification callback required")
	}
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
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if credentialErr := manifestError(err); credentialErr != nil {
			return credentialErr
		}
		if response != nil {
			return &HTTPError{Status: response.StatusCode}
		}
		return ErrUnavailable
	}
	defer conn.CloseNow()
	conn.SetReadLimit(32 << 10)
	if err := writeNotificationFrame(ctx, conn, map[string]any{
		"type": "subscribe", "consumer_id": consumerID, "events": []string{controllers.GlobalNotifyEventID},
	}); err != nil {
		return err
	}
	streamCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	defer func() { stop(); <-done }()
	go func() {
		defer close(done)
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-streamCtx.Done():
				return
			case <-ticker.C:
				if err := writeNotificationFrame(streamCtx, conn, map[string]string{"type": "heartbeat"}); err != nil {
					stop()
					return
				}
			}
		}
	}()
	ready := false
	for {
		var frame struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Event   struct {
				ID      string          `json:"id"`
				UID     string          `json:"uid"`
				Seq     int64           `json:"seq"`
				Payload json.RawMessage `json:"payload"`
			} `json:"event"`
		}
		if err := wsjson.Read(streamCtx, conn, &frame); err != nil {
			return err
		}
		switch frame.Type {
		case "subscribed":
			ready = true
		case "heartbeat", "heartbeat_ack", "acked":
		case "event":
			event := frame.Event
			var announcement struct {
				Message string `json:"message"`
			}
			if !ready || event.ID != controllers.GlobalNotifyEventID || event.UID == "" || event.Seq <= 0 ||
				json.Unmarshal(event.Payload, &announcement) != nil || announcement.Message == "" {
				return fmt.Errorf("invalid global notification event")
			}
			if err := consume(event.UID, announcement.Message); err != nil {
				return err
			}
			if err := writeNotificationFrame(streamCtx, conn, map[string]any{"type": "ack", "seq": event.Seq}); err != nil {
				return err
			}
		case "error":
			return fmt.Errorf("notification stream rejected request: %s", frame.Message)
		default:
			return fmt.Errorf("unknown notification frame %q", frame.Type)
		}
	}
}

func writeNotificationFrame(ctx context.Context, conn *websocket.Conn, value any) error {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return wsjson.Write(ctx, conn, value)
}

// NotificationScope identifies the gateway and enrolled principal that own a
// durable consumer ID. Bearer-only gateways are scoped by endpoint.
func (c *Client) NotificationScope() string {
	if c.clusterID != "" && c.nodeID != "" {
		return c.clusterID + ":" + c.nodeID
	}
	return c.Endpoint()
}
