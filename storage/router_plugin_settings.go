package storage

import "context"

type PluginSetting struct {
	ID      string `json:"id"`
	Enabled bool   `json:"enabled"`
}

// PluginSettings returns the switchable plugins in Rust API order.
func (s *Store) PluginSettings(ctx context.Context) ([]PluginSetting, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	ids := SwitchablePluginIDs()
	settings := make([]PluginSetting, 0, len(ids))
	for _, id := range ids {
		enabled, err := pluginEnabled(ctx, tx, id)
		if err != nil {
			return nil, err
		}
		settings = append(settings, PluginSetting{ID: id, Enabled: enabled})
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return settings, nil
}

// SetPluginEnabled persists the literal setting consumed by PluginEnabled.
func (s *Store) SetPluginEnabled(ctx context.Context, id string, enabled bool) error {
	if !isSwitchablePlugin(id) {
		return ErrUnknownPlugin
	}
	value := "false"
	if enabled {
		value = "true"
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings(key,value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, "plugin."+id+".enabled", value)
	return err
}
