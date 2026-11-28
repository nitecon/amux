package project

import (
	"crypto/sha256"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"unicode"
)

// NormalizeIdent normalizes an existing identifier, without registering a name.
func NormalizeIdent(raw string) string { return strings.ToLower(strings.TrimSpace(raw)) }

// SanitizeIdent derives a channel-compatible identifier for registration.
func SanitizeIdent(raw string) string {
	trimmed := raw
	for strings.HasSuffix(trimmed, ".git") {
		trimmed = strings.TrimSuffix(trimmed, ".git")
	}
	segments := strings.FieldsFunc(trimmed, func(r rune) bool { return r == '/' || r == '\\' })
	basename := trimmed
	if len(segments) > 0 {
		basename = segments[len(segments)-1]
	}
	var b strings.Builder
	hyphen := false
	for _, r := range strings.ToLower(basename) {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_' || r == '-') {
			r = '-'
		}
		if r == '-' && hyphen {
			continue
		}
		b.WriteRune(r)
		hyphen = r == '-'
	}
	result := strings.Trim(b.String(), "-")
	if len(result) > 100 {
		result = strings.Trim(result[:100], "-")
	}
	if result == "" {
		hash := sha256.Sum256([]byte(raw))
		return fmt.Sprintf("project-%x", hash[:4])
	}
	return result
}

func asciiLower(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + ('a' - 'A')
		}
		return r
	}, s)
}

// CanonicalRemote returns host[:port]/path. Errors never echo secret-bearing input.
func CanonicalRemote(value string) (string, error) {
	invalid := func() (string, error) { return "", fmt.Errorf("unsupported Git remote") }
	if len(value) > 4096 || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return invalid()
	}
	if len(value) >= 2 && ((value[0] >= 'A' && value[0] <= 'Z') || (value[0] >= 'a' && value[0] <= 'z')) && value[1] == ':' {
		return invalid()
	}
	canonical := false
	parsedValue := value
	if !strings.Contains(value, "://") {
		authority, _, hasPath := strings.Cut(value, "/")
		if hasPath && !strings.Contains(authority, "@") && (strings.Contains(authority, ".") || strings.HasPrefix(authority, "[") || strings.Split(authority, ":")[0] == "localhost") {
			canonical = true
			if _, port, found := strings.Cut(authority, ":"); found && !strings.HasSuffix(authority, "]") {
				_, err := strconv.ParseUint(port, 10, 16)
				canonical = err == nil
				// Bracketed IPv6 with a port is validated by net/url below.
				if strings.HasPrefix(authority, "[") {
					canonical = true
				}
			}
		}
		if canonical {
			parsedValue = "ssh://" + value
		} else {
			host, path, ok := strings.Cut(value, ":")
			if !ok || strings.Contains(host, "/") {
				return invalid()
			}
			parsedValue = "ssh://" + host + "/" + path
		}
	}
	u, err := url.Parse(parsedValue)
	if err != nil || u.Hostname() == "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(parsedValue, "#") {
		return invalid()
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" && scheme != "ssh" && scheme != "git" {
		return invalid()
	}
	if u.User != nil {
		if _, hasPassword := u.User.Password(); hasPassword {
			return invalid()
		}
	}
	host := asciiLower(u.Hostname())
	if strings.ContainsAny(host, " /\\") {
		return invalid()
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	// Preserve percent-encoded path bytes as the Rust URL implementation does.
	path := asciiLower(strings.Trim(u.EscapedPath(), "/"))
	path = strings.TrimSuffix(path, ".git")
	if path == "" {
		return invalid()
	}
	port := u.Port()
	if port != "" {
		number, err := strconv.ParseUint(port, 10, 16)
		if err != nil {
			return invalid()
		}
		port = strconv.FormatUint(number, 10)
		if !canonical && (scheme == "https" && number == 443 || scheme == "http" && number == 80 || scheme == "ssh" && number == 22 || scheme == "git" && number == 9418) {
			port = ""
		}
	}
	if port != "" {
		host += ":" + port
	}
	return host + "/" + path, nil
}

// RepoRef identifies the provider and namespace independently of display names.
type RepoRef struct{ Provider, Namespace, Name, Canonical string }

func (r RepoRef) FullName() string { return r.Namespace + "/" + r.Name }

// ParseRemote requires a namespace and repository name, unlike CanonicalRemote.
func ParseRemote(raw string) (RepoRef, error) {
	canonical, err := CanonicalRemote(strings.TrimSpace(raw))
	if err != nil {
		return RepoRef{}, err
	}
	authority, path, _ := strings.Cut(canonical, "/")
	split := strings.LastIndexByte(path, '/')
	if split <= 0 || split == len(path)-1 {
		return RepoRef{}, fmt.Errorf("repository requires namespace and name")
	}
	u, err := url.Parse("https://" + authority)
	if err != nil {
		return RepoRef{}, fmt.Errorf("invalid repository host")
	}
	host := strings.ToLower(u.Hostname())
	provider := host
	switch {
	case host == "github.com" || strings.HasSuffix(host, ".github.com"):
		provider = "github"
	case host == "gitlab.com" || strings.HasPrefix(host, "gitlab.") || strings.Contains(host, ".gitlab."):
		provider = "gitlab"
	case host == "bitbucket.org" || strings.HasPrefix(host, "bitbucket."):
		provider = "bitbucket"
	}
	return RepoRef{Provider: provider, Namespace: path[:split], Name: path[split+1:], Canonical: canonical}, nil
}
