package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/gin-gonic/gin"
)

func TestGetAccountLiveStateReturnsVisibleInflightCounts(t *testing.T) {
	gin.SetMode(gin.TestMode)
	store := auth.NewStore(nil, nil, nil)
	t.Cleanup(store.Stop)
	account := &auth.Account{DBID: 42, AccessToken: "token"}
	atomic.StoreInt64(&account.ActiveRequests, 3)
	atomic.StoreInt64(&account.OccupiedRequests, 5)
	store.AddAccount(account)
	store.SetSessionSlotBufferEnabled(true)
	handler := &Handler{store: store}

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts/live?ids=42,99", nil)
	handler.GetAccountLiveState(ctx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Accounts                 map[string]accountLiveItem `json:"accounts"`
		SessionSlotBufferEnabled bool                       `json:"session_slot_buffer_enabled"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if got := response.Accounts["42"].ActiveRequests; got != 3 {
		t.Fatalf("active_requests = %d, want 3", got)
	}
	if got := response.Accounts["42"].OccupiedRequests; got != 5 {
		t.Fatalf("occupied_requests = %d, want 5", got)
	}
	if !response.SessionSlotBufferEnabled {
		t.Fatal("session slot buffer enabled state was not returned")
	}
	if _, exists := response.Accounts["99"]; exists {
		t.Fatal("missing account unexpectedly returned")
	}
}

func TestDeleteAccountSessionsUnbindsOnlyReleasedAccount(t *testing.T) {
	for _, query := range []string{"", "?session_id=released-root"} {
		t.Run("query="+query, func(t *testing.T) {
			runtimeCache := cache.NewMemory(1)
			t.Cleanup(func() { _ = runtimeCache.Close() })
			store := auth.NewStore(nil, runtimeCache, nil)
			t.Cleanup(store.Stop)
			account := &auth.Account{DBID: 42, AccessToken: "token", SessionCapacityEnabled: true, SessionCapacityMax: 3, SessionCapacityIdleTTLSeconds: 3600}
			other := &auth.Account{DBID: 43, AccessToken: "other", SessionCapacityEnabled: true, SessionCapacityMax: 3, SessionCapacityIdleTTLSeconds: 3600}
			store.AddAccount(account)
			store.AddAccount(other)
			store.BindSessionAffinity("released-root", account, "")
			store.BindSessionAffinity("rebound-root", account, "")
			store.BindSessionAffinity("rebound-root", other, "")
			if owner, found := store.SessionAffinityAccountID("released-root"); !found || owner != account.DBID {
				t.Fatal("initial binding is missing")
			}
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Params = gin.Params{{Key: "id", Value: "42"}}
			ctx.Request = httptest.NewRequest(http.MethodDelete, "/api/admin/accounts/42/sessions"+query, nil)
			(&Handler{store: store}).DeleteAccountSessions(ctx)
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d, body=%s", recorder.Code, recorder.Body.String())
			}
			if _, found := store.SessionAffinityAccountID("released-root"); found {
				t.Fatal("released root still has a local affinity")
			}
			if _, found, err := runtimeCache.GetSessionAffinity(t.Context(), "released-root"); err != nil || found {
				t.Fatalf("released cached affinity found=%t err=%v", found, err)
			}
			if owner, found := store.SessionAffinityAccountID("rebound-root"); !found || owner != other.DBID {
				t.Fatal("another account's affinity was removed")
			}
			if binding, found, err := runtimeCache.GetSessionAffinity(t.Context(), "rebound-root"); err != nil || !found || binding.AccountID != other.DBID {
				t.Fatalf("rebound cached affinity = %#v found=%t err=%v", binding, found, err)
			}
			if query == "" && store.AccountSessionCount(account.DBID, time.Now()) != 0 {
				t.Fatal("bulk release left occupied slots")
			}
		})
	}
}
