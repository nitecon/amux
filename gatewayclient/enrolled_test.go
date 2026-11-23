package gatewayclient

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestNewEnrolledUsesPinnedTLSForHTTPAndWebSocket(t *testing.T) {
	var redirect atomic.Bool
	var redirected atomic.Int32
	var sawPeer atomic.Bool
	var sawBearer atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { redirected.Add(1) }))
	defer target.Close()
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS != nil && len(r.TLS.PeerCertificates) == 1 {
			sawPeer.Store(true)
		}
		if r.Header.Get("Authorization") != "" {
			sawBearer.Store(true)
		}
		if redirect.Load() && r.URL.Path == "/v1/projects" {
			http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
			return
		}
		if r.TLS == nil || len(r.TLS.PeerCertificates) != 1 || r.Header.Get("Authorization") != "" {
			http.Error(w, "missing certificate or bearer was added", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/projects":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte("[]"))
		case "/v1/navigation/stream":
			conn, err := websocket.Accept(w, r, nil)
			if err != nil {
				return
			}
			defer conn.CloseNow()
			_ = wsjson.Write(r.Context(), conn, map[string]any{"type": "ready", "version": 1})
		default:
			http.NotFound(w, r)
		}
	})
	srv := httptest.NewUnstartedServer(handler)
	srv.TLS = &tls.Config{MinVersion: tls.VersionTLS12, ClientAuth: tls.RequireAnyClientCert}
	srv.StartTLS()
	defer srv.Close()

	clientCertificate := syntheticEnrolledClientCertificate(t)
	roots := x509.NewCertPool()
	roots.AddCert(srv.Certificate())
	client, err := NewEnrolled(srv.URL, clientCertificate, roots)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Projects(context.Background()); err != nil {
		t.Fatalf("certificate-authenticated HTTP request failed: %v (peer=%v bearer=%v)", err, sawPeer.Load(), sawBearer.Load())
	}
	stop := errors.New("stop after first WebSocket frame")
	err = client.WatchNavigation(context.Background(), func() error { return stop })
	if !errors.Is(err, stop) {
		t.Fatalf("certificate-authenticated WebSocket failed: %v", err)
	}
	redirect.Store(true)
	_, err = client.Projects(context.Background())
	var response *HTTPError
	if !errors.As(err, &response) || response.Status != http.StatusTemporaryRedirect || redirected.Load() != 0 {
		t.Fatalf("enrolled request followed redirect: err=%v target hits=%d", err, redirected.Load())
	}
}

func TestNewEnrolledRequiresHTTPSAndCredentials(t *testing.T) {
	certificate := syntheticEnrolledClientCertificate(t)
	roots := x509.NewCertPool()
	for _, endpoint := range []string{"http://localhost:1234", "http://127.0.0.1:1234"} {
		if _, err := NewEnrolled(endpoint, certificate, roots); err == nil {
			t.Fatalf("unencrypted enrolled endpoint accepted: %s", endpoint)
		}
	}
	if _, err := NewEnrolled("https://localhost:1234", tls.Certificate{}, roots); err == nil {
		t.Fatal("missing client certificate accepted")
	}
	if _, err := NewEnrolled("https://localhost:1234", certificate, nil); err == nil {
		t.Fatal("missing pinned trust roots accepted")
	}
}

func syntheticEnrolledClientCertificate(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	template := &x509.Certificate{
		SerialNumber: big.NewInt(42), Subject: pkix.Name{CommonName: "enrolled-fixture"},
		NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
