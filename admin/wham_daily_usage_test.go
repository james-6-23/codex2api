package admin

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func TestGetAccountWhamDailyUsageRefreshSkipsCodexAT(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	ctx := context.Background()
	id, err := db.InsertAccountWithCredentials(ctx, "codex-at", map[string]interface{}{
		"access_token":      "at-opaque",
		"access_token_type": accessTokenTypeCodexAT,
	}, "")
	if err != nil {
		t.Fatalf("insert: %v", err)
	}

	store := auth.NewStore(db, nil, nil)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("store.Init: %v", err)
	}
	handler := &Handler{store: store, db: db}
	handler.queryWhamDailyUsage = func(context.Context, *auth.Account, string, string, string) (*proxy.WhamDailyUsageResponse, *http.Response, error) {
		t.Fatal("codex_at account must not hit official usage upstream")
		return nil, nil, nil
	}

	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Params = gin.Params{{Key: "id", Value: strconv.FormatInt(id, 10)}}
	ginContext.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/"+strconv.FormatInt(id, 10)+"/wham-daily-usage?days=7&refresh=1", nil)
	handler.GetAccountWhamDailyUsage(ginContext)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		RefreshError string `json:"refresh_error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if payload.RefreshError != errWhamDailyUsageUnsupported.Error() {
		t.Fatalf("refresh_error = %q, want %q", payload.RefreshError, errWhamDailyUsageUnsupported.Error())
	}

	if _, err := handler.syncWhamDailyUsage(ctx, store.FindByID(id)); !errors.Is(err, errWhamDailyUsageUnsupported) {
		t.Fatalf("syncWhamDailyUsage error = %v, want unsupported", err)
	}
}
