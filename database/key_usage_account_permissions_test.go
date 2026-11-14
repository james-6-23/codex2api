package database

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestKeyUsageAccountPermissionsPersistAndDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "portal.db")
	db, err := New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	global, err := db.GetKeyUsageAccountPermissions(ctx, 0)
	if err != nil || global.ViewEnabled || global.ResetEnabled {
		t.Fatalf("global default=%+v err=%v", global, err)
	}
	key, err := db.GetKeyUsageAccountPermissions(ctx, 123)
	if err != nil || !key.ViewEnabled || !key.ResetEnabled {
		t.Fatalf("key default=%+v err=%v", key, err)
	}
	if err := db.SetKeyUsageAccountPermissions(ctx, 0, KeyUsageAccountPermissions{ViewEnabled: true, ResetEnabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := db.SetKeyUsageAccountPermissions(ctx, 123, KeyUsageAccountPermissions{ViewEnabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	global, err = db.GetKeyUsageAccountPermissions(ctx, 0)
	if err != nil || !global.ViewEnabled || !global.ResetEnabled {
		t.Fatalf("persisted global=%+v err=%v", global, err)
	}
	key, err = db.GetKeyUsageAccountPermissions(ctx, 123)
	if err != nil || !key.ViewEnabled || key.ResetEnabled {
		t.Fatalf("persisted key=%+v err=%v", key, err)
	}
}

func TestKeyUsageAccountPermissionsPostgres(t *testing.T) {
	dsn := os.Getenv("CODEX2API_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("requires isolated PostgreSQL database")
	}
	db, err := New("postgres", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	keyID := time.Now().UnixNano()
	scope, owner, requestID := uuid.NewString(), uuid.NewString(), uuid.NewString()
	accountID, err := db.InsertAccountWithCredentials(ctx, "portal-postgres-test", map[string]interface{}{"email": "test@example.invalid", "allowed_api_key_ids": []int64{keyID}, "access_token": "test-secret"}, "")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = db.conn.ExecContext(ctx, `DELETE FROM accounts WHERE id=$1`, accountID)
		_, _ = db.conn.ExecContext(ctx, `DELETE FROM key_usage_account_permissions WHERE key_id=$1`, keyID)
		_, _ = db.conn.ExecContext(ctx, `DELETE FROM key_usage_reset_attempts WHERE request_id=$1`, requestID)
		_, _ = db.conn.ExecContext(ctx, `DELETE FROM key_usage_reset_leases WHERE scope=$1`, scope)
		_, _ = db.conn.ExecContext(ctx, `DELETE FROM key_usage_reset_cooldowns WHERE scope=$1`, scope)
	}()
	accounts, err := db.ListKeyUsageAccountAccess(ctx)
	if err != nil {
		t.Fatal(err)
	}
	account, ok := accounts[accountID]
	if !ok || account.Name != "portal-postgres-test" || account.Email != "test@example.invalid" || len(account.AllowedAPIKeyIDs) != 1 || account.AllowedAPIKeyIDs[0] != keyID {
		t.Fatalf("projected account=%+v", account)
	}
	want := KeyUsageAccountPermissions{ViewEnabled: true}
	if err := db.SetKeyUsageAccountPermissions(ctx, keyID, want); err != nil {
		t.Fatal(err)
	}
	if got, err := db.GetKeyUsageAccountPermissions(ctx, keyID); err != nil || got != want {
		t.Fatalf("policy=%+v err=%v", got, err)
	}
	if ok, err := db.ReserveKeyUsageReset(ctx, requestID, keyID, 123); err != nil || !ok {
		t.Fatalf("reserve=%v err=%v", ok, err)
	}
	if ok, err := db.ReserveKeyUsageReset(ctx, requestID, keyID, 123); err != nil || ok {
		t.Fatalf("duplicate=%v err=%v", ok, err)
	}
	if ok, err := db.AcquireKeyUsageResetLease(ctx, scope, owner, 10, 100); err != nil || !ok {
		t.Fatalf("lease=%v err=%v", ok, err)
	}
	if ok, err := db.AcquireKeyUsageResetLease(ctx, scope, "other", 11, 100); err != nil || ok {
		t.Fatalf("duplicate lease=%v err=%v", ok, err)
	}
	if err := db.ReleaseKeyUsageResetLease(ctx, scope, owner); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.ReserveKeyUsageResetCooldown(ctx, scope, 10, 100); err != nil || !ok {
		t.Fatalf("cooldown=%v err=%v", ok, err)
	}
	if ok, err := db.ReserveKeyUsageResetCooldown(ctx, scope, 11, 101); err != nil || ok {
		t.Fatalf("duplicate cooldown=%v err=%v", ok, err)
	}
	if err := db.RecordKeyUsageResetCooldown(ctx, scope, 102); err != nil {
		t.Fatal(err)
	}
}
