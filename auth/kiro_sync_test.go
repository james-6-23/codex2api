package auth

import (
	"testing"
	"time"

	"github.com/codex2api/database"
)

func TestKiroSyncDoesNotRestoreStaleRegistration(t *testing.T) {
	store := NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	t.Cleanup(store.Stop)
	account := &Account{DBID: 905, UpstreamType: UpstreamKiro, AccessToken: "at-new", RefreshToken: "rt-new",
		CredentialGeneration: 3, Kiro: KiroCredential{AuthKind: KiroAuthKindIdC, ClientID: "client-new", ClientSecret: "secret-new", TokenEndpoint: "https://login.microsoftonline.com/new/oauth2/v2.0/token", MachineID: "machine-new"}}
	store.AddAccount(account)
	registration := account.Kiro
	stale := &database.AccountRow{ID: 905, CredentialGeneration: 2, Credentials: map[string]interface{}{
		"upstream_type": UpstreamKiro, "refresh_token": "rt-old", "plan_type": "KIRO PRO",
		KiroClientIDCredentialKey: "client-old", KiroClientSecretCredentialKey: "secret-old",
		"models": []interface{}{"vision-model"}, KiroImageModelsCredentialKey: "vision-model",
		KiroUsageCredentialKey: `{"next_reset_at":"2027-01-01T00:00:00Z"}`,
	}}
	if !store.ApplyKiroAccountRow(stale) {
		t.Fatal("sync metadata was not applied")
	}
	if account.RefreshToken != "rt-new" || account.AccessToken != "at-new" || account.CredentialGeneration != 3 ||
		account.Kiro.ClientID != registration.ClientID || account.Kiro.ClientSecret != registration.ClientSecret ||
		account.Kiro.AuthKind != registration.AuthKind || account.Kiro.TokenEndpoint != registration.TokenEndpoint || account.Kiro.MachineID != registration.MachineID {
		t.Fatal("metadata synchronization changed live authentication")
	}
	if account.PlanType != "KIRO PRO" || !account.KiroAcceptsImage("vision-model") || len(account.Models) != 1 ||
		!account.Kiro.UsageResetAt.Equal(time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("metadata synchronization did not update plan, reset time and catalog")
	}
}
