package admin

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/internal/imagestore"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func TestRefreshAccountRejectsInvalidID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := &Handler{
		refreshAccount: func(context.Context, int64) error {
			t.Fatal("refresh should not be called for invalid id")
			return nil
		},
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: "bad-id"}}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/bad-id/refresh", nil)

	handler.RefreshAccount(ctx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}

	var payload map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := payload["error"]; got != "无效的账号 ID" {
		t.Fatalf("error = %q, want %q", got, "无效的账号 ID")
	}
}

func TestAccountEmailDomain(t *testing.T) {
	tests := []struct {
		name  string
		email string
		want  string
	}{
		{name: "lowercases domain", email: "User@Example.COM", want: "example.com"},
		{name: "trims whitespace", email: " user@mail.example.com ", want: "mail.example.com"},
		{name: "rejects missing at", email: "https://api.openai.com", want: ""},
		{name: "rejects blank local", email: "@example.com", want: ""},
		{name: "rejects malformed domain", email: "user@example com", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := accountEmailDomain(tt.email); got != tt.want {
				t.Fatalf("accountEmailDomain(%q) = %q, want %q", tt.email, got, tt.want)
			}
		})
	}
}

func TestAccountAccessTokenTypeInfersCodexAT(t *testing.T) {
	row := &database.AccountRow{Credentials: map[string]interface{}{
		"access_token": "at-opaque-codex-token",
	}}
	if got := accountAccessTokenType(row); got != accessTokenTypeCodexAT {
		t.Fatalf("accountAccessTokenType() = %q, want %q", got, accessTokenTypeCodexAT)
	}

	row.Credentials["access_token_type"] = "custom"
	if got := accountAccessTokenType(row); got != "custom" {
		t.Fatalf("accountAccessTokenType() with stored type = %q, want custom", got)
	}
}

func TestSummarizeDashboardAccountsMatchesAccountPageBuckets(t *testing.T) {
	rows := []*database.AccountRow{
		{ID: 1, Status: "error", Enabled: true},   // DB stale, runtime active
		{ID: 2, Status: "active", Enabled: true},  // runtime unauthorized
		{ID: 3, Status: "active", Enabled: false}, // disabled
		{ID: 4, Status: "active", Enabled: true},  // runtime rate limited
		{ID: 5, Status: "active", Enabled: true},  // normal
		{ID: 6, Status: "error", Enabled: true},   // DB error without runtime override
		{ID: 7, Status: "cooldown", Enabled: true, CooldownReason: "rate_limited"},
	}

	activeFromStaleDB := &auth.Account{DBID: 1, Status: auth.StatusReady, AccessToken: "at-1"}
	unauthorized := &auth.Account{DBID: 2, Status: auth.StatusReady, AccessToken: "at-2"}
	unauthorized.SetCooldownWithReason(time.Hour, "unauthorized")
	disabled := &auth.Account{DBID: 3, Status: auth.StatusReady, AccessToken: "at-3"}
	rateLimited := &auth.Account{DBID: 4, Status: auth.StatusReady, AccessToken: "at-4"}
	rateLimited.SetCooldownWithReason(time.Hour, "rate_limited")
	normal := &auth.Account{DBID: 5, Status: auth.StatusReady, AccessToken: "at-5"}

	got, _ := summarizeDashboardAccounts(rows, []*auth.Account{
		activeFromStaleDB,
		unauthorized,
		disabled,
		rateLimited,
		normal,
	})

	if got.total != 7 || got.normal != 3 || got.rateLimited != 2 || got.abnormal != 2 || got.disabled != 1 {
		t.Fatalf("counts = %+v, want total=7 normal=3 rateLimited=2 abnormal=2 disabled=1", got)
	}
}

func TestNormalizeBackgroundUploadMedia(t *testing.T) {
	tests := []struct {
		name        string
		filename    string
		contentType string
		data        []byte
		wantMime    string
		wantExt     string
		wantErr     bool
	}{
		{
			name:        "png by content",
			filename:    "wallpaper.png",
			contentType: "application/octet-stream",
			data:        []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n', 0, 0, 0, 0},
			wantMime:    "image/png",
			wantExt:     "png",
		},
		{
			name:        "svg by extension",
			filename:    "wallpaper.svg",
			contentType: "image/svg+xml",
			data:        []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1 1"></svg>`),
			wantMime:    "image/svg+xml",
			wantExt:     "svg",
		},
		{
			name:        "mp4 by signature",
			filename:    "wallpaper.mp4",
			contentType: "video/mp4",
			data:        []byte{0, 0, 0, 24, 'f', 't', 'y', 'p', 'm', 'p', '4', '2'},
			wantMime:    "video/mp4",
			wantExt:     "mp4",
		},
		{
			name:        "reject fake mp4",
			filename:    "wallpaper.mp4",
			contentType: "video/mp4",
			data:        []byte("not actually an mp4 file"),
			wantErr:     true,
		},
		{
			name:        "reject html",
			filename:    "wallpaper.png",
			contentType: "image/png",
			data:        []byte("<html><script>alert(1)</script></html>"),
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotMime, gotExt, err := normalizeBackgroundUploadMedia(tt.filename, tt.contentType, tt.data)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotMime != tt.wantMime || gotExt != tt.wantExt {
				t.Fatalf("mime/ext = %q/%q, want %q/%q", gotMime, gotExt, tt.wantMime, tt.wantExt)
			}
		})
	}
}

func TestDecodeBackgroundConfigGlassDefaultsAndClamp(t *testing.T) {
	defaulted := decodeBackgroundConfig(`{"image":"/wallpaper.jpg"}`)
	if defaulted.Opacity != defaultBackgroundOpacity {
		t.Fatalf("default opacity = %d, want %d", defaulted.Opacity, defaultBackgroundOpacity)
	}
	if defaulted.GlassOpacity != defaultBackgroundGlassOpacity {
		t.Fatalf("default glass opacity = %d, want %d", defaulted.GlassOpacity, defaultBackgroundGlassOpacity)
	}
	if defaulted.GlassBlur != defaultBackgroundGlassBlur {
		t.Fatalf("default glass blur = %d, want %d", defaulted.GlassBlur, defaultBackgroundGlassBlur)
	}

	clamped := decodeBackgroundConfig(`{"image":"/wallpaper.jpg","opacity":200,"blur":200,"glass_opacity":200,"glass_blur":200}`)
	if clamped.Opacity != 100 {
		t.Fatalf("clamped opacity = %d, want 100", clamped.Opacity)
	}
	if clamped.Blur != maxBackgroundBlur {
		t.Fatalf("clamped blur = %d, want %d", clamped.Blur, maxBackgroundBlur)
	}
	if clamped.GlassOpacity != 100 {
		t.Fatalf("clamped glass opacity = %d, want 100", clamped.GlassOpacity)
	}
	if clamped.GlassBlur != maxBackgroundGlassBlur {
		t.Fatalf("clamped glass blur = %d, want %d", clamped.GlassBlur, maxBackgroundGlassBlur)
	}

	transparent := decodeBackgroundConfig(`{"image":"/wallpaper.jpg","glass_opacity":0,"glass_blur":0}`)
	if transparent.GlassOpacity != 0 || transparent.GlassBlur != 0 {
		t.Fatalf("transparent glass = %d/%d, want 0/0", transparent.GlassOpacity, transparent.GlassBlur)
	}
}

func TestBackgroundUploadLimitBytes(t *testing.T) {
	if got := backgroundUploadLimitBytes("image/png"); got != maxBackgroundImageAssetUploadBytes {
		t.Fatalf("image upload limit = %d, want %d", got, maxBackgroundImageAssetUploadBytes)
	}
	if got := backgroundUploadLimitBytes("video/mp4"); got != maxBackgroundVideoAssetUploadBytes {
		t.Fatalf("video upload limit = %d, want %d", got, maxBackgroundVideoAssetUploadBytes)
	}
	if maxBackgroundVideoAssetUploadBytes != 40*1024*1024 {
		t.Fatalf("video upload limit = %d, want 40MB", maxBackgroundVideoAssetUploadBytes)
	}
}

func TestUploadBackgroundAssetStoresFile(t *testing.T) {
	gin.SetMode(gin.TestMode)
	t.Setenv("BACKGROUND_ASSET_DIR", t.TempDir())

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	part, err := writer.CreateFormFile("file", "wallpaper.svg")
	if err != nil {
		t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 1 1"></svg>`)); err != nil {
		t.Fatalf("write form file: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close multipart writer: %v", err)
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/settings/background-upload", &body)
	ctx.Request.Header.Set("Content-Type", writer.FormDataContentType())

	handler := &Handler{}
	handler.UploadBackgroundAsset(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}

	var payload backgroundAssetUploadResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !strings.HasPrefix(payload.URL, backgroundAssetURLPrefix) {
		t.Fatalf("url = %q, want prefix %q", payload.URL, backgroundAssetURLPrefix)
	}
	if payload.MimeType != "image/svg+xml" || payload.Bytes <= 0 {
		t.Fatalf("payload = %+v, want svg mime and non-empty bytes", payload)
	}

	fullPath, ok := backgroundAssetPath(strings.TrimPrefix(payload.URL, backgroundAssetURLPrefix))
	if !ok {
		t.Fatalf("invalid stored asset path for url %q", payload.URL)
	}
	if _, err := os.Stat(fullPath); err != nil {
		t.Fatalf("stored file missing: %v", err)
	}
}

func TestRefreshAccountRunsSingleRefresh(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var called bool
	var gotID int64
	handler := &Handler{
		refreshAccount: func(_ context.Context, id int64) error {
			called = true
			gotID = id
			return nil
		},
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: "42"}}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/42/refresh", nil)

	handler.RefreshAccount(ctx)

	if !called {
		t.Fatal("expected refresh to be called")
	}
	if gotID != 42 {
		t.Fatalf("refresh id = %d, want %d", gotID, 42)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	var payload map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := payload["message"]; got != "账号刷新成功" {
		t.Fatalf("message = %q, want %q", got, "账号刷新成功")
	}
}

func TestRefreshAccountReturnsNotFoundForMissingAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := &Handler{
		refreshAccount: func(context.Context, int64) error {
			return errors.New("账号 7 不存在")
		},
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: "7"}}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/7/refresh", nil)

	handler.RefreshAccount(ctx)

	if recorder.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusNotFound)
	}

	var payload map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := payload["error"]; got != "账号 7 不存在" {
		t.Fatalf("error = %q, want %q", got, "账号 7 不存在")
	}
}

func TestRefreshAccountReturnsRefreshFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := &Handler{
		refreshAccount: func(context.Context, int64) error {
			return errors.New("upstream unavailable")
		},
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: "9"}}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/9/refresh", nil)

	handler.RefreshAccount(ctx)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusInternalServerError)
	}

	var payload map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := payload["error"]; got != "刷新失败: upstream unavailable" {
		t.Fatalf("error = %q, want %q", got, "刷新失败: upstream unavailable")
	}
}

func TestBatchRefreshAccountsReportsCounts(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := &Handler{
		refreshAccount: func(_ context.Context, id int64) error {
			if id == 8 {
				return errors.New("upstream unavailable")
			}
			return nil
		},
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/batch-refresh", strings.NewReader(`{"ids":[7,8,7,0]}`))

	handler.BatchRefreshAccounts(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	var payload map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := payload["success"]; got != float64(1) {
		t.Fatalf("success = %v, want 1", got)
	}
	if got := payload["failed"]; got != float64(1) {
		t.Fatalf("failed = %v, want 1", got)
	}
}

func TestBatchRefreshAccountsStreamsProgress(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := &Handler{
		refreshAccount: func(_ context.Context, id int64) error {
			if id == 8 {
				return errors.New("账号 8 不存在")
			}
			return nil
		},
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/batch-refresh?stream=true", strings.NewReader(`{"ids":[7,8]}`))

	handler.BatchRefreshAccounts(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("content-type = %q, want event-stream", got)
	}
	body := recorder.Body.String()
	for _, want := range []string{
		`"type":"start"`,
		`"type":"progress"`,
		`"type":"complete"`,
		`"action":"batch_refresh"`,
		`"success":1`,
		`"failed":1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream body missing %s:\n%s", want, body)
		}
	}
}

func TestResetAccountStatusSyncsPlanMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := auth.NewStore(nil, nil, nil)
	account := &auth.Account{DBID: 42, AccessToken: "at", PlanType: "free"}
	account.SetUsageSnapshot(88, time.Now().Add(time.Hour))
	store.AddAccount(account)

	called := make(chan int64, 1)
	handler := &Handler{
		store: store,
		syncAccountPlanOnReset: func(_ context.Context, acc *auth.Account) error {
			if acc == nil {
				t.Errorf("sync account is nil")
				called <- -1
				return nil
			}
			called <- acc.DBID
			return nil
		},
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: "42"}}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/42/reset-status", nil)

	handler.ResetAccountStatus(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	select {
	case id := <-called:
		if id != 42 {
			t.Fatalf("sync DBID = %d, want 42", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("expected reset to sync plan metadata")
	}
	if _, ok := account.GetUsagePercent7d(); ok {
		t.Fatal("expected reset to clear cached usage")
	}
}

func TestBatchResetStatusSyncsEachResolvedAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := auth.NewStore(nil, nil, nil)
	store.AddAccount(&auth.Account{DBID: 11, AccessToken: "at-11", PlanType: "free"})
	store.AddAccount(&auth.Account{DBID: 22, AccessToken: "at-22", PlanType: "plus"})

	gotIDs := make(chan int64, 2)
	handler := &Handler{
		store: store,
		syncAccountPlanOnReset: func(_ context.Context, acc *auth.Account) error {
			gotIDs <- acc.DBID
			if acc.DBID == 22 {
				return errors.New("temporary upstream failure")
			}
			return nil
		},
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/batch-reset-status", strings.NewReader(`{"ids":[11,99,22]}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.BatchResetStatus(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload["success"] != float64(2) || payload["failed"] != float64(1) {
		t.Fatalf("payload = %#v, want success=2 failed=1", payload)
	}

	collected := make(map[int64]bool)
	deadline := time.After(2 * time.Second)
	for len(collected) < 2 {
		select {
		case id := <-gotIDs:
			collected[id] = true
		case <-deadline:
			t.Fatalf("synced ids = %v, want {11,22}", collected)
		}
	}
	if !collected[11] || !collected[22] {
		t.Fatalf("synced ids = %v, want both 11 and 22", collected)
	}
}

func TestCreateAPIKeyPersistsQuotaAndExpiration(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New 返回错误: %v", err)
	}
	defer db.Close()

	handler := &Handler{db: db}
	body := `{"name":"Client A","key":"sk-test-client-a-1234567890","quota_limit":0.25,"expires_in_days":7}`
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/keys", strings.NewReader(body))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.CreateAPIKey(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	var payload createAPIKeyResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.ID <= 0 || payload.QuotaLimit != 0.25 || payload.ExpiresAt == nil {
		t.Fatalf("payload = %#v, want quota and expiration", payload)
	}

	row, err := db.GetAPIKeyByValue(context.Background(), "sk-test-client-a-1234567890")
	if err != nil {
		t.Fatalf("GetAPIKeyByValue 返回错误: %v", err)
	}
	if row.QuotaLimit != 0.25 || !row.ExpiresAt.Valid {
		t.Fatalf("row = %#v, want quota and expiration", row)
	}
}

func TestUpdateAPIKeyPreservesOmittedFieldsAndUpdatesLimits(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New 返回错误: %v", err)
	}
	defer db.Close()

	expiresAt := sql.NullTime{Time: time.Now().AddDate(0, 0, 3), Valid: true}
	id, err := db.InsertAPIKeyWithOptions(context.Background(), database.APIKeyInput{
		Name:       "Client A",
		Key:        "sk-test-update-client-1234567890",
		QuotaLimit: 0.25,
		ExpiresAt:  expiresAt,
	})
	if err != nil {
		t.Fatalf("InsertAPIKeyWithOptions 返回错误: %v", err)
	}

	handler := &Handler{db: db}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", id)}}
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/api/admin/keys/1", strings.NewReader(`{"name":"Client B"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateAPIKey(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	row, err := db.GetAPIKeyByID(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAPIKeyByID 返回错误: %v", err)
	}
	if row.Name != "Client B" || row.QuotaLimit != 0.25 || !row.ExpiresAt.Valid {
		t.Fatalf("row = %#v, want renamed with quota/expiration preserved", row)
	}

	recorder = httptest.NewRecorder()
	ctx, _ = gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", id)}}
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/api/admin/keys/1", strings.NewReader(`{"quota_limit":0,"expires_at":null}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateAPIKey(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	row, err = db.GetAPIKeyByID(context.Background(), id)
	if err != nil {
		t.Fatalf("GetAPIKeyByID 返回错误: %v", err)
	}
	if row.Name != "Client B" || row.QuotaLimit != 0 || row.ExpiresAt.Valid {
		t.Fatalf("row = %#v, want quota/expiration cleared with name preserved", row)
	}
}

func TestUpdateAPIKeyRefreshesRuntimeStoreAndCache(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("database.New 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	groupID, err := db.CreateAccountGroup(ctx, "Team", "", "#2563eb", 0, 0, sql.NullInt64{}, 0)
	if err != nil {
		t.Fatalf("CreateAccountGroup 返回错误: %v", err)
	}
	key := "sk-test-runtime-refresh-1234567890"
	keyID, err := db.InsertAPIKey(ctx, "Client A", key)
	if err != nil {
		t.Fatalf("InsertAPIKey 返回错误: %v", err)
	}
	store := auth.NewStore(nil, nil, nil)
	tc := cache.NewMemory(1)
	handler := &Handler{db: db, store: store, cache: tc}
	payload, err := json.Marshal(map[string]interface{}{
		"id":         keyID,
		"name":       "Client A",
		"created_at": time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("marshal runtime cache: %v", err)
	}
	if err := tc.SetRuntime(ctx, adminAPIKeyCacheNamespace, key, payload, time.Minute); err != nil {
		t.Fatalf("SetRuntime api key: %v", err)
	}

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", keyID)}}
	ginCtx.Request = httptest.NewRequest(http.MethodPatch, "/api/admin/keys/1", strings.NewReader(fmt.Sprintf(`{"allowed_group_ids":[%d]}`, groupID)))
	ginCtx.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateAPIKey(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := store.GetAPIKeyAllowedGroups(keyID); len(got) != 1 || got[0] != groupID {
		t.Fatalf("runtime store allowed groups = %v, want [%d]", got, groupID)
	}
	if _, ok, err := tc.GetRuntime(ctx, adminAPIKeyCacheNamespace, key); err != nil || ok {
		t.Fatalf("runtime api key cache after update ok=%v err=%v, want miss", ok, err)
	}
	if _, ok, err := tc.GetRuntime(ctx, adminAPIKeyCountNamespace, "all"); err != nil || ok {
		t.Fatalf("runtime api key count cache after update ok=%v err=%v, want miss", ok, err)
	}
}

func TestGetAccountAuthJSONRejectsInvalidID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := &Handler{}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: "bad-id"}}
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/bad-id/auth-json", nil)

	handler.GetAccountAuthJSON(ctx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	assertErrorMessage(t, recorder, "无效的账号 ID")
}

func TestGetAccountAuthJSONReturnsCodexAuthFile(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	if err := db.UpdateCredentials(context.Background(), accountID, map[string]interface{}{
		"id_token":     "id_test",
		"access_token": "access_test",
		"workspace_id": "account_test",
	}); err != nil {
		t.Fatalf("seed credentials: %v", err)
	}
	handler := &Handler{db: db}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", accountID)}}
	ctx.Request = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/admin/accounts/%d/auth-json", accountID), nil)

	handler.GetAccountAuthJSON(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Disposition"); got != `attachment; filename="auth.json"` {
		t.Fatalf("Content-Disposition = %q, want auth.json attachment", got)
	}

	var payload struct {
		AuthMode     string  `json:"auth_mode"`
		OpenAIAPIKey *string `json:"OPENAI_API_KEY"`
		Tokens       struct {
			IDToken      string `json:"id_token"`
			AccessToken  string `json:"access_token"`
			RefreshToken string `json:"refresh_token"`
			AccountID    string `json:"account_id"`
		} `json:"tokens"`
		LastRefresh string `json:"last_refresh"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.AuthMode != "chatgpt" {
		t.Fatalf("auth_mode = %q, want chatgpt", payload.AuthMode)
	}
	if payload.OpenAIAPIKey != nil {
		t.Fatalf("OPENAI_API_KEY = %q, want null", *payload.OpenAIAPIKey)
	}
	if payload.Tokens.IDToken != "id_test" || payload.Tokens.AccessToken != "access_test" || payload.Tokens.RefreshToken != "rt_test" || payload.Tokens.AccountID != "account_test" {
		t.Fatalf("tokens = %+v, want seeded credentials", payload.Tokens)
	}
	if payload.LastRefresh == "" {
		t.Fatal("last_refresh is empty")
	}
}

func TestGetAccountAuthJSONRejectsIncompleteTokens(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	handler := &Handler{db: db}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", accountID)}}
	ctx.Request = httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/admin/accounts/%d/auth-json", accountID), nil)

	handler.GetAccountAuthJSON(ctx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	assertErrorMessage(t, recorder, "账号缺少 access_token 或 id_token，请先刷新账号后再生成 auth.json")
}

func TestGetUsageLogsRejectsInvalidAPIKeyID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := &Handler{}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/admin/usage/logs?start=2026-01-01T00:00:00Z&end=2026-01-02T00:00:00Z&page=1&api_key_id=bad", nil)

	handler.GetUsageLogs(ctx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}

	var payload map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := payload["error"]; got != "api_key_id 参数无效，需要正整数" {
		t.Fatalf("error = %q, want %q", got, "api_key_id 参数无效，需要正整数")
	}
}

func TestGetUsageLogsAllowsFiveHundredPageSize(t *testing.T) {
	gin.SetMode(gin.TestMode)

	dbPath := filepath.Join(t.TempDir(), "usage-logs-page-size.sqlite")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("new test db: %v", err)
	}
	dbClosed := false
	t.Cleanup(func() {
		if !dbClosed {
			_ = db.Close()
		}
		_ = os.Remove(dbPath)
	})

	db.SetUsageLogConfig(database.UsageLogModeFull, 1000, 3600)
	accountID := insertTestAccount(t, db)
	ctx := context.Background()

	baseTime := time.Date(2026, 6, 4, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 501; i++ {
		if err := db.InsertUsageLog(ctx, &database.UsageLogInput{
			AccountID:  accountID,
			Endpoint:   fmt.Sprintf("/v1/log-%03d", i),
			Model:      "gpt-5.4",
			StatusCode: http.StatusOK,
		}); err != nil {
			t.Fatalf("InsertUsageLog %d returned error: %v", i, err)
		}
	}
	dbClosed = true
	if err := db.Close(); err != nil {
		t.Fatalf("flush usage logs: %v", err)
	}

	rawDB, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw sqlite db: %v", err)
	}
	if _, err := rawDB.ExecContext(ctx, `
		UPDATE usage_logs
		SET created_at = datetime(?, printf('+%d seconds', id - 1))
	`, baseTime.Format("2006-01-02 15:04:05")); err != nil {
		_ = rawDB.Close()
		t.Fatalf("update created_at: %v", err)
	}
	if err := rawDB.Close(); err != nil {
		t.Fatalf("close raw sqlite db: %v", err)
	}

	db, err = database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopen test db: %v", err)
	}
	dbClosed = false
	handler := &Handler{db: db}

	start := baseTime.Add(-time.Minute).Format(time.RFC3339)
	end := baseTime.Add(502 * time.Second).Format(time.RFC3339)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/api/admin/usage/logs?start="+start+"&end="+end+"&page=2&page_size=500", nil)

	handler.GetUsageLogs(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	var payload database.UsageLogPage
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Total != 501 {
		t.Fatalf("total = %d, want 501", payload.Total)
	}
	if len(payload.Logs) != 1 {
		t.Fatalf("len(logs) = %d, want 1", len(payload.Logs))
	}
	if got := payload.Logs[0].Endpoint; got != "/v1/log-000" {
		t.Fatalf("endpoint = %q, want /v1/log-000", got)
	}
}

func TestRuntimeStatusRouteReturnsDependencySnapshot(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	tc := cache.NewMemory(4)
	defer tc.Close()
	store := auth.NewStore(db, tc, nil)
	imageDir := t.TempDir()
	if err := imagestore.Configure(imagestore.Config{Backend: imagestore.BackendLocal, LocalDir: imageDir}); err != nil {
		t.Fatalf("imagestore.Configure: %v", err)
	}

	handler := NewHandler(store, db, tc, nil, "admin-secret")
	router := gin.New()
	handler.RegisterRoutes(router)

	req := httptest.NewRequest(http.MethodGet, "/api/admin/runtime-status", nil)
	req.Header.Set("X-Admin-Key", "admin-secret")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, req)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	var payload runtimeStatusResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Status != runtimeStatusDegraded {
		t.Fatalf("status = %q, want %q for empty account pool", payload.Status, runtimeStatusDegraded)
	}
	if !payload.Database.Healthy || payload.Database.Driver != "sqlite" {
		t.Fatalf("database = healthy:%v driver:%q, want healthy sqlite", payload.Database.Healthy, payload.Database.Driver)
	}
	if !payload.Cache.Healthy || payload.Cache.Driver != "memory" {
		t.Fatalf("cache = healthy:%v driver:%q, want healthy memory", payload.Cache.Healthy, payload.Cache.Driver)
	}
	if !payload.AdminAuth.Configured || payload.AdminAuth.Source != "env" {
		t.Fatalf("admin auth = configured:%v source:%q, want env configured", payload.AdminAuth.Configured, payload.AdminAuth.Source)
	}
	if payload.ImageStorage.Backend != imagestore.BackendLocal || payload.ImageStorage.LocalDir != imageDir {
		t.Fatalf("image storage = %q %q, want local %q", payload.ImageStorage.Backend, payload.ImageStorage.LocalDir, imageDir)
	}
	if payload.UsageLog.Mode != database.UsageLogModeFull || !payload.UsageLog.Enabled {
		t.Fatalf("usage log = mode:%q enabled:%v, want full enabled", payload.UsageLog.Mode, payload.UsageLog.Enabled)
	}
}

func TestUpdateSettingsPersistsAutoResetCreditsAcrossPartialUpdates(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousRuntime := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previousRuntime) })

	db := newTestAdminDB(t)
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	settings := defaultBootstrapSettings()
	settings.ModelPricingOverrides = `{"gpt-5":{"input":1}}`
	settings.ModelPricingSyncURL = "https://example.com/pricing.json"
	if err := db.UpdateSystemSettings(context.Background(), settings); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	store := auth.NewStore(db, tc, settings)
	t.Cleanup(store.Stop)
	proxy.ApplyRuntimeSettingsFromSystem(settings)
	handler := NewHandler(store, db, tc, proxy.NewRateLimiter(settings.GlobalRPM), "admin-secret")

	update := func(body string) {
		t.Helper()
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")

		handler.UpdateSettings(ctx)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
		}
	}

	// 阈值必须能在开关关闭时先保存，避免启用后的即时扫描使用旧默认值。
	update(`{"auto_reset_credits_before_expiry_min":90}`)
	select {
	case <-handler.autoResetCreditsWake:
	default:
		t.Fatal("threshold change did not queue an immediate scan")
	}
	beforeEnable := proxy.CurrentRuntimeSettings()
	if beforeEnable.AutoResetCreditsEnabled {
		t.Fatal("AutoResetCreditsEnabled = true before explicit enable")
	}
	if beforeEnable.AutoResetCreditsBeforeExpiryMin != 90 {
		t.Fatalf("runtime threshold before enable = %d, want 90", beforeEnable.AutoResetCreditsBeforeExpiryMin)
	}
	update(`{"auto_reset_credits_enabled":true}`)
	select {
	case <-handler.autoResetCreditsWake:
	default:
		t.Fatal("enable change did not queue an immediate scan")
	}
	update(`{"auto_reset_credits_enabled":true,"auto_reset_credits_before_expiry_min":90}`)
	select {
	case <-handler.autoResetCreditsWake:
		t.Fatal("same-value auto-reset settings queued another scan")
	default:
	}
	update(`{"site_name":"Codex2API Test"}`)
	for _, boundary := range []int{10, 10080, 90} {
		update(fmt.Sprintf(`{"auto_reset_credits_before_expiry_min":%d}`, boundary))
		if got := proxy.CurrentRuntimeSettings().AutoResetCreditsBeforeExpiryMin; got != boundary {
			t.Fatalf("runtime boundary = %d, want %d", got, boundary)
		}
		select {
		case <-handler.autoResetCreditsWake:
		default:
			t.Fatalf("boundary %d did not queue an immediate scan", boundary)
		}
	}

	persisted, err := db.GetSystemSettings(context.Background())
	if err != nil {
		t.Fatalf("GetSystemSettings: %v", err)
	}
	if persisted == nil {
		t.Fatal("GetSystemSettings returned nil")
	}
	if !persisted.AutoResetCreditsEnabled {
		t.Fatal("AutoResetCreditsEnabled = false, want true after unrelated partial update")
	}
	if persisted.AutoResetCreditsBeforeExpiryMin != 90 {
		t.Fatalf("AutoResetCreditsBeforeExpiryMin = %d, want 90", persisted.AutoResetCreditsBeforeExpiryMin)
	}
	if persisted.ModelPricingOverrides != settings.ModelPricingOverrides {
		t.Fatalf("ModelPricingOverrides = %q, want %q", persisted.ModelPricingOverrides, settings.ModelPricingOverrides)
	}
	if persisted.ModelPricingSyncURL != settings.ModelPricingSyncURL {
		t.Fatalf("ModelPricingSyncURL = %q, want %q", persisted.ModelPricingSyncURL, settings.ModelPricingSyncURL)
	}
}

func TestUpdateSettingsResponseIncludesRetrySettings(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	settings := defaultBootstrapSettings()
	if err := db.UpdateSystemSettings(context.Background(), settings); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	store := auth.NewStore(db, tc, settings)
	t.Cleanup(store.Stop)
	handler := NewHandler(store, db, tc, proxy.NewRateLimiter(settings.GlobalRPM), "admin-secret")

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(
		http.MethodPut,
		"/api/admin/settings",
		strings.NewReader(`{"retry_interval_ms":2500,"transport_retry_policy":"sticky"}`),
	)
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateSettings(ctx)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	var response settingsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.RetryIntervalMS != 2500 {
		t.Fatalf("retry_interval_ms = %d, want 2500", response.RetryIntervalMS)
	}
	if response.TransportRetryPolicy != "sticky" {
		t.Fatalf("transport_retry_policy = %q, want sticky", response.TransportRetryPolicy)
	}
}

func TestUpdateSettingsRejectsAutoResetCreditsWindowOutOfRange(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := &Handler{}

	for _, value := range []int{9, 10081} {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		body := fmt.Sprintf(`{"auto_reset_credits_before_expiry_min":%d}`, value)
		ctx.Request = httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")

		handler.UpdateSettings(ctx)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("value=%d status=%d, want %d body=%s", value, recorder.Code, http.StatusBadRequest, recorder.Body.String())
		}
	}
}

func TestUpdateSettingsDoesNotEnableAutoResetCreditsWhenPersistenceFails(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousRuntime := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previousRuntime) })

	db := newTestAdminDB(t)
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	settings := defaultBootstrapSettings()
	store := auth.NewStore(db, tc, settings)
	t.Cleanup(store.Stop)
	proxy.ApplyRuntimeSettingsFromSystem(settings)
	handler := NewHandler(store, db, tc, proxy.NewRateLimiter(settings.GlobalRPM), "admin-secret")

	requestCtx, cancel := context.WithCancel(context.Background())
	cancel()
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	request := httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(`{"auto_reset_credits_enabled":true}`))
	ctx.Request = request.WithContext(requestCtx)
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateSettings(ctx)
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d, want %d body=%s", recorder.Code, http.StatusInternalServerError, recorder.Body.String())
	}
	if current := proxy.CurrentRuntimeSettings(); current.AutoResetCreditsEnabled {
		t.Fatal("AutoResetCreditsEnabled became true after persistence failure")
	}
}

func TestAutoResetCreditsSettingsUseDatabaseAuthorityOnStaleInstance(t *testing.T) {
	gin.SetMode(gin.TestMode)

	previousRuntime := proxy.CurrentRuntimeSettings()
	t.Cleanup(func() { proxy.ApplyRuntimeSettings(previousRuntime) })

	db := newTestAdminDB(t)
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	settings := defaultBootstrapSettings()
	settings.AutoResetCreditsEnabled = false
	settings.AutoResetCreditsBeforeExpiryMin = 60
	if err := db.UpdateSystemSettings(context.Background(), settings); err != nil {
		t.Fatalf("seed settings: %v", err)
	}
	store := auth.NewStore(db, tc, settings)
	t.Cleanup(store.Stop)
	handler := NewHandler(store, db, tc, proxy.NewRateLimiter(settings.GlobalRPM), "admin-secret")

	staleRuntime := proxy.DefaultRuntimeSettings()
	staleRuntime.AutoResetCreditsEnabled = true
	staleRuntime.AutoResetCreditsBeforeExpiryMin = 90
	proxy.ApplyRuntimeSettings(staleRuntime)

	getRecorder := httptest.NewRecorder()
	getCtx, _ := gin.CreateTestContext(getRecorder)
	getCtx.Request = httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil)
	handler.GetSettings(getCtx)
	if getRecorder.Code != http.StatusOK {
		t.Fatalf("GET status=%d body=%s", getRecorder.Code, getRecorder.Body.String())
	}
	var response settingsResponse
	if err := json.Unmarshal(getRecorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode GET settings: %v", err)
	}
	if response.AutoResetCreditsEnabled || response.AutoResetCreditsBeforeExpiryMin != 60 {
		t.Fatalf("GET auto settings=(%v,%d), want DB authority (false,60)", response.AutoResetCreditsEnabled, response.AutoResetCreditsBeforeExpiryMin)
	}

	updateRecorder := httptest.NewRecorder()
	updateCtx, _ := gin.CreateTestContext(updateRecorder)
	updateCtx.Request = httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(`{"site_name":"Stale Replica"}`))
	updateCtx.Request.Header.Set("Content-Type", "application/json")
	handler.UpdateSettings(updateCtx)
	if updateRecorder.Code != http.StatusOK {
		t.Fatalf("PUT status=%d body=%s", updateRecorder.Code, updateRecorder.Body.String())
	}

	persisted, err := db.GetSystemSettings(context.Background())
	if err != nil {
		t.Fatalf("GetSystemSettings: %v", err)
	}
	if persisted.AutoResetCreditsEnabled || persisted.AutoResetCreditsBeforeExpiryMin != 60 {
		t.Fatalf("persisted auto settings=(%v,%d), want (false,60)", persisted.AutoResetCreditsEnabled, persisted.AutoResetCreditsBeforeExpiryMin)
	}
	current := proxy.CurrentRuntimeSettings()
	if current.AutoResetCreditsEnabled || current.AutoResetCreditsBeforeExpiryMin != 60 {
		t.Fatalf("runtime auto settings=(%v,%d), want refreshed DB authority (false,60)", current.AutoResetCreditsEnabled, current.AutoResetCreditsBeforeExpiryMin)
	}
	select {
	case <-handler.autoResetCreditsWake:
		t.Fatal("unrelated stale-instance update queued an auto-reset scan")
	default:
	}
}

func TestUpdateAccountSchedulerRejectsInvalidID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := &Handler{}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: "bad-id"}}
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/api/admin/accounts/bad-id/scheduler", http.NoBody)

	handler.UpdateAccountScheduler(ctx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	assertErrorMessage(t, recorder, "无效的账号 ID")
}

func TestUpdateAccountSchedulerRejectsInvalidBody(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	handler := &Handler{db: db}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: "1"}}
	ctx.Request = httptest.NewRequest(http.MethodPatch, "/api/admin/accounts/1/scheduler", strings.NewReader(`{"score_bias_override":"abc"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateAccountScheduler(ctx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	assertErrorMessage(t, recorder, "score_bias_override 必须是整数或 null")
}

func TestUpdateAccountSchedulerRejectsInvalidSkipWarmTier(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	handler := &Handler{db: db}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", accountID)}}
	ctx.Request = httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/admin/accounts/%d/scheduler", accountID), strings.NewReader(`{"skip_warm_tier":"yes"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateAccountScheduler(ctx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	assertErrorMessage(t, recorder, "skip_warm_tier 必须是布尔值或 null")
}

func TestUpdateAccountSchedulerRejectsInvalidAllowedAPIKeyIDs(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	handler := &Handler{db: db}

	testCases := []struct {
		name    string
		body    string
		message string
	}{
		{
			name:    "invalid type",
			body:    `{"allowed_api_key_ids":"abc"}`,
			message: "allowed_api_key_ids 必须是整数数组或 null",
		},
		{
			name:    "non positive",
			body:    `{"allowed_api_key_ids":[0]}`,
			message: "allowed_api_key_ids 中的值必须是正整数",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", accountID)}}
			ctx.Request = httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/admin/accounts/%d/scheduler", accountID), strings.NewReader(tc.body))
			ctx.Request.Header.Set("Content-Type", "application/json")

			handler.UpdateAccountScheduler(ctx)

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
			}
			assertErrorMessage(t, recorder, tc.message)
		})
	}
}

func TestUpdateAccountSchedulerRejectsOutOfRangeValues(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	handler := &Handler{db: db}

	testCases := []struct {
		name    string
		body    string
		message string
	}{
		{
			name:    "score bias out of range",
			body:    `{"score_bias_override":201}`,
			message: "score_bias_override 超出范围，必须在 -200..200 之间",
		},
		{
			name:    "base concurrency out of range",
			body:    `{"base_concurrency_override":0}`,
			message: "base_concurrency_override 超出范围，必须在 1..50 之间",
		},
		{
			name:    "5h auto pause threshold out of range",
			body:    `{"auto_pause_5h_threshold":1.01}`,
			message: "auto_pause_5h_threshold 超出范围，必须在 0..1 之间",
		},
		{
			name:    "7d auto pause threshold out of range",
			body:    `{"auto_pause_7d_threshold":-0.01}`,
			message: "auto_pause_7d_threshold 超出范围，必须在 0..1 之间",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", accountID)}}
			ctx.Request = httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/admin/accounts/%d/scheduler", accountID), strings.NewReader(tc.body))
			ctx.Request.Header.Set("Content-Type", "application/json")

			handler.UpdateAccountScheduler(ctx)

			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
			}
			assertErrorMessage(t, recorder, tc.message)
		})
	}
}

func TestUpdateAccountSchedulerPersistsOverrides(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	handler := &Handler{db: db}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", accountID)}}
	ctx.Request = httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/admin/accounts/%d/scheduler", accountID), strings.NewReader(`{"score_bias_override":88,"base_concurrency_override":7,"skip_warm_tier":true}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateAccountScheduler(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	rows, err := db.ListActive(context.Background())
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if !rows[0].ScoreBiasOverride.Valid || rows[0].ScoreBiasOverride.Int64 != 88 {
		t.Fatalf("score_bias_override = %+v, want 88", rows[0].ScoreBiasOverride)
	}
	if !rows[0].BaseConcurrencyOverride.Valid || rows[0].BaseConcurrencyOverride.Int64 != 7 {
		t.Fatalf("base_concurrency_override = %+v, want 7", rows[0].BaseConcurrencyOverride)
	}
	if !rows[0].SkipWarmTier {
		t.Fatal("skip_warm_tier = false, want true")
	}
}

func TestUpdateAccountSchedulerPersistsAllowedAPIKeyIDs(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	keyID1 := insertTestAPIKey(t, db, "Team A")
	keyID2 := insertTestAPIKey(t, db, "Team B")
	handler := &Handler{db: db}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", accountID)}}
	ctx.Request = httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/admin/accounts/%d/scheduler", accountID), strings.NewReader(fmt.Sprintf(`{"score_bias_override":88,"base_concurrency_override":7,"allowed_api_key_ids":[%d,%d,%d]}`, keyID2, keyID1, keyID2)))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateAccountScheduler(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	rows, err := db.ListActive(context.Background())
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if got := rows[0].GetCredentialInt64Slice("allowed_api_key_ids"); len(got) != 2 || got[0] != keyID1 || got[1] != keyID2 {
		t.Fatalf("allowed_api_key_ids = %v, want [%d %d]", got, keyID1, keyID2)
	}
}

func TestUpdateAccountSchedulerPersistsQuotaAutoPauseConfig(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	handler := &Handler{db: db}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", accountID)}}
	ctx.Request = httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/admin/accounts/%d/scheduler", accountID), strings.NewReader(`{"auto_pause_5h_threshold":0.95,"auto_pause_7d_threshold":null,"auto_pause_5h_disabled":true,"auto_pause_7d_disabled":false}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateAccountScheduler(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	rows, err := db.ListActive(context.Background())
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	threshold5h, ok := rows[0].GetCredentialFloat64("auto_pause_5h_threshold")
	if !ok || threshold5h != 0.95 {
		t.Fatalf("auto_pause_5h_threshold = (%v, %t), want (0.95, true)", threshold5h, ok)
	}
	threshold7d, ok := rows[0].GetCredentialFloat64("auto_pause_7d_threshold")
	if !ok || threshold7d != 0 {
		t.Fatalf("auto_pause_7d_threshold = (%v, %t), want (0, true)", threshold7d, ok)
	}
	if !rows[0].GetCredentialBool("auto_pause_5h_disabled") {
		t.Fatal("auto_pause_5h_disabled = false, want true")
	}
	if rows[0].GetCredentialBool("auto_pause_7d_disabled") {
		t.Fatal("auto_pause_7d_disabled = true, want false")
	}
}

func TestUpdateAccountSchedulerResetsToAutoOnNull(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	ctx := context.Background()
	if err := db.UpdateAccountSchedulerConfig(ctx, accountID, database.OptionalNullInt64{Set: true, Value: sql.NullInt64{Int64: 20, Valid: true}}, database.OptionalNullInt64{Set: true, Value: sql.NullInt64{Int64: 4, Valid: true}}, database.OptionalInt64Slice{}); err != nil {
		t.Fatalf("seed scheduler config: %v", err)
	}

	handler := &Handler{db: db}
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", accountID)}}
	ginCtx.Request = httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/admin/accounts/%d/scheduler", accountID), strings.NewReader(`{"score_bias_override":null,"base_concurrency_override":null}`))
	ginCtx.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateAccountScheduler(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	rows, err := db.ListActive(context.Background())
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].ScoreBiasOverride.Valid {
		t.Fatalf("score_bias_override = %+v, want null", rows[0].ScoreBiasOverride)
	}
	if rows[0].BaseConcurrencyOverride.Valid {
		t.Fatalf("base_concurrency_override = %+v, want null", rows[0].BaseConcurrencyOverride)
	}
}

func TestUpdateAccountSchedulerPartialMetadataPatchPreservesSchedulerConfig(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	keyID := insertTestAPIKey(t, db, "Team A")
	ctx := context.Background()
	if err := db.UpdateAccountSchedulerConfig(ctx, accountID,
		database.OptionalNullInt64{Set: true, Value: sql.NullInt64{Int64: 20, Valid: true}},
		database.OptionalNullInt64{Set: true, Value: sql.NullInt64{Int64: 4, Valid: true}},
		database.OptionalInt64Slice{Set: true, Values: []int64{keyID}},
	); err != nil {
		t.Fatalf("seed scheduler config: %v", err)
	}

	handler := &Handler{db: db}
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", accountID)}}
	ginCtx.Request = httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/admin/accounts/%d/scheduler", accountID), strings.NewReader(`{"tags":["ops"]}`))
	ginCtx.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateAccountScheduler(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	rows, err := db.ListActive(context.Background())
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if !rows[0].ScoreBiasOverride.Valid || rows[0].ScoreBiasOverride.Int64 != 20 {
		t.Fatalf("score_bias_override = %+v, want 20", rows[0].ScoreBiasOverride)
	}
	if !rows[0].BaseConcurrencyOverride.Valid || rows[0].BaseConcurrencyOverride.Int64 != 4 {
		t.Fatalf("base_concurrency_override = %+v, want 4", rows[0].BaseConcurrencyOverride)
	}
	if got := rows[0].GetCredentialInt64Slice("allowed_api_key_ids"); len(got) != 1 || got[0] != keyID {
		t.Fatalf("allowed_api_key_ids = %v, want [%d]", got, keyID)
	}
}

func TestUpdateAccountSchedulerClearsAllowedAPIKeyIDsOnNull(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	keyID := insertTestAPIKey(t, db, "Team A")
	if err := db.UpdateCredentials(context.Background(), accountID, map[string]interface{}{
		"allowed_api_key_ids": []int64{keyID},
	}); err != nil {
		t.Fatalf("seed allowed api keys: %v", err)
	}

	handler := &Handler{db: db}
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", accountID)}}
	ginCtx.Request = httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/admin/accounts/%d/scheduler", accountID), strings.NewReader(`{"score_bias_override":null,"base_concurrency_override":null,"allowed_api_key_ids":null}`))
	ginCtx.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateAccountScheduler(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	rows, err := db.ListActive(context.Background())
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if got := rows[0].GetCredentialInt64Slice("allowed_api_key_ids"); len(got) != 0 {
		t.Fatalf("allowed_api_key_ids = %v, want empty", got)
	}
}

func TestUpdateAccountSchedulerKeepsAllowedAPIKeyIDsWhenFieldOmitted(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	keyID := insertTestAPIKey(t, db, "Team A")
	if err := db.UpdateCredentials(context.Background(), accountID, map[string]interface{}{
		"allowed_api_key_ids": []int64{keyID},
	}); err != nil {
		t.Fatalf("seed allowed api keys: %v", err)
	}

	handler := &Handler{db: db}
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", accountID)}}
	ginCtx.Request = httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/admin/accounts/%d/scheduler", accountID), strings.NewReader(`{"score_bias_override":12,"base_concurrency_override":3}`))
	ginCtx.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateAccountScheduler(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	rows, err := db.ListActive(context.Background())
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if got := rows[0].GetCredentialInt64Slice("allowed_api_key_ids"); len(got) != 1 || got[0] != keyID {
		t.Fatalf("allowed_api_key_ids = %v, want [%d]", got, keyID)
	}
}

func TestUpdateAccountSchedulerClearsProxyURLOnNull(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	accountID, err := db.InsertAccount(context.Background(), "proxy-account", "rt_proxy", "http://127.0.0.1:8080")
	if err != nil {
		t.Fatalf("InsertAccount: %v", err)
	}

	handler := &Handler{db: db}
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", accountID)}}
	ginCtx.Request = httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/admin/accounts/%d/scheduler", accountID), strings.NewReader(`{"proxy_url":null}`))
	ginCtx.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateAccountScheduler(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	rows, err := db.ListActive(context.Background())
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	if rows[0].ProxyURL != "" {
		t.Fatalf("proxy_url = %q, want empty", rows[0].ProxyURL)
	}
}

func TestUpdateAccountSchedulerRejectsMissingAllowedAPIKeyID(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	handler := &Handler{db: db}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", accountID)}}
	ctx.Request = httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/admin/accounts/%d/scheduler", accountID), strings.NewReader(`{"allowed_api_key_ids":[999]}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateAccountScheduler(ctx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	assertErrorMessage(t, recorder, "allowed_api_key_ids 包含不存在的 API Key ID: 999")
}

func TestUpdateAccountSchedulerUpdatesRuntimeOverrides(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	keyID1 := insertTestAPIKey(t, db, "Team A")
	keyID2 := insertTestAPIKey(t, db, "Team B")
	runtimeAccount := &auth.Account{
		DBID:        accountID,
		AccessToken: "token",
		Status:      auth.StatusReady,
		PlanType:    "pro",
	}
	store := &auth.Store{}
	store.AddAccount(runtimeAccount)

	handler := &Handler{db: db, store: store}
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", accountID)}}
	ginCtx.Request = httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/admin/accounts/%d/scheduler", accountID), strings.NewReader(fmt.Sprintf(`{"score_bias_override":33,"base_concurrency_override":5,"skip_warm_tier":true,"allowed_api_key_ids":[%d,%d],"auto_pause_5h_threshold":0.95,"auto_pause_7d_threshold":0.9,"auto_pause_5h_disabled":true,"auto_pause_7d_disabled":false}`, keyID2, keyID1)))
	ginCtx.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateAccountScheduler(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	scoreBias, ok := runtimeAccount.GetScoreBiasOverride()
	if !ok || scoreBias != 33 {
		t.Fatalf("runtime score_bias_override = (%d, %t), want (33, true)", scoreBias, ok)
	}
	baseConcurrency, ok := runtimeAccount.GetBaseConcurrencyOverride()
	if !ok || baseConcurrency != 5 {
		t.Fatalf("runtime base_concurrency_override = (%d, %t), want (5, true)", baseConcurrency, ok)
	}
	if !runtimeAccount.SkipWarmTier {
		t.Fatal("runtime skip_warm_tier = false, want true")
	}
	if got := runtimeAccount.GetAllowedAPIKeyIDs(); len(got) != 2 || got[0] != keyID1 || got[1] != keyID2 {
		t.Fatalf("runtime allowed_api_key_ids = %v, want [%d %d]", got, keyID1, keyID2)
	}
	runtimeAccount.Mu().RLock()
	defer runtimeAccount.Mu().RUnlock()
	if runtimeAccount.AutoPause5hThreshold != 0.95 {
		t.Fatalf("runtime auto_pause_5h_threshold = %v, want 0.95", runtimeAccount.AutoPause5hThreshold)
	}
	if runtimeAccount.AutoPause7dThreshold != 0.9 {
		t.Fatalf("runtime auto_pause_7d_threshold = %v, want 0.9", runtimeAccount.AutoPause7dThreshold)
	}
	if !runtimeAccount.AutoPause5hDisabled {
		t.Fatal("runtime auto_pause_5h_disabled = false, want true")
	}
	if runtimeAccount.AutoPause7dDisabled {
		t.Fatal("runtime auto_pause_7d_disabled = true, want false")
	}
}

func TestUpdateAccountSchedulerRuntimePatchPreservesOmittedOverrides(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	runtimeAccount := &auth.Account{
		DBID:                    accountID,
		AccessToken:             "token",
		Status:                  auth.StatusReady,
		PlanType:                "pro",
		ScoreBiasOverride:       int64Ptr(10),
		BaseConcurrencyOverride: int64Ptr(4),
	}
	store := auth.NewStore(nil, nil, nil)
	store.AddAccount(runtimeAccount)

	handler := &Handler{db: db, store: store}
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", accountID)}}
	ginCtx.Request = httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/admin/accounts/%d/scheduler", accountID), strings.NewReader(`{"score_bias_override":20}`))
	ginCtx.Request.Header.Set("Content-Type", "application/json")

	handler.UpdateAccountScheduler(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	scoreBias, ok := runtimeAccount.GetScoreBiasOverride()
	if !ok || scoreBias != 20 {
		t.Fatalf("runtime score_bias_override = (%d, %t), want (20, true)", scoreBias, ok)
	}
	baseConcurrency, ok := runtimeAccount.GetBaseConcurrencyOverride()
	if !ok || baseConcurrency != 4 {
		t.Fatalf("runtime base_concurrency_override = (%d, %t), want (4, true)", baseConcurrency, ok)
	}
}

func TestUpdateAccountSchedulerPersistsIgnoreUsageLimitOverrideAndInheritance(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	accountID := insertTestAccount(t, db)
	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:         2,
		TestConcurrency:        1,
		TestModel:              "gpt-5.4",
		IgnoreUsageLimitStatus: true,
	})
	runtimeAccount := &auth.Account{DBID: accountID, AccessToken: "token", Status: auth.StatusReady}
	store.AddAccount(runtimeAccount)
	handler := &Handler{db: db, store: store}

	patchOverride := func(body string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		ginCtx, _ := gin.CreateTestContext(recorder)
		ginCtx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", accountID)}}
		ginCtx.Request = httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/admin/accounts/%d/scheduler", accountID), strings.NewReader(body))
		ginCtx.Request.Header.Set("Content-Type", "application/json")
		handler.UpdateAccountScheduler(ginCtx)
		return recorder
	}

	if recorder := patchOverride(`{"ignore_usage_limit_status_override":false}`); recorder.Code != http.StatusOK {
		t.Fatalf("disable override status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if runtimeAccount.IgnoresUsageLimitStatus() {
		t.Fatal("explicit account false should override global true")
	}
	if override := runtimeAccount.GetIgnoreUsageLimitStatusOverride(); override == nil || *override {
		t.Fatalf("runtime override = %v, want false", override)
	}
	row, err := db.GetAccountByID(context.Background(), accountID)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}
	if override := row.GetCredentialOptionalBool("ignore_usage_limit_status_override"); override == nil || *override {
		t.Fatalf("persisted override = %v, want false", override)
	}

	if recorder := patchOverride(`{"ignore_usage_limit_status_override":null}`); recorder.Code != http.StatusOK {
		t.Fatalf("inherit override status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	if !runtimeAccount.IgnoresUsageLimitStatus() {
		t.Fatal("null account override should inherit global true")
	}
	if override := runtimeAccount.GetIgnoreUsageLimitStatusOverride(); override != nil {
		t.Fatalf("runtime override = %v, want nil", override)
	}
	row, err = db.GetAccountByID(context.Background(), accountID)
	if err != nil {
		t.Fatalf("GetAccountByID after inherit: %v", err)
	}
	if override := row.GetCredentialOptionalBool("ignore_usage_limit_status_override"); override != nil {
		t.Fatalf("persisted override = %v, want nil", override)
	}
}

func TestBatchUpdateAccountsPersistsMetadataAndSyncsRuntime(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	ctx := context.Background()
	accountID1, err := db.InsertAccount(ctx, "batch-1", "rt_batch_1", "")
	if err != nil {
		t.Fatalf("InsertAccount 1: %v", err)
	}
	accountID2, err := db.InsertAccount(ctx, "batch-2", "rt_batch_2", "")
	if err != nil {
		t.Fatalf("InsertAccount 2: %v", err)
	}
	groupID, err := db.CreateAccountGroup(ctx, "Batch Group", "", "#2563eb", 0, 0, sql.NullInt64{}, 0)
	if err != nil {
		t.Fatalf("CreateAccountGroup: %v", err)
	}

	runtimeAccount1 := &auth.Account{DBID: accountID1, AccessToken: "token-1", Status: auth.StatusReady, PlanType: "pro"}
	runtimeAccount2 := &auth.Account{DBID: accountID2, AccessToken: "token-2", Status: auth.StatusReady, PlanType: "pro"}
	store := auth.NewStore(nil, nil, nil)
	store.AddAccount(runtimeAccount1)
	store.AddAccount(runtimeAccount2)
	handler := &Handler{db: db, store: store}

	body := fmt.Sprintf(`{"ids":[%d,%d,%d,%d],"enabled":false,"locked":true,"tags":["Ops","ops","blue"],"group_ids":[%d],"score_bias_override":33,"base_concurrency_override":5,"scheduler_priority":7,"auto_pause_5h_threshold":0.8,"auto_pause_7d_disabled":true}`,
		accountID1, accountID2, accountID1, accountID2+1000, groupID)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/batch-update", strings.NewReader(body))
	ginCtx.Request.Header.Set("Content-Type", "application/json")

	handler.BatchUpdateAccounts(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body=%s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var payload map[string]interface{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload["success"] != float64(2) || payload["failed"] != float64(1) {
		t.Fatalf("payload = %#v, want success=2 failed=1", payload)
	}

	rows, err := db.ListActive(ctx)
	if err != nil {
		t.Fatalf("ListActive: %v", err)
	}
	byID := make(map[int64]*database.AccountRow, len(rows))
	for _, row := range rows {
		byID[row.ID] = row
	}
	for _, id := range []int64{accountID1, accountID2} {
		row := byID[id]
		if row == nil {
			t.Fatalf("account %d missing from ListActive", id)
		}
		if row.Enabled || !row.Locked {
			t.Fatalf("account %d enabled/locked = %v/%v, want false/true", id, row.Enabled, row.Locked)
		}
		if len(row.Tags) != 2 || row.Tags[0] != "Ops" || row.Tags[1] != "blue" {
			t.Fatalf("account %d tags = %v, want [Ops blue]", id, row.Tags)
		}
		if !row.ScoreBiasOverride.Valid || row.ScoreBiasOverride.Int64 != 33 {
			t.Fatalf("account %d score_bias_override = %+v, want 33", id, row.ScoreBiasOverride)
		}
		if !row.BaseConcurrencyOverride.Valid || row.BaseConcurrencyOverride.Int64 != 5 {
			t.Fatalf("account %d base_concurrency_override = %+v, want 5", id, row.BaseConcurrencyOverride)
		}
		if priority, ok := row.GetCredentialInt64("scheduler_priority"); !ok || priority != 7 {
			t.Fatalf("account %d scheduler_priority = (%d, %t), want (7, true)", id, priority, ok)
		}
		threshold5h, ok := row.GetCredentialFloat64("auto_pause_5h_threshold")
		if !ok || threshold5h != 0.8 {
			t.Fatalf("account %d auto_pause_5h_threshold = (%v, %t), want (0.8, true)", id, threshold5h, ok)
		}
		if !row.GetCredentialBool("auto_pause_7d_disabled") {
			t.Fatalf("account %d auto_pause_7d_disabled = false, want true", id)
		}
		groupIDs, err := db.GetAccountGroupIDs(ctx, id)
		if err != nil {
			t.Fatalf("GetAccountGroupIDs(%d): %v", id, err)
		}
		if len(groupIDs) != 1 || groupIDs[0] != groupID {
			t.Fatalf("account %d group ids = %v, want [%d]", id, groupIDs, groupID)
		}
	}

	if atomic.LoadInt32(&runtimeAccount1.DispatchPaused) != 1 || atomic.LoadInt32(&runtimeAccount2.DispatchPaused) != 1 {
		t.Fatal("runtime dispatch pause flags were not updated")
	}
	if atomic.LoadInt32(&runtimeAccount1.Locked) != 1 || atomic.LoadInt32(&runtimeAccount2.Locked) != 1 {
		t.Fatal("runtime locked flags were not updated")
	}
	runtimeAccount1.Mu().RLock()
	if len(runtimeAccount1.Tags) != 2 || runtimeAccount1.Tags[0] != "Ops" || runtimeAccount1.Tags[1] != "blue" || len(runtimeAccount1.GroupIDs) != 1 || runtimeAccount1.GroupIDs[0] != groupID {
		t.Fatalf("runtime account 1 metadata tags=%v groups=%v", runtimeAccount1.Tags, runtimeAccount1.GroupIDs)
	}
	runtimeAccount1.Mu().RUnlock()
	for _, account := range []*auth.Account{runtimeAccount1, runtimeAccount2} {
		if scoreBias, ok := account.GetScoreBiasOverride(); !ok || scoreBias != 33 {
			t.Fatalf("runtime account %d score bias = (%d, %t), want (33, true)", account.ID(), scoreBias, ok)
		}
		if baseConcurrency, ok := account.GetBaseConcurrencyOverride(); !ok || baseConcurrency != 5 {
			t.Fatalf("runtime account %d base concurrency = (%d, %t), want (5, true)", account.ID(), baseConcurrency, ok)
		}
		if priority := account.GetSchedulerPriority(); priority != 7 {
			t.Fatalf("runtime account %d scheduler priority = %d, want 7", account.ID(), priority)
		}
	}

	resetRecorder := httptest.NewRecorder()
	resetCtx, _ := gin.CreateTestContext(resetRecorder)
	resetCtx.Request = httptest.NewRequest(
		http.MethodPost,
		"/api/admin/accounts/batch-update",
		strings.NewReader(fmt.Sprintf(`{"ids":[%d,%d],"score_bias_override":null,"base_concurrency_override":null,"scheduler_priority":null}`, accountID1, accountID2)),
	)
	resetCtx.Request.Header.Set("Content-Type", "application/json")

	handler.BatchUpdateAccounts(resetCtx)
	if resetRecorder.Code != http.StatusOK {
		t.Fatalf("reset status = %d, want %d, body=%s", resetRecorder.Code, http.StatusOK, resetRecorder.Body.String())
	}
	rows, err = db.ListActive(ctx)
	if err != nil {
		t.Fatalf("ListActive after reset: %v", err)
	}
	for _, row := range rows {
		if row.ID != accountID1 && row.ID != accountID2 {
			continue
		}
		if row.ScoreBiasOverride.Valid || row.BaseConcurrencyOverride.Valid {
			t.Fatalf("account %d overrides after reset = score %+v concurrency %+v, want null", row.ID, row.ScoreBiasOverride, row.BaseConcurrencyOverride)
		}
		if priority, _ := row.GetCredentialInt64("scheduler_priority"); priority != 0 {
			t.Fatalf("account %d scheduler_priority after reset = %d, want 0", row.ID, priority)
		}
	}
	for _, account := range []*auth.Account{runtimeAccount1, runtimeAccount2} {
		if _, ok := account.GetScoreBiasOverride(); ok {
			t.Fatalf("runtime account %d score bias still overridden after reset", account.ID())
		}
		if _, ok := account.GetBaseConcurrencyOverride(); ok {
			t.Fatalf("runtime account %d base concurrency still overridden after reset", account.ID())
		}
		if priority := account.GetSchedulerPriority(); priority != 0 {
			t.Fatalf("runtime account %d scheduler priority after reset = %d, want 0", account.ID(), priority)
		}
	}
}

func TestBatchUpdateAccountsRejectsMissingUpdateFields(t *testing.T) {
	gin.SetMode(gin.TestMode)

	handler := &Handler{}
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/batch-update", strings.NewReader(`{"ids":[1,2]}`))
	ginCtx.Request.Header.Set("Content-Type", "application/json")

	handler.BatchUpdateAccounts(ginCtx)

	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadRequest)
	}
	assertErrorMessage(t, recorder, "请提供要更新的字段")
}

// AT-only 账号(没有 refresh_token,只靠 access_token)是规避 Codex Plus "add
// phone" 流程的常用形态。导出/迁移以前会因为 rt=="" 直接跳过这些账号,导致
// issue #123 中的迁移丢号。下面两个测试保护已修好的过滤逻辑。
func TestExportAccountsIncludesATOnly(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)

	rtID, err := db.InsertAccount(context.Background(), "rt-account", "rt_value", "")
	if err != nil {
		t.Fatalf("insert rt account: %v", err)
	}
	if err := db.UpdateCredentials(context.Background(), rtID, map[string]interface{}{
		"email":        "rt@example.com",
		"access_token": "at_for_rt",
	}); err != nil {
		t.Fatalf("update rt credentials: %v", err)
	}

	atID, err := db.InsertAccount(context.Background(), "at-account", "", "")
	if err != nil {
		t.Fatalf("insert at-only account: %v", err)
	}
	if err := db.UpdateCredentials(context.Background(), atID, map[string]interface{}{
		"email":        "at@example.com",
		"access_token": "at_only_value",
	}); err != nil {
		t.Fatalf("update at-only credentials: %v", err)
	}

	handler := &Handler{db: db}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/export?filter=all", nil)

	handler.ExportAccounts(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	var entries []cpaExportEntry
	if err := json.Unmarshal(recorder.Body.Bytes(), &entries); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if len(entries) != 2 {
		t.Fatalf("got %d entries, want 2 (rt + at-only)", len(entries))
	}

	byEmail := make(map[string]cpaExportEntry, len(entries))
	for _, e := range entries {
		byEmail[e.Email] = e
	}

	rt, ok := byEmail["rt@example.com"]
	if !ok {
		t.Fatal("rt-based account missing from export")
	}
	if rt.RefreshToken != "rt_value" || rt.AccessToken != "at_for_rt" {
		t.Fatalf("rt entry tokens = (rt=%q, at=%q), want (rt_value, at_for_rt)", rt.RefreshToken, rt.AccessToken)
	}

	at, ok := byEmail["at@example.com"]
	if !ok {
		t.Fatal("AT-only account missing from export")
	}
	if at.RefreshToken != "" {
		t.Fatalf("AT-only RefreshToken = %q, want empty", at.RefreshToken)
	}
	if at.AccessToken != "at_only_value" {
		t.Fatalf("AT-only AccessToken = %q, want at_only_value", at.AccessToken)
	}
}

func TestExportAccountsSkipsAccountsWithoutCredentials(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)

	if _, err := db.InsertAccount(context.Background(), "empty-account", "", ""); err != nil {
		t.Fatalf("insert empty account: %v", err)
	}

	handler := &Handler{db: db}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/export?filter=all", nil)

	handler.ExportAccounts(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	var entries []cpaExportEntry
	if err := json.Unmarshal(recorder.Body.Bytes(), &entries); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("got %d entries, want 0 (account has no credentials)", len(entries))
	}
}

func TestListAccountsPopulatesBilledWindows(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	ctx := context.Background()
	now := time.Now().UTC()
	accountID, err := db.InsertAccountWithCredentials(ctx, "cost-account", map[string]interface{}{
		"refresh_token":           "rt_cost_account",
		"codex_5h_used_percent":   12.5,
		"codex_5h_reset_at":       now.Add(30 * time.Minute).Format(time.RFC3339),
		"codex_7d_used_percent":   34.5,
		"codex_7d_reset_at":       now.Add(24 * time.Hour).Format(time.RFC3339),
		"codex_usage_updated_at":  now.Format(time.RFC3339),
		"email":                   "cost@example.com",
		"plan_type":               "plus",
		"subscription_expires_at": now.AddDate(0, 1, 0).Format(time.RFC3339),
	}, "")
	if err != nil {
		t.Fatalf("InsertAccountWithCredentials 返回错误: %v", err)
	}

	db.SetUsageLogConfig(database.UsageLogModeFull, 1, 1)
	if err := db.InsertUsageLog(ctx, &database.UsageLogInput{
		AccountID:      accountID,
		Endpoint:       "/v1/responses",
		Model:          "gpt-5.4",
		EffectiveModel: "gpt-5.4",
		StatusCode:     http.StatusOK,
		InputTokens:    1000,
		OutputTokens:   500,
		TotalTokens:    1500,
	}); err != nil {
		t.Fatalf("InsertUsageLog 返回错误: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		usage, err := db.GetAccountTimeRangeUsage(ctx, now.Add(-time.Hour))
		if err == nil {
			if row, ok := usage[accountID]; ok && row.AccountBilled > 0 {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("usage log was not flushed before deadline")
		}
		time.Sleep(10 * time.Millisecond)
	}

	store := auth.NewStore(db, nil, nil)
	store.SetLazyMode(true)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("store.Init 返回错误: %v", err)
	}
	handler := &Handler{db: db, store: store}

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts", nil)

	handler.ListAccounts(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var payload accountsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(payload.Accounts) != 1 {
		t.Fatalf("accounts len = %d, want 1", len(payload.Accounts))
	}
	account := payload.Accounts[0]
	if account.EmailDomain != "example.com" {
		t.Fatalf("EmailDomain = %q, want example.com", account.EmailDomain)
	}
	if account.Billed5h == nil || *account.Billed5h <= 0 {
		t.Fatalf("Billed5h = %v, want positive value", account.Billed5h)
	}
	if account.Billed7d == nil || *account.Billed7d <= 0 {
		t.Fatalf("Billed7d = %v, want positive value", account.Billed7d)
	}
	if account.Usage5hDetail == nil || account.Usage5hDetail.AccountBilled <= 0 {
		t.Fatalf("Usage5hDetail = %#v, want positive account_billed", account.Usage5hDetail)
	}
	if account.Usage7dDetail == nil || account.Usage7dDetail.AccountBilled <= 0 {
		t.Fatalf("Usage7dDetail = %#v, want positive account_billed", account.Usage7dDetail)
	}
}

func TestForceUsageProbeTriggersInLazyMode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	store.SetLazyMode(true)
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "token", Status: auth.StatusReady})

	called := make(chan struct{}, 1)
	store.SetUsageProbeFunc(func(ctx context.Context, acc *auth.Account) error {
		called <- struct{}{}
		return nil
	})
	handler := &Handler{store: store}

	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/usage/probe", nil)

	handler.ForceUsageProbe(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var payload struct {
		Triggered bool   `json:"triggered"`
		Mode      string `json:"mode"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !payload.Triggered || payload.Mode != "wham_only" {
		t.Fatalf("payload = %#v, want triggered wham_only", payload)
	}

	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("usage probe was not triggered")
	}
}

func TestRestoreAccountRejectsDuplicateOAuthIdentity(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	handler := &Handler{db: db}

	activeID, err := db.InsertAccountWithCredentials(context.Background(), "active", map[string]interface{}{
		"refresh_token": "rt-active",
		"email":         "restore@example.com",
		"workspace_id":  "acc-restore",
	}, "")
	if err != nil {
		t.Fatalf("Insert active: %v", err)
	}
	deletedID, err := db.InsertAccountWithCredentials(context.Background(), "deleted", map[string]interface{}{
		"refresh_token": "rt-deleted",
	}, "")
	if err != nil {
		t.Fatalf("Insert deleted: %v", err)
	}
	if err := db.SoftDeleteAccount(context.Background(), deletedID); err != nil {
		t.Fatalf("SoftDeleteAccount: %v", err)
	}
	if err := db.UpdateCredentials(context.Background(), deletedID, map[string]interface{}{
		"email":        "Restore@Example.com",
		"workspace_id": "acc-restore",
	}); err != nil {
		t.Fatalf("Update deleted identity: %v", err)
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", deletedID)}}
	ctx.Request = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/admin/accounts/%d/restore", deletedID), nil)

	handler.RestoreAccount(ctx)

	if recorder.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusConflict, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), fmt.Sprintf("%d", activeID)) {
		t.Fatalf("response = %s, want active duplicate id %d", recorder.Body.String(), activeID)
	}
	if _, err := db.GetAccountByID(context.Background(), deletedID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("deleted account should remain outside active pool, err=%v", err)
	}
}

func TestRestoreAccountReportsRuntimeLoadFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)

	db := newTestAdminDB(t)
	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 2, TestConcurrency: 1, TestModel: "gpt-5.4"})
	handler := &Handler{db: db, store: store}

	deletedID, err := db.InsertAccountWithCredentials(context.Background(), "deleted-empty", map[string]interface{}{}, "")
	if err != nil {
		t.Fatalf("Insert deleted-empty: %v", err)
	}
	if err := db.SoftDeleteAccount(context.Background(), deletedID); err != nil {
		t.Fatalf("SoftDeleteAccount: %v", err)
	}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", deletedID)}}
	ctx.Request = httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/admin/accounts/%d/restore", deletedID), nil)

	handler.RestoreAccount(ctx)

	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusInternalServerError, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "加载运行时失败") {
		t.Fatalf("response = %s, want runtime load failure", recorder.Body.String())
	}
	if account := store.FindByID(deletedID); account != nil {
		t.Fatalf("account %d should not be in runtime store", deletedID)
	}
}

func newTestAdminDB(t *testing.T) *database.DB {
	t.Helper()

	tempDir := t.TempDir()
	dbPath := filepath.Join(tempDir, "admin-handler-test.sqlite")
	db, err := database.New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("new test db: %v", err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		_ = os.Remove(dbPath)
	})
	return db
}

func insertTestAccount(t *testing.T, db *database.DB) int64 {
	t.Helper()

	id, err := db.InsertAccount(context.Background(), "test-account", "rt_test", "")
	if err != nil {
		t.Fatalf("insert account: %v", err)
	}
	return id
}

func insertTestAPIKey(t *testing.T, db *database.DB, name string) int64 {
	t.Helper()

	id, err := db.InsertAPIKey(context.Background(), name, fmt.Sprintf("sk-test-%s-1234567890", strings.ToLower(strings.ReplaceAll(name, " ", "-"))))
	if err != nil {
		t.Fatalf("insert api key: %v", err)
	}
	return id
}

func int64Ptr(v int64) *int64 {
	return &v
}

func assertErrorMessage(t *testing.T, recorder *httptest.ResponseRecorder, want string) {
	t.Helper()

	var payload map[string]string
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := payload["error"]; got != want {
		t.Fatalf("error = %q, want %q", got, want)
	}
}
