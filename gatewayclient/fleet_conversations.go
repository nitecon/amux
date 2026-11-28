package gatewayclient

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/nitecon/amux/storage"
)

func (c *Client) CloseFleetConversation(ctx context.Context, project, id string) error {
	path := "/v1/projects/" + url.PathEscape(project) + "/fleet-conversations/" + url.PathEscape(id)
	return c.do(ctx, http.MethodDelete, path, nil, nil, nil)
}

func (c *Client) OpenFleetConversation(ctx context.Context, project, session string) (*storage.FleetConversation, error) {
	var info storage.FleetConversation
	path := "/v1/projects/" + url.PathEscape(project) + "/fleet-agents/" + url.PathEscape(session) + "/conversation"
	if err := c.do(ctx, http.MethodPost, path, nil, nil, &info); err != nil {
		return nil, err
	}
	if info.ID == "" || info.ProjectIdent != project || info.Owner.SessionID != session || info.TaskID != "" {
		return nil, fmt.Errorf("gateway returned a different fleet conversation")
	}
	return &info, nil
}

func (c *Client) WatchFleetConversations(ctx context.Context, consume func([]storage.FleetConversation) error) error {
	u, _ := url.Parse(c.base + "/v1/fleet/conversations/stream")
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
	conn.SetReadLimit(5 << 20)
	streamCtx, stop := context.WithCancel(ctx)
	defer stop()
	go func() {
		timer := time.NewTicker(15 * time.Second)
		defer timer.Stop()
		for {
			select {
			case <-streamCtx.Done():
				return
			case <-timer.C:
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
	for {
		var frame struct {
			Version int                         `json:"version"`
			Chats   []storage.FleetConversation `json:"chats"`
		}
		if err := wsjson.Read(streamCtx, conn, &frame); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return ErrUnavailable
		}
		if frame.Version != 1 {
			return fmt.Errorf("unsupported fleet conversation protocol")
		}
		if err := consume(frame.Chats); err != nil {
			return err
		}
	}
}
