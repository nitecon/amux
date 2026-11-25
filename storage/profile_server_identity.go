package storage

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ProfileServerTLSNames adds local names to the configured server SANs.
func ProfileServerTLSNames(ctx context.Context, configured []string) []string {
	names := make([]string, 0, len(configured)+4)
	add := func(name string) {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	for _, name := range configured {
		add(name)
	}
	for _, name := range []string{"localhost", "127.0.0.1", "::1"} {
		add(name)
	}

	lookupCtx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	resolved, _ := exec.CommandContext(lookupCtx, "hostname", "-f").Output()
	hostname, _ := os.Hostname()
	for _, candidate := range []string{string(resolved), hostname} {
		name := strings.TrimSuffix(strings.TrimSpace(candidate), ".")
		if validateProfileTLSNames([]string{name}) == nil && !strings.Contains(name, "*") {
			add(name)
			break
		}
	}
	return names
}

const (
	profileServerLeafLifetime = 30 * 24 * time.Hour
	profileServerRenewBefore  = profileServerLeafLifetime / 3
	profileServerOverlap      = 7 * 24 * time.Hour
)

type profileServerIdentity struct {
	Serial      string                         `json:"serial"`
	Certificate string                         `json:"certificate"`
	PrivateKey  string                         `json:"private_key"`
	ExpiresAt   int64                          `json:"expires_at"`
	Names       []string                       `json:"names"`
	Previous    *profilePreviousServerIdentity `json:"previous"`
}

type profilePreviousServerIdentity struct {
	Serial      string `json:"serial"`
	Certificate string `json:"certificate"`
	PrivateKey  string `json:"private_key"`
	Until       int64  `json:"until"`
}

// ProfileServerLeaseSnapshot is one coordinator snapshot for server leaf
// connections and successful handshake observations, keyed by Rust-compatible
// certificate serial. The caller must include only verified handshakes.
type ProfileServerLeaseSnapshot struct {
	ActiveSerialCounts map[string]int
	ObservedSerials    map[string]bool
}

var ErrProfileServerRotationBlocked = errors.New("profile server rotation is waiting for previous generation retirement")

// EnsureProfileServerTLS returns the profile CA-signed server leaf for names.
// It preserves startup behavior: the held profile lock means the prior server
// process and its connections have drained before a previous leaf is retired.
func EnsureProfileServerTLS(profile, clusterID string, names []string) (tls.Certificate, error) {
	return ensureProfileServerTLSAt(profile, clusterID, names, time.Now().UTC())
}

// EnsureProfileServerTLSLive manages a live server generation using a snapshot
// of active verified TLS serials and successful-handshake observations. A
// It must run under the held profile owner lock. The previous leaf is retired
// only after the current leaf has been observed and all previous-leaf leases
// drain, or its overlap deadline expires. If that
// prevents a requested rotation, the current certificate is returned with
// ErrProfileServerRotationBlocked so the listener can keep serving it.
func EnsureProfileServerTLSLive(profile, clusterID string, names []string, leases ProfileServerLeaseSnapshot, now time.Time) (tls.Certificate, error) {
	active := make(map[string]int, len(leases.ActiveSerialCounts))
	for serial, count := range leases.ActiveSerialCounts {
		active[serial] = count
	}
	observed := make(map[string]bool, len(leases.ObservedSerials))
	for serial, seen := range leases.ObservedSerials {
		observed[serial] = seen
	}
	return ensureProfileServerTLSAtWithLeases(profile, clusterID, names, now.UTC(), &ProfileServerLeaseSnapshot{ActiveSerialCounts: active, ObservedSerials: observed})
}

func ensureProfileServerTLSAt(profile, clusterID string, names []string, now time.Time) (tls.Certificate, error) {
	return ensureProfileServerTLSAtWithLeases(profile, clusterID, names, now, nil)
}

func ensureProfileServerTLSAtWithLeases(profile, clusterID string, names []string, now time.Time, leases *ProfileServerLeaseSnapshot) (tls.Certificate, error) {
	var empty tls.Certificate
	if strings.TrimSpace(profile) == "" || clusterID == "" {
		return empty, fmt.Errorf("security profile and cluster identity are required")
	}
	canonicalProfile, err := filepath.Abs(profile)
	if err != nil {
		return empty, err
	}
	canonicalProfile, err = filepath.EvalSymlinks(canonicalProfile)
	if err != nil {
		return empty, err
	}
	if err := validateProfileTLSNames(names); err != nil {
		return empty, err
	}
	if err := EnsureSecurityRoot(canonicalProfile, clusterID); err != nil {
		return empty, err
	}
	root, rootKey, err := loadProfileRoot(canonicalProfile)
	if err != nil {
		return empty, err
	}
	manifestPath := filepath.Join(canonicalProfile, "server-identity.json")
	exists, err := regularProfileFile(manifestPath)
	if err != nil {
		return empty, err
	}
	var old *profileServerIdentity
	var current tls.Certificate
	if exists {
		data, err := os.ReadFile(manifestPath)
		if err != nil {
			return empty, err
		}
		var identity profileServerIdentity
		if err := json.Unmarshal(data, &identity); err != nil {
			return empty, fmt.Errorf("invalid server identity manifest")
		}
		current, err = validateProfileServerIdentity(canonicalProfile, identity, root, true)
		if err != nil {
			return empty, fmt.Errorf("invalid server identity manifest: %w", err)
		}
		if err := os.Chmod(manifestPath, 0600); err != nil {
			return empty, fmt.Errorf("secure server identity manifest permissions: %w", err)
		}
		if identity.Previous != nil {
			if err := validatePreviousProfileIdentity(canonicalProfile, identity, *identity.Previous, root); err != nil {
				return empty, fmt.Errorf("invalid previous server identity manifest: %w", err)
			}
		}
		old = &identity
		needsRotation := identity.ExpiresAt-now.UnixMilli() <= profileServerRenewBefore.Milliseconds() || !sameProfileNameSet(identity.Names, names)
		if leases != nil && identity.Previous != nil {
			if canRetireProfileServerPrevious(identity, *leases, now) {
				retired := identity.Previous
				identity.Previous = nil
				manifest, err := json.MarshalIndent(identity, "", "  ")
				if err != nil {
					return current, err
				}
				if err := publishProfileServerIdentity(manifestPath, canonicalProfile, append(manifest, '\n'), retired); err != nil {
					return current, fmt.Errorf("retire previous server identity: %w", err)
				}
				old = &identity
			} else if needsRotation {
				return current, ErrProfileServerRotationBlocked
			}
		}
		if !needsRotation {
			return current, nil
		}
	}

	leaf, certificatePath, keyPath, expires, err := createProfileServerLeaf(canonicalProfile, names, root, rootKey, now)
	if err != nil {
		return empty, err
	}
	serial, err := certificateSerialCompat(leaf.Certificate[0])
	if err != nil {
		return empty, err
	}
	identity := profileServerIdentity{
		Serial:      serial,
		Certificate: certificatePath,
		PrivateKey:  keyPath,
		ExpiresAt:   expires,
		Names:       append([]string(nil), names...),
	}
	var retired *profilePreviousServerIdentity
	if old != nil {
		retired = old.Previous
		identity.Previous = &profilePreviousServerIdentity{
			Serial:      old.Serial,
			Certificate: old.Certificate,
			PrivateKey:  old.PrivateKey,
			Until:       min(now.Add(profileServerOverlap).UnixMilli(), old.ExpiresAt),
		}
	}
	manifest, err := json.MarshalIndent(identity, "", "  ")
	if err != nil {
		return empty, err
	}
	if err := publishProfileServerIdentity(manifestPath, canonicalProfile, append(manifest, '\n'), retired); err != nil {
		return empty, fmt.Errorf("publish server identity manifest: %w", err)
	}
	return leaf, nil
}

func publishProfileServerIdentity(manifestPath, profile string, manifest []byte, retired *profilePreviousServerIdentity) error {
	if err := replacePrivateFile(manifestPath, manifest); err != nil {
		return err
	}
	if retired != nil {
		return removePreviousProfileIdentity(profile, *retired)
	}
	return nil
}

func canRetireProfileServerPrevious(current profileServerIdentity, leases ProfileServerLeaseSnapshot, now time.Time) bool {
	previous := current.Previous
	if previous == nil {
		return true
	}
	return now.UnixMilli() >= previous.Until ||
		(leases.ObservedSerials[current.Serial] && leases.ActiveSerialCounts[previous.Serial] <= 0)
}

func createProfileServerLeaf(profile string, names []string, root *x509.Certificate, rootKey *ecdsa.PrivateKey, now time.Time) (tls.Certificate, string, string, int64, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, "", "", 0, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return tls.Certificate{}, "", "", 0, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, "", "", 0, err
	}
	if serial.Sign() == 0 {
		serial.SetInt64(1)
	}
	expires := now.UnixMilli()/1000*1000 + profileServerLeafLifetime.Milliseconds()
	leafTemplate := &x509.Certificate{
		SerialNumber:       serial,
		Subject:            pkix.Name{CommonName: "rcgen self signed cert"},
		NotBefore:          time.UnixMilli(now.UnixMilli() - 300_000).UTC(),
		NotAfter:           time.UnixMilli(expires).UTC(),
		KeyUsage:           x509.KeyUsageDigitalSignature,
		ExtKeyUsage:        []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		SignatureAlgorithm: x509.ECDSAWithSHA256,
		AuthorityKeyId:     append([]byte(nil), root.SubjectKeyId...),
	}
	for _, name := range names {
		if ip := net.ParseIP(name); ip != nil {
			leafTemplate.IPAddresses = append(leafTemplate.IPAddresses, ip)
		} else {
			leafTemplate.DNSNames = append(leafTemplate.DNSNames, name)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, leafTemplate, root, &key.PublicKey, rootKey)
	if err != nil {
		return tls.Certificate{}, "", "", 0, err
	}
	certificatePEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	keyPEM, err = protectProfileRootKey(keyPEM)
	if err != nil {
		return tls.Certificate{}, "", "", 0, err
	}
	generation, err := uuid.NewV7()
	if err != nil {
		return tls.Certificate{}, "", "", 0, err
	}
	certificatePath := filepath.Join(profile, "server-"+generation.String()+".pem")
	keyPath := filepath.Join(profile, "server-"+generation.String()+"-key.pem")
	if err := writePrivateExclusive(keyPath, keyPEM); err != nil {
		return tls.Certificate{}, "", "", 0, err
	}
	if err := writePrivateExclusive(certificatePath, certificatePEM); err != nil {
		return tls.Certificate{}, "", "", 0, err
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, "", "", 0, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: parsed}, certificatePath, keyPath, expires, nil
}

func loadProfileRoot(profile string) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	certificatePEM, err := os.ReadFile(filepath.Join(profile, "root.pem"))
	if err != nil {
		return nil, nil, err
	}
	block, _ := pem.Decode(certificatePEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, nil, fmt.Errorf("invalid profile root certificate")
	}
	certificate, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid profile root certificate")
	}
	keyPEM, err := os.ReadFile(filepath.Join(profile, "root-key.pem"))
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err = unprotectProfileRootKey(keyPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("read profile root key: %w", err)
	}
	key, err := parseProfileRootKey(keyPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid profile root key")
	}
	publicKey, ok := certificate.PublicKey.(*ecdsa.PublicKey)
	if !ok || publicKey.X.Cmp(key.X) != 0 || publicKey.Y.Cmp(key.Y) != 0 ||
		!certificate.IsCA || !certificate.BasicConstraintsValid || certificate.SubjectKeyId == nil ||
		!bytes.Equal(certificate.RawSubject, certificate.RawIssuer) || certificate.CheckSignatureFrom(certificate) != nil {
		return nil, nil, fmt.Errorf("profile root certificate and key do not match")
	}
	return certificate, key, nil
}

func validateProfileServerIdentity(profile string, identity profileServerIdentity, root *x509.Certificate, requireCurrent bool) (tls.Certificate, error) {
	certificatePath, err := confinedProfilePath(profile, identity.Certificate, requireCurrent)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPath, err := confinedProfilePath(profile, identity.PrivateKey, requireCurrent)
	if err != nil {
		return tls.Certificate{}, err
	}
	certificatePEM, err := os.ReadFile(certificatePath)
	if err != nil {
		return tls.Certificate{}, err
	}
	certificateBlock, _ := pem.Decode(certificatePEM)
	if certificateBlock == nil || certificateBlock.Type != "CERTIFICATE" {
		return tls.Certificate{}, fmt.Errorf("invalid leaf certificate")
	}
	certificate, err := x509.ParseCertificate(certificateBlock.Bytes)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("invalid leaf certificate")
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM, err = unprotectProfileRootKey(keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("read leaf private key: %w", err)
	}
	key, err := parseProfileRootKey(keyPEM)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("invalid leaf private key")
	}
	publicKey, ok := certificate.PublicKey.(*ecdsa.PublicKey)
	serial, serialErr := certificateSerialCompat(certificate.Raw)
	if !ok || publicKey.X.Cmp(key.X) != 0 || publicKey.Y.Cmp(key.Y) != 0 ||
		certificate.IsCA || certificate.BasicConstraintsValid || certificate.KeyUsage&x509.KeyUsageDigitalSignature == 0 ||
		!containsExtKeyUsage(certificate.ExtKeyUsage, x509.ExtKeyUsageServerAuth) ||
		!bytes.Equal(certificate.RawIssuer, root.RawSubject) ||
		!bytes.Equal(certificate.AuthorityKeyId, root.SubjectKeyId) || certificate.CheckSignatureFrom(root) != nil ||
		serialErr != nil || identity.Serial != serial || identity.ExpiresAt != certificate.NotAfter.UnixMilli() ||
		!sameCertificateNameSet(certificateNames(certificate), identity.Names) {
		return tls.Certificate{}, fmt.Errorf("leaf certificate does not match manifest and profile")
	}
	if err := os.Chmod(keyPath, 0600); err != nil {
		return tls.Certificate{}, fmt.Errorf("secure leaf key permissions: %w", err)
	}
	if err := os.Chmod(certificatePath, 0600); err != nil {
		return tls.Certificate{}, fmt.Errorf("secure leaf certificate permissions: %w", err)
	}
	return tls.Certificate{Certificate: [][]byte{certificate.Raw}, PrivateKey: key, Leaf: certificate}, nil
}

func validatePreviousProfileIdentity(profile string, current profileServerIdentity, previous profilePreviousServerIdentity, root *x509.Certificate) error {
	certificatePath, err := confinedProfilePath(profile, previous.Certificate, false)
	if err != nil {
		return err
	}
	keyPath, err := confinedProfilePath(profile, previous.PrivateKey, false)
	if err != nil {
		return err
	}
	currentCertificatePath, err := confinedProfilePath(profile, current.Certificate, true)
	if err != nil {
		return err
	}
	currentKeyPath, err := confinedProfilePath(profile, current.PrivateKey, true)
	if err != nil {
		return err
	}
	if certificatePath == currentCertificatePath || keyPath == currentKeyPath {
		return fmt.Errorf("previous generation aliases current files")
	}
	certExists, err := regularProfileFile(certificatePath)
	if err != nil {
		return err
	}
	keyExists, err := regularProfileFile(keyPath)
	if err != nil {
		return err
	}
	if certExists != keyExists {
		return fmt.Errorf("previous generation is incomplete")
	}
	if !certExists {
		return nil
	}
	certificatePEM, err := os.ReadFile(certificatePath)
	if err != nil {
		return err
	}
	certificateBlock, _ := pem.Decode(certificatePEM)
	if certificateBlock == nil || certificateBlock.Type != "CERTIFICATE" {
		return fmt.Errorf("invalid previous certificate")
	}
	certificate, err := x509.ParseCertificate(certificateBlock.Bytes)
	serial, serialErr := certificateSerialCompat(certificateBlock.Bytes)
	if err != nil || serialErr != nil || serial != previous.Serial ||
		certificate.IsCA || certificate.BasicConstraintsValid || certificate.KeyUsage&x509.KeyUsageDigitalSignature == 0 ||
		!containsExtKeyUsage(certificate.ExtKeyUsage, x509.ExtKeyUsageServerAuth) ||
		!bytes.Equal(certificate.RawIssuer, root.RawSubject) ||
		!bytes.Equal(certificate.AuthorityKeyId, root.SubjectKeyId) || certificate.CheckSignatureFrom(root) != nil {
		return fmt.Errorf("invalid previous certificate")
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return err
	}
	keyPEM, err = unprotectProfileRootKey(keyPEM)
	if err != nil {
		return err
	}
	key, err := parseProfileRootKey(keyPEM)
	if err != nil {
		return fmt.Errorf("invalid previous private key")
	}
	publicKey, ok := certificate.PublicKey.(*ecdsa.PublicKey)
	if !ok || publicKey.X.Cmp(key.X) != 0 || publicKey.Y.Cmp(key.Y) != 0 {
		return fmt.Errorf("previous certificate and key do not match")
	}
	if err := os.Chmod(keyPath, 0600); err != nil {
		return fmt.Errorf("secure previous leaf key permissions: %w", err)
	}
	if err := os.Chmod(certificatePath, 0600); err != nil {
		return fmt.Errorf("secure previous leaf certificate permissions: %w", err)
	}
	return nil
}

func removePreviousProfileIdentity(profile string, previous profilePreviousServerIdentity) error {
	for _, path := range []string{previous.Certificate, previous.PrivateKey} {
		resolved, err := confinedProfilePath(profile, path, false)
		if err != nil {
			return err
		}
		if err := os.Remove(resolved); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}

func confinedProfilePath(profile, value string, required bool) (string, error) {
	if value == "" {
		return "", fmt.Errorf("server identity path is empty")
	}
	path := value
	if !filepath.IsAbs(path) {
		path = filepath.Join(profile, path)
	}
	path, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	// Compare the resolved directories so Rust's canonical paths and Go's
	// profile aliases identify the same on-disk location. Keep the leaf
	// unresolved so regularProfileFile still rejects symlink substitution.
	profileDir, err := filepath.EvalSymlinks(profile)
	if err != nil {
		return "", err
	}
	fileDir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return "", err
	}
	if relative, err := filepath.Rel(profileDir, fileDir); err != nil || relative != "." {
		return "", fmt.Errorf("server identity path escapes profile or is not directly inside it")
	}
	path = filepath.Join(fileDir, filepath.Base(path))
	exists, err := regularProfileFile(path)
	if err != nil {
		return "", err
	}
	if required && !exists {
		return "", fmt.Errorf("server identity file is missing")
	}
	return path, nil
}

func validateProfileTLSNames(names []string) error {
	if len(names) == 0 {
		return fmt.Errorf("profile TLS requires at least one DNS or IP name")
	}
	for _, name := range names {
		if name == "" || strings.TrimSpace(name) != name || strings.ContainsAny(name, "\x00/\\") {
			return fmt.Errorf("invalid profile TLS name")
		}
		if net.ParseIP(name) != nil {
			continue
		}
		candidate := strings.TrimPrefix(name, "*.")
		if candidate == name && strings.Contains(name, "*") {
			return fmt.Errorf("invalid profile TLS name")
		}
		if len(candidate) == 0 || len(candidate) > 253 || !profileDNSNamePattern.MatchString(candidate) {
			return fmt.Errorf("invalid profile TLS name")
		}
		for _, label := range strings.Split(candidate, ".") {
			if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
				return fmt.Errorf("invalid profile TLS name")
			}
		}
	}
	return nil
}

var profileDNSNamePattern = regexp.MustCompile(`^[A-Za-z0-9.-]+$`)

func sameProfileNameSet(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	set := make(map[string]int, len(left))
	for _, name := range left {
		set[name]++
	}
	for _, name := range right {
		set[name]--
		if set[name] < 0 {
			return false
		}
	}
	for _, count := range set {
		if count != 0 {
			return false
		}
	}
	return true
}

func sameCertificateNameSet(certificateNames, manifestNames []string) bool {
	normalize := func(names []string) map[string]int {
		set := make(map[string]int, len(names))
		for _, name := range names {
			if ip := net.ParseIP(name); ip != nil {
				name = ip.String()
			}
			set[name]++
		}
		return set
	}
	return sameNameCounts(normalize(certificateNames), normalize(manifestNames))
}

func sameNameCounts(left, right map[string]int) bool {
	if len(left) != len(right) {
		return false
	}
	for name, count := range left {
		if right[name] != count {
			return false
		}
	}
	return true
}

func certificateNames(certificate *x509.Certificate) []string {
	names := append([]string(nil), certificate.DNSNames...)
	for _, ip := range certificate.IPAddresses {
		names = append(names, ip.String())
	}
	return names
}

func containsExtKeyUsage(usages []x509.ExtKeyUsage, required x509.ExtKeyUsage) bool {
	for _, usage := range usages {
		if usage == required {
			return true
		}
	}
	return false
}

// certificateSerialCompat mirrors x509-parser::format_serial over the raw DER
// INTEGER contents, including a leading sign-padding byte.
// SecurityCertificateSerial returns the Rust-compatible serial encoded in signed DER.
func SecurityCertificateSerial(der []byte) (string, error) {
	return certificateSerialCompat(der)
}

func certificateSerialCompat(der []byte) (string, error) {
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		return "", err
	}
	var sequence asn1.RawValue
	if rest, err := asn1.Unmarshal(certificate.RawTBSCertificate, &sequence); err != nil || len(rest) != 0 || sequence.Class != asn1.ClassUniversal || sequence.Tag != asn1.TagSequence {
		return "", fmt.Errorf("invalid certificate TBSCertificate")
	}
	fields := sequence.Bytes
	var field asn1.RawValue
	fields, err = asn1.Unmarshal(fields, &field)
	if err != nil {
		return "", fmt.Errorf("invalid certificate version")
	}
	if field.Class == asn1.ClassContextSpecific && field.Tag == 0 {
		fields, err = asn1.Unmarshal(fields, &field)
		if err != nil {
			return "", fmt.Errorf("invalid certificate serial")
		}
	}
	if field.Class != asn1.ClassUniversal || field.Tag != asn1.TagInteger || len(field.Bytes) == 0 {
		return "", fmt.Errorf("invalid certificate serial")
	}
	var result strings.Builder
	for i, b := range field.Bytes {
		if i > 0 {
			result.WriteByte(':')
		}
		fmt.Fprintf(&result, "%02x", b)
	}
	return result.String(), nil
}

func replacePrivateFile(path string, data []byte) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".server-identity-*")
	if err != nil {
		return err
	}
	defer os.Remove(temporary.Name())
	if err := temporary.Chmod(0600); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporary.Name(), path)
}
