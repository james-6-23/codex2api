package auth

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/codex2api/database"
)

func TestCodexDeviceIdentitySurvivesStoreReload(t *testing.T) {
	ctx := context.Background()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "device.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	accountID, err := db.InsertAccount(ctx, "rt-only", "refresh-token", "")
	if err != nil {
		t.Fatal(err)
	}
	var want string
	if err := db.UpdateCredentials(ctx, accountID, map[string]interface{}{database.CodexInstallationIDCredentialKey: nil}); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{CodexFingerprintModeDevice, CodexFingerprintModeOff, CodexFingerprintModeDevice} {
		if err := db.UpdateCredentials(ctx, accountID, map[string]interface{}{CodexFingerprintModeCredentialKey: mode, "account_id": "later-upstream-uuid"}); err != nil {
			t.Fatal(err)
		}
		store := NewStore(db, nil, nil)
		if err := store.LoadAccountByID(ctx, accountID); err != nil {
			t.Fatal(err)
		}
		account := store.FindByID(accountID)
		got := account.EffectiveCodexInstallationID()
		if want == "" {
			want = got
		}
		if got == "" || got != want {
			t.Fatalf("reloaded installation ID = %q, want %q", got, want)
		}
		snapshot := &Account{DBID: accountID, CodexInstallationID: got, CodexFingerprintMode: mode}
		replica := &Account{DBID: accountID}
		store.applyPersistentAccountSnapshot(replica, snapshot, true)
		if replica.EffectiveCodexInstallationID() != want {
			t.Fatal("scheduler snapshot dropped device identity")
		}
		store.Stop()
	}
}
