// Package gatewayorigin parses credential-free HTTP(S) gateway origins.
package gatewayorigin

import (
	"fmt"
	"net/url"
)

// Parse validates the origin properties shared by gateway clients and stored
// worker-pool server addresses. Callers retain their own path and host rules.
func Parse(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, fmt.Errorf("invalid HTTP(S) gateway origin")
	}
	return u, nil
}
