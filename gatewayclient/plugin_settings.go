package gatewayclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/nitecon/amux/storage"
)

type pluginSettingWire struct {
	ID      string `json:"id"`
	Enabled *bool  `json:"enabled"`
}

// PluginSettings reads the fixed ordered set of switchable native plugins.
func (c *Client) PluginSettings(ctx context.Context) ([]storage.PluginSetting, error) {
	var raw json.RawMessage
	if err := c.do(ctx, "GET", "/v1/plugins", nil, nil, &raw); err != nil {
		return nil, err
	}
	var response []pluginSettingWire
	if err := json.Unmarshal(raw, &response); err != nil {
		return nil, fmt.Errorf("invalid gateway plugin settings response")
	}
	ids := storage.SwitchablePluginIDs()
	if len(response) != len(ids) {
		return nil, fmt.Errorf("gateway plugin settings response is incomplete")
	}
	settings := make([]storage.PluginSetting, len(ids))
	for i, id := range ids {
		if response[i].ID != id || response[i].Enabled == nil {
			return nil, fmt.Errorf("gateway plugin settings response has an invalid ID, order, or enabled value")
		}
		settings[i] = storage.PluginSetting{ID: id, Enabled: *response[i].Enabled}
	}
	return settings, nil
}

// SetPluginEnabled updates one switchable plugin and validates its receipt.
// It intentionally makes one request: callers must resolve an uncertain
// acknowledgement by reading PluginSettings.
func (c *Client) SetPluginEnabled(ctx context.Context, id string, enabled bool) (*storage.PluginSetting, error) {
	var raw json.RawMessage
	input := struct {
		Enabled bool `json:"enabled"`
	}{Enabled: enabled}
	path := "/v1/plugins/" + url.PathEscape(id)
	if err := c.do(ctx, "PUT", path, input, nil, &raw); err != nil {
		return nil, err
	}
	var response pluginSettingWire
	if json.Unmarshal(raw, &response) != nil || response.ID != id || response.Enabled == nil || *response.Enabled != enabled {
		return nil, fmt.Errorf("gateway plugin setting acknowledgement did not match the requested value")
	}
	for _, known := range storage.SwitchablePluginIDs() {
		if id == known {
			return &storage.PluginSetting{ID: id, Enabled: *response.Enabled}, nil
		}
	}
	return nil, fmt.Errorf("gateway plugin setting acknowledgement used an unknown ID")
}
