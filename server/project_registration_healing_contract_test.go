package server

import (
	"encoding/json"
	"net/http"
	"testing"
)

func TestExternalAdhocRegistrationHealsRepositoryWithoutChannelPlugin(t *testing.T) {
	_, handler := taskAPI(t)
	register := func(body string) projectRegistration {
		t.Helper()
		response := taskRequest(handler, http.MethodPost, "/v1/projects", body, nil)
		var result projectRegistration
		if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil {
			t.Fatalf("registration: %d %s", response.Code, response.Body.String())
		}
		return result
	}
	// The source fixture defaults to an unconfigured external channel. The Go
	// server defaults to internal, so select that external channel explicitly.
	adhoc := register(`{"ident":"/home/someone/tmp","channel":"missing-provider"}`)
	if adhoc.Ident != "tmp" || adhoc.Channel != "missing-provider" || adhoc.Kind != "adhoc" || adhoc.Room != "" || adhoc.RepoFullName != nil {
		t.Fatalf("adhoc registration did not stay lazy: %+v", adhoc)
	}
	healed := register(`{"ident":"tmp","repo_url":"git@github.com:nitecon/tmp.git"}`)
	if healed.Ident != "tmp" || healed.Channel != "missing-provider" || healed.Kind != "repo" || healed.Room != "" || healed.RepoFullName == nil || *healed.RepoFullName != "nitecon/tmp" {
		t.Fatalf("repository healing changed room or required a plugin: %+v", healed)
	}
}
