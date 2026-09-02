package database

import (
	"context"
	"path/filepath"
	"testing"
)

func TestAPIKeyLimitsResolveUpstreamChannel(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "auto empty", in: "", want: UpstreamChannelAuto},
		{name: "codex", in: " CODEX ", want: UpstreamChannelCodex},
		{name: "grok", in: "Grok", want: UpstreamChannelGrok},
		{name: "antigravity", in: " Antigravity ", want: UpstreamChannelAntigravity},
		{name: "claude", in: " Claude ", want: UpstreamChannelClaude},
		{name: "unknown", in: "other", want: UpstreamChannelAuto},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (APIKeyLimits{UpstreamChannel: tt.in}).ResolveUpstreamChannel(); got != tt.want {
				t.Fatalf("ResolveUpstreamChannel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNormalizeAccountGroupChannel(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{in: "codex", want: AccountGroupChannelCodex},
		{in: " GROK ", want: AccountGroupChannelGrok},
		{in: "Antigravity", want: AccountGroupChannelAntigravity},
		{in: "", want: AccountGroupChannelCodex},
		{in: "other", want: AccountGroupChannelCodex},
	}
	for _, tt := range tests {
		if got := NormalizeAccountGroupChannel(tt.in); got != tt.want {
			t.Errorf("NormalizeAccountGroupChannel(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestSQLiteListAccountListProjectionByChannel(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "account-list-projection-channel.db"))
	if err != nil {
		t.Fatalf("New(sqlite) error: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	codexID, err := db.InsertAccount(ctx, "codex", "codex-refresh", "")
	if err != nil {
		t.Fatalf("insert codex account: %v", err)
	}
	grokID, err := db.InsertAccountWithUpstream(ctx, "grok", "xai", "oauth", map[string]interface{}{
		"upstream_type": "grok",
		"api_key":       "grok-secret",
	}, "")
	if err != nil {
		t.Fatalf("insert grok account: %v", err)
	}
	antigravityID, err := db.InsertAccountWithUpstream(ctx, "antigravity", "google", "oauth", map[string]interface{}{
		"upstream_type":            "antigravity",
		"refresh_token":            "antigravity-secret",
		"avatar_url":               "https://example.com/avatar.png",
		"verified_email":           true,
		"project_id":               "project-1",
		"antigravity_sync_error":   "sync failed",
		"antigravity_sync_warning": "credits snapshot preserved",
		"antigravity_permissions":  `{"allowed":false}`,
		"antigravity_quota":        `{"models":[],"forbidden":true}`,
	}, "")
	if err != nil {
		t.Fatalf("insert antigravity account: %v", err)
	}
	claudeID, err := db.InsertAccountWithUpstream(ctx, "claude", "anthropic", "oauth", map[string]interface{}{
		"upstream_type":            "claude",
		"access_token":             "claude-secret",
		"claude_usage_probe_at":    "2026-08-29T05:00:00Z",
		"claude_usage_probe_error": "",
		"models":                   []string{"claude-sonnet-4-5"},
	}, "")
	if err != nil {
		t.Fatalf("insert Claude account: %v", err)
	}

	tests := []struct {
		channel string
		wantID  int64
	}{
		{channel: UpstreamChannelCodex, wantID: codexID},
		{channel: UpstreamChannelGrok, wantID: grokID},
		{channel: UpstreamChannelAntigravity, wantID: antigravityID},
		{channel: UpstreamChannelClaude, wantID: claudeID},
	}
	for _, tt := range tests {
		t.Run(tt.channel, func(t *testing.T) {
			rows, err := db.ListAccountListProjection(ctx, tt.channel)
			if err != nil {
				t.Fatalf("ListAccountListProjection(%q) error: %v", tt.channel, err)
			}
			if len(rows) != 1 || rows[0].ID != tt.wantID {
				t.Fatalf("ListAccountListProjection(%q) = %+v, want id %d", tt.channel, rows, tt.wantID)
			}
			if tt.channel == UpstreamChannelAntigravity && (rows[0].GetCredential("avatar_url") == "" || !rows[0].GetCredentialBool("verified_email") || rows[0].GetCredential("project_id") != "project-1" || rows[0].GetCredential("antigravity_sync_error") != "sync failed" || rows[0].GetCredential("antigravity_sync_warning") == "" || rows[0].GetCredential("antigravity_permissions") == "" || rows[0].GetCredential("antigravity_quota") == "") {
				t.Fatalf("Antigravity projection omitted control-plane status fields: %#v", rows[0].Credentials)
			}
			if tt.channel == UpstreamChannelClaude && (rows[0].GetCredential("claude_usage_probe_at") == "" || rows[0].GetCredential("claude_usage_probe_error") != "" || len(rows[0].GetCredentialStringSlice("models")) != 1) {
				t.Fatalf("Claude projection omitted sampling metadata: %#v", rows[0].Credentials)
			}
		})
	}
}

func TestReplaceAccountCredentialsCASUpdatesCanonicalFamily(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "antigravity-family-cas.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx := context.Background()
	id, err := db.InsertAccountWithUpstream(ctx, "antigravity", "google", "oauth", map[string]interface{}{
		"upstream_type":        "antigravity",
		"refresh_token":        "old-refresh",
		"credential_family_id": "ag_old",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	newGeneration, applied, err := db.ReplaceAccountCredentialsCAS(ctx, id, 1, "ag_new", map[string]any{
		"refresh_token": "new-refresh",
	})
	if err != nil || !applied || newGeneration != 2 {
		t.Fatalf("ReplaceAccountCredentialsCAS() = generation %d applied %v err %v", newGeneration, applied, err)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.CredentialGeneration != 2 || row.CredentialFamilyID != "ag_new" || row.GetCredential("credential_family_id") != "ag_new" || row.GetCredential("refresh_token") != "new-refresh" {
		t.Fatalf("updated row = generation %d family %q credentials %#v", row.CredentialGeneration, row.CredentialFamilyID, row.Credentials)
	}
	if _, applied, err := db.ReplaceAccountCredentialsCAS(ctx, id, 1, "ag_stale", map[string]any{"refresh_token": "stale"}); err != nil || applied {
		t.Fatalf("stale ReplaceAccountCredentialsCAS() applied=%v err=%v", applied, err)
	}
}

func TestUpdateAccountCredentialsCASKeepsEmbeddedFamilyCanonical(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "antigravity-family-refresh-cas.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	ctx := context.Background()
	id, err := db.InsertAccountWithUpstream(ctx, "antigravity", "google", "oauth", map[string]interface{}{
		"upstream_type":        "antigravity",
		"refresh_token":        "old-refresh",
		"credential_family_id": "ag_canonical",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	newGeneration, applied, err := db.UpdateAccountCredentialsCAS(ctx, id, 1, map[string]any{
		"refresh_token":        "rotated-refresh",
		"credential_family_id": "ag_drifted",
	})
	if err != nil || !applied || newGeneration != 2 {
		t.Fatalf("UpdateAccountCredentialsCAS() = generation %d applied %v err %v", newGeneration, applied, err)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if row.CredentialGeneration != 2 || row.CredentialFamilyID != "ag_canonical" || row.GetCredential("credential_family_id") != "ag_canonical" || row.GetCredential("refresh_token") != "rotated-refresh" {
		t.Fatalf("updated row = generation %d family %q credentials %#v", row.CredentialGeneration, row.CredentialFamilyID, row.Credentials)
	}
}
