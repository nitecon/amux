package authz

import "errors"

// CertificateState describes the node's authorized certificate serials.
type CertificateState struct {
	CurrentSerial  string
	PendingSerial  *string
	PreviousSerial *string
	PreviousUntil  *int64
	Revoked        bool
}

// CheckCertificateSerial applies revocation and overlap rules to a certificate.
func CheckCertificateSerial(node CertificateState, serial string, now int64) error {
	if node.Revoked {
		return errors.New("node revoked")
	}
	if node.CurrentSerial != serial && (node.PendingSerial == nil || *node.PendingSerial != serial) &&
		(node.PreviousSerial == nil || *node.PreviousSerial != serial || node.PreviousUntil == nil || *node.PreviousUntil <= now) {
		return errors.New("credential retired")
	}
	return nil
}
