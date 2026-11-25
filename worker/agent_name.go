package worker

import (
	_ "embed"
	"hash/fnv"
	"runtime"
	"strings"

	"github.com/nitecon/amux/storage"
)

//go:embed agent_names.txt
var agentNamesText string

var agentNames = strings.Split(strings.TrimSpace(agentNamesText), "\n")

// AgentDisplayName uses the random session ID so the same worker keeps its
// name across restarts and instance changes without changing its routing identity.
func AgentDisplayName(origin storage.AgentOrigin) string {
	hash := fnv.New32a()
	_, _ = hash.Write([]byte(origin.SessionID))
	name := agentNames[int(hash.Sum32()%uint32(len(agentNames)))]
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

// AgentIdentity combines the saved worker origin with facts from its host.
type AgentIdentity struct {
	storage.AgentOrigin
	DisplayName string `json:"display_name"`
	Hostname    string `json:"hostname"`
	Arch        string `json:"arch"`
}

func CurrentAgentIdentity(origin storage.AgentOrigin) AgentIdentity {
	platform, hostname := workerMachineMetadata()
	if origin.OS == "" {
		origin.OS = platform
	}
	identity := AgentIdentity{AgentOrigin: origin, DisplayName: AgentDisplayName(origin), Arch: runtime.GOARCH}
	if hostname != nil {
		identity.Hostname = *hostname
	}
	return identity
}
