package gatewayorigin

import "testing"

func TestParseSharedOriginRules(t *testing.T) {
	for _, raw := range []string{
		"http://gateway.example",
		"https://gateway.example/",
	} {
		if _, err := Parse(raw); err != nil {
			t.Errorf("Parse(%q): %v", raw, err)
		}
	}
	for _, raw := range []string{
		"file:///tmp/state",
		"https:///missing-host",
		"https://user:secret@gateway.example",
		"https://gateway.example?token=secret",
		"https://gateway.example#fragment",
	} {
		if _, err := Parse(raw); err == nil {
			t.Errorf("Parse(%q) accepted", raw)
		}
	}
}
