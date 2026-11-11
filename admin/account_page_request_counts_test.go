package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func decodeAccountsPage(t *testing.T, recorder *httptest.ResponseRecorder) accountsPageResponse {
	t.Helper()
	var page accountsPageResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode page: %v body=%s", err, recorder.Body.String())
	}
	return page
}

func insertAntigravityAccount(t *testing.T, handler *Handler, name string) int64 {
	t.Helper()
	id, err := handler.db.InsertAccountWithUpstream(context.Background(), name, "google", auth.UpstreamAntigravity, map[string]interface{}{
		"upstream_type": auth.UpstreamAntigravity,
		"refresh_token": "rt-" + name,
		"email":         name + "@example.org",
	}, "")
	if err != nil {
		t.Fatalf("insert %s: %v", name, err)
	}
	return id
}

// listAccountsPageAsync runs the paged list off the test goroutine so a scan
// that still blocks the handler fails the deadline instead of the test binary.
func listAccountsPageAsync(handler *Handler, target string) <-chan *httptest.ResponseRecorder {
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		ginContext, _ := gin.CreateTestContext(recorder)
		ginContext.Request = httptest.NewRequest(http.MethodGet, target, nil)
		handler.ListAccounts(ginContext)
		done <- recorder
	}()
	return done
}

func blockPageRequestCounts(t *testing.T, handler *Handler, counts map[int64]*database.AccountRequestCount) func() {
	t.Helper()
	release := make(chan struct{})
	var once sync.Once
	unlock := func() { once.Do(func() { close(release) }) }
	t.Cleanup(unlock)
	handler.pageRequestCountHook = func(ctx context.Context, _ []int64) (map[int64]*database.AccountRequestCount, error) {
		select {
		case <-release:
			return counts, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return unlock
}

func TestListAccountsPageDoesNotBlockOnRequestCountScan(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, _, _ := newPagedAccountsHandler(t)
	accountID := insertAntigravityAccount(t, handler, "ag-slow")
	releaseScan := blockPageRequestCounts(t, handler, map[int64]*database.AccountRequestCount{
		accountID: {
			AccountID:          accountID,
			SuccessCount:       4,
			ErrorCount:         1,
			ErrorStatusCounts:  map[int]int64{500: 1},
			SuccessModelCounts: map[string]int64{"gemini-test": 4},
		},
	})

	const target = "/api/admin/accounts?view=page&channel=antigravity&page=1&page_size=20"
	started := time.Now()
	select {
	case recorder := <-listAccountsPageAsync(handler, target):
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("paged account list took %s; request-count scan must not block it", elapsed)
		}
		if recorder.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		page := decodeAccountsPage(t, recorder)
		if page.Total != 1 || len(page.Accounts) != 1 {
			t.Fatalf("total=%d accounts=%d, want the antigravity row", page.Total, len(page.Accounts))
		}
		account := page.Accounts[0]
		if account.ID != accountID || account.Name != "ag-slow" || account.Email != "ag-slow@example.org" {
			t.Fatalf("account id=%d name=%q email=%q", account.ID, account.Name, account.Email)
		}
		if account.SuccessRequests != 0 || page.StatsState == "ready" {
			t.Fatalf("success_requests=%d stats_state=%q, want zeros while the scan is detached", account.SuccessRequests, page.StatsState)
		}
	case <-time.After(time.Second):
		t.Fatal("paged account list blocked on the usage-log request-count scan")
	}

	// Unblock the detached scan. The list must then serve those counts from
	// the cache instead of aggregating usage_logs again on the request path.
	releaseScan()

	deadline := time.Now().Add(3 * time.Second)
	for {
		if time.Now().After(deadline) {
			t.Fatal("request counts were not served after the detached scan finished")
		}
		recorder := invokeListAccounts(t, handler, target)
		if recorder.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		page := decodeAccountsPage(t, recorder)
		if len(page.Accounts) == 1 && page.Accounts[0].SuccessRequests == 4 &&
			page.Accounts[0].ErrorStatusCounts["500"] == 1 &&
			page.Accounts[0].SuccessModelCounts["gemini-test"] == 4 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestListAccountsPageServesCachedRequestCountsWithoutWaiting(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler, _, _ := newPagedAccountsHandler(t)
	accountID := insertAntigravityAccount(t, handler, "ag-cached")
	handler.storeRequestCountCache(database.UpstreamChannelAntigravity, map[int64]*database.AccountRequestCount{
		accountID: {
			AccountID:          accountID,
			SuccessCount:       9,
			ErrorCount:         2,
			ErrorStatusCounts:  map[int]int64{429: 2},
			SuccessModelCounts: map[string]int64{"gemini-test": 9},
		},
	}, map[int64]*database.AccountTimeRangeUsage{}, time.Time{}, true, []int64{accountID})
	blockPageRequestCounts(t, handler, nil)

	started := time.Now()
	select {
	case recorder := <-listAccountsPageAsync(handler, "/api/admin/accounts?view=page&channel=antigravity&page=1&page_size=20"):
		if elapsed := time.Since(started); elapsed > time.Second {
			t.Fatalf("cached paged list took %s", elapsed)
		}
		if recorder.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		page := decodeAccountsPage(t, recorder)
		if len(page.Accounts) != 1 || page.Accounts[0].Name != "ag-cached" {
			t.Fatalf("accounts=%d", len(page.Accounts))
		}
		account := page.Accounts[0]
		if account.SuccessRequests != 9 || account.ErrorRequests != 2 ||
			account.ErrorStatusCounts["429"] != 2 || account.SuccessModelCounts["gemini-test"] != 9 {
			t.Fatalf("cached counts were not served: success=%d error=%d status=%v models=%v",
				account.SuccessRequests, account.ErrorRequests, account.ErrorStatusCounts, account.SuccessModelCounts)
		}
		if page.StatsState != "ready" {
			t.Fatalf("stats_state=%q, want ready when the breakdown cache covers the page", page.StatsState)
		}
	case <-time.After(time.Second):
		t.Fatal("paged account list blocked even though request counts were cached")
	}
}
