package startupguidance

import (
	"path/filepath"
	"testing"
)

func TestMemoryQueryUsesProjectBasename(t *testing.T) {
	query, ok := MemoryQuery("/work/checkouts/amux-go")
	if !ok || query != "amux-go project decisions and conventions" {
		t.Fatalf("query=%q ok=%v", query, ok)
	}
	if _, ok := MemoryQuery(string(filepath.Separator)); ok {
		t.Fatal("filesystem root produced a project memory query")
	}
}
