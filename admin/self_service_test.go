package admin

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestNormalizeContactEmail(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"user@example.com", "user@example.com", true},
		{"  User@Example.com  ", "User@Example.com", true},
		{"", "", false},
		{"not-an-email", "", false},
		{"a@b", "", false},
		{"a@@b.com", "", false},
		{"a b@x.com", "", false},
	}
	for _, tc := range cases {
		got, ok := normalizeContactEmail(tc.in)
		if ok != tc.ok || got != tc.want {
			t.Fatalf("normalizeContactEmail(%q) = (%q,%v), want (%q,%v)", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}

func TestSelfServiceRateLimiter(t *testing.T) {
	l := &selfServiceRateLimiter{hits: make(map[string][]time.Time)}
	base := time.Unix(1_700_000_000, 0)
	for i := 0; i < selfServiceRateLimit; i++ {
		if !l.allow("1.2.3.4", base) {
			t.Fatalf("请求 %d 应放行", i)
		}
	}
	if l.allow("1.2.3.4", base) {
		t.Fatal("超过上限应被限流")
	}
	// 另一 IP 不受影响
	if !l.allow("9.9.9.9", base) {
		t.Fatal("其他 IP 应放行")
	}
	// 窗口外恢复
	if !l.allow("1.2.3.4", base.Add(selfServiceRateWin+time.Second)) {
		t.Fatal("窗口过后应恢复放行")
	}
}

func newSelfServiceHandler(t *testing.T, enabled bool) (*Handler, *database.DB) {
	t.Helper()
	db := newTestAdminDB(t)
	tc := cache.NewMemory(1)
	t.Cleanup(func() { tc.Close() })
	store := auth.NewStore(db, tc, nil)
	handler := NewHandler(store, db, tc, nil, "admin-secret")

	ctx := context.Background()
	settings, err := db.GetSystemSettings(ctx)
	if err != nil {
		t.Fatalf("GetSystemSettings: %v", err)
	}
	if settings == nil {
		settings = &database.SystemSettings{}
	}
	settings.PublicAccountPortalPageEnabled = enabled
	if err := db.UpdateSystemSettings(ctx, settings); err != nil {
		t.Fatalf("UpdateSystemSettings: %v", err)
	}
	return handler, db
}

func TestAccountPortalDisabledReturns404(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, _ := newSelfServiceHandler(t, false)
	router := gin.New()
	handler.RegisterRoutes(router)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/account-portal/generate-auth-url",
		strings.NewReader(`{"contact_email":"user@example.com"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("门户关闭时应 404, got %d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAccountPortalGenerateAuthURL(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, _ := newSelfServiceHandler(t, true)
	router := gin.New()
	handler.RegisterRoutes(router)

	// 无效邮箱 → 400
	badRec := httptest.NewRecorder()
	badReq := httptest.NewRequest(http.MethodPost, "/api/account-portal/generate-auth-url",
		strings.NewReader(`{"contact_email":"nope"}`))
	badReq.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(badRec, badReq)
	if badRec.Code != http.StatusBadRequest {
		t.Fatalf("无效邮箱应 400, got %d", badRec.Code)
	}

	// 有效邮箱 → 200 且返回 auth_url + session_id
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/account-portal/generate-auth-url",
		strings.NewReader(`{"contact_email":"user@example.com"}`))
	req.Header.Set("Content-Type", "application/json")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("有效邮箱应 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "auth_url") || !strings.Contains(body, "session_id") {
		t.Fatalf("响应应含 auth_url 与 session_id: %s", body)
	}
	if !strings.Contains(body, "auth.openai.com/oauth/authorize") {
		t.Fatalf("auth_url 应指向 OpenAI 授权端点: %s", body)
	}
}

func TestUpsertSelfServiceAccountPendingAndNotScheduled(t *testing.T) {
	handler, db := newSelfServiceHandler(t, true)
	ctx := context.Background()

	seed := tokenCredentialSeed{
		refreshToken: "rt-abc",
		accessToken:  "at-abc",
		email:        "provider@example.com",
		accountID:    "acct-1",
		workspaceID:  "workspace-1",
	}
	id, err := handler.upsertSelfServiceAccount(ctx, "provider@example.com", "", seed, "contact@x.com")
	if err != nil {
		t.Fatalf("upsertSelfServiceAccount: %v", err)
	}
	if id <= 0 {
		t.Fatalf("id = %d, want > 0", id)
	}

	row, err := db.GetAccountByID(ctx, id)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if row.Enabled {
		t.Fatal("自助账号应为禁用（待审核）")
	}
	if !strings.Contains(row.Note, "contact@x.com") {
		t.Fatalf("note 应含联系人邮箱, got %q", row.Note)
	}
	hasTag := false
	for _, tag := range row.Tags {
		if tag == selfServiceTag {
			hasTag = true
		}
	}
	if !hasTag {
		t.Fatalf("应打 self-service 标签, tags=%v", row.Tags)
	}
	// 未进调度池
	if handler.store.FindByID(id) != nil {
		t.Fatal("待审核账号不应进入运行时调度池")
	}

	// 重复提交 → errDuplicateOAuthIdentity
	if _, err := handler.upsertSelfServiceAccount(ctx, "provider@example.com", "", seed, "contact2@x.com"); err != errDuplicateOAuthIdentity {
		t.Fatalf("重复提交应返回 errDuplicateOAuthIdentity, got %v", err)
	}
}

func TestApproveSelfServiceAccountLoadsIntoPool(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, db := newSelfServiceHandler(t, true)
	router := gin.New()
	handler.RegisterRoutes(router)
	ctx := context.Background()

	seed := tokenCredentialSeed{refreshToken: "rt-x", accessToken: "at-x", email: "p2@example.com", accountID: "acct-2", workspaceID: "workspace-2"}
	id, err := handler.upsertSelfServiceAccount(ctx, "p2@example.com", "", seed, "c@x.com")
	if err != nil {
		t.Fatalf("upsertSelfServiceAccount: %v", err)
	}
	if handler.store.FindByID(id) != nil {
		t.Fatal("批准前不应在池中")
	}

	// 管理员批准（enable=true）应把账号加载进调度池
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/api/admin/accounts/"+strconv.FormatInt(id, 10)+"/enable",
		strings.NewReader(`{"enabled":true}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Admin-Key", "admin-secret")
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("批准应 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	if handler.store.FindByID(id) == nil {
		t.Fatal("批准后账号应进入运行时调度池")
	}
	row, _ := db.GetAccountByID(ctx, id)
	if !row.Enabled {
		t.Fatal("批准后 enabled 应为 true")
	}
}
