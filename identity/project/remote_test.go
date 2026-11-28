package project

import (
	"strings"
	"testing"
)

// Rust: crates/gateway-backend/src/rs::parses_common_remote_forms.
func TestCommonRemoteForms(t *testing.T) {
	want := RepoRef{Provider: "github", Namespace: "nitecon", Name: "agent-gateway", Canonical: "github.com/nitecon/agent-gateway"}
	for _, raw := range []string{"github.com/nitecon/agent-gateway.git", "https://github.com/nitecon/agent-gateway.git", "https://github.com/Nitecon/Agent-Gateway", "git@github.com:nitecon/agent-gateway.git", "ssh://git@github.com/nitecon/agent-gateway.git", "ssh://git@github.com:22/nitecon/agent-gateway.git"} {
		got, err := ParseRemote(raw)
		if err != nil || got != want {
			t.Errorf("ParseRemote(%q) = %+v, %v", raw, got, err)
		}
	}
}

// Rust: rs::parses_nested_gitlab_groups_and_self_hosted_hosts.
func TestNestedAndSelfHosted(t *testing.T) {
	gl, err := ParseRemote("https://gitlab.com/group/sub/repo.git")
	if err != nil || gl.Provider != "gitlab" || gl.Namespace != "group/sub" || gl.FullName() != "group/sub/repo" {
		t.Fatalf("GitLab: %+v, %v", gl, err)
	}
	own, err := ParseRemote("git@git.example.com:team/tool.git")
	if err != nil || own.Provider != "git.example.com" || own.Canonical != "git.example.com/team/tool" {
		t.Fatalf("self hosted: %+v, %v", own, err)
	}
}

// Rust: rs::repository_identity_preserves_distinct_endpoints_and_rejects_url_metadata.
func TestEndpointIdentityAndMetadata(t *testing.T) {
	for raw, want := range map[string]string{
		"ssh://git@git.example.com:2222/team/repo.git": "git.example.com:2222/team/repo",
		"https://git.example.com:8443/team/repo.git":   "git.example.com:8443/team/repo",
		"ssh://git@git.example.com:22/team/repo.git":   "git.example.com/team/repo",
		"git://git.example.com:9418/team/repo.git":     "git.example.com/team/repo",
		"https://github.com:443/org/repo":              "github.com/org/repo",
		"http://github.com:80/org/repo":                "github.com/org/repo",
		"https://github.com/org/repo.git/":             "github.com/org/repo",
	} {
		got, err := CanonicalRemote(raw)
		if err != nil || got != want {
			t.Errorf("%q = %q, %v; want %q", raw, got, err, want)
		}
	}
	for _, port := range []string{"22", "80", "443", "9418"} {
		raw := "git.example.com:" + port + "/team/repo"
		got, err := CanonicalRemote(raw)
		if err != nil || got != raw {
			t.Errorf("canonical port lost: %q, %v", got, err)
		}
	}
	for _, raw := range []string{"/home/user/repo", "C:\\Users\\repo", "C:/team/repo", "my-app", "nitecon/bruce", "", "https://git.example.com/team/repo?branch=main", "https://git.example.com/team/repo#main", "https://user:secret@git.example.com/team/repo", "file:///team/repo", "https://github.com/", "github.com/org/\nrepo", "https://host:65536/team/repo", "github.com/" + strings.Repeat("a", 4096)} {
		if got, err := CanonicalRemote(raw); err == nil {
			t.Errorf("accepted %q => %q", raw, got)
		}
	}
}

// Rust: rs identifier sanitization tests.
func TestSanitizeIdent(t *testing.T) {
	for raw, want := range map[string]string{"https://github.com/org/MyRepo.git": "myrepo", "My Cool Project": "my-cool-project", "---hello---": "hello", "a___b": "a___b", "Hello!!!World": "hello-world", "C:\\Users\\repo.git": "repo"} {
		if got := SanitizeIdent(raw); got != want {
			t.Errorf("sanitize %q = %q; want %q", raw, got, want)
		}
	}
	if got := SanitizeIdent(strings.Repeat("a", 101)); len(got) != 100 {
		t.Fatalf("unbounded name %q", got)
	}
	if got := SanitizeIdent("!!!"); !strings.HasPrefix(got, "project-") || len(got) != 16 || got != SanitizeIdent("!!!") {
		t.Fatalf("unstable fallback %q", got)
	}
	if got := NormalizeIdent("  My-Project  "); got != "my-project" {
		t.Fatalf("normalize %q", got)
	}
}

func TestRustIdentifierExamples(t *testing.T) {
	for raw, want := range map[string]string{
		"github.com/nitecon/bruce.git":            "bruce",
		"/home/user/projects/my-app":              "my-app",
		`C:\Users\nitec\Documents\Projects\bruce`: "bruce",
		"My Project!": "my-project", "hello world": "hello-world", "foo---bar": "foo-bar",
	} {
		if got := SanitizeIdent(raw); got != want {
			t.Errorf("%q = %q; want %q", raw, got, want)
		}
	}
	if got := NormalizeIdent("  My-Project_1  "); got != "my-project_1" {
		t.Fatalf("normalize = %q", got)
	}
	if got := SanitizeIdent("!!!!"); !strings.HasPrefix(got, "project-") {
		t.Fatalf("fallback = %q", got)
	}
}
