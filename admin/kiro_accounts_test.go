package admin

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestParseKiroImportFormats(t *testing.T) {
	kam := `{"version":"1.0","accounts":[{"email":"a@example.com","idp":"Google","machineId":"m-1","profileArn":"arn:aws:codewhisperer:us-east-1:1:profile/P",
	  "credentials":{"accessToken":"at-1","refreshToken":"rt-1","expiresAt":1767225600000,"authMethod":"social","provider":"Google"}}]}`
	kiroRS := `[{"refreshToken":"rt-2","authMethod":"idc","clientId":"cid","clientSecret":"csec","region":"eu-west-1","apiRegion":"us-east-1","startUrl":"https://d-1.awsapps.com/start"},
	  {"kiroApiKey":"ksk_test","authMethod":"api_key"}]`
	ide := `{"accessToken":"at-3","refreshToken":"rt-3","expiresAt":"2026-12-01T00:00:00Z","authMethod":"social","provider":"Github","profileArn":"arn:p"}`

	items, err := parseKiroImport(kam)
	if err != nil || len(items) != 1 {
		t.Fatalf("KAM: %v %+v", err, items)
	}
	if got := items[0]; got.Email != "a@example.com" || got.RefreshToken != "rt-1" || got.Credential.LoginSource != "Google" || got.Credential.MachineID != "m-1" || got.Credential.ProfileARN == "" || got.ExpiresAt.UnixMilli() != 1767225600000 {
		t.Fatalf("KAM item = %+v", got)
	}
	items, err = parseKiroImport(kiroRS)
	if err != nil || len(items) != 2 {
		t.Fatalf("kiro.rs: %v %+v", err, items)
	}
	if got := items[0].Credential; got.ClientSecret != "csec" || got.Region != "eu-west-1" || got.APIRegion != "us-east-1" || got.StartURL == "" {
		t.Fatalf("kiro.rs idc = %+v", got)
	}
	if items[1].APIKey != "ksk_test" {
		t.Fatalf("kiro.rs api key = %+v", items[1])
	}
	items, err = parseKiroImport(ide)
	if err != nil || len(items) != 1 || items[0].Credential.LoginSource != "Github" || items[0].ExpiresAt.IsZero() {
		t.Fatalf("IDE token: %v %+v", err, items)
	}
	if _, err := parseKiroImport(`[{"accessToken":"only-at"}]`); err == nil {
		t.Fatal("an entry without refreshToken or kiroApiKey must be rejected")
	}
}

func TestExportKiroAccountsRoundTripsThroughImporter(t *testing.T) {
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, nil)
	t.Cleanup(store.Stop)
	handler := &Handler{db: db, store: store}
	id, err := db.InsertAccountWithUpstream(context.Background(), "kiro", "kiro", auth.UpstreamKiro, map[string]any{
		"upstream_type": auth.UpstreamKiro, "email": "a@example.com", "access_token": "at-1", "refresh_token": "rt-1",
		"expires_at": "2026-12-01T00:00:00Z", "kiro_auth_kind": auth.KiroAuthKindSocial, "kiro_login_source": "Google",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"kam", "kiro-rs"} {
		recorder := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(recorder)
		c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/kiro/export?format="+format+"&ids="+strconv.FormatInt(id, 10), nil)
		handler.ExportKiroAccounts(c)
		if recorder.Code != http.StatusOK || recorder.Header().Get("Cache-Control") != "no-store, max-age=0" || !strings.Contains(recorder.Header().Get("Content-Disposition"), "attachment") {
			t.Fatalf("%s: status=%d headers=%v", format, recorder.Code, recorder.Header())
		}
		items, err := parseKiroImport(recorder.Body.String())
		if err != nil || len(items) != 1 || items[0].RefreshToken != "rt-1" || items[0].Email != "a@example.com" || items[0].ExpiresAt.IsZero() {
			t.Fatalf("%s round trip: %v %+v\n%s", format, err, items, recorder.Body.String())
		}
	}
}

func TestKiroSettingsRoundTrip(t *testing.T) {
	handler, db := newTestAdminHandlerFixture(t)
	t.Cleanup(func() { ApplyKiroSettings(database.KiroSettings{}) })
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/admin/settings/kiro", strings.NewReader(`{"api_domain":"kiro","cache_simulation":true}`))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.UpdateKiroSettings(c)
	if recorder.Code != http.StatusOK || auth.KiroAPIDomain() != "kiro" {
		t.Fatalf("status=%d body=%s domain=%q", recorder.Code, recorder.Body.String(), auth.KiroAPIDomain())
	}
	saved, err := db.LoadKiroSettings(context.Background())
	if err != nil || saved.APIDomain != "kiro" || !saved.CacheSimulation {
		t.Fatalf("saved = %+v err=%v", saved, err)
	}
	recorder = httptest.NewRecorder()
	c, _ = gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPut, "/api/admin/settings/kiro", strings.NewReader(`{"api_domain":"example.com"}`))
	c.Request.Header.Set("Content-Type", "application/json")
	handler.UpdateKiroSettings(c)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("unknown domain must be rejected: %d", recorder.Code)
	}
}

func TestKiroUsageSnapshotSurvivesCredentialStorage(t *testing.T) {
	db := newTestAdminDB(t)
	ctx := context.Background()
	id, err := db.InsertAccountWithUpstream(ctx, "kiro", "kiro", auth.UpstreamKiro, map[string]any{"upstream_type": auth.UpstreamKiro, "refresh_token": "rt"}, "")
	if err != nil {
		t.Fatal(err)
	}
	usage := &auth.KiroUsageSnapshot{SubscriptionTitle: "KIRO FREE", Used: 1, Limit: 50, BaseLimit: 50, FetchedAt: time.Now()}
	if err := db.UpdateCredentials(ctx, id, map[string]interface{}{auth.KiroUsageCredentialKey: kiroUsageCredential(usage)}); err != nil {
		t.Fatal(err)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if raw := kiroUsageForRow(row, true); !strings.Contains(string(raw), `"subscription_title":"KIRO FREE"`) {
		t.Fatalf("kiro_usage must be readable after storage, got %q", raw)
	}
}

func TestKiroAccountsAreNeverUnsampled(t *testing.T) {
	if accountListUnsampled(&accountListSnapshotItem{Status: "active", KiroAuthKind: auth.KiroAuthKindSocial}) {
		t.Fatal("Kiro quota comes from the control plane, not 5h/7d windows; it must not count as unsampled")
	}
}

func TestKiroUsageCooldownFollowsUpstreamSnapshot(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	t.Cleanup(store.Stop)
	handler := &Handler{store: store}
	resetAt := time.Now().UTC().Add(48 * time.Hour).Truncate(time.Second)
	newAccount := func(id int64) *auth.Account {
		acc := &auth.Account{DBID: id, UpstreamType: auth.UpstreamKiro, AccessToken: "at", RefreshToken: "rt", Status: auth.StatusReady,
			Kiro: auth.KiroCredential{AuthKind: auth.KiroAuthKindSocial, UsageResetAt: resetAt}}
		store.AddAccount(acc)
		return acc
	}

	exhausted := newAccount(1)
	handler.syncKiroUsageCooldown(1, &auth.KiroUsageSnapshot{Used: 1000, Limit: 1000, OverageStatus: "DISABLED"})
	reason, until := exhausted.GetCooldownSnapshot()
	if !exhausted.HasActiveCooldown() || reason != "usage_limited" || until.Sub(resetAt).Abs() > time.Second {
		t.Fatalf("exhausted account: cooling=%v reason=%q until=%v want %v", exhausted.HasActiveCooldown(), reason, until, resetAt)
	}
	exhausted.FailureStreak = 0
	handler.syncKiroUsageCooldown(1, &auth.KiroUsageSnapshot{Used: 1000, Limit: 1000})
	if exhausted.FailureStreak != 0 {
		t.Fatal("an unchanged usage_limit cooldown must not be re-marked on every sync")
	}

	// 上游改了重置时间（同步写库后经 ApplyKiroAccountRow 生效）：冷却截止跟着走。
	movedResetAt := resetAt.Add(72 * time.Hour)
	store.ApplyKiroAccountRow(&database.AccountRow{ID: 1, Credentials: map[string]interface{}{
		"upstream_type": auth.UpstreamKiro, auth.KiroUsageCredentialKey: `{"next_reset_at":"` + movedResetAt.Format(time.RFC3339) + `"}`,
	}})
	handler.syncKiroUsageCooldown(1, &auth.KiroUsageSnapshot{Used: 1000, Limit: 1000})
	if _, until := exhausted.GetCooldownSnapshot(); until.Sub(movedResetAt).Abs() > time.Second {
		t.Fatalf("cooldown until=%v, want upstream reset %v", until, movedResetAt)
	}
	// 解析不到额度时不能放行。
	handler.syncKiroUsageCooldown(1, &auth.KiroUsageSnapshot{})
	if !exhausted.HasActiveCooldown() {
		t.Fatal("an empty snapshot must not release a usage_limit cooldown")
	}
	// 上游恢复额度（重置 / 补发积分）：立即解除。
	handler.syncKiroUsageCooldown(1, &auth.KiroUsageSnapshot{Used: 0, Limit: 1000})
	if exhausted.HasActiveCooldown() {
		t.Fatal("restored upstream credits must release the usage_limit cooldown")
	}

	for id, usage := range map[int64]*auth.KiroUsageSnapshot{
		2: {Used: 0.17, Limit: 1000},
		3: {Used: 1000, Limit: 1000, OverageStatus: "ENABLED"},
		4: {},
	} {
		acc := newAccount(id)
		handler.syncKiroUsageCooldown(id, usage)
		if acc.HasActiveCooldown() {
			t.Fatalf("account %d with usage %+v must stay schedulable", id, usage)
		}
	}
}

func TestKiroSyncWaitsForSameAccountSync(t *testing.T) {
	lock := accountProbeLock(9001)
	lock <- struct{}{} // 模拟同一账号的另一次同步正在进行
	defer func() { <-lock }()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, _, err := (&Handler{}).syncKiroAccount(ctx, 9001); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("sync must wait for the in-flight sync of the same account, got %v", err)
	}
}

func TestExportKiroKAMKeepsExternalIdPFields(t *testing.T) {
	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, nil)
	t.Cleanup(store.Stop)
	handler := &Handler{db: db, store: store}
	id, err := db.InsertAccountWithUpstream(context.Background(), "entra", "kiro", auth.UpstreamKiro, map[string]any{
		"upstream_type": auth.UpstreamKiro, "email": "e@example.com", "refresh_token": "rt-e", "kiro_auth_kind": auth.KiroAuthKindExternalIdP,
		auth.KiroTokenEndpointCredentialKey: "https://login.microsoftonline.com/tenant/oauth2/v2.0/token", auth.KiroIssuerURLCredentialKey: "https://login.microsoftonline.com/tenant/v2.0",
		auth.KiroScopesCredentialKey: "openid offline_access", auth.KiroAPIRegionCredentialKey: "eu-central-1", auth.KiroClientIDCredentialKey: "client-e",
	}, "")
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/kiro/export?ids="+strconv.FormatInt(id, 10), nil)
	handler.ExportKiroAccounts(c)
	items, err := parseKiroImport(recorder.Body.String())
	if err != nil || len(items) != 1 {
		t.Fatalf("round trip: %v %s", err, recorder.Body.String())
	}
	got := items[0].Credential
	if got.TokenEndpoint == "" || got.IssuerURL == "" || got.Scopes == "" || got.APIRegion != "eu-central-1" || items[0].RawAuthMethod != auth.KiroAuthKindExternalIdP {
		t.Fatalf("KAM export dropped authentication fields: %+v", got)
	}
}

func TestKiroAccountPageFiltersByAuthKindAndPlan(t *testing.T) {
	for _, kind := range []string{auth.KiroAuthKindSocial, auth.KiroAuthKindIdC, auth.KiroAuthKindExternalIdP, auth.KiroAuthKindAPIKey} {
		if err := validateAccountPageFilters(accountPageQuery{AuthKind: kind}); err != nil {
			t.Fatalf("auth_kind=%s rejected: %v", kind, err)
		}
	}
	student := &accountListSnapshotItem{PlanType: "KIRO STUDENT", KiroAuthKind: auth.KiroAuthKindIdC}
	if !accountListItemMatches(student, accountPageQuery{Plan: "kiro student", AuthKind: auth.KiroAuthKindIdC}, database.UpstreamChannelKiro) {
		t.Fatal("Kiro plan filter must match the normalized plan key")
	}
	if accountListItemMatches(student, accountPageQuery{Plan: "kiro pro"}, database.UpstreamChannelKiro) {
		t.Fatal("a student account must not match the pro filter")
	}
	quarantined := &accountListSnapshotItem{KiroAuthKind: auth.KiroAuthKindSocial, HealthTier: "banned", Status: "active"}
	cooling := &accountListSnapshotItem{KiroAuthKind: auth.KiroAuthKindSocial, HealthTier: "risky", Status: "unauthorized"}
	summary, _ := summarizeAccountList([]*accountListSnapshotItem{student, quarantined, cooling}, database.UpstreamChannelKiro)
	if summary.KiroAuthKinds[auth.KiroAuthKindIdC] != 1 || summary.KiroAuthKinds[auth.KiroAuthKindSocial] != 2 {
		t.Fatalf("kiro_auth_kinds = %v", summary.KiroAuthKinds)
	}
	// 「已隔离」标签按健康档计数，点进去的 health_tier=banned 筛选才不会是空的。
	if summary.HealthBanned != 1 || !accountListItemMatches(quarantined, accountPageQuery{HealthTier: "banned"}, database.UpstreamChannelKiro) {
		t.Fatalf("health_banned = %d", summary.HealthBanned)
	}
}
