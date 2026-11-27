package channels

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/rs/zerolog/log"
)

const (
	discordGatewayVersion      = "10"
	discordGatewayIntents      = 1 | (1 << 9) | (1 << 15) // GUILDS, GUILD_MESSAGES, MESSAGE_CONTENT
	discordGatewayReadLimit    = 4 << 20
	discordGatewayQueueSize    = 64
	discordIdentifyInterval    = 5 * time.Second
	discordGatewayMaxHeartbeat = time.Hour
	discordGatewayMaxReset     = 7 * 24 * time.Hour
	discordGatewayHelloTimeout = 30 * time.Second
	discordGatewayWriteTimeout = 10 * time.Second
)

type discordGatewayEnvelope struct {
	Op int             `json:"op"`
	D  json.RawMessage `json:"d"`
	S  *int64          `json:"s"`
	T  string          `json:"t"`
}

type discordGatewayClientEvent struct {
	Op int `json:"op"`
	D  any `json:"d"`
}

type discordIdentifyPayload struct {
	Token      string                    `json:"token"`
	Intents    int                       `json:"intents"`
	Properties discordIdentifyProperties `json:"properties"`
}

type discordIdentifyProperties struct {
	OS      string `json:"os"`
	Browser string `json:"browser"`
	Device  string `json:"device"`
}

type discordResumePayload struct {
	Token    string `json:"token"`
	Session  string `json:"session_id"`
	Sequence int64  `json:"seq"`
}

type discordGatewayHello struct {
	HeartbeatInterval int64 `json:"heartbeat_interval"`
}

type discordGatewayReady struct {
	SessionID        string `json:"session_id"`
	ResumeGatewayURL string `json:"resume_gateway_url"`
	User             struct {
		ID string `json:"id"`
	} `json:"user"`
}

type discordGatewayDiscovery struct {
	URL               string `json:"url"`
	SessionStartLimit struct {
		Total          int   `json:"total"`
		Remaining      int   `json:"remaining"`
		ResetAfterMS   int64 `json:"reset_after"`
		MaxConcurrency int   `json:"max_concurrency"`
	} `json:"session_start_limit"`
}

type discordGatewayInfo struct {
	url       string
	limited   bool
	remaining int
	resetAt   time.Time
}

type discordGatewaySession struct {
	id        string
	resumeURL string
	sequence  atomic.Int64 // -1 means no accepted dispatch sequence yet.
}

func newDiscordGatewaySession() *discordGatewaySession {
	session := &discordGatewaySession{}
	session.sequence.Store(-1)
	return session
}

func (s *discordGatewaySession) canResume() bool {
	return s.id != "" && s.resumeURL != "" && s.sequence.Load() >= 0
}

func (s *discordGatewaySession) clear() {
	s.id = ""
	s.resumeURL = ""
	s.sequence.Store(-1)
}

type discordGatewayWork struct {
	backfill bool
	message  json.RawMessage
}

type discordGatewayNext int

const (
	gatewayNextResume discordGatewayNext = iota
	gatewayNextIdentify
	gatewayNextStop
)

// Run owns the Discord Gateway connection until ctx is canceled or a fatal
// provider error occurs. Gateway reads, message work, and caller delivery use
// separate bounded queues so backfill or a blocked consumer cannot stall
// heartbeat processing; queue saturation reconnects from the last accepted
// dispatch sequence instead of dropping a business event.
func (d *Discord) Run(ctx context.Context, sink chan<- InboundEvent) error {
	if sink == nil {
		return errors.New("Discord Gateway requires an event sink")
	}
	if !d.runMu.TryLock() {
		return errors.New("Discord Gateway is already running")
	}
	defer d.runMu.Unlock()

	runCtx, cancel := context.WithCancel(ctx)
	work := make(chan discordGatewayWork, discordGatewayQueueSize)
	deliveries := make(chan InboundEvent, discordGatewayQueueSize)
	workerDone := make(chan struct{})
	senderDone := make(chan struct{})
	go d.runGatewayWork(runCtx, work, deliveries, workerDone)
	go deliverGatewayEvents(runCtx, deliveries, sink, senderDone)
	defer func() {
		cancel()
		close(work)
		<-workerDone
		close(deliveries)
		<-senderDone
	}()

	info := discordGatewayInfo{url: d.gatewayURL}
	if info.url == "" {
		var err error
		info, err = d.discoverGateway(runCtx)
		if err != nil {
			if runCtx.Err() != nil {
				return nil
			}
			return err
		}
	}
	session := newDiscordGatewaySession()
	var lastIdentify time.Time
	for {
		if runCtx.Err() != nil {
			return nil
		}
		resuming := session.canResume()
		gatewayURL := info.url
		if resuming {
			gatewayURL = session.resumeURL
		} else {
			var err error
			info, err = d.prepareIdentify(runCtx, info, &lastIdentify)
			if err != nil {
				if runCtx.Err() != nil {
					return nil
				}
				return err
			}
			gatewayURL = info.url
		}
		wsClient := *d.client
		wsClient.Timeout = 0 // The REST timeout must not cap the persistent Gateway connection.
		dialCtx, cancelDial := context.WithTimeout(runCtx, discordGatewayHelloTimeout)
		conn, response, err := websocket.Dial(dialCtx, gatewayURL, &websocket.DialOptions{HTTPClient: &wsClient})
		cancelDial()
		if err != nil {
			if runCtx.Err() != nil {
				return nil
			}
			if response != nil && (response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden) {
				return fmt.Errorf("Discord Gateway handshake rejected with HTTP %d", response.StatusCode)
			}
			if err := waitGateway(runCtx, time.Second); err != nil {
				return nil
			}
			continue
		}
		next, delay, err := d.runGatewayConnection(runCtx, conn, session, work, resuming, func() {
			lastIdentify = time.Now()
			if info.limited && info.remaining > 0 {
				info.remaining--
			}
		})
		_ = conn.CloseNow()
		if err != nil {
			return err
		}
		if runCtx.Err() != nil || next == gatewayNextStop {
			return nil
		}
		if next == gatewayNextIdentify {
			session.clear()
		}
		if delay > 0 {
			if err := waitGateway(runCtx, delay); err != nil {
				return nil
			}
		}
	}
}

func (d *Discord) discoverGateway(ctx context.Context) (discordGatewayInfo, error) {
	var response discordGatewayDiscovery
	if err := d.request(ctx, http.MethodGet, "/gateway/bot", nil, &response); err != nil {
		return discordGatewayInfo{}, fmt.Errorf("discover Discord Gateway: %w", err)
	}
	gatewayURL, err := normalizeGatewayURL(response.URL)
	if err != nil {
		return discordGatewayInfo{}, errors.New("Discord returned an invalid Gateway URL")
	}
	limit := response.SessionStartLimit
	if limit.Total <= 0 || limit.Remaining < 0 || limit.Remaining > limit.Total || limit.MaxConcurrency <= 0 || limit.ResetAfterMS < 0 || limit.ResetAfterMS > int64(discordGatewayMaxReset/time.Millisecond) {
		return discordGatewayInfo{}, errors.New("Discord returned invalid Gateway session limits")
	}
	resetAfter := time.Duration(limit.ResetAfterMS) * time.Millisecond
	return discordGatewayInfo{
		url: gatewayURL, limited: true, remaining: limit.Remaining,
		resetAt: time.Now().Add(resetAfter),
	}, nil
}

func normalizeGatewayURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" || parsed.User != nil || parsed.Fragment != "" {
		return "", errors.New("invalid Gateway URL")
	}
	if parsed.Scheme != "wss" && !(parsed.Scheme == "ws" && isLoopbackHost(parsed.Hostname())) {
		return "", errors.New("Gateway URL must use wss or loopback ws")
	}
	query := parsed.Query()
	for key := range query {
		switch strings.ToLower(key) {
		case "token", "bot_token", "access_token", "authorization", "auth", "api_key":
			return "", errors.New("Gateway URL must not contain credentials")
		}
	}
	query.Set("v", discordGatewayVersion)
	query.Set("encoding", "json")
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

func (d *Discord) prepareIdentify(ctx context.Context, info discordGatewayInfo, lastIdentify *time.Time) (discordGatewayInfo, error) {
	if !lastIdentify.IsZero() {
		if err := waitGateway(ctx, time.Until(lastIdentify.Add(discordIdentifyInterval))); err != nil {
			return info, err
		}
	}
	if !info.limited || info.remaining > 0 {
		return info, nil
	}
	if err := waitGateway(ctx, time.Until(info.resetAt)); err != nil {
		return info, err
	}
	if d.gatewayURL != "" {
		return info, errors.New("Discord Gateway session start limit exhausted")
	}
	refreshed, err := d.discoverGateway(ctx)
	if err != nil {
		return info, err
	}
	if refreshed.remaining == 0 {
		return info, errors.New("Discord Gateway session start limit remains exhausted after reset")
	}
	return refreshed, nil
}

func (d *Discord) runGatewayConnection(
	ctx context.Context,
	conn *websocket.Conn,
	session *discordGatewaySession,
	work chan<- discordGatewayWork,
	resuming bool,
	identified func(),
) (discordGatewayNext, time.Duration, error) {
	conn.SetReadLimit(discordGatewayReadLimit)
	helloCtx, cancelHello := context.WithTimeout(ctx, discordGatewayHelloTimeout)
	helloType, helloRaw, err := conn.Read(helloCtx)
	cancelHello()
	if err != nil {
		return discordGatewayReadFailure(ctx, session, err)
	}
	if helloType != websocket.MessageText {
		return gatewayNextStop, 0, errors.New("Discord Gateway sent a non-text Hello frame")
	}
	var hello discordGatewayEnvelope
	if err := json.Unmarshal(helloRaw, &hello); err != nil || hello.Op != 10 {
		return gatewayNextStop, 0, errors.New("Discord Gateway did not begin with Hello")
	}
	var heartbeat discordGatewayHello
	if err := json.Unmarshal(hello.D, &heartbeat); err != nil || heartbeat.HeartbeatInterval <= 0 || heartbeat.HeartbeatInterval > int64(discordGatewayMaxHeartbeat/time.Millisecond) {
		return gatewayNextStop, 0, errors.New("Discord Gateway returned an invalid heartbeat interval")
	}
	heartbeatRequests := make(chan struct{}, 1)
	heartbeatAcks := make(chan struct{}, 1)
	heartbeatCtx, cancelHeartbeat := context.WithCancel(ctx)
	heartbeatErr := make(chan error, 1)
	heartbeatDone := make(chan struct{})
	go func() {
		defer close(heartbeatDone)
		if err := runDiscordHeartbeats(heartbeatCtx, conn, time.Duration(heartbeat.HeartbeatInterval)*time.Millisecond, &session.sequence, heartbeatRequests, heartbeatAcks); err != nil {
			heartbeatErr <- err
			_ = conn.CloseNow()
		}
	}()
	defer func() {
		cancelHeartbeat()
		_ = conn.CloseNow()
		<-heartbeatDone
	}()

	if resuming {
		payload := discordGatewayClientEvent{Op: 6, D: discordResumePayload{
			Token: d.token, Session: session.id, Sequence: session.sequence.Load(),
		}}
		if err := writeDiscordGateway(ctx, conn, payload); err != nil {
			return gatewayNextResume, time.Second, nil
		}
	} else {
		identified()
		payload := discordGatewayClientEvent{Op: 2, D: discordIdentifyPayload{
			Token: d.token, Intents: discordGatewayIntents,
			Properties: discordIdentifyProperties{OS: runtime.GOOS, Browser: "amux-go", Device: "amux-go"},
		}}
		if err := writeDiscordGateway(ctx, conn, payload); err != nil {
			return gatewayNextResume, time.Second, nil
		}
	}

	for {
		frameType, raw, err := conn.Read(ctx)
		if err != nil {
			select {
			case heartbeatFailure := <-heartbeatErr:
				_ = heartbeatFailure // Do not expose provider or transport payloads.
			default:
			}
			return discordGatewayReadFailure(ctx, session, err)
		}
		if frameType != websocket.MessageText {
			return gatewayNextStop, 0, errors.New("Discord Gateway sent a non-text frame")
		}
		var event discordGatewayEnvelope
		if err := json.Unmarshal(raw, &event); err != nil {
			return gatewayNextStop, 0, errors.New("decode Discord Gateway event")
		}
		switch event.Op {
		case 0: // Dispatch
			if event.S == nil || *event.S < 0 {
				return gatewayNextStop, 0, errors.New("Discord Gateway dispatch has no valid sequence")
			}
			switch event.T {
			case "READY":
				var ready discordGatewayReady
				if err := json.Unmarshal(event.D, &ready); err != nil || ready.SessionID == "" {
					return gatewayNextStop, 0, errors.New("Discord Gateway Ready event is invalid")
				}
				resumeURL, err := normalizeGatewayURL(ready.ResumeGatewayURL)
				if err != nil {
					return gatewayNextStop, 0, errors.New("Discord Gateway Ready event has an invalid resume URL")
				}
				if _, err := parseSnowflake(ready.User.ID); err != nil {
					return gatewayNextStop, 0, errors.New("Discord Gateway Ready event has an invalid user ID")
				}
				session.id = ready.SessionID
				session.resumeURL = resumeURL
				d.setDiscordBotID(ready.User.ID)
				if !enqueueDiscordWork(work, discordGatewayWork{backfill: true}) {
					return gatewayNextResume, time.Second, nil
				}
				session.sequence.Store(*event.S)
			case "MESSAGE_CREATE":
				if !enqueueDiscordWork(work, discordGatewayWork{message: append(json.RawMessage(nil), event.D...)}) {
					return gatewayNextResume, time.Second, nil
				}
				session.sequence.Store(*event.S)
			default:
				session.sequence.Store(*event.S)
			}
		case 1: // Server requested an immediate heartbeat.
			select {
			case heartbeatRequests <- struct{}{}:
			default:
				return gatewayNextResume, time.Second, nil
			}
		case 7: // Reconnect; retain the session and resume immediately.
			return gatewayNextResume, 0, nil
		case 9: // Invalid Session.
			var resumable bool
			if err := json.Unmarshal(event.D, &resumable); err != nil {
				return gatewayNextStop, 0, errors.New("Discord Gateway Invalid Session event is invalid")
			}
			if resumable {
				if !session.canResume() {
					return gatewayNextStop, 0, errors.New("Discord Gateway requested resume without an active session")
				}
				return gatewayNextResume, discordInvalidSessionBackoff(), nil
			}
			session.clear()
			return gatewayNextIdentify, discordInvalidSessionBackoff(), nil
		case 10: // Hello should occur once per connection.
			return gatewayNextStop, 0, errors.New("Discord Gateway sent duplicate Hello")
		case 11: // Heartbeat ACK.
			select {
			case heartbeatAcks <- struct{}{}:
			default:
			}
		}
	}
}

func discordGatewayReadFailure(ctx context.Context, session *discordGatewaySession, err error) (discordGatewayNext, time.Duration, error) {
	if ctx.Err() != nil {
		return gatewayNextStop, 0, nil
	}
	if code := int(websocket.CloseStatus(err)); code != -1 {
		switch {
		case code == 4004 || (code >= 4010 && code <= 4014):
			return gatewayNextStop, 0, fmt.Errorf("Discord Gateway closed with fatal code %d", code)
		case code == 4007 || code == 4009 || code == 1000 || code == 1001:
			session.clear()
			return gatewayNextIdentify, 0, nil
		}
	}
	return gatewayNextResume, time.Second, nil
}

func runDiscordHeartbeats(ctx context.Context, conn *websocket.Conn, interval time.Duration, sequence *atomic.Int64, requests <-chan struct{}, acks <-chan struct{}) error {
	firstDelay := time.Duration(rand.Int63n(int64(interval)))
	return runDiscordHeartbeatsWithFirstDelay(ctx, conn, interval, sequence, requests, acks, firstDelay)
}

func runDiscordHeartbeatsWithFirstDelay(ctx context.Context, conn *websocket.Conn, interval time.Duration, sequence *atomic.Int64, requests <-chan struct{}, acks <-chan struct{}, firstDelay time.Duration) error {
	timer := time.NewTimer(firstDelay)
	defer timer.Stop()
	ackTimer := time.NewTimer(interval)
	if !ackTimer.Stop() {
		<-ackTimer.C
	}
	defer ackTimer.Stop()
	var ackTimerC <-chan time.Time
	pendingACK := false
	markPending := func() {
		if pendingACK {
			return
		}
		ackTimer.Reset(interval)
		ackTimerC = ackTimer.C
		pendingACK = true
	}
	clearPending := func() {
		if !pendingACK {
			return
		}
		if !ackTimer.Stop() {
			select {
			case <-ackTimer.C:
			default:
			}
		}
		ackTimerC = nil
		pendingACK = false
	}
	send := func() error {
		var seq any
		if value := sequence.Load(); value >= 0 {
			seq = value
		}
		return writeDiscordGateway(ctx, conn, discordGatewayClientEvent{Op: 1, D: seq})
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-acks:
			clearPending()
		case <-requests:
			if err := send(); err != nil {
				return fmt.Errorf("Discord Gateway heartbeat write failed: %w", err)
			}
			markPending()
		case <-ackTimerC:
			pendingACK = false
			ackTimerC = nil
			select {
			case <-acks:
			default:
				return errors.New("Discord Gateway heartbeat ACK missing")
			}
		case <-timer.C:
			if err := send(); err != nil {
				return fmt.Errorf("Discord Gateway heartbeat write failed: %w", err)
			}
			markPending()
			timer.Reset(interval)
		}
	}
}

func writeDiscordGateway(ctx context.Context, conn *websocket.Conn, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil || len(data) > 4096 {
		return errors.New("invalid Discord Gateway client event")
	}
	writeCtx, cancel := context.WithTimeout(ctx, discordGatewayWriteTimeout)
	defer cancel()
	return conn.Write(writeCtx, websocket.MessageText, data)
}

func enqueueDiscordWork(work chan<- discordGatewayWork, item discordGatewayWork) bool {
	select {
	case work <- item:
		return true
	default:
		return false
	}
}

func (d *Discord) runGatewayWork(ctx context.Context, work <-chan discordGatewayWork, deliveries chan<- InboundEvent, done chan<- struct{}) {
	defer close(done)
	for {
		select {
		case <-ctx.Done():
			return
		case item, ok := <-work:
			if !ok {
				return
			}
			var err error
			if item.backfill {
				err = d.backfillDiscordRooms(ctx, deliveries)
			} else {
				err = d.dispatchDiscordMessage(ctx, deliveries, item.message)
			}
			if err != nil && ctx.Err() == nil {
				log.Warn().Ctx(ctx).Msg("Discord inbound work failed")
			}
		}
	}
}

func deliverGatewayEvents(ctx context.Context, deliveries <-chan InboundEvent, sink chan<- InboundEvent, done chan<- struct{}) {
	defer close(done)
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-deliveries:
			if !ok {
				return
			}
			select {
			case <-ctx.Done():
				return
			case sink <- event:
			}
		}
	}
}

func discordInvalidSessionBackoff() time.Duration {
	return time.Duration(1+rand.Intn(5)) * time.Second
}

func waitGateway(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
