package database

import (
	"context"
	"path/filepath"
	"testing"
)

func TestPromptSessionLimitOverrideLifecycle(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "session-limit.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer db.Close()
	ctx := context.Background()

	item, err := db.UpsertPromptSessionLimitOverride(ctx, PromptSessionLimitOverride{
		Platform: " NewAPI ", NewAPIUserID: "42", Mode: PromptSessionLimitModeCustom,
		Limit: 3, WindowSeconds: 4800,
	})
	if err != nil {
		t.Fatalf("UpsertPromptSessionLimitOverride: %v", err)
	}
	if item.Platform != "newapi" || item.Limit != 3 || item.WindowSeconds != 4800 {
		t.Fatalf("item=%#v", item)
	}
	items, err := db.ListPromptSessionLimitOverrides(ctx)
	if err != nil || len(items) != 1 {
		t.Fatalf("items=%#v err=%v", items, err)
	}
	if err := db.DeletePromptSessionLimitOverride(ctx, "newapi", "42"); err != nil {
		t.Fatalf("DeletePromptSessionLimitOverride: %v", err)
	}
	items, err = db.ListPromptSessionLimitOverrides(ctx)
	if err != nil || len(items) != 0 {
		t.Fatalf("items after delete=%#v err=%v", items, err)
	}
}

func TestPromptSessionLimitOverrideValidatesCustomRange(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "session-limit-invalid.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer db.Close()
	_, err = db.UpsertPromptSessionLimitOverride(context.Background(), PromptSessionLimitOverride{
		Platform: "newapi", NewAPIUserID: "42", Mode: PromptSessionLimitModeCustom,
		Limit: 0, WindowSeconds: 59,
	})
	if err == nil {
		t.Fatal("invalid custom override was accepted")
	}
}
