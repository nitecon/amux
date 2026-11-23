package server

import (
	"net/http"
	"strings"
	"unicode"

	"github.com/nitecon/amux/projects"
)

type artifactAuthorization struct {
	Boundary       string   `json:"boundary"`
	RequiredScopes []string `json:"required_scopes"`
}

// authorizeArtifact runs after gateway authentication. Scoped deployments
// provide artifact project and scope claims through the trusted gateway client.
func (a *api) authorizeArtifact(w http.ResponseWriter, r *http.Request, required ...string) (artifactAuthorization, bool) {
	decision := artifactAuthorization{Boundary: "trusted-single-tenant", RequiredScopes: required}
	if !a.artifactAuthEnforced {
		return decision, true
	}
	project := strings.TrimSpace(r.Header.Get("X-Agent-Project"))
	if project == "" || projects.NormalizeIdent(project) != projects.NormalizeIdent(r.PathValue("ident")) {
		writeError(w, http.StatusForbidden, "artifact_authorization_forbidden: missing or mismatched x-agent-project")
		return artifactAuthorization{}, false
	}
	held := make(map[string]bool)
	for _, scope := range strings.FieldsFunc(r.Header.Get("X-Agent-Scopes"), func(r rune) bool {
		return r == ',' || unicode.IsSpace(r)
	}) {
		held[scope] = true
	}
	var missing []string
	for _, scope := range required {
		prefix, _, _ := strings.Cut(scope, ".")
		if !held["*"] && !held[scope] && !held[prefix+".*"] {
			missing = append(missing, scope)
		}
	}
	if len(missing) != 0 {
		writeError(w, http.StatusForbidden, "artifact_authorization_forbidden: missing scopes "+strings.Join(missing, ","))
		return artifactAuthorization{}, false
	}
	decision.Boundary = "project-scoped"
	return decision, true
}

// authorizeArtifactMutation applies the operation scopes and the optional
// project administration gate for an explicit quota override. The override
// never changes storage quota enforcement or returned operation scopes.
func (a *api) authorizeArtifactMutation(w http.ResponseWriter, r *http.Request, required ...string) (artifactAuthorization, bool) {
	decision, ok := a.authorizeArtifact(w, r, required...)
	if !ok || !artifactQuotaOverrideRequested(r) {
		return decision, ok
	}
	if _, ok := a.authorizeArtifact(w, r, "project.administer"); !ok {
		return artifactAuthorization{}, false
	}
	return decision, true
}

func artifactQuotaOverrideRequested(r *http.Request) bool {
	value := strings.TrimSpace(r.Header.Get("X-Artifact-Quota-Override"))
	return strings.EqualFold(value, "1") || strings.EqualFold(value, "true") || strings.EqualFold(value, "yes")
}
