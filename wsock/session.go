package wsock

import "github.com/coder/websocket"

// ClaimSession installs a socket for an authorized session. claim persists
// ownership before the replacement is published. The returned function releases
// only this socket's registration.
func (m *SocketManager) ClaimSession(key string, conn *websocket.Conn, claim func() error) (func(), error) {
	m.sessionsMu.Lock()
	if err := claim(); err != nil {
		m.sessionsMu.Unlock()
		return nil, err
	}
	previous := m.sessions[key]
	if m.sessions == nil {
		m.sessions = make(map[string]*websocket.Conn)
	}
	m.sessions[key] = conn
	m.sessionsMu.Unlock()

	if previous != nil && previous != conn {
		previous.CloseNow()
	}

	return func() {
		m.sessionsMu.Lock()
		if m.sessions[key] == conn {
			delete(m.sessions, key)
		}
		m.sessionsMu.Unlock()
	}, nil
}
