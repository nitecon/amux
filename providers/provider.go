// Package providers selects and starts native agent providers.
package providers

import (
	"context"
	"fmt"
	"strings"
	"unicode"

	"github.com/nitecon/amux/conversation"
	"github.com/nitecon/amux/providers/process"
)

// Session is the running conversation returned by a provider.
type Session interface {
	Done() <-chan struct{}
	Err() error
	Prompt(context.Context, string) (string, error)
	ResolvePermission(context.Context, string, string) error
	Close() error
}

// StartOptions binds the subprocess and its tools to one conversation.
type StartOptions struct {
	Command   []string
	Directory string
	ResumeID  string
	Env       []string
	MCP       *process.MCPConfig
}

// Preferences are optional provider-specific launch preferences.
type Preferences struct {
	Model   string `json:"model,omitempty"`
	Effort  string `json:"effort,omitempty"`
	Persona string `json:"persona,omitempty"`
}

// Provider describes one native agent integration. Use Lookup with the saved
// provider ID, then call Start to open a new or resumed Session.
type Provider interface {
	ID() string
	Label() string
	LaunchCommand(Preferences) ([]string, error)
	Start(context.Context, StartOptions, conversation.Sink) (Session, error)
}

var builtins = map[string]Provider{
	"antigravity": nativeProvider{id: "antigravity", label: "Agy", launch: antigravityCommand, start: startAntigravity},
	"claude":      nativeProvider{id: "claude", label: "Claude", launch: claudeCommand, start: startClaude},
	"codex":       nativeProvider{id: "codex", label: "Codex", launch: codexCommand, start: startCodex},
}

// Lookup returns a built-in provider by its saved ID. Call LaunchCommand when
// building a command from user preferences, then pass the command to Start.
// Leave StartOptions.Command empty to use the provider's default command.
func Lookup(id string) (Provider, bool) {
	provider, ok := builtins[id]
	return provider, ok
}

type nativeProvider struct {
	id, label string
	launch    func(Preferences) ([]string, error)
	start     func(context.Context, StartOptions, conversation.Sink) (Session, error)
}

func (p nativeProvider) ID() string    { return p.id }
func (p nativeProvider) Label() string { return p.label }

func (p nativeProvider) LaunchCommand(prefs Preferences) ([]string, error) {
	prefs.Model = strings.TrimSpace(prefs.Model)
	prefs.Effort = strings.TrimSpace(prefs.Effort)
	prefs.Persona = strings.TrimSpace(prefs.Persona)
	for _, value := range []string{prefs.Model, prefs.Effort, prefs.Persona} {
		if len(value) > 4096 || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return nil, fmt.Errorf("provider launch preference is too long or contains control characters")
		}
	}
	return p.launch(prefs)
}

func (p nativeProvider) Start(ctx context.Context, options StartOptions, sink conversation.Sink) (Session, error) {
	return p.start(ctx, options, sink)
}
