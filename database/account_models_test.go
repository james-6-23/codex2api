package database

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
)

func TestMergeAccountModelsAppendsOnlyToExplicitAllowlist(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "account-models.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	id, err := db.InsertAccountWithCredentials(ctx, "codex", map[string]interface{}{
		"refresh_token": "rt-models",
		"models":        []string{"gpt-5.6-sol"},
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}

	merged, added, err := db.MergeAccountModels(ctx, id, []string{"gpt-6-astra", "gpt-5.6-sol", "gpt-6-astra"})
	if err != nil {
		t.Fatalf("MergeAccountModels: %v", err)
	}
	if !reflect.DeepEqual(merged, []string{"gpt-5.6-sol", "gpt-6-astra"}) {
		t.Fatalf("merged = %#v", merged)
	}
	if !reflect.DeepEqual(added, []string{"gpt-6-astra"}) {
		t.Fatalf("added = %#v", added)
	}

	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if got := row.GetCredentialStringSlice("models"); !reflect.DeepEqual(got, merged) {
		t.Fatalf("persisted models = %#v, want %#v", got, merged)
	}
}

func TestMergeAccountModelsLeavesUnlimitedAccountEmpty(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "account-models-empty.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	defer db.Close()

	id, err := db.InsertAccountWithCredentials(context.Background(), "codex", map[string]interface{}{
		"refresh_token": "rt-models-empty",
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}
	merged, added, err := db.MergeAccountModels(context.Background(), id, []string{"gpt-6-astra"})
	if err != nil {
		t.Fatalf("MergeAccountModels: %v", err)
	}
	if len(merged) != 0 || len(added) != 0 {
		t.Fatalf("unlimited account changed: merged=%#v added=%#v", merged, added)
	}
}
