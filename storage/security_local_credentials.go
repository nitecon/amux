package storage

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/google/uuid"
)

// LocalClientCredentials is the validated local administrator identity used
// by profile-bound server operations. It is read from profile files only.
type LocalClientCredentials struct {
	Profile     LocalSecurityProfile
	Certificate tls.Certificate
	RootCAs     *x509.CertPool
}

// LoadLocalClientCredentials reads and validates local-client.json and its
// files without opening security.sqlite.
func LoadLocalClientCredentials(profileDir string) (LocalClientCredentials, error) {
	var result LocalClientCredentials
	canonical, err := filepath.Abs(profileDir)
	if err != nil {
		return result, err
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return result, fmt.Errorf("invalid server profile directory")
	}
	manifest := filepath.Join(canonical, "local-client.json")
	exists, err := regularProfileFile(manifest)
	if err != nil || !exists {
		return result, fmt.Errorf("local server credentials are unavailable")
	}
	data, err := os.ReadFile(manifest)
	if err != nil {
		return result, fmt.Errorf("read local server credentials: %w", err)
	}
	var profile LocalSecurityProfile
	if err := json.Unmarshal(data, &profile); err != nil {
		return result, fmt.Errorf("invalid local server credentials")
	}
	if profile.Version != 1 || profile.Serial == "" || profile.ExpiresAt <= time.Now().UnixMilli() {
		return result, fmt.Errorf("local server credentials are invalid or expired")
	}
	for _, value := range []string{profile.ClusterID, profile.NodeID, profile.InstanceID} {
		id, err := uuid.Parse(value)
		if err != nil || id == uuid.Nil || id.String() != value {
			return result, fmt.Errorf("invalid local server identity")
		}
	}
	serverURL, err := localServerURL(profile.URL)
	if err != nil {
		return result, err
	}
	for _, path := range []string{filepath.Join(canonical, "root.pem"), filepath.Join(canonical, "root-key.pem")} {
		regular, err := regularProfileFile(path)
		if err != nil || !regular {
			return result, fmt.Errorf("profile trust root files are unavailable")
		}
	}
	root, rootKey, err := loadProfileRoot(canonical)
	if err != nil || !validProfileRoot(root, rootKey, profile.ClusterID) {
		return result, fmt.Errorf("invalid profile trust root")
	}
	rootPath, err := confinedProfilePath(canonical, profile.RootPEMPath, true)
	if err != nil {
		return result, err
	}
	rootPEM, err := os.ReadFile(rootPath)
	if err != nil {
		return result, err
	}
	rootBlock, rest := pem.Decode(rootPEM)
	if rootBlock == nil || rootBlock.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 || !bytes.Equal(rootBlock.Bytes, root.Raw) {
		return result, fmt.Errorf("local trust root does not match profile")
	}
	certificatePath, err := confinedProfilePath(canonical, profile.CertificatePEMPath, true)
	if err != nil {
		return result, err
	}
	certificatePEM, err := os.ReadFile(certificatePath)
	if err != nil {
		return result, err
	}
	certificateBlock, rest := pem.Decode(certificatePEM)
	if certificateBlock == nil || certificateBlock.Type != "CERTIFICATE" || len(bytes.TrimSpace(rest)) != 0 {
		return result, fmt.Errorf("invalid local client certificate")
	}
	certificate, err := x509.ParseCertificate(certificateBlock.Bytes)
	if err != nil {
		return result, fmt.Errorf("invalid local client certificate")
	}
	serial, serialErr := certificateSerialCompat(certificate.Raw)
	if serialErr != nil || serial != profile.Serial || certificate.Subject.CommonName != profile.NodeID ||
		certificate.NotAfter.UnixMilli() != profile.ExpiresAt || time.Now().Before(certificate.NotBefore) ||
		time.Now().After(certificate.NotAfter) || !validLocalClientCertificate(certificate, root) {
		return result, fmt.Errorf("local client certificate does not match profile")
	}
	keyPath, err := confinedProfilePath(canonical, profile.PrivateKeyPEMPath, true)
	if err != nil {
		return result, err
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return result, err
	}
	keyPEM, err = unprotectProfileRootKey(keyPEM)
	if err != nil {
		return result, fmt.Errorf("read local client private key: %w", err)
	}
	key, err := parseProfileRootKey(keyPEM)
	if err != nil {
		return result, fmt.Errorf("invalid local client private key")
	}
	publicKey, ok := certificate.PublicKey.(*ecdsa.PublicKey)
	if !ok || publicKey.X.Cmp(key.X) != 0 || publicKey.Y.Cmp(key.Y) != 0 {
		return result, fmt.Errorf("local client certificate and key do not match")
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)
	profile.URL = serverURL.String()
	result = LocalClientCredentials{
		Profile: profile,
		Certificate: tls.Certificate{
			Certificate: [][]byte{certificate.Raw}, PrivateKey: key, Leaf: certificate,
		},
		RootCAs: roots,
	}
	return result, nil
}

func localServerURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Path != "" && u.Path != "/" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return nil, fmt.Errorf("local server URL must be an HTTPS loopback origin")
	}
	ip := net.ParseIP(u.Hostname())
	port := u.Port()
	number, portErr := strconv.Atoi(port)
	if ip == nil || !ip.IsLoopback() || portErr != nil || number < 1 || number > 65535 {
		return nil, fmt.Errorf("local server URL must use a loopback IP and explicit port")
	}
	return u, nil
}
