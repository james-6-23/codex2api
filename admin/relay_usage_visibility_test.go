package admin

import (
	"context"
	"testing"
	"time"

	"github.com/codex2api/auth"
)

func TestRelayAccountHidesHistoricalCodexQuotaState(t *testing.T) {
	db := newTestAdminDB(t)
	accountID, err := db.InsertOpenAIResponsesAccount(context.Background(), "relay", map[string]interface{}{
		"upstream_type":           auth.UpstreamOpenAIResponses,
		"base_url":                "https://relay.example",
		"api_key":                 "sk-relay",
		"models":                  []string{"gpt-5.6"},
		"codex_7d_used_percent":   88,
		"codex_7d_reset_at":       time.Now().Add(7 * 24 * time.Hour).Format(time.RFC3339),
		"codex_7d_window_seconds": 604800,
	}, "")
	if err != nil {
		t.Fatalf("InsertOpenAIResponsesAccount: %v", err)
	}
	row, err := db.GetAccountByID(context.Background(), accountID)
	if err != nil {
		t.Fatalf("GetAccountByID: %v", err)
	}

	runtime := &auth.Account{
		DBID:         accountID,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      "https://relay.example",
		APIKey:       "sk-relay",
		PlanType:     "api",
	}
	runtime.SetUsageSnapshot(88, time.Now())
	runtime.SetReset7dAt(time.Now().Add(7 * 24 * time.Hour))
	runtime.SetWindow7dSeconds(604800)
	runtime.SetUsageSnapshot5h(77, time.Now().Add(5*time.Hour))
	store := auth.NewStore(db, nil, nil)
	store.AddAccount(runtime)
	handler := &Handler{db: db, store: store}

	response := handler.buildAccountResponse(row, runtime, nil, nil, nil, true)
	if response.UsagePercent7d != nil || response.UsagePercent5h != nil || response.Window7dSeconds != nil || response.Reset7dAt != "" {
		t.Fatalf("relay detail exposed Codex quota state: %+v", response)
	}
	item := handler.buildAccountListSnapshotItem(row, nil, nil, map[int64]string{}, map[int64]string{})
	if item.UsagePercent7dOK || item.UsagePercent5hOK || item.Window7dSeconds != 0 || !item.Reset7dAt.IsZero() {
		t.Fatalf("relay list exposed Codex quota state: %+v", item)
	}
	windows := handler.accountBillingWindows([]int64{accountID})
	if len(windows) != 0 {
		t.Fatalf("relay account entered Codex billing windows: %v", windows)
	}
}
