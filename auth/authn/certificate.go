package authn

import (
	"crypto/x509"
	"fmt"
	"time"
)

// VerifyClientCertificate checks the encoded client leaf against the profile CA.
func VerifyClientCertificate(certificate, root *x509.Certificate, now time.Time) (*x509.Certificate, error) {

	if certificate == nil || len(certificate.Raw) == 0 {
		return nil, fmt.Errorf("client certificate is required")
	}
	certificate, err := x509.ParseCertificate(certificate.Raw)
	if err != nil {
		return nil, fmt.Errorf("invalid client certificate: %w", err)
	}
	roots := x509.NewCertPool()
	roots.AddCert(root)
	if _, err := certificate.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, CurrentTime: now}); err != nil {
		return nil, fmt.Errorf("client certificate is not trusted: %w", err)
	}
	return certificate, nil
}
