package wsock

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// SocketManager tracks upgraded connections for the lifetime of a server.
// Its zero value is ready to use.
type SocketManager struct {
	mu          sync.Mutex
	connections map[*websocket.Conn]struct{}
	closing     bool
	streams     sync.WaitGroup
	sessionsMu  sync.Mutex
	sessions    map[string]*websocket.Conn
}

// Serve upgrades an authorized request and tracks its connection until serve returns.
func (m *SocketManager) Serve(w http.ResponseWriter, r *http.Request, readLimit int64, serve func(context.Context, *websocket.Conn)) {
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	m.mu.Lock()
	if m.closing {
		m.mu.Unlock()
		c.CloseNow()
		return
	}
	if m.connections == nil {
		m.connections = make(map[*websocket.Conn]struct{})
	}
	m.connections[c] = struct{}{}
	m.streams.Add(1)
	m.mu.Unlock()

	defer func() {
		c.CloseNow()
		m.mu.Lock()
		delete(m.connections, c)
		m.mu.Unlock()
		m.streams.Done()
	}()

	c.SetReadLimit(readLimit)
	serve(r.Context(), c)
}

// Shutdown closes upgraded sockets and waits for their handlers to finish.
func (m *SocketManager) Shutdown(ctx context.Context) error {
	m.mu.Lock()
	m.closing = true
	conns := make([]*websocket.Conn, 0, len(m.connections))
	for c := range m.connections {
		conns = append(conns, c)
	}
	m.mu.Unlock()

	for _, c := range conns {
		c.CloseNow()
	}
	done := make(chan struct{})
	go func() {
		m.streams.Wait()
		close(done)
	}()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("stop event streams: %w", ctx.Err())
	}
}

// WriteJSON sends a frame with the supplied write deadline.
func WriteJSON(ctx context.Context, c *websocket.Conn, timeout time.Duration, value any) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return wsjson.Write(ctx, c, value)
}
