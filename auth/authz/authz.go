// Package authz applies gateway authorization policy.
package authz

import (
	"errors"
	"strings"
	"unicode"

	projectid "github.com/nitecon/amux/identity/project"
)

type ArtifactDecision struct {
	Boundary       string   `json:"boundary"`
	RequiredScopes []string `json:"required_scopes"`
}

type ArtifactAccess struct {
	Enforced       bool
	Project        string
	ClaimedProject string
	ClaimedScopes  string
}

// AuthorizeArtifact evaluates the trusted gateway's project and scope claims.
func AuthorizeArtifact(access ArtifactAccess, required ...string) (ArtifactDecision, error) {
	decision := ArtifactDecision{Boundary: "trusted-single-tenant", RequiredScopes: required}
	if !access.Enforced {
		return decision, nil
	}
	project := strings.TrimSpace(access.ClaimedProject)
	if project == "" || projectid.NormalizeIdent(project) != projectid.NormalizeIdent(access.Project) {
		return ArtifactDecision{}, errors.New("artifact_authorization_forbidden: missing or mismatched x-agent-project")
	}
	held := make(map[string]bool)
	for _, scope := range strings.FieldsFunc(access.ClaimedScopes, func(r rune) bool {
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
		return ArtifactDecision{}, errors.New("artifact_authorization_forbidden: missing scopes " + strings.Join(missing, ","))
	}
	decision.Boundary = "project-scoped"
	return decision, nil
}
