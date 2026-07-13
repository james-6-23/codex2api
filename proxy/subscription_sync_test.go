package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

const testWorkspaceUUID = "288c5d93-a113-4ed3-b6a9-08b6a4d35417"

func TestQueryChatGPTSubscription_ParsesResponse(t *testing.T) {
	var gotAccountID, gotAuth, gotOrigin, gotUA string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccountID = r.URL.Query().Get("account_id")
		gotAuth = r.Header.Get("Authorization")
		gotOrigin = r.Header.Get("Origin")
		gotUA = r.Header.Get("User-Agent")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "22f6c975-8c24-4323-9e2d-97c4cb61905a",
			"plan_type": "plus",
			"active_start": "2026-01-29T14:54:57Z",
			"active_until": "2026-07-29T14:54:57Z",
			"billing_period": "monthly",
			"will_renew": true
		}`))
	}))
	defer server.Close()
	defer SetSubscriptionsURLForTest(server.URL)()

	account := &auth.Account{AccessToken: "test-at", AccountID: testWorkspaceUUID}
	sub, err := QueryChatGPTSubscription(context.Background(), account, "")
	if err != nil {
		t.Fatalf("QueryChatGPTSubscription: %v", err)
	}

	if sub.PlanType != "plus" {
		t.Fatalf("PlanType = %q, want plus", sub.PlanType)
	}
	want := time.Date(2026, 7, 29, 14, 54, 57, 0, time.UTC)
	if !sub.ActiveUntilTime().Equal(want) {
		t.Fatalf("ActiveUntilTime() = %v, want %v", sub.ActiveUntilTime(), want)
	}
	if !sub.WillRenew {
		t.Fatal("WillRenew = false, want true")
	}
	if gotAccountID != testWorkspaceUUID {
		t.Fatalf("query account_id = %q, want %q", gotAccountID, testWorkspaceUUID)
	}
	if gotAuth != "Bearer test-at" {
		t.Fatalf("Authorization = %q, want Bearer test-at", gotAuth)
	}
	if gotOrigin != "https://chatgpt.com" {
		t.Fatalf("Origin = %q, want https://chatgpt.com", gotOrigin)
	}
	if gotUA == "" || gotUA != subscriptionsBrowserUserAgent {
		t.Fatalf("User-Agent = %q, want browser UA", gotUA)
	}
}

// TestQueryChatGPTSubscription_RejectsUserAccountID 验证 user-... 形态的账号 ID
// （历史污染数据）不发请求直接报错——上游对非工作区 UUID 返回 500。
func TestQueryChatGPTSubscription_RejectsUserAccountID(t *testing.T) {
	requested := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requested = true
	}))
	defer server.Close()
	defer SetSubscriptionsURLForTest(server.URL)()

	account := &auth.Account{AccessToken: "test-at", AccountID: "user-MpgpQk00uRgvPiLR1iMGAiaX"}
	if _, err := QueryChatGPTSubscription(context.Background(), account, ""); err == nil {
		t.Fatal("QueryChatGPTSubscription should reject user-... account id")
	}
	if requested {
		t.Fatal("no request should be sent for user-... account id")
	}
}

// TestQueryChatGPTSubscription_FallsBackToJWTWorkspaceID 验证 account_id 为 user-...
// （历史污染）时回退用 AT JWT 里的 chatgpt_account_id（真实工作区 UUID）。
func TestQueryChatGPTSubscription_FallsBackToJWTWorkspaceID(t *testing.T) {
	var gotAccountID string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAccountID = r.URL.Query().Get("account_id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plan_type":"plus","active_until":"2026-07-29T14:54:57Z","will_renew":true}`))
	}))
	defer server.Close()
	defer SetSubscriptionsURLForTest(server.URL)()

	payload, _ := json.Marshal(map[string]interface{}{
		"https://api.openai.com/auth": map[string]interface{}{
			"chatgpt_account_id": testWorkspaceUUID,
			"user_id":            "user-MpgpQk00uRgvPiLR1iMGAiaX",
			"chatgpt_plan_type":  "plus",
		},
	})
	jwt := "eyJhbGciOiJSUzI1NiJ9." + base64.RawURLEncoding.EncodeToString(payload) + ".fake_signature"

	account := &auth.Account{AccessToken: jwt, AccountID: "user-MpgpQk00uRgvPiLR1iMGAiaX"}
	sub, err := QueryChatGPTSubscription(context.Background(), account, "")
	if err != nil {
		t.Fatalf("QueryChatGPTSubscription: %v", err)
	}
	if sub.PlanType != "plus" {
		t.Fatalf("PlanType = %q, want plus", sub.PlanType)
	}
	if gotAccountID != testWorkspaceUUID {
		t.Fatalf("query account_id = %q, want JWT workspace uuid %q", gotAccountID, testWorkspaceUUID)
	}
}

// TestMaybeSyncSubscriptionExpiry_SyncsAndThrottles 验证：付费套餐 + 到期时间缺失时
// 从网页端同步 active_until 到内存与 DB；同一账号在节流间隔内不重复请求。
func TestMaybeSyncSubscriptionExpiry_SyncsAndThrottles(t *testing.T) {
	ctx := context.Background()
	activeUntil := time.Now().Add(20 * 24 * time.Hour).UTC().Truncate(time.Second)
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plan_type":"plus","active_until":"` + activeUntil.Format(time.RFC3339) + `","will_renew":true}`))
	}))
	defer server.Close()
	defer SetSubscriptionsURLForTest(server.URL)()

	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New: %v", err)
	}
	defer db.Close()
	id, err := db.InsertAccountWithCredentials(ctx, "subscription-sync", map[string]interface{}{"plan_type": "plus"}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials: %v", err)
	}

	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	account := &auth.Account{DBID: id, AccessToken: "test-at", PlanType: "plus", AccountID: testWorkspaceUUID}

	if !MaybeSyncSubscriptionExpiry(ctx, store, account, "") {
		t.Fatal("MaybeSyncSubscriptionExpiry should update expiry on first call")
	}
	if !account.SubscriptionExpiresAt.Equal(activeUntil) {
		t.Fatalf("SubscriptionExpiresAt = %v, want %v", account.SubscriptionExpiresAt, activeUntil)
	}
	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if got := row.GetCredential("subscription_expires_at"); got != activeUntil.Format(time.RFC3339) {
		t.Fatalf("persisted subscription_expires_at = %q, want %q", got, activeUntil.Format(time.RFC3339))
	}

	// 已有远期到期时间 + 节流窗口内：第二次调用不应再发请求。
	if MaybeSyncSubscriptionExpiry(ctx, store, account, "") {
		t.Fatal("second call should be a no-op")
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("upstream requests = %d, want 1 (throttled)", n)
	}
}

// TestMaybeSyncSubscriptionExpiry_SkipsPastActiveUntil 验证：上游返回已过去的
// active_until（宽限期/降级中）不写入，避免与陈旧值清理逻辑互相打架。
func TestMaybeSyncSubscriptionExpiry_SkipsPastActiveUntil(t *testing.T) {
	ctx := context.Background()
	past := time.Now().Add(-24 * time.Hour).UTC().Truncate(time.Second)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"plan_type":"plus","active_until":"` + past.Format(time.RFC3339) + `","will_renew":false}`))
	}))
	defer server.Close()
	defer SetSubscriptionsURLForTest(server.URL)()

	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	account := &auth.Account{DBID: 1, AccessToken: "test-at", PlanType: "plus", AccountID: testWorkspaceUUID}

	if MaybeSyncSubscriptionExpiry(ctx, store, account, "") {
		t.Fatal("past active_until should not be applied")
	}
	if !account.SubscriptionExpiresAt.IsZero() {
		t.Fatalf("SubscriptionExpiresAt = %v, want zero", account.SubscriptionExpiresAt)
	}
}

// Resin 启用时订阅到期查询也必须经反代（issue #372），指纹由 Resin 侧承担。
func TestQueryChatGPTSubscriptionRoutesThroughResin(t *testing.T) {
	var gotPath, gotResinAccount, gotAccountIDQuery string
	fakeResin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotResinAccount = r.Header.Get("X-Resin-Account")
		gotAccountIDQuery = r.URL.Query().Get("account_id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer fakeResin.Close()

	SetResinConfig(&ResinConfig{BaseURL: fakeResin.URL, PlatformName: "test"})
	t.Cleanup(func() { SetResinConfig(nil) })

	account := &auth.Account{DBID: 11, AccessToken: "at-11", AccountID: testWorkspaceUUID}
	if _, err := QueryChatGPTSubscription(context.Background(), account, ""); err != nil {
		t.Fatalf("QueryChatGPTSubscription error: %v", err)
	}
	if want := "/test/https/chatgpt.com/backend-api/subscriptions"; gotPath != want {
		t.Fatalf("resin path = %q, want %q", gotPath, want)
	}
	if gotResinAccount != "11" {
		t.Fatalf("X-Resin-Account = %q, want %q", gotResinAccount, "11")
	}
	if gotAccountIDQuery != testWorkspaceUUID {
		t.Fatalf("account_id query = %q, want workspace uuid", gotAccountIDQuery)
	}
}
