package server

import (
	"context"
	"crypto/tls"
	"errors"
	"maps"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/nitecon/amux/storage"
)

// SecureConnections tracks verified peer certificates for the lifetime of the
// underlying sockets, including HTTP connections upgraded to WebSockets.
type SecureConnections struct {
	mu             sync.Mutex
	active         map[string]int
	serverActive   map[string]int
	serverSeen     string
	certificate    tls.Certificate
	serverSerial   string
	previousSerial string
	previousUntil  int64
}

// TrackSecureConnections wraps a listener and config before the first handshake.
func TrackSecureConnections(listener net.Listener, config *tls.Config) (net.Listener, *SecureConnections, error) {
	if config == nil || len(config.Certificates) != 1 || len(config.Certificates[0].Certificate) == 0 {
		return nil, nil, errors.New("secure listener requires one server certificate")
	}
	serial, err := storage.SecurityCertificateSerial(config.Certificates[0].Certificate[0])
	if err != nil {
		return nil, nil, err
	}
	tracker := &SecureConnections{
		active: make(map[string]int), serverActive: make(map[string]int),
		certificate: config.Certificates[0], serverSerial: serial,
	}
	base := config.Clone()
	base.GetConfigForClient = func(hello *tls.ClientHelloInfo) (*tls.Config, error) {
		socket := hello.Conn.(*secureSocket)
		handshake := config.Clone()
		handshake.Certificates = []tls.Certificate{socket.reserveServerCertificate()}
		handshake.SessionTicketsDisabled = true
		handshake.VerifyConnection = func(state tls.ConnectionState) error {
			if config.VerifyConnection != nil {
				if err := config.VerifyConnection(state); err != nil {
					return err
				}
			}
			if len(state.VerifiedChains) == 0 || len(state.PeerCertificates) == 0 {
				return nil
			}
			serial, err := storage.SecurityCertificateSerial(state.PeerCertificates[0].Raw)
			if err != nil {
				return err
			}
			socket.recordPeer(serial)
			return nil
		}
		return handshake, nil
	}
	return tls.NewListener(secureSocketListener{Listener: listener, tracker: tracker}, base), tracker, nil
}

func (s *SecureConnections) Snapshot() map[string]int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return maps.Clone(s.active)
}

// RetireCredentials serializes retirement with adding and releasing socket
// leases so a newly verified connection cannot be missed by a stale snapshot.
func (s *SecureConnections) RetireCredentials(ctx context.Context, enrollment *storage.SecurityEnrollment, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return enrollment.RetireCredentials(ctx, s.active, now)
}

// RefreshServerCertificate reconciles the profile manifest and swaps the leaf
// used by new handshakes. Generation selection and retirement share the lock.
func (s *SecureConnections) RefreshServerCertificate(enrollment *storage.SecurityEnrollment, profile, clusterID string, names []string, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	certificate, err := storage.EnsureProfileServerTLSLive(profile, clusterID, names, storage.ProfileServerLeaseSnapshot{
		ActiveSerialCounts: s.serverActive,
		ObservedSerials:    map[string]bool{s.serverSeen: true},
	}, now)
	if errors.Is(err, storage.ErrProfileServerRotationBlocked) {
		return nil
	}
	if err != nil {
		return err
	}
	serial, err := storage.SecurityCertificateSerial(certificate.Certificate[0])
	if err != nil {
		return err
	}
	identity, err := enrollment.ServerIdentityStatus(context.Background())
	if err != nil {
		return err
	}
	s.previousSerial, s.previousUntil = "", 0
	if identity.PreviousSerial != nil && identity.PreviousExpiresAt != nil {
		s.previousSerial, s.previousUntil = *identity.PreviousSerial, *identity.PreviousExpiresAt
	}
	s.certificate, s.serverSerial = certificate, serial
	return nil
}

type secureSocketListener struct {
	net.Listener
	tracker *SecureConnections
}

func (l secureSocketListener) Accept() (net.Conn, error) {
	socket, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	return &secureSocket{Conn: socket, tracker: l.tracker}, nil
}

type secureSocket struct {
	net.Conn
	tracker      *SecureConnections
	serial       string
	serverSerial string
	closed       bool
}

func (s *secureSocket) reserveServerCertificate() tls.Certificate {
	s.tracker.mu.Lock()
	defer s.tracker.mu.Unlock()
	if !s.closed && s.serverSerial == "" {
		s.serverSerial = s.tracker.serverSerial
		s.tracker.serverActive[s.serverSerial]++
	}
	return s.tracker.certificate
}

func observeSecureHandshake(conn net.Conn, state http.ConnState) {
	if state != http.StateActive {
		return
	}
	if tlsConn, ok := conn.(*tls.Conn); ok {
		if socket, ok := tlsConn.NetConn().(*secureSocket); ok {
			socket.tracker.mu.Lock()
			if socket.serverSerial == socket.tracker.serverSerial {
				socket.tracker.serverSeen = socket.serverSerial
			}
			socket.tracker.mu.Unlock()
		}
	}
}

type secureSocketContextKey struct{}

func secureSocketContext(ctx context.Context, conn net.Conn) context.Context {
	if tlsConn, ok := conn.(*tls.Conn); ok {
		if socket, ok := tlsConn.NetConn().(*secureSocket); ok {
			return context.WithValue(ctx, secureSocketContextKey{}, socket)
		}
	}
	return ctx
}

func validServerGeneration(ctx context.Context, now time.Time) bool {
	socket, _ := ctx.Value(secureSocketContextKey{}).(*secureSocket)
	if socket == nil {
		return true
	}
	tracker := socket.tracker
	tracker.mu.Lock()
	defer tracker.mu.Unlock()
	return socket.serverSerial == tracker.serverSerial ||
		(socket.serverSerial == tracker.previousSerial && now.UnixMilli() < tracker.previousUntil)
}

func releaseSerial(active map[string]int, serial string) {
	if serial == "" {
		return
	}
	active[serial]--
	if active[serial] == 0 {
		delete(active, serial)
	}
}

func (s *secureSocket) recordPeer(serial string) {
	s.tracker.mu.Lock()
	defer s.tracker.mu.Unlock()
	if s.closed || s.serial != "" {
		return
	}
	s.serial = serial
	s.tracker.active[serial]++
}

func (s *secureSocket) Close() error {
	err := s.Conn.Close()
	s.tracker.mu.Lock()
	defer s.tracker.mu.Unlock()
	if !s.closed {
		s.closed = true
		releaseSerial(s.tracker.active, s.serial)
		releaseSerial(s.tracker.serverActive, s.serverSerial)
	}
	return err
}
