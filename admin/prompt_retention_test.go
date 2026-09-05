package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func newPromptRetentionTestHandler(t *testing.T) *Handler {
	t.Helper()
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "retention.db"))
	if err != nil {
		t.Fatalf("New(sqlite): %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return &Handler{db: db}
}

func TestPromptLogRetentionEndpoints(t *testing.T) {
	h := newPromptRetentionTestHandler(t)

	get := func() promptLogRetentionResponse {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodGet, "/api/admin/prompt-filter/retention", nil).WithContext(context.Background())
		h.GetPromptLogRetention(c)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET status=%d body=%s", rec.Code, rec.Body.String())
		}
		var resp promptLogRetentionResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return resp
	}
	if resp := get(); resp.RetentionDays != database.DefaultPromptLogRetentionDays || resp.Running {
		t.Fatalf("default = %+v", resp)
	}

	put := func(body string) (int, string) {
		rec := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(rec)
		c.Request = httptest.NewRequest(http.MethodPut, "/api/admin/prompt-filter/retention", strings.NewReader(body)).WithContext(context.Background())
		c.Request.Header.Set("Content-Type", "application/json")
		h.UpdatePromptLogRetention(c)
		return rec.Code, rec.Body.String()
	}
	if code, body := put(`{"retention_days": 400}`); code != http.StatusBadRequest {
		t.Fatalf("out-of-range days should be rejected: %d %s", code, body)
	}
	if code, body := put(`{"retention_days": 0}`); code != http.StatusOK {
		t.Fatalf("0 days (disabled) should be accepted: %d %s", code, body)
	}

	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/admin/prompt-filter/retention/run", nil).WithContext(context.Background())
	h.RunPromptLogRetentionNow(c)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("run with retention disabled should be rejected: %d %s", rec.Code, rec.Body.String())
	}

	if code, _ := put(`{"retention_days": 10}`); code != http.StatusOK {
		t.Fatalf("10 days should be accepted: %d", code)
	}
	if resp := get(); resp.RetentionDays != 10 {
		t.Fatalf("days not persisted: %+v", resp)
	}
}
