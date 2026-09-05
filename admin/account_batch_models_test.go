package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/gin-gonic/gin"
)

func TestBatchUpdateAccountModelsReplacesCodexWhitelistAndSyncsRuntime(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	ctx := context.Background()
	id1, err := db.InsertAccount(ctx, "codex-batch-1", "rt-1", "")
	if err != nil {
		t.Fatalf("InsertAccount 1: %v", err)
	}
	id2, err := db.InsertAccount(ctx, "codex-batch-2", "rt-2", "")
	if err != nil {
		t.Fatalf("InsertAccount 2: %v", err)
	}
	relayID, err := db.InsertAccountWithUpstream(ctx, "relay", "openai", auth.UpstreamOpenAIResponses, map[string]interface{}{
		"upstream_type": auth.UpstreamOpenAIResponses,
		"api_key":       "sk-test",
	}, "")
	if err != nil {
		t.Fatalf("Insert relay: %v", err)
	}

	account1 := &auth.Account{DBID: id1, AccessToken: "at-1", Models: []string{"gpt-old"}}
	account2 := &auth.Account{DBID: id2, AccessToken: "at-2", Models: []string{"gpt-old"}}
	relay := &auth.Account{DBID: relayID, UpstreamType: auth.UpstreamOpenAIResponses, APIKey: "sk-test"}
	relay.BaseURL = "https://api.openai.com"
	store := auth.NewStore(nil, nil, nil)
	store.AddAccount(account1)
	store.AddAccount(account2)
	store.AddAccount(relay)
	handler := &Handler{db: db, store: store}

	body := fmt.Sprintf(`{"ids":[%d,%d,%d,%d,%d],"models":["gpt-5.6-sol"," custom-model ","gpt-5.6-sol"]}`,
		id1, id2, id1, relayID, id2+1000)
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/batch-models", strings.NewReader(body))
	ginCtx.Request.Header.Set("Content-Type", "application/json")
	handler.BatchUpdateAccountModels(ginCtx)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var payload struct {
		Success int64    `json:"success"`
		Failed  int64    `json:"failed"`
		Models  []string `json:"models"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	want := []string{"custom-model", "gpt-5.6-sol"}
	if payload.Success != 2 || payload.Failed != 2 || !reflect.DeepEqual(payload.Models, want) {
		t.Fatalf("payload=%#v want success=2 failed=2 models=%v", payload, want)
	}
	for _, id := range []int64{id1, id2} {
		row, err := db.GetAccountByID(ctx, id)
		if err != nil {
			t.Fatalf("GetAccountByID(%d): %v", id, err)
		}
		if got := row.GetCredentialStringSlice("models"); !reflect.DeepEqual(got, want) {
			t.Fatalf("account %d models=%v want=%v", id, got, want)
		}
	}
	if !reflect.DeepEqual(account1.Models, want) || !reflect.DeepEqual(account2.Models, want) {
		t.Fatalf("runtime models=%v/%v want=%v", account1.Models, account2.Models, want)
	}
}

func TestBatchUpdateAccountModelsClearsWhitelist(t *testing.T) {
	gin.SetMode(gin.TestMode)
	db := newTestAdminDB(t)
	ctx := context.Background()
	id, err := db.InsertAccount(ctx, "codex-clear", "rt-clear", "")
	if err != nil {
		t.Fatalf("InsertAccount: %v", err)
	}
	account := &auth.Account{DBID: id, AccessToken: "at", Models: []string{"gpt-old"}}
	store := auth.NewStore(nil, nil, nil)
	store.AddAccount(account)
	handler := &Handler{db: db, store: store}
	recorder := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(recorder)
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/api/admin/accounts/batch-models", strings.NewReader(fmt.Sprintf(`{"ids":[%d],"models":[]}`, id)))
	ginCtx.Request.Header.Set("Content-Type", "application/json")
	handler.BatchUpdateAccountModels(ginCtx)
	if recorder.Code != http.StatusOK || len(account.Models) != 0 {
		t.Fatalf("status=%d body=%s runtime=%v", recorder.Code, recorder.Body.String(), account.Models)
	}
}
