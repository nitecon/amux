package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
	"unicode"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/nitecon/amux/eventbus"
	"github.com/nitecon/amux/router"
	"github.com/nitecon/amux/storage"
)

const (
	remoteStreamIdleTimeout = time.Minute
	remoteHeartbeatInterval = 20 * time.Second
	remoteWriteTimeout      = 5 * time.Second
	remoteBusBuffer         = 64
)

type RemoteConfig struct {
	Context   context.Context
	Registry  *router.Registry
	Forwarded []string
}

type remoteStreamConfig struct {
	ctx       context.Context
	registry  *router.Registry
	forwarded map[string]struct{}
}

func validateRemoteConfig(config RemoteConfig) (*remoteStreamConfig, error) {
	if config.Context == nil || config.Registry == nil {
		return nil, fmt.Errorf("remote event stream requires a live runtime context and registry")
	}
	if err := config.Context.Err(); err != nil {
		return nil, fmt.Errorf("remote event runtime is not live: %w", err)
	}
	forwarded := make(map[string]struct{}, len(config.Forwarded))
	for _, event := range config.Forwarded {
		if event == "" || strings.TrimSpace(event) != event || len(event) > 128 || strings.IndexFunc(event, unicode.IsControl) >= 0 {
			return nil, fmt.Errorf("invalid forwarded event ID")
		}
		if _, exists := forwarded[event]; exists {
			return nil, fmt.Errorf("duplicate forwarded event ID %q", event)
		}
		forwarded[event] = struct{}{}
	}
	return &remoteStreamConfig{ctx: config.Context, registry: config.Registry, forwarded: forwarded}, nil
}

func (a *api) remoteEventStream(w http.ResponseWriter, r *http.Request) {
	if authenticatedPrincipal(r.Context()) == "" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if a.remote == nil || a.remote.ctx.Err() != nil {
		writeError(w, http.StatusServiceUnavailable, "event router is not running")
		return
	}
	if len(r.URL.Query()) != 0 {
		writeError(w, http.StatusBadRequest, "event stream accepts no query parameters")
		return
	}
	a.withStream(w, r, streamFrameBytes, a.serveRemoteEventStream)
}

type remoteInput struct {
	kind        string
	consumerID  string
	events      []string
	targets     []string
	after       *int64
	seq         int64
	id          string
	version     uint32
	payload     json.RawMessage
	correlation *string
}

func decodeRemoteInput(raw []byte) (remoteInput, error) {
	var discriminator struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &discriminator); err != nil {
		return remoteInput{}, fmt.Errorf("invalid stream frame")
	}
	var input remoteInput
	switch discriminator.Type {
	case "subscribe":
		var req struct {
			Type       string   `json:"type"`
			ConsumerID string   `json:"consumer_id"`
			Events     []string `json:"events"`
			Targets    []string `json:"targets"`
			After      *int64   `json:"after"`
		}
		if err := strictRemoteFrame(raw, &req); err != nil || req.ConsumerID == "" || req.Events == nil {
			return remoteInput{}, fmt.Errorf("invalid subscribe frame")
		}
		input = remoteInput{kind: req.Type, consumerID: req.ConsumerID, events: req.Events, targets: req.Targets, after: req.After}
	case "ack":
		var req struct {
			Type string `json:"type"`
			Seq  *int64 `json:"seq"`
		}
		if err := strictRemoteFrame(raw, &req); err != nil || req.Seq == nil {
			return remoteInput{}, fmt.Errorf("invalid ack frame")
		}
		input = remoteInput{kind: req.Type, seq: *req.Seq}
	case "emit":
		var req struct {
			Type        string          `json:"type"`
			ID          string          `json:"id"`
			Version     uint32          `json:"version"`
			Payload     json.RawMessage `json:"payload"`
			Correlation *string         `json:"correlation"`
		}
		if err := strictRemoteFrame(raw, &req); err != nil || req.ID == "" || req.Version == 0 || len(req.Payload) == 0 || !json.Valid(req.Payload) {
			return remoteInput{}, fmt.Errorf("invalid emit frame")
		}
		input = remoteInput{kind: req.Type, id: req.ID, version: req.Version, payload: req.Payload, correlation: req.Correlation}
	case "heartbeat":
		var req struct {
			Type string `json:"type"`
		}
		if err := strictRemoteFrame(raw, &req); err != nil {
			return remoteInput{}, fmt.Errorf("invalid heartbeat frame")
		}
		input.kind = req.Type
	default:
		return remoteInput{}, fmt.Errorf("invalid stream frame type")
	}
	return input, nil
}

func strictRemoteFrame(raw []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("invalid stream frame")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return fmt.Errorf("invalid stream frame")
	}
	return nil
}

func writeRemoteFrame(ctx context.Context, conn *websocket.Conn, value any) error {
	body, err := json.Marshal(value)
	if err != nil {
		return err
	}
	writeCtx, cancel := context.WithTimeout(ctx, remoteWriteTimeout)
	defer cancel()
	return conn.Write(writeCtx, websocket.MessageText, body)
}

func remoteEventFrame(event *router.Event) any {
	return map[string]any{
		"type": "event",
		"event": map[string]any{
			"seq": event.Seq, "id": event.ID, "version": event.Version, "uid": event.UID,
			"payload": event.Payload, "source": event.Source, "emitted_at": event.EmittedAt,
			"correlation": event.Correlation,
		},
	}
}

func resetRemoteDeadline(timer *time.Timer, subscribed bool, lastHeartbeat time.Time, inflight *router.Event, sentAt time.Time, attempts int) <-chan time.Time {
	if !subscribed {
		timer.Stop()
		return nil
	}
	deadline := lastHeartbeat.Add(remoteHeartbeatInterval)
	if inflight != nil {
		retryAt := sentAt.Add(router.RetryDelay(int64(attempts)))
		if retryAt.Before(deadline) {
			deadline = retryAt
		}
	}
	delay := time.Until(deadline)
	if delay < 0 {
		delay = 0
	}
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
	timer.Reset(delay)
	return timer.C
}

func (a *api) serveRemoteEventStream(parent context.Context, conn *websocket.Conn) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	input := make(chan streamMessage, 1)
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		for {
			typ, body, err := conn.Read(ctx)
			select {
			case input <- streamMessage{typ: typ, body: body, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	defer func() { cancel(); conn.CloseNow(); <-readerDone }()
	connectionID := uuid.NewString()
	principal := authenticatedSecurePrincipal(parent)
	consumerID := ""
	var cursor int64
	var inflight *router.Event
	var inflightFrame any
	attempts := 0
	var sentAt time.Time
	lastHeartbeat := time.Now()
	var remoteEvents, remoteTargets []string
	idle := time.NewTimer(remoteStreamIdleTimeout)
	defer idle.Stop()
	deadline := time.NewTimer(time.Hour)
	if !deadline.Stop() {
		<-deadline.C
	}
	defer deadline.Stop()
	securityCheck := time.NewTicker(time.Second)
	defer securityCheck.Stop()
	bus := a.store.EventBus()
	sub := bus.Subscribe(remoteBusBuffer)
	defer func() { sub.Unsubscribe() }()
	defer func() {
		if consumerID != "" {
			cleanup, stop := context.WithTimeout(context.WithoutCancel(parent), 2*time.Second)
			defer stop()
			_ = a.store.CloseRemoteConsumer(cleanup, consumerID, connectionID)
		}
	}()

	for {
		deadlineC := resetRemoteDeadline(deadline, consumerID != "", lastHeartbeat, inflight, sentAt, attempts)
		select {
		case <-ctx.Done():
			return
		case <-a.remote.ctx.Done():
			return
		case <-idle.C:
			return
		case <-securityCheck.C:
			if principal != nil && !principal.valid(ctx) {
				return
			}
		case <-deadlineC:
			if consumerID == "" || a.store.CheckRemoteConsumer(ctx, consumerID, connectionID) != nil {
				return
			}
			now := time.Now()
			if now.Sub(lastHeartbeat) >= remoteHeartbeatInterval {
				if writeRemoteFrame(ctx, conn, map[string]any{"type": "heartbeat"}) != nil {
					return
				}
				lastHeartbeat = now
			}
			if inflight != nil && now.Sub(sentAt) >= router.RetryDelay(int64(attempts)) {
				if writeRemoteFrame(ctx, conn, inflightFrame) != nil {
					return
				}
				attempts++
				sentAt = now
			} else if inflight == nil && !a.offerRemoteEvent(ctx, conn, consumerID, connectionID, remoteEvents, remoteTargets, &inflight, &inflightFrame, &attempts, &sentAt) {
				return
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
			idle.Reset(remoteStreamIdleTimeout)
			request, err := decodeRemoteInput(message.body)
			if err != nil {
				if writeRemoteFrame(ctx, conn, map[string]any{"type": "error", "message": err.Error()}) != nil {
					return
				}
				continue
			}
			switch request.kind {
			case "subscribe":
				if consumerID != "" {
					err = fmt.Errorf("already subscribed")
					break
				}
				parsedID, parseErr := uuid.Parse(request.consumerID)
				if parseErr != nil {
					err = fmt.Errorf("consumer_id must be a UUID")
					break
				}
				request.consumerID = parsedID.String()
				for _, event := range request.events {
					if _, ok := a.remote.forwarded[event]; !ok {
						err = fmt.Errorf("event %q is not forwarded to remote consumers", event)
						break
					}
				}
				if err != nil {
					break
				}
				if principal != nil {
					if err = principal.enrollment.BindConsumer(ctx, principal.NodeID, request.consumerID); err != nil {
						break
					}
				}
				cursor, err = a.store.OpenRemoteConsumer(ctx, request.consumerID, connectionID, request.after)
				if err != nil {
					break
				}
				consumerID = request.consumerID
				remoteEvents = append([]string(nil), request.events...)
				remoteTargets = append([]string(nil), request.targets...)
				if err = writeRemoteFrame(ctx, conn, map[string]any{
					"type": "subscribed", "consumer_id": consumerID, "cursor": cursor,
					"server_restart_reminders": a.remote.registry.Subscribes("reminder", "client.heartbeat", 1),
				}); err != nil {
					return
				}
				if !a.offerRemoteEvent(ctx, conn, consumerID, connectionID, remoteEvents, remoteTargets, &inflight, &inflightFrame, &attempts, &sentAt) {
					return
				}
				continue
			case "heartbeat":
				if consumerID != "" {
					if err = a.store.TouchRemoteConsumer(ctx, consumerID, connectionID); err != nil {
						return
					}
				}
				if err = writeRemoteFrame(ctx, conn, map[string]any{"type": "heartbeat_ack"}); err != nil {
					return
				}
				continue
			case "ack":
				if consumerID == "" {
					err = fmt.Errorf("subscribe first")
					break
				}
				if request.seq <= cursor {
					err = a.store.CheckRemoteConsumer(ctx, consumerID, connectionID)
					if err == nil {
						err = a.store.TouchRemoteConsumer(ctx, consumerID, connectionID)
					}
				} else if inflight == nil || request.seq != inflight.Seq {
					err = fmt.Errorf("event %d is not in flight", request.seq)
				} else {
					err = a.store.AckRemoteEvent(ctx, consumerID, connectionID, request.seq)
					if err == nil {
						cursor = request.seq
						inflight, inflightFrame, attempts = nil, nil, 0
					}
				}
				if errors.Is(err, storage.ErrRemoteConsumerReplaced) {
					return
				}
				if err == nil {
					if err = writeRemoteFrame(ctx, conn, map[string]any{"type": "acked", "seq": request.seq}); err != nil {
						return
					}
					if !a.offerRemoteEvent(ctx, conn, consumerID, connectionID, remoteEvents, remoteTargets, &inflight, &inflightFrame, &attempts, &sentAt) {
						return
					}
					continue
				}
			case "emit":
				if !a.remote.registry.AcceptsRemote(request.id, request.version) {
					err = fmt.Errorf("event %s v%d cannot be emitted by clients", request.id, request.version)
					break
				}
				var correlation *string
				if request.correlation != nil {
					id, parseErr := uuid.Parse(*request.correlation)
					if parseErr != nil {
						err = fmt.Errorf("correlation must be an event uid")
						break
					}
					value := id.String()
					correlation = &value
				}
				if consumerID != "" {
					if err = a.store.CheckRemoteConsumer(ctx, consumerID, connectionID); err != nil {
						return
					}
				}
				result, emitErr := a.store.EmitRouterEvent(ctx, a.remote.registry, router.EmitRequest{
					ID: request.id, Version: request.version, Payload: request.payload,
					Source: "client:" + authenticatedPrincipal(ctx), Correlation: correlation,
				})
				err = emitErr
				if err == nil {
					if consumerID != "" {
						if err = a.store.TouchRemoteConsumer(ctx, consumerID, connectionID); err != nil {
							return
						}
					}
					if err = writeRemoteFrame(ctx, conn, map[string]any{"type": "emitted", "uid": result.Event.UID}); err != nil {
						return
					}
					continue
				}
			}
			if err != nil {
				if errors.Is(err, storage.ErrRemoteConsumerReplaced) {
					return
				}
				if writeRemoteFrame(ctx, conn, map[string]any{"type": "error", "message": err.Error()}) != nil {
					return
				}
			}
		case event, ok := <-sub.C:
			if !ok {
				subErr := sub.Err()
				if errors.Is(subErr, eventbus.ErrBusClosed) || !errors.Is(subErr, eventbus.ErrSlowConsumer) {
					return
				}
				sub.Unsubscribe()
				sub = bus.Subscribe(remoteBusBuffer)
				if consumerID != "" && inflight == nil && !a.offerRemoteEvent(ctx, conn, consumerID, connectionID, remoteEvents, remoteTargets, &inflight, &inflightFrame, &attempts, &sentAt) {
					return
				}
				continue
			}
			if consumerID == "" {
				continue
			}
			if event.Topic == eventbus.Tick {
				if err := a.store.CheckRemoteConsumer(ctx, consumerID, connectionID); err != nil {
					return
				}
				if inflight == nil && !a.offerRemoteEvent(ctx, conn, consumerID, connectionID, remoteEvents, remoteTargets, &inflight, &inflightFrame, &attempts, &sentAt) {
					return
				}
			} else if event.Topic == eventbus.RouterChanged && inflight == nil {
				if err := a.store.CheckRemoteConsumer(ctx, consumerID, connectionID); err != nil {
					return
				}
				if !a.offerRemoteEvent(ctx, conn, consumerID, connectionID, remoteEvents, remoteTargets, &inflight, &inflightFrame, &attempts, &sentAt) {
					return
				}
			}
		}
	}
}

func (a *api) offerRemoteEvent(ctx context.Context, conn *websocket.Conn, consumerID, connectionID string, events, targets []string, inflight **router.Event, inflightFrame *any, attempts *int, sentAt *time.Time) bool {
	if *inflight != nil {
		return true
	}
	event, err := a.store.NextRemoteEvent(ctx, consumerID, connectionID, events, targets)
	if err != nil {
		return false
	}
	if event == nil {
		return true
	}
	frame := remoteEventFrame(event)
	if err = writeRemoteFrame(ctx, conn, frame); err != nil {
		return false
	}
	*inflight = event
	*inflightFrame = frame
	*attempts = 1
	*sentAt = time.Now()
	return true
}
