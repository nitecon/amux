package server

import (
	"net/http"
	"strings"
	"testing"
)

func TestPatternListRejectsInvalidLifecycleFilters(t *testing.T) {
	_, handler := newArtifactWriteHTTPFixture(t, false)
	for _, query := range []string{"version=", "version=v1", "state=", "state=closed"} {
		response := artifactWriteRequest(handler, http.MethodGet, "/v1/patterns?"+query, nil, nil)
		field, _, _ := strings.Cut(query, "=")
		if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), field) {
			t.Errorf("pattern query %q: status=%d body=%s", query, response.Code, response.Body.String())
		}
	}
}
