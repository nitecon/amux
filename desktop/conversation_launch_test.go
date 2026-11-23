package desktop

import (
	"reflect"
	"strings"
	"testing"
)

func TestProviderLaunchCommandUsesOnlySupportedOptions(t *testing.T) {
	tests := []struct {
		provider string
		prefs    ConversationLaunchPreferences
		want     []string
		wantErr  bool
	}{
		{"claude", ConversationLaunchPreferences{Model: "sonnet", Persona: "reviewer"}, []string{"claude", "--model", "sonnet", "--agent", "reviewer"}, false},
		{"codex", ConversationLaunchPreferences{Model: "gpt-5.1"}, []string{"codex", "app-server", "--listen", "stdio://", "--config", `model="gpt-5.1"`}, false},
		{"codex", ConversationLaunchPreferences{Persona: "review carefully"}, nil, true},
		{"antigravity", ConversationLaunchPreferences{}, nil, false},
		{"antigravity", ConversationLaunchPreferences{Model: "unknown"}, nil, true},
	}
	for _, test := range tests {
		got, err := providerLaunchCommand(test.provider, test.prefs)
		if (err != nil) != test.wantErr || !reflect.DeepEqual(got, test.want) {
			t.Errorf("providerLaunchCommand(%q) = %v, %v; want %v, error=%t", test.provider, got, err, test.want, test.wantErr)
		}
	}
	if _, err := providerLaunchCommand("claude", ConversationLaunchPreferences{Model: "sonnet\n--danger"}); err == nil || !strings.Contains(err.Error(), "control") {
		t.Fatalf("control character preference error = %v", err)
	}
}
