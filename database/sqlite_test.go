package database

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestNewSQLiteInitializesFreshDatabase(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	if got := db.Driver(); got != "sqlite" {
		t.Fatalf("Driver() = %q, want %q", got, "sqlite")
	}
}

func TestSQLiteAccountsEnabledDefaultsAndCanToggle(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	id, err := db.InsertAccount(ctx, "test", "rt", "")
	if err != nil {
		t.Fatalf("InsertAccount 返回错误: %v", err)
	}

	rows, err := db.ListActive(ctx)
	if err != nil {
		t.Fatalf("ListActive 返回错误: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListActive 返回 %d 条，want 1", len(rows))
	}
	if !rows[0].Enabled {
		t.Fatal("new account Enabled = false, want true")
	}

	if err := db.SetAccountEnabled(ctx, id, false); err != nil {
		t.Fatalf("SetAccountEnabled 返回错误: %v", err)
	}
	rows, err = db.ListActive(ctx)
	if err != nil {
		t.Fatalf("ListActive 返回错误: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListActive 返回 %d 条，want 1", len(rows))
	}
	if rows[0].Enabled {
		t.Fatal("disabled account Enabled = true, want false")
	}

	if err := db.SetAccountEnabled(ctx, id+1, false); err != sql.ErrNoRows {
		t.Fatalf("SetAccountEnabled missing account error = %v, want sql.ErrNoRows", err)
	}
}

func TestSQLiteUsageLogsHasAPIKeyColumns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	columns, err := db.sqliteTableColumns(context.Background(), "usage_logs")
	if err != nil {
		t.Fatalf("sqliteTableColumns 返回错误: %v", err)
	}

	for _, name := range []string{"api_key_id", "api_key_name", "api_key_masked", "image_count", "image_width", "image_height", "image_bytes", "image_format", "image_size", "effective_model", "account_billed", "user_billed"} {
		if _, ok := columns[name]; !ok {
			t.Fatalf("usage_logs 缺少列 %q", name)
		}
	}
}

func TestSQLiteUsageStatsBaselineHasBillingColumns(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	columns, err := db.sqliteTableColumns(context.Background(), "usage_stats_baseline")
	if err != nil {
		t.Fatalf("sqliteTableColumns 返回错误: %v", err)
	}

	for _, name := range []string{"account_billed", "user_billed"} {
		if _, ok := columns[name]; !ok {
			t.Fatalf("usage_stats_baseline 缺少列 %q", name)
		}
	}
}

func TestUsageLogsPersistEffectiveModel(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if err := db.InsertUsageLog(ctx, &UsageLogInput{
		AccountID:        1,
		Endpoint:         "/v1/messages",
		InboundEndpoint:  "/v1/messages",
		UpstreamEndpoint: "/v1/responses",
		Model:            "claude-haiku-4-5-20251001",
		EffectiveModel:   "gpt-5.4",
		StatusCode:       200,
		ReasoningEffort:  "high",
	}); err != nil {
		t.Fatalf("InsertUsageLog 返回错误: %v", err)
	}
	db.flushLogs()

	logs, err := db.ListRecentUsageLogs(ctx, 10)
	if err != nil {
		t.Fatalf("ListRecentUsageLogs 返回错误: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("len(logs) = %d, want 1", len(logs))
	}
	if logs[0].Model != "claude-haiku-4-5-20251001" {
		t.Fatalf("Model = %q, want claude-haiku-4-5-20251001", logs[0].Model)
	}
	if logs[0].EffectiveModel != "gpt-5.4" {
		t.Fatalf("EffectiveModel = %q, want gpt-5.4", logs[0].EffectiveModel)
	}
	if logs[0].ReasoningEffort != "high" {
		t.Fatalf("ReasoningEffort = %q, want high", logs[0].ReasoningEffort)
	}
}

func TestUsageLogsPersistImageMetadata(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if err := db.InsertUsageLog(ctx, &UsageLogInput{
		AccountID:        1,
		Endpoint:         "/v1/images/generations",
		InboundEndpoint:  "/v1/images/generations",
		UpstreamEndpoint: "/v1/responses",
		Model:            "gpt-image-2-4k",
		StatusCode:       200,
		DurationMs:       1200,
		ImageCount:       1,
		ImageWidth:       3840,
		ImageHeight:      2160,
		ImageBytes:       2457600,
		ImageFormat:      "png",
		ImageSize:        "3840x2160",
	}); err != nil {
		t.Fatalf("InsertUsageLog 返回错误: %v", err)
	}
	db.flushLogs()

	logs, err := db.ListRecentUsageLogs(ctx, 10)
	if err != nil {
		t.Fatalf("ListRecentUsageLogs 返回错误: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("len(logs) = %d, want 1", len(logs))
	}
	got := logs[0]
	if got.ImageCount != 1 || got.ImageWidth != 3840 || got.ImageHeight != 2160 || got.ImageBytes != 2457600 || got.ImageFormat != "png" || got.ImageSize != "3840x2160" {
		t.Fatalf("image metadata = %#v", got)
	}
}

func TestUsageLogsReturnBillingFields(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	if err := db.InsertUsageLog(ctx, &UsageLogInput{
		AccountID:        1,
		Endpoint:         "/v1/responses",
		InboundEndpoint:  "/v1/responses",
		UpstreamEndpoint: "/v1/responses",
		Model:            "gpt-5.5",
		StatusCode:       200,
		InputTokens:      476,
		OutputTokens:     252,
		TotalTokens:      728,
		ServiceTier:      "default",
	}); err != nil {
		t.Fatalf("InsertUsageLog 返回错误: %v", err)
	}
	db.flushLogs()

	logs, err := db.ListRecentUsageLogs(ctx, 10)
	if err != nil {
		t.Fatalf("ListRecentUsageLogs 返回错误: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("len(logs) = %d, want 1", len(logs))
	}

	got := logs[0]
	want := calculateCost(476, 252, 0, "gpt-5.5", "default")
	if got.AccountBilled != want || got.UserBilled != want {
		t.Fatalf("billing = account %.12f user %.12f, want %.12f", got.AccountBilled, got.UserBilled, want)
	}
	if got.InputCost <= 0 || got.OutputCost <= 0 || got.TotalCost != want {
		t.Fatalf("billing breakdown = input %.12f output %.12f total %.12f, want total %.12f", got.InputCost, got.OutputCost, got.TotalCost, want)
	}
}

func TestUsageStatsIncludeBillingTotals(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	for _, usageLog := range []*UsageLogInput{
		{
			AccountID:    1,
			Endpoint:     "/v1/responses",
			Model:        "gpt-5.5",
			StatusCode:   200,
			InputTokens:  1000,
			OutputTokens: 500,
			TotalTokens:  1500,
		},
		{
			AccountID:    1,
			Endpoint:     "/v1/responses",
			Model:        "gpt-5.5",
			StatusCode:   499,
			InputTokens:  1000,
			OutputTokens: 500,
			TotalTokens:  1500,
		},
	} {
		if err := db.InsertUsageLog(ctx, usageLog); err != nil {
			t.Fatalf("InsertUsageLog 返回错误: %v", err)
		}
	}
	db.flushLogs()

	stats, err := db.GetUsageStats(ctx)
	if err != nil {
		t.Fatalf("GetUsageStats 返回错误: %v", err)
	}

	want := calculateCost(1000, 500, 0, "gpt-5.5", "")
	if stats.TotalAccountBilled != want || stats.TotalUserBilled != want {
		t.Fatalf("total billing = account %.12f user %.12f, want %.12f", stats.TotalAccountBilled, stats.TotalUserBilled, want)
	}
	if stats.TodayAccountBilled != want || stats.TodayUserBilled != want {
		t.Fatalf("today billing = account %.12f user %.12f, want %.12f", stats.TodayAccountBilled, stats.TodayUserBilled, want)
	}
}

func TestSoftDeleteAccountMarksDeletedStatus(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	id, err := db.InsertAccount(ctx, "delete-me", "rt-delete-me", "")
	if err != nil {
		t.Fatalf("InsertAccount 返回错误: %v", err)
	}
	if err := db.SoftDeleteAccount(ctx, id); err != nil {
		t.Fatalf("SoftDeleteAccount 返回错误: %v", err)
	}

	active, err := db.ListActive(ctx)
	if err != nil {
		t.Fatalf("ListActive 返回错误: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("ListActive 返回 %d 条，want 0", len(active))
	}
	if _, err := db.GetAccountByID(ctx, id); err == nil {
		t.Fatal("GetAccountByID 应该排除已删除账号")
	}

	var status string
	var errorMessage string
	var deletedAt sql.NullString
	if err := db.conn.QueryRowContext(ctx, `SELECT status, error_message, deleted_at FROM accounts WHERE id = $1`, id).Scan(&status, &errorMessage, &deletedAt); err != nil {
		t.Fatalf("查询账号状态返回错误: %v", err)
	}
	if status != "deleted" {
		t.Fatalf("status = %q, want deleted", status)
	}
	if errorMessage != "" {
		t.Fatalf("error_message = %q, want empty", errorMessage)
	}
	if !deletedAt.Valid || deletedAt.String == "" {
		t.Fatal("deleted_at 未写入")
	}
}

func TestSQLiteMigratesLegacyDeletedAccounts(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")
	ctx := context.Background()

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	id, err := db.InsertAccount(ctx, "legacy-delete", "rt-legacy-delete", "")
	if err != nil {
		t.Fatalf("InsertAccount 返回错误: %v", err)
	}
	if err := db.SetError(ctx, id, "deleted"); err != nil {
		t.Fatalf("SetError 返回错误: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close 返回错误: %v", err)
	}

	db, err = New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("reopen New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	var status string
	var errorMessage string
	var deletedAt sql.NullString
	if err := db.conn.QueryRowContext(ctx, `SELECT status, error_message, deleted_at FROM accounts WHERE id = $1`, id).Scan(&status, &errorMessage, &deletedAt); err != nil {
		t.Fatalf("查询迁移后账号返回错误: %v", err)
	}
	if status != "deleted" {
		t.Fatalf("status = %q, want deleted", status)
	}
	if errorMessage != "" {
		t.Fatalf("error_message = %q, want empty", errorMessage)
	}
	if !deletedAt.Valid || deletedAt.String == "" {
		t.Fatal("deleted_at 未迁移")
	}
}

func TestListActiveIncludesErrorAccounts(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	id, err := db.InsertAccount(ctx, "error-account", "rt-error", "")
	if err != nil {
		t.Fatalf("InsertAccount 返回错误: %v", err)
	}
	if err := db.SetError(ctx, id, "batch test failed"); err != nil {
		t.Fatalf("SetError 返回错误: %v", err)
	}

	rows, err := db.ListActive(ctx)
	if err != nil {
		t.Fatalf("ListActive 返回错误: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("ListActive 返回 %d 条，want 1", len(rows))
	}
	if rows[0].Status != "error" {
		t.Fatalf("status = %q, want error", rows[0].Status)
	}
	if rows[0].ErrorMessage != "batch test failed" {
		t.Fatalf("error_message = %q, want batch test failed", rows[0].ErrorMessage)
	}
}

func TestUsageLogsFilterByAPIKeyID(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	now := time.Now().UTC()
	targetAPIKeyID := int64(7)

	logs := []*UsageLogInput{
		{
			AccountID:    1,
			Endpoint:     "/v1/chat/completions",
			Model:        "gpt-5.4",
			StatusCode:   200,
			DurationMs:   120,
			APIKeyID:     targetAPIKeyID,
			APIKeyName:   "Team A",
			APIKeyMasked: "sk-a****...****1111",
		},
		{
			AccountID:    1,
			Endpoint:     "/v1/responses",
			Model:        "gpt-5.4",
			StatusCode:   200,
			DurationMs:   220,
			APIKeyID:     targetAPIKeyID,
			APIKeyName:   "Team A",
			APIKeyMasked: "sk-a****...****1111",
		},
		{
			AccountID:    2,
			Endpoint:     "/v1/responses",
			Model:        "gpt-5.4-mini",
			StatusCode:   200,
			DurationMs:   320,
			APIKeyID:     8,
			APIKeyName:   "Team B",
			APIKeyMasked: "sk-b****...****2222",
		},
	}

	for _, usageLog := range logs {
		if err := db.InsertUsageLog(ctx, usageLog); err != nil {
			t.Fatalf("InsertUsageLog 返回错误: %v", err)
		}
	}
	db.flushLogs()

	recentLogs, err := db.ListRecentUsageLogs(ctx, 10)
	if err != nil {
		t.Fatalf("ListRecentUsageLogs 返回错误: %v", err)
	}
	if len(recentLogs) != len(logs) {
		t.Fatalf("recentLogs 长度 = %d, want %d", len(recentLogs), len(logs))
	}

	foundSnapshot := false
	for _, usageLog := range recentLogs {
		if usageLog.APIKeyID == targetAPIKeyID {
			foundSnapshot = true
			if usageLog.APIKeyName != "Team A" {
				t.Fatalf("APIKeyName = %q, want %q", usageLog.APIKeyName, "Team A")
			}
			if usageLog.APIKeyMasked != "sk-a****...****1111" {
				t.Fatalf("APIKeyMasked = %q, want %q", usageLog.APIKeyMasked, "sk-a****...****1111")
			}
		}
	}
	if !foundSnapshot {
		t.Fatal("未找到带 API 密钥快照的最近日志")
	}

	page, err := db.ListUsageLogsByTimeRangePaged(ctx, UsageLogFilter{
		Start:    now.Add(-1 * time.Hour),
		End:      now.Add(1 * time.Hour),
		Page:     1,
		PageSize: 10,
		APIKeyID: &targetAPIKeyID,
	})
	if err != nil {
		t.Fatalf("ListUsageLogsByTimeRangePaged 返回错误: %v", err)
	}

	if page.Total != 2 {
		t.Fatalf("page.Total = %d, want %d", page.Total, 2)
	}
	if len(page.Logs) != 2 {
		t.Fatalf("len(page.Logs) = %d, want %d", len(page.Logs), 2)
	}
	for _, usageLog := range page.Logs {
		if usageLog.APIKeyID != targetAPIKeyID {
			t.Fatalf("APIKeyID = %d, want %d", usageLog.APIKeyID, targetAPIKeyID)
		}
		if usageLog.APIKeyName != "Team A" {
			t.Fatalf("APIKeyName = %q, want %q", usageLog.APIKeyName, "Team A")
		}
	}
}

func TestSQLiteUsageLogsTimeRangeUsesUTCStorage(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "codex2api.db")

	db, err := New("sqlite", dbPath)
	if err != nil {
		t.Fatalf("New(sqlite) 返回错误: %v", err)
	}
	defer db.Close()

	ctx := context.Background()
	createdUTC := time.Date(2026, 4, 23, 20, 6, 0, 0, time.UTC)
	if _, err := db.conn.ExecContext(ctx, `
		INSERT INTO usage_logs (
			account_id, endpoint, inbound_endpoint, upstream_endpoint, model,
			status_code, total_tokens, input_tokens, output_tokens, created_at
		)
		VALUES (1, '/v1/images/generations', '/v1/images/generations', '/v1/responses', 'gpt-image-2',
			200, 1790, 34, 1756, $1)
	`, sqliteTimeParam(createdUTC)); err != nil {
		t.Fatalf("insert usage log 返回错误: %v", err)
	}

	shanghai := time.FixedZone("Asia/Shanghai", 8*60*60)
	localCreated := createdUTC.In(shanghai)
	page, err := db.ListUsageLogsByTimeRangePaged(ctx, UsageLogFilter{
		Start:    localCreated.Add(-1 * time.Hour),
		End:      localCreated.Add(1 * time.Hour),
		Page:     1,
		PageSize: 10,
	})
	if err != nil {
		t.Fatalf("ListUsageLogsByTimeRangePaged 返回错误: %v", err)
	}
	if page.Total != 1 {
		t.Fatalf("page.Total = %d, want %d", page.Total, 1)
	}
	if len(page.Logs) != 1 {
		t.Fatalf("len(page.Logs) = %d, want %d", len(page.Logs), 1)
	}
	if got := page.Logs[0].InboundEndpoint; got != "/v1/images/generations" {
		t.Fatalf("InboundEndpoint = %q, want /v1/images/generations", got)
	}
	if got := page.Logs[0].Model; got != "gpt-image-2" {
		t.Fatalf("Model = %q, want gpt-image-2", got)
	}
}
