package gatewayclient

import (
	"context"
	"fmt"
	"net/url"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// WatchNavigation holds one authenticated invalidation subscription. Ready
// always invalidates so a caller can reload after reconnect without a cursor.
// The caller owns retry policy; callback errors are returned unchanged.
func (c *Client) WatchNavigation(ctx context.Context, invalidate func() error) error {
	if invalidate == nil {
		return fmt.Errorf("navigation callback required")
	}
	u, _ := url.Parse(c.base + "/v1/navigation/stream")
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
	conn.SetReadLimit(4096)
	streamCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-streamCtx.Done():
				return
			case <-ticker.C:
				pingCtx, cancel := context.WithTimeout(streamCtx, 5*time.Second)
				err := conn.Ping(pingCtx)
				cancel()
				if err != nil {
					stop()
					return
				}
			}
		}
	}()
	ready := false
	for {
		var frame struct {
			Version int    `json:"version"`
			Type    string `json:"type"`
		}
		if err := wsjson.Read(streamCtx, conn, &frame); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return ErrUnavailable
		}
		if frame.Version != 1 || (!ready && frame.Type != "ready") || (ready && frame.Type != "changed") {
			return fmt.Errorf("invalid gateway navigation protocol")
		}
		ready = true
		if err := invalidate(); err != nil {
			return err
		}
	}
}
