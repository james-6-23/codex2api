package database

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestModelCapabilitiesPersistenceAndGeneration(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "capabilities.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	id, err := db.InsertAccount(ctx, "caps", "test-refresh", "")
	if err != nil {
		t.Fatal(err)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := ModelCapabilitySnapshot{AccountID: id, CredentialGeneration: row.CredentialGeneration, ObservedAt: 10, Models: map[string]map[string]json.RawMessage{"gpt-5.6-sol": {"context_window": json.RawMessage("1000"), "use_responses_lite": json.RawMessage("true")}, "gpt-5.4": {"context_window": json.RawMessage("500")}}}
	if err := db.SaveModelCapabilities(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot.ObservedAt = 20
	snapshot.Models = map[string]map[string]json.RawMessage{"gpt-5.6-sol": {"context_window": json.RawMessage("800")}}
	if err := db.SaveModelCapabilities(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	snapshot.ObservedAt = 15
	snapshot.Models["gpt-5.6-sol"]["context_window"] = json.RawMessage("900")
	if err := db.SaveModelCapabilities(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	stored, err := db.ListModelCapabilities(ctx, []int64{id})
	if err != nil {
		t.Fatal(err)
	}
	got := stored[id]
	if len(got.Models) != 1 || string(got.Models["gpt-5.6-sol"]["context_window"]) != "800" || string(got.Models["gpt-5.6-sol"]["use_responses_lite"]) != "true" {
		t.Fatalf("partial/stale sync lost capabilities: %+v", got)
	}
	if _, err := db.conn.ExecContext(ctx, `UPDATE accounts SET credential_generation=credential_generation+1 WHERE id=$1`, id); err != nil {
		t.Fatal(err)
	}
	snapshot.ObservedAt = 30
	if err := db.SaveModelCapabilities(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	stored, err = db.ListModelCapabilities(ctx, []int64{id})
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 0 {
		t.Fatal("old credential generation remained visible")
	}
	snapshot.CredentialGeneration++
	snapshot.Models = map[string]map[string]json.RawMessage{"gpt-5.6-sol": {"context_window": json.RawMessage("600")}}
	if err := db.SaveModelCapabilities(ctx, snapshot); err != nil {
		t.Fatal(err)
	}
	stored, err = db.ListModelCapabilities(ctx, []int64{id})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := stored[id].Models["gpt-5.6-sol"]["use_responses_lite"]; ok {
		t.Fatal("new credentials inherited old capabilities")
	}
}
