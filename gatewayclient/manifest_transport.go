package gatewayclient

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"time"

	"github.com/nitecon/amux/storage"
)

type manifestCredentials struct {
	profile     storage.LocalSecurityProfile
	certificate tls.Certificate
	roots       *x509.CertPool
}

type manifestTransport struct {
	path        string
	peer        bool
	endpoint    string
	clusterID   string
	nodeID      string
	instanceID  string
	pinnedRoots *x509.CertPool
	mu          sync.Mutex
	generation  string
	transport   *http.Transport
}

type manifestCredentialError struct{ err error }

func (e *manifestCredentialError) Error() string { return e.err.Error() }
func (e *manifestCredentialError) Unwrap() error { return e.err }

func manifestError(err error) error {
	var credentialError *manifestCredentialError
	if errors.As(err, &credentialError) {
		return credentialError
	}
	return nil
}

func newManifestClient(endpoint, path string) (*Client, error) {
	credentials, err := loadManifestCredentials(path)
	if err != nil {
		return nil, err
	}
	profile := credentials.profile
	if endpoint != "" && endpoint != profile.URL {
		return nil, fmt.Errorf("enrolled profile belongs to another gateway")
	}
	base := profile.URL
	transport := &manifestTransport{
		path: path, peer: filepath.Base(path) == "client-profile.json", endpoint: base,
		clusterID: profile.ClusterID, nodeID: profile.NodeID, instanceID: profile.InstanceID,
		pinnedRoots: credentials.roots.Clone(), generation: profile.Serial,
	}
	transport.transport, err = enrolledHTTPTransport(credentials.certificate, credentials.roots)
	if err != nil {
		return nil, err
	}
	client, err := newClient(base, "", &http.Client{Timeout: 15 * time.Second, Transport: transport}, true)
	if err != nil {
		transport.transport.CloseIdleConnections()
		return nil, err
	}
	client.clusterID = profile.ClusterID
	client.nodeID = profile.NodeID
	return client, nil
}

func (m *manifestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	transport, err := m.current(request.Context())
	if err != nil {
		return nil, &manifestCredentialError{err: err}
	}
	return transport.RoundTrip(request)
}

func (m *manifestTransport) current(ctx context.Context) (*http.Transport, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	credentials, err := loadManifestCredentials(m.path)
	if err != nil {
		return nil, err
	}
	if err := m.validate(credentials); err != nil {
		return nil, err
	}
	if m.peer {
		// Validate the manifest before allowing renewal code to contact its URL.
		// Reload and validate again because renewal may publish a new generation.
		_ = storage.RenewPeerClientIfDue(ctx, m.path, func(peer storage.PeerClientCredentials) error {
			return m.validate(manifestCredentials{profile: peer.Profile, roots: peer.RootCAs})
		})
		credentials, err = loadManifestCredentials(m.path)
		if err != nil {
			return nil, err
		}
		if err := m.validate(credentials); err != nil {
			return nil, err
		}
	}
	profile := credentials.profile
	if profile.Serial == m.generation {
		return m.transport, nil
	}
	updated, err := enrolledHTTPTransport(credentials.certificate, credentials.roots)
	if err != nil {
		return nil, err
	}
	previous := m.transport
	m.transport = updated
	m.generation = profile.Serial
	previous.CloseIdleConnections()
	return updated, nil
}

func (m *manifestTransport) validate(credentials manifestCredentials) error {
	profile := credentials.profile
	if profile.URL != m.endpoint || profile.ClusterID != m.clusterID || profile.NodeID != m.nodeID || profile.InstanceID != m.instanceID {
		return fmt.Errorf("enrolled profile identity or gateway changed")
	}
	if credentials.roots == nil || !credentials.roots.Equal(m.pinnedRoots) {
		return fmt.Errorf("enrolled profile trust root changed")
	}
	return nil
}

func loadManifestCredentials(path string) (manifestCredentials, error) {
	var result manifestCredentials
	switch filepath.Base(path) {
	case "local-client.json":
		credentials, err := storage.LoadLocalClientCredentials(filepath.Dir(path))
		if err != nil {
			return result, err
		}
		result = manifestCredentials{profile: credentials.Profile, certificate: credentials.Certificate, roots: credentials.RootCAs}
	case "client-profile.json":
		credentials, err := storage.LoadPeerClientCredentials(path)
		if err != nil {
			return result, err
		}
		result = manifestCredentials{profile: credentials.Profile, certificate: credentials.Certificate, roots: credentials.RootCAs}
	default:
		return result, fmt.Errorf("enrolled profile must name a client-profile.json or local-client.json manifest")
	}
	return result, nil
}

func enrolledHTTPTransport(certificate tls.Certificate, roots *x509.CertPool) (*http.Transport, error) {
	if roots == nil || len(certificate.Certificate) == 0 || certificate.PrivateKey == nil {
		return nil, errors.New("enrolled client certificate and trust roots are required")
	}
	return &http.Transport{TLSClientConfig: &tls.Config{
		MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{certificate}, RootCAs: roots.Clone(),
	}}, nil
}
