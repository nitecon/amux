// Package gatewayclient implements the authenticated native worker protocol.
package gatewayclient

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/gatewayorigin"
	"github.com/nitecon/amux/identity/agent"
	"github.com/nitecon/amux/storage"
)

var ErrUnavailable = errors.New("gateway is unavailable")

type HTTPError struct {
	Status int
	// Code is populated only for known task confirmation responses. Raw
	// response messages are intentionally excluded from transport errors.
	Code string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("gateway request failed (HTTP %d)", e.Status) }
func Retryable(err error) bool {
	if errors.Is(err, ErrUnavailable) {
		return true
	}
	var response *HTTPError
	return errors.As(err, &response) && (response.Status == 429 || response.Status >= 500)
}

type Client struct {
	base      string
	key       string
	http      *http.Client
	clusterID string
	nodeID    string
}

func New(endpoint, key string, httpClient *http.Client) (*Client, error) {
	return newClient(endpoint, key, httpClient, false)
}

// NewEnrolled creates a client authenticated by the supplied TLS client
// certificate and trusting only roots in roots. Caller identity is resolved
// separately by the server; this client does not add bearer credentials.
func NewEnrolled(endpoint string, certificate tls.Certificate, roots *x509.CertPool) (*Client, error) {
	if roots == nil || len(certificate.Certificate) == 0 || certificate.PrivateKey == nil {
		return nil, fmt.Errorf("enrolled client certificate and trust roots are required")
	}
	transport, err := enrolledHTTPTransport(certificate, roots)
	if err != nil {
		return nil, err
	}
	return newClient(endpoint, "", &http.Client{Timeout: 15 * time.Second, Transport: transport}, true)
}

func newClient(endpoint, key string, httpClient *http.Client, requireHTTPS bool) (*Client, error) {
	u, err := gatewayorigin.Parse(endpoint)
	if err != nil || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("gateway requires an HTTP(S) origin without credentials, path, query or fragment")
	}
	if requireHTTPS && u.Scheme != "https" {
		return nil, fmt.Errorf("enrolled gateway connections require HTTPS")
	}
	if !requireHTTPS && (key == "" || strings.ContainsAny(key, "\r\n")) {
		return nil, fmt.Errorf("gateway credential is required")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme == "http" && u.Hostname() != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return nil, fmt.Errorf("remote gateway connections require HTTPS")
	}
	client := http.Client{Timeout: 15 * time.Second}
	if httpClient != nil {
		client = *httpClient
		if client.Timeout <= 0 {
			client.Timeout = 15 * time.Second
		}
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{base: strings.TrimRight(u.String(), "/"), key: key, http: &client}, nil
}
func (c *Client) Endpoint() string { return c.base }

// ClusterID returns the authenticated cluster identity pinned by an enrolled
// profile, or an empty string when this client has no enrolled profile.
func (c *Client) ClusterID() string {
	if c == nil {
		return ""
	}
	return c.clusterID
}

func (c *Client) headers(origin *agent.Origin) http.Header {
	h := make(http.Header)
	if c.key != "" {
		h.Set("Authorization", "Bearer "+c.key)
	}
	if origin != nil {
		h.Set("X-Agent-Session-Id", origin.SessionID)
		h.Set("X-Agent-Instance-Id", origin.InstanceID)
		h.Set("X-Agent-Provider", origin.Provider)
		h.Set("X-Agent-OS", origin.OS)
	}
	return h
}
func (c *Client) do(ctx context.Context, method, path string, body any, origin *agent.Origin, result any) error {
	return c.doTaskGoal(ctx, method, path, body, origin, nil, result)
}

func (c *Client) doTaskGoal(ctx context.Context, method, path string, body any, origin *agent.Origin, goal *string, result any) error {
	var selectedGoal string
	if goal != nil {
		normalized, err := storage.NormalizeTaskGoal(goal)
		if err != nil {
			return err
		}
		if normalized != nil {
			selectedGoal = *normalized
		}
	}
	var input io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		input = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, input)
	if err != nil {
		return fmt.Errorf("invalid gateway request")
	}
	req.Header = c.headers(origin)
	if goal != nil {
		req.Header.Set("X-Task-Goal", selectedGoal)
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if credentialErr := manifestError(err); credentialErr != nil {
			return credentialErr
		}
		return ErrUnavailable // Never include credential-bearing URLs or payloads in errors.
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		failure := &HTTPError{Status: response.StatusCode}
		if response.StatusCode == http.StatusConflict {
			raw, err := io.ReadAll(io.LimitReader(response.Body, 4097))
			var envelope struct {
				Code string `json:"code"`
			}
			if err == nil && len(raw) <= 4096 && json.Unmarshal(raw, &envelope) == nil {
				switch envelope.Code {
				case "task_confirmation_required", "changeset_confirmation_required":
					failure.Code = envelope.Code
				}
			}
		}
		return failure
	}
	if result == nil {
		return nil
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, (5<<20)+1))
	if err != nil {
		return ErrUnavailable
	}
	if len(raw) > 5<<20 {
		return fmt.Errorf("gateway response exceeds wire limit")
	}
	if err = json.Unmarshal(raw, result); err != nil {
		return fmt.Errorf("invalid gateway response")
	}
	return nil
}
func conversationPath(project, id string) string {
	return "/v1/projects/" + url.PathEscape(project) + "/conversations/" + url.PathEscape(id)
}
func (c *Client) CreateConversation(ctx context.Context, project, task string, origin agent.Origin) (*storage.Conversation, error) {
	return c.CreateConversationWithGoal(ctx, project, task, origin, nil)
}

// CreateConversationWithGoal preserves queue selection for the initial native
// claim made when opening a task conversation.
func (c *Client) CreateConversationWithGoal(ctx context.Context, project, task string, origin agent.Origin, goal *string) (*storage.Conversation, error) {
	path := "/v1/projects/" + url.PathEscape(project)
	if task != "" {
		path += "/tasks/" + url.PathEscape(task)
	}
	var result storage.Conversation
	err := c.doTaskGoal(ctx, "POST", path+"/conversations", nil, &origin, goal, &result)
	return &result, err
}
func (c *Client) Conversation(ctx context.Context, project, id string) (*storage.Conversation, error) {
	var result storage.Conversation
	err := c.do(ctx, "GET", conversationPath(project, id), nil, nil, &result)
	return &result, err
}
func (c *Client) PendingTaskConversations(ctx context.Context, project string, origin agent.Origin) ([]string, error) {
	var ids []string
	err := c.do(ctx, "GET", "/v1/projects/"+url.PathEscape(project)+"/worker-task-conversations/pending", nil, &origin, &ids)
	return ids, err
}
func (c *Client) Resume(ctx context.Context, r storage.WorkerRun, origin agent.Origin) error {
	return c.do(ctx, "POST", conversationPath(r.Project, r.ConversationID)+"/resume", map[string]string{"previous_instance_id": r.Owner.InstanceID}, &origin, nil)
}
func (c *Client) Publish(ctx context.Context, r storage.WorkerRun, e storage.WorkerOutboxEntry) (int64, error) {
	path := conversationPath(r.Project, r.ConversationID)
	switch e.Kind {
	case "bind":
		return 0, c.do(ctx, "PUT", path+"/session", e.Payload, &r.Owner, nil)
	case "event":
		var result storage.ConversationEvent
		err := c.do(ctx, "POST", path+"/events", map[string]any{"id": e.ID, "event": e.Payload}, &r.Owner, &result)
		return result.Sequence, err
	case "result":
		var result struct {
			CommandID string `json:"command_id"`
			State     string `json:"state"`
		}
		if err := json.Unmarshal(e.Payload, &result); err != nil {
			return 0, err
		}
		return 0, c.do(ctx, "POST", path+"/commands/"+url.PathEscape(result.CommandID)+"/result", map[string]string{"state": result.State}, &r.Owner, nil)
	default:
		return 0, fmt.Errorf("unknown worker publication kind")
	}
}
func (c *Client) Claim(ctx context.Context, r storage.WorkerRun, id string) (*storage.ConversationCommand, error) {
	var result storage.ConversationCommand
	err := c.do(ctx, "POST", conversationPath(r.Project, r.ConversationID)+"/commands/"+url.PathEscape(id)+"/claim", nil, &r.Owner, &result)
	return &result, err
}
func (c *Client) Enqueue(ctx context.Context, project, id, commandID string, input conversation.Command) error {
	return c.do(ctx, "POST", conversationPath(project, id)+"/commands", map[string]any{"id": commandID, "command": input}, nil, nil)
}

// Cursor independently tracks durable transcript sequence and command revisions.
type Cursor struct{ Events, Commands int64 }
type Frame struct {
	Type           string                       `json:"type"`
	Version        int                          `json:"version"`
	ConversationID string                       `json:"conversation_id"`
	Owner          *agent.Origin                `json:"owner"`
	Event          *storage.ConversationEvent   `json:"event"`
	Command        *storage.ConversationCommand `json:"command"`
}

// Stream delivers one conversation's validated frames in wire order. A handler
// commits its projection/cursor before returning; a failed callback does not
// acknowledge a frame. Reconnect with the last successfully committed cursors.
func (c *Client) Stream(ctx context.Context, project, id string, cursor Cursor, consume func(Frame) error) error {
	if cursor.Events < 0 || cursor.Commands < 0 || consume == nil {
		return fmt.Errorf("invalid conversation subscription")
	}
	u, _ := url.Parse(c.base + conversationPath(project, id) + "/stream")
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	u.RawQuery = url.Values{"after": {fmt.Sprint(cursor.Events)}, "after_commands": {fmt.Sprint(cursor.Commands)}}.Encode()
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
	ready := false
	for {
		var frame Frame
		if err = wsjson.Read(ctx, conn, &frame); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return ErrUnavailable
		}
		if frame.Version != 1 {
			return fmt.Errorf("unsupported gateway conversation protocol")
		}
		next := cursor
		if !ready {
			if frame.Type != "ready" || frame.ConversationID != id || frame.Owner == nil {
				return fmt.Errorf("gateway did not confirm conversation")
			}
			ready = true
		} else {
			switch frame.Type {
			case "owner":
				if frame.Owner == nil {
					return fmt.Errorf("gateway owner notification is invalid")
				}
			case "event":
				if frame.Event == nil || frame.Event.ConversationID != id || frame.Event.Sequence != cursor.Events+1 {
					return fmt.Errorf("invalid gateway event cursor")
				}
				next.Events = frame.Event.Sequence
			case "command":
				if frame.Command == nil || frame.Command.ConversationID != id || frame.Command.Revision <= cursor.Commands {
					return fmt.Errorf("invalid gateway command cursor")
				}
				next.Commands = frame.Command.Revision
			default:
				return fmt.Errorf("unknown gateway conversation frame")
			}
		}
		if err = consume(frame); err != nil {
			return err
		}
		cursor = next
	}
}

// Watch binds the shared stream to one exact worker origin. Transcript output
// is already journaled locally; only command snapshots need a callback here.
func (c *Client) Watch(ctx context.Context, r storage.WorkerRun, observe func(storage.ConversationCommand) error) error {
	if observe == nil {
		return fmt.Errorf("worker command observer is required")
	}
	return c.Stream(ctx, r.Project, r.ConversationID, Cursor{Events: r.EventCursor, Commands: r.CommandCursor}, func(frame Frame) error {
		switch frame.Type {
		case "ready", "owner":
			if frame.Owner == nil || *frame.Owner != r.Owner {
				return &HTTPError{Status: 409}
			}
		case "command":
			return observe(*frame.Command)
		}
		return nil
	})
}

func (c *Client) Task(ctx context.Context, project, id string) (*storage.Task, error) {
	var result storage.Task
	err := c.do(ctx, "GET", "/v1/projects/"+url.PathEscape(project)+"/tasks/"+url.PathEscape(id), nil, nil, &result)
	return &result, err
}
