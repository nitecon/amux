package storage

import (
	"context"
	"crypto/ecdsa"
	"crypto/x509"
	"database/sql"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// RetireCredentials consumes one immutable snapshot of counts for verified TLS
// peer serials. A zero count means the previous credential has drained. The
// caller owns connection tracking and supplies the shared tick time.
func (s *SecurityEnrollment) RetireCredentials(ctx context.Context, verifiedActiveSerialCounts map[string]int, now time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return err
	}
	active := make(map[string]int, len(verifiedActiveSerialCounts))
	for serial, count := range verifiedActiveSerialCounts {
		active[serial] = count
	}
	inviteExpired := s.active != nil && !now.Before(s.active.deadline)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if inviteExpired {
		if _, err = tx.ExecContext(ctx, "UPDATE invitations SET revoked=1"); err != nil {
			return err
		}
	}
	rows, err := tx.QueryContext(ctx, "SELECT id,previous_serial,previous_until FROM nodes WHERE previous_serial IS NOT NULL")
	if err != nil {
		return err
	}
	type previousCredential struct {
		nodeID string
		serial string
		until  sql.NullInt64
	}
	var previous []previousCredential
	for rows.Next() {
		var item previousCredential
		if err = rows.Scan(&item.nodeID, &item.serial, &item.until); err != nil {
			rows.Close()
			return err
		}
		previous = append(previous, item)
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	if err = rows.Close(); err != nil {
		return err
	}
	for _, item := range previous {
		if item.until.Valid && item.until.Int64 > now.UnixMilli() && active[item.serial] > 0 {
			continue
		}
		if _, err = tx.ExecContext(ctx, "UPDATE nodes SET previous_serial=NULL,previous_until=NULL WHERE id=? AND previous_serial=?", item.nodeID, item.serial); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	if inviteExpired {
		s.active = nil
	}
	return s.retireLocalPreviousAt(now)
}

func (s *SecurityEnrollment) retireLocalPreviousAt(now time.Time) error {
	manifest := filepath.Join(s.profile, "local-previous.json")
	exists, err := regularProfileFile(manifest)
	if err != nil || !exists {
		return err
	}
	previous, err := s.loadLocalPrevious()
	if err != nil {
		return err
	}
	if now.UnixMilli() < previous.Until {
		return nil
	}
	localManifest := filepath.Join(s.profile, "local-client.json")
	var diskProfile LocalSecurityProfile
	data, err := os.ReadFile(localManifest)
	if err != nil {
		return fmt.Errorf("local profile required to retire previous identity: %w", err)
	}
	if err = json.Unmarshal(data, &diskProfile); err != nil {
		return err
	}
	current, _, _, err := s.loadLocalProfile(localManifest, diskProfile.InstanceID)
	if err != nil {
		return err
	}
	if previous.Serial == current.Serial {
		return fmt.Errorf("previous local identity matches current credential")
	}
	previousCertificatePath, err := confinedProfilePath(s.profile, previous.Certificate, false)
	if err != nil {
		return err
	}
	previousKeyPath, err := confinedProfilePath(s.profile, previous.PrivateKey, false)
	if err != nil {
		return err
	}
	currentCertificatePath, err := confinedProfilePath(s.profile, current.CertificatePEMPath, true)
	if err != nil {
		return err
	}
	currentKeyPath, err := confinedProfilePath(s.profile, current.PrivateKeyPEMPath, true)
	if err != nil {
		return err
	}
	if previousCertificatePath == currentCertificatePath || previousKeyPath == currentKeyPath {
		return fmt.Errorf("previous local identity paths overlap current credential")
	}
	certificateExists, err := regularProfileFile(previousCertificatePath)
	if err != nil {
		return err
	}
	keyExists, err := regularProfileFile(previousKeyPath)
	if err != nil {
		return err
	}
	if certificateExists {
		certificatePEM, readErr := os.ReadFile(previousCertificatePath)
		if readErr != nil {
			return readErr
		}
		block, _ := pem.Decode(certificatePEM)
		if block == nil || block.Type != "CERTIFICATE" {
			return fmt.Errorf("invalid previous local certificate")
		}
		certificate, parseErr := x509.ParseCertificate(block.Bytes)
		if parseErr != nil {
			return parseErr
		}
		serial, serialErr := certificateSerialCompat(certificate.Raw)
		if serialErr != nil || serial != previous.Serial || certificate.Subject.CommonName != current.NodeID || !validLocalClientCertificate(certificate, s.root) || previous.Until > certificate.NotAfter.UnixMilli() {
			return fmt.Errorf("previous local certificate does not match retirement manifest")
		}
		if keyExists {
			keyPEM, readErr := os.ReadFile(previousKeyPath)
			if readErr != nil {
				return readErr
			}
			keyPEM, readErr = unprotectProfileRootKey(keyPEM)
			if readErr != nil {
				return readErr
			}
			key, keyErr := parseProfileRootKey(keyPEM)
			if keyErr != nil {
				return keyErr
			}
			publicKey, ok := certificate.PublicKey.(*ecdsa.PublicKey)
			if !ok || publicKey.X.Cmp(key.X) != 0 || publicKey.Y.Cmp(key.Y) != 0 {
				return fmt.Errorf("previous local certificate and key do not match")
			}
		}
	} else if keyExists {
		return fmt.Errorf("previous local private key has no certificate to validate")
	}
	// Delete the key first. If interrupted, the remaining certificate still
	// validates ownership on retry; if both are gone, retry can remove the manifest.
	if keyExists {
		if err := os.Remove(previousKeyPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if certificateExists {
		if err := os.Remove(previousCertificatePath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Remove(manifest); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
