package database

import (
	"context"
	"errors"
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
	valid := PromptSessionLimitOverride{Platform: "newapi", NewAPIUserID: "42", Mode: PromptSessionLimitModeCustom, Limit: 3, WindowSeconds: 3600}
	for _, field := range []string{"identity", "mode", "limit", "window"} {
		t.Run(field, func(t *testing.T) {
			item := valid
			switch field {
			case "identity":
				item.NewAPIUserID = ""
			case "mode":
				item.Mode = "unknown"
			case "limit":
				item.Limit = 0
			case "window":
				item.WindowSeconds = 59
			}
			_, err := (*DB)(nil).UpsertPromptSessionLimitOverride(t.Context(), item)
			if !errors.Is(err, ErrInvalidPromptSessionLimitOverride) {
				t.Fatalf("validation error = %v", err)
			}
		})
	}
	_, err := (*DB)(nil).UpsertPromptSessionLimitOverride(t.Context(), valid)
	if err == nil || errors.Is(err, ErrInvalidPromptSessionLimitOverride) {
		t.Fatalf("database failure misclassified as validation: %v", err)
	}
}
