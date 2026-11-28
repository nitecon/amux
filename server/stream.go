package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"
	"github.com/nitecon/amux/channels"
	"github.com/nitecon/amux/storage"
	"github.com/nitecon/amux/wsock"
)

const streamFrameBytes = 65536

type api struct {
	http.Handler
	store                     *storage.Store
	secret                    string
	remote                    *remoteStreamConfig
	channels                  map[string]channels.Outbound
	artifactAuthEnforced      bool
	disableArtifactBodySchema bool
	securityEnrollment        *storage.SecurityEnrollment
	localNodeID               string
	discoveryPort             uint16
	processNonce              string
	secureBind                string
	discoveryBind             string
	shutdown                  func()
	shutdownStarted           atomic.Bool
	bootstrapMu               sync.Mutex
	bootstrapAttempts         map[string]bootstrapAttempt
	sockets                   wsock.SocketManager
}

func (a *api) taskConsumerStatuses(w http.ResponseWriter, r *http.Request) {
	if !allowBrowserAdmin(w, r) {
		return
	}
	statuses, err := a.store.TaskConsumerStatuses(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "cannot read task stream consumers")
		return
	}
	for i := range statuses {
		statuses[i].ConsumerID = redacted(statuses[i].ConsumerID, a.secret, 128)
	}
	writeJSON(w, statuses)
}

func newAPI(store *storage.Store, secret string) *api {
	return &api{store: store, secret: secret, bootstrapAttempts: map[string]bootstrapAttempt{}}
}

func (a *api) ShutdownStreams(ctx context.Context) error {
	return a.sockets.Shutdown(ctx)
}

func (a *api) taskStream(w http.ResponseWriter, r *http.Request) {
	a.withStream(w, r, streamFrameBytes, a.serveTaskStream)
}

func (a *api) withStream(w http.ResponseWriter, r *http.Request, readLimit int64, serve func(context.Context, *websocket.Conn)) {
	a.sockets.Serve(w, r, readLimit, serve)
}

func (a *api) taskEvents(w http.ResponseWriter, r *http.Request) {
	query, err := parseEventQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid event query")
		return
	}
	if !allowBrowserAdmin(w, r) {
		return
	}
	events, err := a.store.TaskEvents(r.Context(), query)
	if err != nil {
		writeError(w, 500, "cannot read task events")
		return
	}
	for i := range events {
		if err := boundTaskEvent(&events[i], a.secret); err != nil {
			writeError(w, 500, "event exceeds wire limit; inspect task through REST")
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(events)
}

var errInvalidEventQuery = errors.New("invalid event query")

// parseEventQuery mirrors the Rust EventQuery used by both the event and
// execution-history handlers. Its fields are scalar, so repeated known keys
// and values that cannot decode as the declared type are rejected.
func parseEventQuery(rawQuery string) (storage.EventQuery, error) {
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return storage.EventQuery{}, errInvalidEventQuery
	}
	var query storage.EventQuery
	getScalar := func(key string) (string, bool, error) {
		items, ok := values[key]
		if !ok {
			return "", false, nil
		}
		if len(items) != 1 {
			return "", false, errInvalidEventQuery
		}
		return items[0], true, nil
	}
	if text, ok, err := getScalar("after_event_id"); err != nil {
		return storage.EventQuery{}, err
	} else if ok {
		id, err := strconv.ParseInt(text, 10, 64)
		if err != nil {
			return storage.EventQuery{}, errInvalidEventQuery
		}
		query.After = &id
	}
	if text, ok, err := getScalar("project"); err != nil {
		return storage.EventQuery{}, err
	} else if ok {
		query.Project = &text
	}
	if text, ok, err := getScalar("task_id"); err != nil {
		return storage.EventQuery{}, err
	} else if ok {
		query.TaskID = &text
	}
	return query, nil
}

type streamMessage struct {
	typ  websocket.MessageType
	body []byte
	err  error
}

func sendFrame(ctx context.Context, c *websocket.Conn, value any) error {
	return wsock.WriteJSON(ctx, c, 5*time.Second, value)
}
func strictFrame(data []byte, value any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return fmt.Errorf("invalid stream frame")
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		return fmt.Errorf("invalid stream frame")
	}
	return nil
}

func (a *api) serveTaskStream(parent context.Context, c *websocket.Conn) {
	ctx, cancel := context.WithCancel(parent)
	input := make(chan streamMessage, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			typ, body, err := c.Read(ctx)
			select {
			case input <- streamMessage{typ, body, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	defer func() { cancel(); c.CloseNow(); <-readerDone }()
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return
	}
	connection := hex.EncodeToString(random)
	defer func() {
		cleanup, stop := context.WithTimeout(context.WithoutCancel(parent), 2*time.Second)
		defer stop()
		a.store.DisconnectTaskConsumer(cleanup, connection)
	}()
	consumer := ""
	principal := authenticatedSecurePrincipal(parent)
	cursor := int64(0)
	var inflight *int64
	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	securityCheck := time.NewTicker(time.Second)
	defer securityCheck.Stop()
	idle := time.NewTimer(60 * time.Second)
	defer idle.Stop()
	for {
		// Subscribe to a commit notification before querying. A concurrent append
		// either appears in that query or closes this channel; it cannot be missed.
		changed := a.store.TaskEventChanges()
		if consumer != "" && inflight == nil {
			event, err := a.store.OfferTaskEvent(ctx, connection, consumer, cursor)
			if err != nil {
				return
			}
			if event != nil {
				if err := boundTaskEvent(event, a.secret); err != nil {
					sendFrame(ctx, c, map[string]any{"type": "error", "message": "event exceeds wire limit; inspect task through REST"})
					return
				}
				if err := sendFrame(ctx, c, map[string]any{"type": "event", "event": event}); err != nil {
					return
				}
				id := event.ID
				inflight = &id
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-idle.C:
			return
		case <-securityCheck.C:
			if principal != nil && !principal.valid(ctx) {
				return
			}
		case <-changed:
			continue
		case <-heartbeat.C:
			if consumer != "" {
				if _, err := a.store.TouchTaskConsumer(ctx, connection, consumer); err != nil {
					return
				}
				if err := sendFrame(ctx, c, map[string]any{"type": "heartbeat"}); err != nil {
					return
				}
			}
		case message := <-input:
			if message.err != nil {
				return
			}
			if message.typ != websocket.MessageText {
				continue
			}
			if !idle.Stop() {
				select {
				case <-idle.C:
				default:
				}
			}
			idle.Reset(60 * time.Second)
			if consumer != "" {
				if _, err := a.store.TouchTaskConsumer(ctx, connection, consumer); err != nil {
					return
				}
			}
			response, err := func() (any, error) {
				var discriminator struct {
					Type string `json:"type"`
				}
				if err := json.Unmarshal(message.body, &discriminator); err != nil {
					return nil, fmt.Errorf("invalid stream frame")
				}
				switch discriminator.Type {
				case "subscribe":
					var req struct {
						Type     string  `json:"type"`
						Version  *uint32 `json:"protocol_version"`
						Consumer *string `json:"consumer_id"`
						After    *int64  `json:"after_event_id"`
					}
					if err := strictFrame(message.body, &req); err != nil {
						return nil, err
					}
					if consumer != "" {
						return nil, fmt.Errorf("already subscribed")
					}
					if req.Version == nil || *req.Version != 1 {
						return nil, fmt.Errorf("unsupported protocol version")
					}
					if req.Consumer == nil {
						return nil, fmt.Errorf("consumer_id required")
					}
					if principal != nil {
						if err := principal.enrollment.BindConsumer(ctx, principal.NodeID, *req.Consumer); err != nil {
							return nil, err
						}
					}
					sub, err := a.store.SubscribeTaskEvents(ctx, connection, *req.Consumer, req.After)
					if err != nil {
						return nil, err
					}
					consumer = *req.Consumer
					cursor = sub.Cursor
					return map[string]any{"type": "subscribed", "protocol_version": 1, "consumer_id": consumer, "cursor": cursor, "latest_event_id": sub.LatestEventID}, nil
				case "heartbeat":
					var req struct {
						Type string `json:"type"`
					}
					if err := strictFrame(message.body, &req); err != nil {
						return nil, err
					}
					if consumer == "" {
						return nil, fmt.Errorf("subscribe first")
					}
					return map[string]any{"type": "heartbeat_ack"}, nil
				case "ack":
					var req struct {
						Type      string  `json:"type"`
						ID        *int64  `json:"event_id"`
						Status    *string `json:"status"`
						Workspace *string `json:"workspace_id"`
						Surface   *string `json:"surface_id"`
						Message   *string `json:"message"`
						Summary   *string `json:"summary"`
					}
					if err := strictFrame(message.body, &req); err != nil {
						return nil, err
					}
					if consumer == "" {
						return nil, fmt.Errorf("subscribe first")
					}
					if req.ID == nil || req.Status == nil {
						return nil, fmt.Errorf("event_id and status required")
					}
					if *req.ID > cursor && (inflight == nil || *inflight != *req.ID) {
						return nil, fmt.Errorf("event is not in flight")
					}
					receipt := storage.EventReceipt{EventID: *req.ID, Status: *req.Status, WorkspaceID: req.Workspace, SurfaceID: req.Surface}
					if req.Message != nil {
						receipt.Message = redacted(*req.Message, a.secret, 4096)
					}
					if req.Summary != nil {
						receipt.Summary = redacted(*req.Summary, a.secret, 16384)
					}
					status, err := a.store.AcknowledgeTaskEvent(ctx, connection, consumer, receipt)
					if err != nil {
						return nil, err
					}
					if inflight != nil && *inflight == *req.ID {
						cursor = *req.ID
						inflight = nil
					}
					return map[string]any{"type": "recorded", "event_id": *req.ID, "status": status}, nil
				default:
					return nil, fmt.Errorf("invalid stream frame type")
				}
			}()
			if err != nil {
				response = map[string]any{"type": "error", "message": redacted(err.Error(), a.secret, 4096)}
			}
			if err := sendFrame(ctx, c, response); err != nil {
				return
			}
		}
	}
}

func redacted(text, key string, limit int) string {
	if key != "" {
		text = strings.ReplaceAll(text, key, "[redacted]")
	}
	if len(text) <= limit {
		return text
	}
	end := limit - 16
	if end < 0 {
		end = 0
	}
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end] + "\n[truncated]"
}
func boundValue(value any, key string, limit int) (any, bool) {
	switch v := value.(type) {
	case string:
		clean := redacted(v, key, limit)
		beforeTruncation := v
		if key != "" {
			beforeTruncation = strings.ReplaceAll(v, key, "[redacted]")
		}
		return clean, len(beforeTruncation) > limit
	case []any:
		changed := len(v) > 32
		if changed {
			v = v[:32]
		}
		for i := range v {
			item, trimmed := boundValue(v[i], key, limit)
			v[i] = item
			changed = changed || trimmed
		}
		return v, changed
	case map[string]any:
		changed := false
		for k, item := range v {
			clean, trimmed := boundValue(item, key, limit)
			v[k] = clean
			changed = changed || trimmed
		}
		return v, changed
	default:
		return value, false
	}
}
func boundTaskEvent(event *storage.TaskEvent, key string) error {
	// Work on decoded copies so bounding a frame never mutates stored snapshots.
	fields := []*json.RawMessage{&event.Task, &event.Comment, &event.Delegation}
	values := make([]any, len(fields))
	for i, field := range fields {
		if *field == nil {
			continue
		}
		d := json.NewDecoder(bytes.NewReader(*field))
		d.UseNumber()
		if err := d.Decode(&values[i]); err != nil {
			return err
		}
	}
	receipts := event.Deliveries
	event.Deliveries = []storage.EventDelivery{}
	for _, limit := range []int{16384, 4096, 512} {
		event.ProjectIdent = redacted(event.ProjectIdent, key, 512)
		if event.CanonicalRemote != nil {
			clean := redacted(*event.CanonicalRemote, key, 1024)
			event.CanonicalRemote = &clean
		}
		for i, value := range values {
			if value == nil {
				continue
			}
			fieldLimit := limit
			if i == 2 {
				fieldLimit = 512
			}
			clean, trimmed := boundValue(value, key, fieldLimit)
			values[i] = clean
			event.Truncated = event.Truncated || trimmed
			b, err := json.Marshal(clean)
			if err != nil {
				return err
			}
			*fields[i] = b
		}
		encoded, err := json.Marshal(event)
		if err != nil {
			return err
		}
		if len(encoded) < streamFrameBytes-1024 {
			for i := range receipts {
				d := &receipts[i]
				d.ConsumerID = redacted(d.ConsumerID, key, 128)
				if d.WorkspaceID != nil {
					v := redacted(*d.WorkspaceID, key, 128)
					d.WorkspaceID = &v
				}
				if d.SurfaceID != nil {
					v := redacted(*d.SurfaceID, key, 128)
					d.SurfaceID = &v
				}
				d.Message = redacted(d.Message, key, 4096)
				d.Summary = redacted(d.Summary, key, 16384)
			}
			event.Deliveries = receipts
			return nil
		}
	}
	return fmt.Errorf("event exceeds wire limit")
}
