package database

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/google/uuid"
)

func TestCodexDeviceIdentityExistsAtInsert(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "devices.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	seen := make(map[string]bool)
	for _, insert := range []func() (int64, error){
		func() (int64, error) { return db.InsertAccount(ctx, "rt", "rt-only", "") },
		func() (int64, error) { return db.InsertATAccount(ctx, "at", "at-opaque", "") },
		func() (int64, error) {
			return db.InsertAccountWithCredentials(ctx, "st", map[string]interface{}{"session_token": "st-only"}, "")
		},
	} {
		accountID, err := insert()
		if err != nil {
			t.Fatal(err)
		}
		row, err := db.GetAccountByID(ctx, accountID)
		if err != nil {
			t.Fatal(err)
		}
		installationID := row.GetCredential(CodexInstallationIDCredentialKey)
		parsed, err := uuid.Parse(installationID)
		if err != nil || parsed.Version() != 4 || seen[installationID] {
			t.Fatalf("invalid or duplicate installation ID %q", installationID)
		}
		seen[installationID] = true
		if row.GetCredential("account_id") != "" {
			t.Fatal("device initialization fabricated an upstream account UUID")
		}
	}
	other, err := New("sqlite", filepath.Join(t.TempDir(), "other-deployment.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	accountID, err := other.InsertAccount(ctx, "rt", "rt-only", "")
	if err != nil {
		t.Fatal(err)
	}
	row, err := other.GetAccountByID(ctx, accountID)
	if err != nil || seen[row.GetCredential(CodexInstallationIDCredentialKey)] {
		t.Fatal("another deployment reused the device identity for the same numeric ID")
	}
}

func TestCodexDeviceIdentityBackfillConcurrentAndPersistent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "devices.db")
	db, err := New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.conn.ExecContext(ctx, `INSERT INTO accounts (id, name, credentials) VALUES (1536, 'legacy', '{"refresh_token":"original-token"}')`); err != nil {
		t.Fatal(err)
	}
	second, err := New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	results := make(chan string, 12)
	var workers sync.WaitGroup
	for worker := range 12 {
		workers.Go(func() {
			connection := []*DB{db, second}[worker%2]
			installationID, err := connection.EnsureCodexInstallationID(ctx, 1536)
			if err != nil {
				t.Errorf("backfill: %v", err)
			}
			results <- installationID
		})
	}
	workers.Wait()
	close(results)
	want := ""
	for got := range results {
		if want == "" {
			want = got
		}
		if got == "" || got != want {
			t.Fatalf("concurrent identity = %q, want %q", got, want)
		}
	}
	for _, mode := range []string{"device", "off", "device"} {
		if err := db.UpdateOAuthAccountCredentials(ctx, 1536, map[string]interface{}{"access_token": "refreshed-token", "account_id": "uuid-arrived-later", "codex_fingerprint_mode": mode}, ""); err != nil {
			t.Fatal(err)
		}
		got, err := second.EnsureCodexInstallationID(ctx, 1536)
		if err != nil || got != want {
			t.Fatalf("mode %q: identity = %q, err = %v", mode, got, err)
		}
	}
	row, err := second.GetAccountByID(ctx, 1536)
	if err != nil || row.GetCredential("refresh_token") != "original-token" || row.GetCredential(CodexInstallationIDCredentialKey) != want {
		t.Fatalf("persisted credentials lost on refresh: row=%+v err=%v", row, err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if got, err := db.EnsureCodexInstallationID(canceled, 1536); err == nil || got != "" {
		t.Fatalf("failed persistence returned an identity: %q, %v", got, err)
	}
}
