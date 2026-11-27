package amuxruntime

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestServerDiscordConfigurationFailsBeforeOpeningState(t *testing.T) {
	for _, test := range []struct {
		name  string
		flags []string
		token string
	}{
		{name: "missing guild", flags: []string{"--discord-token-env", "TEST_DISCORD_TOKEN"}, token: "private-test-token"},
		{name: "missing token environment", flags: []string{"--discord-guild-id", "123"}},
		{name: "category alone", flags: []string{"--discord-category-id", "456"}},
		{name: "unset token", flags: []string{"--discord-token-env", "TEST_DISCORD_TOKEN", "--discord-guild-id", "123"}},
		{name: "invalid guild", flags: []string{"--discord-token-env", "TEST_DISCORD_TOKEN", "--discord-guild-id", "not-a-snowflake"}, token: "private-test-token"},
		{name: "invalid category", flags: []string{"--discord-token-env", "TEST_DISCORD_TOKEN", "--discord-guild-id", "123", "--discord-category-id", "invalid"}, token: "private-test-token"},
	} {
		t.Run(test.name, func(t *testing.T) {
			database := filepath.Join(t.TempDir(), "state.db")
			args := append([]string{"server", "run", "--database", database, "--listen", "127.0.0.1:0"}, test.flags...)
			var output bytes.Buffer
			err := run(context.Background(), args, &output, &output, func(name string) string {
				if name == "GATEWAY_API_KEY" {
					return "test-gateway-key"
				}
				if name == "TEST_DISCORD_TOKEN" {
					return test.token
				}
				return ""
			})
			if err == nil {
				t.Fatal("invalid Discord configuration accepted")
			}
			if strings.Contains(err.Error()+output.String(), "private-test-token") {
				t.Fatal("configuration failure exposed the token")
			}
			if _, statErr := os.Stat(database); !os.IsNotExist(statErr) {
				t.Fatalf("invalid configuration opened server state: %v", statErr)
			}
		})
	}
}
