package storage

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestPluginSettingsWhitelistOrderDefaultsAndPersistence(t *testing.T) {
	ctx := context.Background()
	s := taskStore(t)
	wantIDs := []string{"assignment", "notifier", "reminder", "review", "reclaim"}
	ids := SwitchablePluginIDs()
	if !reflect.DeepEqual(ids, wantIDs) {
		t.Fatalf("switchable IDs=%v, want %v", ids, wantIDs)
	}
	ids[0] = "mutated"
	settings, err := s.PluginSettings(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(settings) != len(wantIDs) {
		t.Fatalf("settings count=%d, want %d", len(settings), len(wantIDs))
	}
	for i, setting := range settings {
		if setting.ID != wantIDs[i] || !setting.Enabled {
			t.Fatalf("default setting[%d]=%+v, want %s enabled", i, setting, wantIDs[i])
		}
	}
	if err := s.SetPluginEnabled(ctx, "review", false); err != nil {
		t.Fatal(err)
	}
	if err := s.SetPluginEnabled(ctx, "review", true); err != nil {
		t.Fatal(err)
	}
	var stored string
	if err := s.db.QueryRowContext(ctx, "SELECT value FROM settings WHERE key='plugin.review.enabled'").Scan(&stored); err != nil || stored != "true" {
		t.Fatalf("stored setting=%q err=%v, want literal true", stored, err)
	}
	if err := s.SetPluginEnabled(ctx, "native-delivery", false); !errors.Is(err, ErrUnknownPlugin) {
		t.Fatalf("unsupported plugin error=%v, want ErrUnknownPlugin", err)
	}
}
