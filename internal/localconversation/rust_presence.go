package localconversation

import (
	"context"
	"os"
	"runtime"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/nitecon/amux/gatewayclient"
)

type RustPresence struct {
	mu      sync.Mutex
	agents  map[string]gatewayclient.RustPresenceAgent
	changed chan struct{}
}

func (p *RustPresence) set(agent gatewayclient.RustPresenceAgent, present bool) {
	p.mu.Lock()
	if present {
		p.agents[agent.SessionID] = agent
	} else {
		delete(p.agents, agent.SessionID)
	}
	p.mu.Unlock()
	select {
	case p.changed <- struct{}{}:
	default:
	}
}

func (p *RustPresence) Run(ctx context.Context, client *gatewayclient.Client) {
	host, _ := os.Hostname()
	platform := runtime.GOOS
	if platform == "darwin" {
		platform = "macos"
	}
	run := uuid.NewString()
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	publish := func(ctx context.Context) {
		p.mu.Lock()
		agents := make([]gatewayclient.RustPresenceAgent, 0, len(p.agents))
		for _, agent := range p.agents {
			agents = append(agents, agent)
		}
		p.mu.Unlock()
		request, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		_ = client.ReportRustPresence(request, run, host, platform, agents)
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-p.changed:
		case <-ticker.C:
		}
		publish(ctx)
	}
}

func NewRustPresence() *RustPresence {
	return &RustPresence{agents: make(map[string]gatewayclient.RustPresenceAgent), changed: make(chan struct{}, 1)}
}
