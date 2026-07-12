package admin

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestAccountGroupBaseConcurrencyOverrideAPIThreeState(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 8})
	t.Cleanup(store.Stop)
	account := &auth.Account{DBID: 99, AccessToken: "access-token"}
	store.AddAccount(account)
	handler := &Handler{db: db, store: store}

	create := func(body string) (int64, *httptest.ResponseRecorder) {
		t.Helper()
		recorder := invokeAccountGroupHandler(t, http.MethodPost, "/api/admin/account-groups", nil, body, handler.CreateAccountGroup)
		if recorder.Code != http.StatusOK {
			return 0, recorder
		}
		var payload struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode create response: %v", err)
		}
		return payload.ID, recorder
	}

	absentID, recorder := create(`{"name":"inherit-absent"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("create absent status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	nullID, recorder := create(`{"name":"inherit-null","base_concurrency_override":null}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("create null status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	limitedID, recorder := create(`{"name":"limited","base_concurrency_override":4}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("create value status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}

	groups, err := db.ListAccountGroups(context.Background())
	if err != nil {
		t.Fatalf("ListAccountGroups: %v", err)
	}
	byID := make(map[int64]database.AccountGroup, len(groups))
	for _, group := range groups {
		byID[group.ID] = group
	}
	if byID[absentID].BaseConcurrencyOverride.Valid {
		t.Fatalf("absent create override = %+v, want NULL", byID[absentID].BaseConcurrencyOverride)
	}
	if byID[nullID].BaseConcurrencyOverride.Valid {
		t.Fatalf("null create override = %+v, want NULL", byID[nullID].BaseConcurrencyOverride)
	}
	if got := byID[limitedID].BaseConcurrencyOverride; !got.Valid || got.Int64 != 4 {
		t.Fatalf("value create override = %+v, want 4", got)
	}

	store.ApplyAccountGroups(account.DBID, []int64{limitedID})
	if got := account.GetBaseConcurrencyEffective(); got != 4 {
		t.Fatalf("runtime effective after create = %d, want 4", got)
	}

	patch := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		return invokeAccountGroupHandler(t, http.MethodPatch, fmt.Sprintf("/api/admin/account-groups/%d", limitedID), gin.Params{{Key: "id", Value: fmt.Sprintf("%d", limitedID)}}, body, handler.UpdateAccountGroup)
	}
	loadLimitedOverride := func() sql.NullInt64 {
		t.Helper()
		groups, err := db.ListAccountGroups(context.Background())
		if err != nil {
			t.Fatalf("ListAccountGroups after patch: %v", err)
		}
		for _, group := range groups {
			if group.ID == limitedID {
				return group.BaseConcurrencyOverride
			}
		}
		t.Fatalf("group %d not found", limitedID)
		return sql.NullInt64{}
	}

	recorder = patch(`{"name":"limited-renamed"}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("patch absent status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := account.GetBaseConcurrencyEffective(); got != 4 {
		t.Fatalf("runtime effective after absent patch = %d, want 4", got)
	}
	if got := loadLimitedOverride(); !got.Valid || got.Int64 != 4 {
		t.Fatalf("database override after absent patch = %+v, want 4", got)
	}

	recorder = patch(`{"base_concurrency_override":null}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("patch null status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := account.GetBaseConcurrencyEffective(); got != 8 {
		t.Fatalf("runtime effective after null patch = %d, want global 8", got)
	}
	if got := loadLimitedOverride(); got.Valid {
		t.Fatalf("database override after null patch = %+v, want NULL", got)
	}

	recorder = patch(`{"base_concurrency_override":2}`)
	if recorder.Code != http.StatusOK {
		t.Fatalf("patch value status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	if got := account.GetBaseConcurrencyEffective(); got != 2 {
		t.Fatalf("runtime effective after value patch = %d, want 2", got)
	}
	if got := loadLimitedOverride(); !got.Valid || got.Int64 != 2 {
		t.Fatalf("database override after value patch = %+v, want 2", got)
	}

	for _, body := range []string{
		`{"base_concurrency_override":0}`,
		`{"base_concurrency_override":51}`,
		`{"base_concurrency_override":"2"}`,
	} {
		recorder = patch(body)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("invalid patch %s status = %d, want %d: %s", body, recorder.Code, http.StatusBadRequest, recorder.Body.String())
		}
	}
	if got := account.GetBaseConcurrencyEffective(); got != 2 {
		t.Fatalf("runtime effective after rejected patches = %d, want 2", got)
	}

	listRecorder := invokeAccountGroupHandler(t, http.MethodGet, "/api/admin/account-groups", nil, "", handler.ListAccountGroups)
	if listRecorder.Code != http.StatusOK {
		t.Fatalf("list status = %d, want %d: %s", listRecorder.Code, http.StatusOK, listRecorder.Body.String())
	}
	var listed struct {
		Groups []accountGroupResponse `json:"groups"`
	}
	if err := json.Unmarshal(listRecorder.Body.Bytes(), &listed); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	var listedOverride *int64
	for _, group := range listed.Groups {
		if group.ID == limitedID {
			listedOverride = group.BaseConcurrencyOverride
		}
	}
	if listedOverride == nil || *listedOverride != 2 {
		t.Fatalf("listed base_concurrency_override = %v, want 2", listedOverride)
	}

	deleteRecorder := invokeAccountGroupHandler(t, http.MethodDelete, fmt.Sprintf("/api/admin/account-groups/%d", limitedID), gin.Params{{Key: "id", Value: fmt.Sprintf("%d", limitedID)}}, "", handler.DeleteAccountGroup)
	if deleteRecorder.Code != http.StatusOK {
		t.Fatalf("delete status = %d, want %d: %s", deleteRecorder.Code, http.StatusOK, deleteRecorder.Body.String())
	}
	if got := account.GetBaseConcurrencyEffective(); got != 8 {
		t.Fatalf("runtime effective after group delete = %d, want global 8", got)
	}
	account.Mu().RLock()
	remainingGroups := append([]int64(nil), account.GroupIDs...)
	account.Mu().RUnlock()
	if len(remainingGroups) != 0 {
		t.Fatalf("runtime groups after delete = %v, want empty", remainingGroups)
	}
}

func TestCreateAccountGroupRejectsInvalidBaseConcurrencyOverride(t *testing.T) {
	gin.SetMode(gin.TestMode)
	handler := &Handler{db: newTestAdminDB(t)}
	for _, body := range []string{
		`{"name":"zero","base_concurrency_override":0}`,
		`{"name":"large","base_concurrency_override":51}`,
		`{"name":"fraction","base_concurrency_override":1.5}`,
	} {
		recorder := invokeAccountGroupHandler(t, http.MethodPost, "/api/admin/account-groups", nil, body, handler.CreateAccountGroup)
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("body %s status = %d, want %d: %s", body, recorder.Code, http.StatusBadRequest, recorder.Body.String())
		}
	}
}

func TestListAccountsReportsGroupBaseConcurrencyEffective(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	ctx := context.Background()
	accountID, err := db.InsertAccount(ctx, "grouped", "refresh-token", "")
	if err != nil {
		t.Fatalf("InsertAccount: %v", err)
	}
	groupID, err := db.CreateAccountGroup(ctx, "limited", "", "", 0, 0, sql.NullInt64{Int64: 3, Valid: true})
	if err != nil {
		t.Fatalf("CreateAccountGroup: %v", err)
	}
	if err := db.SetAccountGroups(ctx, accountID, []int64{groupID}); err != nil {
		t.Fatalf("SetAccountGroups: %v", err)
	}

	store := auth.NewStore(db, nil, &database.SystemSettings{MaxConcurrency: 8})
	t.Cleanup(store.Stop)
	if err := store.Init(ctx); err != nil {
		t.Fatalf("store.Init: %v", err)
	}
	handler := &Handler{db: db, store: store}
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Request = httptest.NewRequest(http.MethodGet, "/api/admin/accounts", nil)
	handler.ListAccounts(ginContext)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, http.StatusOK, recorder.Body.String())
	}
	var payload accountsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode accounts response: %v", err)
	}
	if len(payload.Accounts) != 1 {
		t.Fatalf("accounts len = %d, want 1", len(payload.Accounts))
	}
	if got := payload.Accounts[0].BaseConcurrencyEffective; got != 3 {
		t.Fatalf("base_concurrency_effective = %d, want group value 3", got)
	}
	if got := payload.Accounts[0].ConcurrencyCap; got != 3 {
		t.Fatalf("dynamic_concurrency_limit = %d, want 3", got)
	}
}

func invokeAccountGroupHandler(t *testing.T, method, path string, params gin.Params, body string, handler func(*gin.Context)) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	ginContext, _ := gin.CreateTestContext(recorder)
	ginContext.Params = params
	ginContext.Request = httptest.NewRequest(method, path, strings.NewReader(body))
	ginContext.Request.Header.Set("Content-Type", "application/json")
	handler(ginContext)
	return recorder
}
