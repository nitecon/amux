package worker

import (
	"testing"
)

func TestDesktopGuidanceIsForwardedOnlyWhenNonempty(t *testing.T) {
	t.Setenv("AMUX_DESKTOP_GUIDANCE", "")
	if got := nativeMCPEnvironment(); got != nil {
		t.Fatalf("empty desktop guidance was forwarded: %#v", got)
	}
	t.Setenv("AMUX_DESKTOP_GUIDANCE", "provided by desktop")
	if got := nativeMCPEnvironment(); got["AMUX_DESKTOP_GUIDANCE"] != "provided by desktop" {
		t.Fatalf("desktop guidance environment=%#v", got)
	}
}
