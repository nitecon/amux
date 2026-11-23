package storage

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/pem"
	"fmt"
	"math/big"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

// EnsureSecurityInstallation creates or reuses the Rust-compatible installation
// identity in profile/security.sqlite. Callers must hold the profile owner lock.
func EnsureSecurityInstallation(ctx context.Context, profile string) (string, error) {
	if strings.TrimSpace(profile) == "" {
		return "", fmt.Errorf("security profile path is required")
	}
	profile, err := filepath.Abs(profile)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(profile, 0700); err != nil {
		return "", err
	}
	databasePath := filepath.Join(profile, "security.sqlite")
	info, err := os.Lstat(databasePath)
	if err == nil && !info.Mode().IsRegular() {
		return "", fmt.Errorf("security database must be a regular file")
	}
	if err != nil && !os.IsNotExist(err) {
		return "", err
	}
	if os.IsNotExist(err) {
		file, err := os.OpenFile(databasePath, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return "", err
		}
		if err := file.Close(); err != nil {
			return "", err
		}
	}
	filePath := filepath.ToSlash(databasePath)
	if !strings.HasPrefix(filePath, "/") {
		filePath = "/" + filePath
	}
	dsn := (&url.URL{Scheme: "file", Path: filePath, RawQuery: "_pragma=foreign_keys%281%29&_pragma=journal_mode%28DELETE%29&_txlock=immediate"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return "", err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS installation(
		id INTEGER PRIMARY KEY CHECK(id=1),
		cluster_id TEXT NOT NULL
	);`); err != nil {
		return "", fmt.Errorf("initialize security installation schema: %w", err)
	}
	id, err := uuid.NewV7()
	if err != nil {
		return "", fmt.Errorf("create installation identity: %w", err)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO installation(id,cluster_id) VALUES(1,?)", id.String()); err != nil {
		return "", err
	}
	var clusterID string
	if err := tx.QueryRowContext(ctx, "SELECT cluster_id FROM installation WHERE id=1").Scan(&clusterID); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return clusterID, nil
}

// EnsureSecurityRoot preserves a valid Rust-compatible CA pair or creates it
// when both files are absent. The caller must hold the security profile lock.
func EnsureSecurityRoot(profile, clusterID string) error {
	if strings.TrimSpace(profile) == "" || clusterID == "" {
		return fmt.Errorf("security profile and cluster identity are required")
	}
	certificatePath := filepath.Join(profile, "root.pem")
	keyPath := filepath.Join(profile, "root-key.pem")
	certificateExists, err := regularProfileFile(certificatePath)
	if err != nil {
		return err
	}
	keyExists, err := regularProfileFile(keyPath)
	if err != nil {
		return err
	}
	if certificateExists != keyExists {
		return fmt.Errorf("incomplete installation root; refusing replacement")
	}
	if !certificateExists {
		certificatePEM, keyPEM, err := generateProfileRoot(clusterID)
		if err != nil {
			return fmt.Errorf("generate installation root: %w", err)
		}
		protectedKey, err := protectProfileRootKey(keyPEM)
		if err != nil {
			return fmt.Errorf("protect installation root key: %w", err)
		}
		if err := writePrivateExclusive(keyPath, protectedKey); err != nil {
			return fmt.Errorf("write installation root key: %w", err)
		}
		if err := writePrivateExclusive(certificatePath, certificatePEM); err != nil {
			return fmt.Errorf("write installation root certificate: %w", err)
		}
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return err
	}
	keyPEM, err = unprotectProfileRootKey(keyPEM)
	if err != nil {
		return fmt.Errorf("read installation root key: %w", err)
	}
	privateKey, err := parseProfileRootKey(keyPEM)
	if err != nil {
		return fmt.Errorf("invalid installation root key")
	}
	certificatePEM, err := os.ReadFile(certificatePath)
	if err != nil {
		return err
	}
	certificateBlock, _ := pem.Decode(certificatePEM)
	if certificateBlock == nil || certificateBlock.Type != "CERTIFICATE" {
		return fmt.Errorf("invalid installation root certificate")
	}
	certificate, err := x509.ParseCertificate(certificateBlock.Bytes)
	if err != nil || !validProfileRoot(certificate, privateKey, clusterID) {
		return fmt.Errorf("installation root certificate and key do not match this profile")
	}
	if err := os.Chmod(keyPath, 0600); err != nil {
		return fmt.Errorf("secure installation root key permissions: %w", err)
	}
	if err := os.Chmod(certificatePath, 0600); err != nil {
		return fmt.Errorf("secure installation root certificate permissions: %w", err)
	}
	return nil
}

func regularProfileFile(path string) (bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if !info.Mode().IsRegular() {
		return false, fmt.Errorf("profile credential must be a regular file")
	}
	return true, nil
}

func generateProfileRoot(clusterID string) ([]byte, []byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, nil, err
	}
	if serial.Sign() == 0 {
		serial.SetInt64(1)
	}
	publicKeyDER, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		return nil, nil, err
	}
	keyID := sha256.Sum256(publicKeyDER)
	root := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "amux " + clusterID},
		NotBefore:             time.Date(1975, 1, 1, 0, 0, 0, 0, time.UTC),
		NotAfter:              time.Date(4096, 1, 1, 0, 0, 0, 0, time.UTC),
		BasicConstraintsValid: true,
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		SubjectKeyId:          append([]byte(nil), keyID[:20]...),
		SignatureAlgorithm:    x509.ECDSAWithSHA256,
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, root, root, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER}),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER}), nil
}

func parseProfileRootKey(data []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(data)
	if block == nil {
		return nil, fmt.Errorf("private key PEM is absent")
	}
	var key any
	var err error
	if block.Type == "PRIVATE KEY" {
		key, err = x509.ParsePKCS8PrivateKey(block.Bytes)
	} else if block.Type == "EC PRIVATE KEY" {
		key, err = x509.ParseECPrivateKey(block.Bytes)
	} else {
		return nil, fmt.Errorf("unsupported private key format")
	}
	if err != nil {
		return nil, err
	}
	ecdsaKey, ok := key.(*ecdsa.PrivateKey)
	if !ok || ecdsaKey.Curve.Params().Name != elliptic.P256().Params().Name {
		return nil, fmt.Errorf("root key must be ECDSA P-256")
	}
	return ecdsaKey, nil
}

func validProfileRoot(certificate *x509.Certificate, key *ecdsa.PrivateKey, clusterID string) bool {
	publicKey, ok := certificate.PublicKey.(*ecdsa.PublicKey)
	if !ok || publicKey.Curve.Params().Name != elliptic.P256().Params().Name ||
		publicKey.X.Cmp(key.X) != 0 || publicKey.Y.Cmp(key.Y) != 0 {
		return false
	}
	requiredUsage := x509.KeyUsageCertSign | x509.KeyUsageCRLSign
	if !certificate.IsCA || !certificate.BasicConstraintsValid || len(certificate.SubjectKeyId) == 0 ||
		certificate.KeyUsage&requiredUsage != requiredUsage ||
		certificate.Subject.CommonName != "amux "+clusterID ||
		!bytes.Equal(certificate.RawSubject, certificate.RawIssuer) {
		return false
	}
	return certificate.CheckSignatureFrom(certificate) == nil
}

func writePrivateExclusive(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	if err := file.Chmod(0600); err != nil {
		file.Close()
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err := file.Sync(); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}
