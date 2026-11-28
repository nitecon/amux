//go:build desktop

package desktopapp

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestEndpointIsThisHost(t *testing.T) {
	ctx := context.Background()
	name, err := os.Hostname()
	if err != nil {
		t.Skip("hostname unavailable")
	}
	for _, endpoint := range []string{"https://127.0.0.1:7913", "https://localhost:7913", "https://[::1]:7913", "https://" + name + ":7913"} {
		if !endpointIsThisHost(ctx, endpoint) {
			t.Fatalf("%s should be this host", endpoint)
		}
	}
	label := strings.SplitN(name, ".", 2)[0]
	for _, endpoint := range []string{"", "https://192.0.2.10:7913", "not a url", "https://" + label + ".remote.invalid:7913"} {
		if endpointIsThisHost(ctx, endpoint) {
			t.Fatalf("%q should not be this host", endpoint)
		}
	}
}
