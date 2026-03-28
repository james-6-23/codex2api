package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
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

func TestCleanErrorRemovesOnlyErrorAccounts(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := auth.NewStore(nil, cache.NewMemory(1), &database.SystemSettings{
		MaxConcurrency:  2,
		TestConcurrency: 50,
		TestModel:       "gpt-5.4",
	})
	store.AddAccount(&auth.Account{DBID: 1, AccessToken: "token-active", Status: auth.StatusReady})
	store.AddAccount(&auth.Account{DBID: 2, Status: auth.StatusError, ErrorMsg: "refresh_token_reused"})
	store.AddAccount(&auth.Account{DBID: 3, Status: auth.StatusCooldown, CooldownReason: "rate_limited"})

	handler := &Handler{store: store}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/clean-error", nil)

	handler.CleanError(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	var payload struct {
		Message string `json:"message"`
		Cleaned int    `json:"cleaned"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.Cleaned != 1 {
		t.Fatalf("cleaned = %d, want %d", payload.Cleaned, 1)
	}
	if payload.Message != "已清理 1 个账号" {
		t.Fatalf("message = %q, want %q", payload.Message, "已清理 1 个账号")
	}

	if got := store.FindByID(1); got == nil {
		t.Fatal("active account should remain after cleaning error accounts")
	}
	if got := store.FindByID(2); got != nil {
		t.Fatal("error account should be removed after cleaning")
	}
	if got := store.FindByID(3); got == nil {
		t.Fatal("rate-limited account should remain after cleaning error accounts")
	}
}
