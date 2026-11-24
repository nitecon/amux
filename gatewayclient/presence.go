package gatewayclient

import (
	"context"
	"net/http"
	"net/url"

	"github.com/nitecon/amux/storage"
)

// ReportPresence sends one complete source snapshot. Keep the sequence stable
// across a lost-ack retry; later snapshots must use a greater sequence.
func (c *Client) ReportPresence(ctx context.Context, report storage.PresenceReport) (bool, error) {
	var result struct {
		Accepted bool `json:"accepted"`
	}
	err := c.do(ctx, http.MethodPost, "/v1/worker-presence", report, nil, &result)
	return result.Accepted, err
}

func (c *Client) Presence(ctx context.Context, project string) ([]storage.AgentPresence, error) {
	var result []storage.AgentPresence
	err := c.do(ctx, http.MethodGet, "/v1/projects/"+url.PathEscape(project)+"/worker-presence", nil, nil, &result)
	return result, err
}

func (c *Client) PresenceSummary(ctx context.Context, hostname string) (storage.PresenceSummary, error) {
	path := "/v1/presence/summary"
	if hostname != "" {
		path += "?" + url.Values{"hostname": {hostname}}.Encode()
	}
	var response struct {
		storage.PresenceSummary
		Idle *int `json:"idle"`
	}
	err := c.do(ctx, http.MethodGet, path, nil, nil, &response)
	if response.Idle != nil {
		response.PresenceSummary.Idle = *response.Idle
	} else if err == nil {
		// Rust reports total agents and active agents, and omits idle.
		response.PresenceSummary.Idle = max(0, response.Agents-response.Active)
	}
	return response.PresenceSummary, err
}
