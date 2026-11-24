package worker

import (
	_ "embed"
	"hash/fnv"
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
