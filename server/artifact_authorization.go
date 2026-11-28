package server

import (
	"net/http"
	"strings"

	"github.com/nitecon/amux/auth/authz"
)

// authorizeArtifact applies the gateway policy and writes the HTTP denial.
func (a *api) authorizeArtifact(w http.ResponseWriter, r *http.Request, required ...string) (authz.ArtifactDecision, bool) {
	decision, err := authz.AuthorizeArtifact(authz.ArtifactAccess{
		Enforced: a.artifactAuthEnforced, Project: r.PathValue("ident"),
		ClaimedProject: r.Header.Get("X-Agent-Project"), ClaimedScopes: r.Header.Get("X-Agent-Scopes"),
	}, required...)
	if err != nil {
		writeError(w, http.StatusForbidden, err.Error())
		return authz.ArtifactDecision{}, false
	}
	return decision, true
}

// authorizeArtifactMutation applies the operation scopes and the optional
// project administration gate for an explicit quota override. The override
// never changes storage quota enforcement or returned operation scopes.
func (a *api) authorizeArtifactMutation(w http.ResponseWriter, r *http.Request, required ...string) (authz.ArtifactDecision, bool) {
	decision, ok := a.authorizeArtifact(w, r, required...)
	if !ok || !artifactQuotaOverrideRequested(r) {
		return decision, ok
	}
	if _, ok := a.authorizeArtifact(w, r, "project.administer"); !ok {
		return authz.ArtifactDecision{}, false
	}
	return decision, true
}

func artifactQuotaOverrideRequested(r *http.Request) bool {
	value := strings.TrimSpace(r.Header.Get("X-Artifact-Quota-Override"))
	return strings.EqualFold(value, "1") || strings.EqualFold(value, "true") || strings.EqualFold(value, "yes")
}
