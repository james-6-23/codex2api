package database

import (
	"context"
	"path/filepath"
	"testing"
)

func TestClaudeSyncedCLIVersionRoundTrip(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "claude-cli-version.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if got, err := db.GetClaudeSyncedCLIVersion(ctx); err != nil || got != "" {
		t.Fatalf("initial = %q, %v", got, err)
	}
	if err := db.UpdateClaudeSyncedCLIVersion(ctx, " 2.1.300 "); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetClaudeSyncedCLIVersion(ctx); got != "2.1.300" {
		t.Fatalf("after update = %q", got)
	}
	if _, err := db.GetSystemSettings(ctx); err != nil {
		t.Fatalf("narrow write must not break full settings read: %v", err)
	}
}

func TestUpdateAccountCustomHeadersReplacesOnlyHeaders(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "claude-headers.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	id, err := db.InsertAccountWithUpstream(ctx, "claude-a", "anthropic", "oauth", map[string]interface{}{
		"upstream_type":  "claude",
		"access_token":   "tok",
		"custom_headers": map[string]interface{}{"User-Agent": "claude-cli/2.1.219 (external, cli)", "X-App": "cli"},
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateAccountCustomHeaders(ctx, id, map[string]string{"User-Agent": "claude-cli/2.1.258 (external, cli)", "X-App": "cli"}); err != nil {
		t.Fatal(err)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	headers := row.GetCredentialStringMap("custom_headers")
	if headers["User-Agent"] != "claude-cli/2.1.258 (external, cli)" || headers["X-App"] != "cli" {
		t.Fatalf("headers = %v", headers)
	}
	if row.Credentials["upstream_type"] != "claude" {
		t.Fatal("other credential fields must survive")
	}
	if err := db.UpdateAccountCustomHeaders(ctx, 999999, map[string]string{"User-Agent": "x"}); err == nil {
		t.Fatal("unknown account must error")
	}
}
