package storage

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/google/uuid"
)

// PeerClientCredentials is the validated client identity for an enrolled peer.
// It contains no issuer key and does not require opening the server security store.
type PeerClientCredentials struct {
	Profile     LocalSecurityProfile
	Certificate tls.Certificate
	RootCAs     *x509.CertPool
}

// LoadPeerClientCredentials loads the client-profile.json manifest written by
// PairRemote and validates its certificate, key, and pinned peer root.
func LoadPeerClientCredentials(manifestPath string) (PeerClientCredentials, error) {
	var result PeerClientCredentials
	path, err := filepath.Abs(manifestPath)
	if err != nil || filepath.Base(path) != "client-profile.json" {
		return result, fmt.Errorf("peer profile must name a client-profile.json manifest")
	}
	directory, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return result, fmt.Errorf("peer profile directory is unavailable")
	}
	path = filepath.Join(directory, filepath.Base(path))
	exists, err := regularProfileFile(path)
	if err != nil || !exists {
		return result, fmt.Errorf("peer credentials are unavailable")
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return result, fmt.Errorf("read peer credentials: %w", err)
	}
	var profile LocalSecurityProfile
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&profile); err != nil {
		return result, fmt.Errorf("invalid peer credentials")
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return result, fmt.Errorf("invalid peer credentials")
	}
	if profile.Version != 1 || profile.Serial == "" || profile.ExpiresAt <= time.Now().UnixMilli() {
		return result, fmt.Errorf("peer credentials are invalid or expired")
	}
	for _, value := range []string{profile.ClusterID, profile.NodeID, profile.InstanceID} {
		id, err := uuid.Parse(value)
		if err != nil || id == uuid.Nil || id.String() != value {
			return result, fmt.Errorf("invalid peer identity")
		}
	}
	serverURL, err := url.Parse(profile.URL)
	if err != nil || serverURL.Scheme != "https" || serverURL.Host == "" || serverURL.User != nil ||
		(serverURL.Path != "" && serverURL.Path != "/") || serverURL.RawQuery != "" || serverURL.ForceQuery || serverURL.Fragment != "" {
		return result, fmt.Errorf("peer URL must be an HTTPS origin")
	}
	if _, err := pairingBaseURL(profile.URL); err != nil {
		return result, fmt.Errorf("peer URL must be an HTTPS origin")
	}
	rootPath, err := confinedProfilePath(directory, profile.RootPEMPath, true)
	if err != nil {
		return result, err
	}
	rootPEM, err := os.ReadFile(rootPath)
	if err != nil {
		return result, err
	}
	rootBlock, rest := pem.Decode(rootPEM)
	if rootBlock == nil || rootBlock.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return result, fmt.Errorf("invalid peer trust root")
	}
	root, err := x509.ParseCertificate(rootBlock.Bytes)
	if err != nil || !root.IsCA || !root.BasicConstraintsValid || root.KeyUsage&x509.KeyUsageCertSign == 0 ||
		root.Subject.CommonName != "amux "+profile.ClusterID || !bytes.Equal(root.RawIssuer, root.RawSubject) ||
		root.CheckSignatureFrom(root) != nil || time.Now().Before(root.NotBefore) || time.Now().After(root.NotAfter) {
		return result, fmt.Errorf("invalid peer trust root")
	}
	certificatePath, err := confinedProfilePath(directory, profile.CertificatePEMPath, true)
	if err != nil {
		return result, err
	}
	certificatePEM, err := os.ReadFile(certificatePath)
	if err != nil {
		return result, err
	}
	certificateBlock, rest := pem.Decode(certificatePEM)
	if certificateBlock == nil || certificateBlock.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return result, fmt.Errorf("invalid peer client certificate")
	}
	certificate, err := x509.ParseCertificate(certificateBlock.Bytes)
	if err != nil {
		return result, fmt.Errorf("invalid peer client certificate")
	}
	serial, serialErr := certificateSerialCompat(certificate.Raw)
	if serialErr != nil || serial != profile.Serial || certificate.Subject.CommonName != profile.NodeID ||
		certificate.NotAfter.UnixMilli() != profile.ExpiresAt || time.Now().Before(certificate.NotBefore) ||
		time.Now().After(certificate.NotAfter) || !validLocalClientCertificate(certificate, root) {
		return result, fmt.Errorf("peer client certificate does not match profile")
	}
	keyPath, err := confinedProfilePath(directory, profile.PrivateKeyPEMPath, true)
	if err != nil {
		return result, err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return result, err
	}
	keyPEM, err = unprotectProfileRootKey(keyPEM)
	if err != nil {
		return result, fmt.Errorf("read peer client private key: %w", err)
	}
	key, err := parseProfileRootKey(keyPEM)
	if err != nil {
		return result, fmt.Errorf("invalid peer client private key")
	}
	public, ok := certificate.PublicKey.(*ecdsa.PublicKey)
	if !ok || public.X.Cmp(key.X) != 0 || public.Y.Cmp(key.Y) != 0 {
		return result, fmt.Errorf("peer certificate and private key do not match")
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)
	result.Profile = profile
	result.Certificate = tls.Certificate{Certificate: [][]byte{certificate.Raw}, PrivateKey: key, Leaf: certificate}
	result.RootCAs = roots
	return result, nil
}
