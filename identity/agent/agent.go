// Package agent defines native agent identity and execution origin.
package agent

import (
	_ "embed"
	"fmt"
	"hash/fnv"
	"os"
	"runtime"
	"strings"
	"unicode"

	"github.com/google/uuid"
)

//go:embed agent_names.txt
var agentNamesText string

// agentNames trims each line because Windows checkouts may embed CRLF endings,
// and a trailing carriage return is rejected as a control character.
var agentNames = func() []string {
	names := strings.Split(strings.TrimSpace(agentNamesText), "\n")
	for i, name := range names {
		names[i] = strings.TrimSpace(name)
	}
	return names
}()

// Name is the stable personal name derived from a worker session ID.
func Name(sessionID string) string {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(sessionID))
	return agentNames[int(hash.Sum32()%uint32(len(agentNames)))]
}

// DisplayName uses the random session ID so the same worker keeps its
// name across restarts and instance changes without changing its routing identity.
func DisplayName(origin Origin) string {
	name := Name(origin.SessionID)
	switch origin.Provider {
	case "codex":
		return name + " - Codex"
	case "claude":
		return name + " - Claude"
	case "antigravity":
		return name + " - Agy"
	default:
		return name
	}
}

// Identity combines the saved worker origin with facts from its host.
type Identity struct {
	Origin
	DisplayName string `json:"display_name"`
	Hostname    string `json:"hostname"`
	Arch        string `json:"arch"`
}

func Current(origin Origin) Identity {
	platform, hostname := MachineMetadata()
	if origin.OS == "" {
		origin.OS = platform
	}
	identity := Identity{Origin: origin, DisplayName: DisplayName(origin), Arch: runtime.GOARCH}
	if hostname != nil {
		identity.Hostname = *hostname
	}
	return identity
}

// Origin fences an execution by stable session and ephemeral instance.
type Origin struct {
	SessionID  string `json:"session_id"`
	InstanceID string `json:"instance_id"`
	Provider   string `json:"provider"`
	OS         string `json:"os"`
}

func NormalizeOrigin(origin *Origin) (*Origin, error) {
	if origin == nil {
		return nil, nil
	}
	copy := *origin
	session, err := uuid.Parse(copy.SessionID)
	if err != nil {
		return nil, fmt.Errorf("agent session and instance IDs must be UUIDs")
	}
	instance, err := uuid.Parse(copy.InstanceID)
	if err != nil {
		return nil, fmt.Errorf("agent session and instance IDs must be UUIDs")
	}
	if (copy.Provider != "codex" && copy.Provider != "claude" && copy.Provider != "antigravity") || (copy.OS != "linux" && copy.OS != "windows" && copy.OS != "macos") {
		return nil, fmt.Errorf("invalid agent provider or OS")
	}
	copy.SessionID = session.String()
	copy.InstanceID = instance.String()
	return &copy, nil
}

func MachineMetadata() (string, *string) {
	machineOS := runtime.GOOS
	if machineOS == "darwin" {
		machineOS = "macos"
	}
	var hostname *string
	if value, err := os.Hostname(); err == nil {
		value = strings.TrimSpace(value)
		if value != "" && len(value) <= 128 && strings.IndexFunc(value, unicode.IsControl) < 0 {
			hostname = &value
		}
	}
	return machineOS, hostname
}
