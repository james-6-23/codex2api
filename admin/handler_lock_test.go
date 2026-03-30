package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/codex2api/auth"
)

// TestHandler_LockAccount 测试锁定账号接口
func TestHandler_LockAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := auth.NewStore(nil, nil, nil)
	acc := &auth.Account{DBID: 1, AccessToken: "test", Status: auth.StatusReady}
	store.AddAccount(acc)

	h := &Handler{store: store}
	router := gin.New()
	router.POST("/admin/accounts/:id/lock", h.LockAccount)

	req := httptest.NewRequest("POST", "/admin/accounts/1/lock", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("期望状态码200，实际%d", w.Code)
	}

	var resp map[string]interface{}
	json.Unmarshal(w.Body.Bytes(), &resp)
	if resp["success"] != true {
		t.Error("响应应该包含success=true")
	}

	// 验证账号已锁定
	if !acc.IsLocked() {
		t.Error("账号应该已被锁定")
	}
}

// TestHandler_UnlockAccount 测试解锁账号接口
func TestHandler_UnlockAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := auth.NewStore(nil, nil, nil)
	acc := &auth.Account{DBID: 1, AccessToken: "test", Status: auth.StatusReady, Locked: 1}
	store.AddAccount(acc)

	h := &Handler{store: store}
	router := gin.New()
	router.POST("/admin/accounts/:id/unlock", h.UnlockAccount)

	req := httptest.NewRequest("POST", "/admin/accounts/1/unlock", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("期望状态码200，实际%d", w.Code)
	}

	// 验证账号已解锁
	if acc.IsLocked() {
		t.Error("账号应该已被解锁")
	}
}

// TestHandler_BatchLockAccount 测试批量锁定接口
func TestHandler_BatchLockAccount(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := auth.NewStore(nil, nil, nil)
	acc1 := &auth.Account{DBID: 1, AccessToken: "test1", Status: auth.StatusReady}
	acc2 := &auth.Account{DBID: 2, AccessToken: "test2", Status: auth.StatusReady}
	store.AddAccount(acc1)
	store.AddAccount(acc2)

	h := &Handler{store: store}
	router := gin.New()
	router.POST("/admin/accounts/batch-lock", h.BatchLockAccount)

	body := map[string]interface{}{
		"account_ids": []int64{1, 2},
		"locked":      true,
	}
	bodyBytes, _ := json.Marshal(body)

	req := httptest.NewRequest("POST", "/admin/accounts/batch-lock", bytes.NewReader(bodyBytes))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Errorf("期望状态码200，实际%d", w.Code)
	}

	// 验证账号已锁定
	if !acc1.IsLocked() || !acc2.IsLocked() {
		t.Error("所有账号应该已被锁定")
	}
}

// TestHandler_LockAccount_NotFound 测试锁定不存在的账号
func TestHandler_LockAccount_NotFound(t *testing.T) {
	gin.SetMode(gin.TestMode)

	store := auth.NewStore(nil, nil, nil)
	h := &Handler{store: store}
	router := gin.New()
	router.POST("/admin/accounts/:id/lock", h.LockAccount)

	req := httptest.NewRequest("POST", "/admin/accounts/999/lock", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Errorf("期望状态码404，实际%d", w.Code)
	}
}
