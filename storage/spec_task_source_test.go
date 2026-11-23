package storage

import (
	"context"
	"testing"
)

func TestFindSpecSourceTaskUsesExactLiteralLines(t *testing.T) {
	store := taskStore(t)
	ctx := context.Background()
	details := "Source:\nsource_spec_artifact_id: art_\nsource_spec_version_id: ver_\nmanifest_item_id: T0010\n\n"
	prefix, err := store.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Prefix", Reporter: "fixture", Details: stringPointerForStorageTest(details)}, nil)
	if err != nil {
		t.Fatal(err)
	}
	details = "Source:\nsource_spec_artifact_id: art_%\nsource_spec_version_id: ver_%\nmanifest_item_id: T001%\n\n"
	wildcard, err := store.InsertTask(ctx, TaskInsert{Project: "tasks", Title: "Literal", Reporter: "fixture", Details: stringPointerForStorageTest(details)}, nil)
	if err != nil {
		t.Fatal(err)
	}

	got, err := findSpecSourceTask(ctx, store.db, "tasks", "art_", "ver_", "T001")
	if err != nil || got != nil {
		t.Fatalf("prefix tuple matched distinct item: task=%+v err=%v", got, err)
	}
	got, err = findSpecSourceTask(ctx, store.db, "tasks", "art_%", "ver_%", "T001%")
	if err != nil || got == nil || got.ID != wildcard.ID {
		t.Fatalf("literal punctuation tuple not found: task=%+v err=%v", got, err)
	}
	got, err = findSpecSourceTask(ctx, store.db, "tasks", "art_", "ver_", "T0010")
	if err != nil || got == nil || got.ID != prefix.ID {
		t.Fatalf("exact item tuple not found: task=%+v err=%v", got, err)
	}

	if hasExactDetailsLine("manifest_item_id: T0010\n", "manifest_item_id: T001") {
		t.Fatal("input validation accepted a prefix match")
	}
	if !hasExactDetailsLine("manifest_item_id: T001%\r\n", "manifest_item_id: T001%") {
		t.Fatal("input validation rejected exact literal punctuation line")
	}
}

func stringPointerForStorageTest(value string) *string { return &value }
