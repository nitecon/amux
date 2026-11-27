package gatewayclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/nitecon/amux/storage"
)

type providerSettingWire struct {
	ID            string `json:"id"`
	Enabled       *bool  `json:"enabled"`
	ReleasedTasks int    `json:"released_tasks"`
}

// ProviderSettings reads the fixed ordered set of switchable providers.
func (c *Client) ProviderSettings(ctx context.Context) ([]storage.ProviderSetting, error) {
	var raw json.RawMessage
	if err := c.do(ctx, "GET", "/v1/providers", nil, nil, &raw); err != nil {
		return nil, err
	}
	var response []providerSettingWire
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, fmt.Errorf("invalid gateway provider settings response")
	}
	ids := storage.SwitchableProviderIDs()
	if len(response) != len(ids) {
		return nil, fmt.Errorf("gateway provider settings response is incomplete")
	}
	settings := make([]storage.ProviderSetting, len(ids))
	for i, id := range ids {
		if response[i].ID != id || response[i].Enabled == nil {
			return nil, fmt.Errorf("gateway provider settings response has an invalid ID, order, or enabled value")
		}
		settings[i] = storage.ProviderSetting{ID: id, Enabled: *response[i].Enabled}
	}
	return settings, nil
}

// SetProviderEnabled updates one provider switch and returns how many active
// tasks the server released back to the automatic queue.
func (c *Client) SetProviderEnabled(ctx context.Context, id string, enabled bool) (*storage.ProviderSetting, int, error) {
	var raw json.RawMessage
	input := struct {
		Enabled bool `json:"enabled"`
	}{Enabled: enabled}
	if err := c.do(ctx, "PUT", "/v1/providers/"+url.PathEscape(id), input, nil, &raw); err != nil {
		return nil, 0, err
	}
	var response providerSettingWire
	if json.Unmarshal(raw, &response) != nil || response.ID != id || response.Enabled == nil || *response.Enabled != enabled {
		return nil, 0, fmt.Errorf("gateway provider setting acknowledgement did not match the requested value")
	}
	return &storage.ProviderSetting{ID: id, Enabled: enabled}, response.ReleasedTasks, nil
}
