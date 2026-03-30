package database

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"time"
)

func (db *DB) configureSQLite(ctx context.Context) error {
	pragmas := []string{
		`PRAGMA journal_mode=WAL;`,
		`PRAGMA busy_timeout=5000;`,
		`PRAGMA synchronous=NORMAL;`,
	}
	for _, pragma := range pragmas {
		if _, err := db.conn.ExecContext(ctx, pragma); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) migrateSQLite(ctx context.Context) error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS accounts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT DEFAULT '',
			platform TEXT DEFAULT 'openai',
			type TEXT DEFAULT 'oauth',
			credentials TEXT NOT NULL DEFAULT '{}',
			proxy_url TEXT DEFAULT '',
			status TEXT DEFAULT 'active',
			cooldown_reason TEXT DEFAULT '',
			cooldown_until TIMESTAMP NULL,
			error_message TEXT DEFAULT '',
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE TABLE IF NOT EXISTS usage_logs (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			account_id INTEGER DEFAULT 0,
			endpoint TEXT DEFAULT '',
			model TEXT DEFAULT '',
			prompt_tokens INTEGER DEFAULT 0,
			completion_tokens INTEGER DEFAULT 0,
			total_tokens INTEGER DEFAULT 0,
			status_code INTEGER DEFAULT 0,
			duration_ms INTEGER DEFAULT 0,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			input_tokens INTEGER DEFAULT 0,
			output_tokens INTEGER DEFAULT 0,
			reasoning_tokens INTEGER DEFAULT 0,
			first_token_ms INTEGER DEFAULT 0,
			reasoning_effort TEXT DEFAULT '',
			inbound_endpoint TEXT DEFAULT '',
			upstream_endpoint TEXT DEFAULT '',
			stream INTEGER DEFAULT 0,
			cached_tokens INTEGER DEFAULT 0,
			service_tier TEXT DEFAULT ''
		);`,
		`CREATE TABLE IF NOT EXISTS api_keys (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT DEFAULT '',
			key TEXT NOT NULL UNIQUE,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);`,
		`CREATE TABLE IF NOT EXISTS system_settings (
			id INTEGER PRIMARY KEY DEFAULT 1 CHECK (id = 1),
			max_concurrency INTEGER DEFAULT 2,
			global_rpm INTEGER DEFAULT 0,
			test_model TEXT DEFAULT 'gpt-5.4',
			test_concurrency INTEGER DEFAULT 50,
			proxy_url TEXT DEFAULT '',
			pg_max_conns INTEGER DEFAULT 50,
			redis_pool_size INTEGER DEFAULT 30,
			auto_clean_unauthorized INTEGER DEFAULT 0,
			auto_clean_rate_limited INTEGER DEFAULT 0,
			admin_secret TEXT DEFAULT '',
			auto_clean_full_usage INTEGER DEFAULT 0,
			auto_clean_error INTEGER DEFAULT 0,
			auto_clean_expired INTEGER DEFAULT 0,
			proxy_pool_enabled INTEGER DEFAULT 0,
			fast_scheduler_enabled INTEGER DEFAULT 0,
			max_retries INTEGER DEFAULT 2,
			allow_remote_migration INTEGER DEFAULT 0
		);`,
		`CREATE TABLE IF NOT EXISTS proxies (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			url TEXT NOT NULL UNIQUE,
			label TEXT DEFAULT '',
			enabled INTEGER DEFAULT 1,
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			test_ip TEXT DEFAULT '',
			test_location TEXT DEFAULT '',
			test_latency_ms INTEGER DEFAULT 0
		);`,
		`CREATE TABLE IF NOT EXISTS account_events (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			account_id INTEGER NOT NULL DEFAULT 0,
			event_type TEXT NOT NULL,
			source TEXT DEFAULT '',
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
		);`,
	}
	for _, stmt := range statements {
		if _, err := db.conn.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}

	columns := []struct {
		table string
		name  string
		def   string
	}{
		{"accounts", "cooldown_reason", "TEXT DEFAULT ''"},
		{"accounts", "cooldown_until", "TIMESTAMP NULL"},
		{"accounts", "model_states", "TEXT DEFAULT '{}'"},
		{"usage_logs", "input_tokens", "INTEGER DEFAULT 0"},
		{"usage_logs", "output_tokens", "INTEGER DEFAULT 0"},
		{"usage_logs", "reasoning_tokens", "INTEGER DEFAULT 0"},
		{"usage_logs", "first_token_ms", "INTEGER DEFAULT 0"},
		{"usage_logs", "reasoning_effort", "TEXT DEFAULT ''"},
		{"usage_logs", "inbound_endpoint", "TEXT DEFAULT ''"},
		{"usage_logs", "upstream_endpoint", "TEXT DEFAULT ''"},
		{"usage_logs", "stream", "INTEGER DEFAULT 0"},
		{"usage_logs", "cached_tokens", "INTEGER DEFAULT 0"},
		{"usage_logs", "service_tier", "TEXT DEFAULT ''"},
		{"system_settings", "pg_max_conns", "INTEGER DEFAULT 50"},
		{"system_settings", "redis_pool_size", "INTEGER DEFAULT 30"},
		{"system_settings", "auto_clean_unauthorized", "INTEGER DEFAULT 0"},
		{"system_settings", "auto_clean_rate_limited", "INTEGER DEFAULT 0"},
		{"system_settings", "admin_secret", "TEXT DEFAULT ''"},
		{"system_settings", "auto_clean_full_usage", "INTEGER DEFAULT 0"},
		{"system_settings", "auto_clean_error", "INTEGER DEFAULT 0"},
		{"system_settings", "auto_clean_expired", "INTEGER DEFAULT 0"},
		{"system_settings", "proxy_pool_enabled", "INTEGER DEFAULT 0"},
		{"system_settings", "fast_scheduler_enabled", "INTEGER DEFAULT 0"},
		{"system_settings", "max_retries", "INTEGER DEFAULT 2"},
		{"system_settings", "allow_remote_migration", "INTEGER DEFAULT 0"},
		{"proxies", "test_ip", "TEXT DEFAULT ''"},
		{"proxies", "test_location", "TEXT DEFAULT ''"},
		{"proxies", "test_latency_ms", "INTEGER DEFAULT 0"},
	}
	for _, column := range columns {
		if err := db.ensureSQLiteColumn(ctx, column.table, column.name, column.def); err != nil {
			return err
		}
	}

	indexStatements := []string{
		`CREATE INDEX IF NOT EXISTS idx_accounts_status ON accounts(status);`,
		`CREATE INDEX IF NOT EXISTS idx_accounts_platform ON accounts(platform);`,
		`CREATE INDEX IF NOT EXISTS idx_accounts_cooldown_until ON accounts(cooldown_until);`,
		`CREATE INDEX IF NOT EXISTS idx_usage_logs_created_at ON usage_logs(created_at);`,
		`CREATE INDEX IF NOT EXISTS idx_usage_logs_account_id ON usage_logs(account_id);`,
		`CREATE INDEX IF NOT EXISTS idx_usage_logs_created_status ON usage_logs(created_at, status_code);`,
		`CREATE INDEX IF NOT EXISTS idx_account_events_created ON account_events(created_at);`,
		`CREATE INDEX IF NOT EXISTS idx_account_events_type_created ON account_events(event_type, created_at);`,
	}
	for _, stmt := range indexStatements {
		if _, err := db.conn.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}

	return nil
}

func (db *DB) ensureSQLiteColumn(ctx context.Context, table string, name string, columnDef string) error {
	columns, err := db.sqliteTableColumns(ctx, table)
	if err != nil {
		return err
	}
	if _, ok := columns[name]; ok {
		return nil
	}
	_, err = db.conn.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE %s ADD COLUMN %s %s`, table, name, columnDef))
	return err
}

func (db *DB) sqliteTableColumns(ctx context.Context, table string) (map[string]struct{}, error) {
	rows, err := db.conn.QueryContext(ctx, fmt.Sprintf(`PRAGMA table_info(%s)`, table))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make(map[string]struct{})
	for rows.Next() {
		var (
			cid        int
			name       string
			columnType string
			notNull    int
			defaultVal sql.NullString
			pk         int
		)
		if err := rows.Scan(&cid, &name, &columnType, &notNull, &defaultVal, &pk); err != nil {
			return nil, err
		}
		result[name] = struct{}{}
	}
	return result, rows.Err()
}

func (db *DB) getTrafficSnapshotSQLite(ctx context.Context) (*TrafficSnapshot, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT created_at, total_tokens
		FROM usage_logs
		WHERE created_at >= $1
	`, time.Now().Add(-5*time.Minute))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	perSecondRequests := make(map[int64]float64)
	perSecondTokens := make(map[int64]float64)
	now := time.Now()
	windowStart := now.Add(-10 * time.Second).Unix()

	for rows.Next() {
		var createdRaw interface{}
		var totalTokens int64
		if err := rows.Scan(&createdRaw, &totalTokens); err != nil {
			return nil, err
		}
		createdAt, err := parseDBTimeValue(createdRaw)
		if err != nil || createdAt.IsZero() {
			continue
		}
		sec := createdAt.Unix()
		perSecondRequests[sec]++
		perSecondTokens[sec] += float64(totalTokens)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	result := &TrafficSnapshot{}
	var qpsPeak float64
	var tpsPeak float64
	var qpsWindow float64
	var tpsWindow float64
	for sec, reqCount := range perSecondRequests {
		if reqCount > qpsPeak {
			qpsPeak = reqCount
		}
		tokenCount := perSecondTokens[sec]
		if tokenCount > tpsPeak {
			tpsPeak = tokenCount
		}
		if sec >= windowStart {
			qpsWindow += reqCount
			tpsWindow += tokenCount
		}
	}
	result.QPS = qpsWindow / 10.0
	result.TPS = tpsWindow / 10.0
	result.QPSPeak = qpsPeak
	result.TPSPeak = tpsPeak
	return result, nil
}

func (db *DB) getChartAggregationSQLite(ctx context.Context, start, end time.Time, bucketMinutes int) (*ChartAggregation, error) {
	rows, err := db.conn.QueryContext(ctx, `
		SELECT created_at, duration_ms, input_tokens, output_tokens, reasoning_tokens, cached_tokens, model, status_code
		FROM usage_logs
		WHERE created_at >= $1 AND created_at <= $2
		  AND status_code <> 499
	`, start, end)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type bucketAgg struct {
		requests        int64
		totalLatency    float64
		inputTokens     int64
		outputTokens    int64
		reasoningTokens int64
		cachedTokens    int64
		errors401       int64
	}

	result := &ChartAggregation{}
	timelineMap := make(map[string]*bucketAgg)
	modelMap := make(map[string]int64)

	for rows.Next() {
		var createdRaw interface{}
		var durationMs int
		var inputTokens int64
		var outputTokens int64
		var reasoningTokens int64
		var cachedTokens int64
		var model sql.NullString
		var statusCode int
		if err := rows.Scan(&createdRaw, &durationMs, &inputTokens, &outputTokens, &reasoningTokens, &cachedTokens, &model, &statusCode); err != nil {
			return nil, err
		}
		createdAt, err := parseDBTimeValue(createdRaw)
		if err != nil || createdAt.IsZero() {
			continue
		}

		bucket := createdAt.Truncate(time.Duration(bucketMinutes) * time.Minute).Format("2006-01-02T15:04:05")
		agg, ok := timelineMap[bucket]
		if !ok {
			agg = &bucketAgg{}
			timelineMap[bucket] = agg
		}
		agg.requests++
		agg.totalLatency += float64(durationMs)
		agg.inputTokens += inputTokens
		agg.outputTokens += outputTokens
		agg.reasoningTokens += reasoningTokens
		agg.cachedTokens += cachedTokens
		if statusCode == 401 {
			agg.errors401++
		}

		modelName := "unknown"
		if model.Valid && model.String != "" {
			modelName = model.String
		}
		modelMap[modelName]++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	keys := make([]string, 0, len(timelineMap))
	for key := range timelineMap {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		agg := timelineMap[key]
		avgLatency := 0.0
		if agg.requests > 0 {
			avgLatency = agg.totalLatency / float64(agg.requests)
		}
		result.Timeline = append(result.Timeline, ChartTimelinePoint{
			Bucket:          key,
			Requests:        agg.requests,
			AvgLatency:      avgLatency,
			InputTokens:     agg.inputTokens,
			OutputTokens:    agg.outputTokens,
			ReasoningTokens: agg.reasoningTokens,
			CachedTokens:    agg.cachedTokens,
			Errors401:       agg.errors401,
		})
	}
	if result.Timeline == nil {
		result.Timeline = []ChartTimelinePoint{}
	}

	type modelAgg struct {
		model    string
		requests int64
	}
	models := make([]modelAgg, 0, len(modelMap))
	for model, requests := range modelMap {
		models = append(models, modelAgg{model: model, requests: requests})
	}
	sort.Slice(models, func(i, j int) bool {
		if models[i].requests == models[j].requests {
			return models[i].model < models[j].model
		}
		return models[i].requests > models[j].requests
	})
	if len(models) > 10 {
		models = models[:10]
	}
	for _, model := range models {
		result.Models = append(result.Models, ChartModelPoint{
			Model:    model.model,
			Requests: model.requests,
		})
	}
	if result.Models == nil {
		result.Models = []ChartModelPoint{}
	}

	return result, nil
}

// getAccountEventTrendSQLite SQLite 版账号事件趋势聚合（内存分桶）
func (db *DB) getAccountEventTrendSQLite(ctx context.Context, start, end time.Time, bucketMinutes int) ([]AccountEventPoint, error) {
	if bucketMinutes < 1 {
		bucketMinutes = 60
	}

	rows, err := db.conn.QueryContext(ctx,
		`SELECT created_at, event_type FROM account_events WHERE created_at >= $1 AND created_at <= $2`,
		start, end,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type bucketAgg struct {
		added   int
		deleted int
	}
	bucketMap := make(map[string]*bucketAgg)

	for rows.Next() {
		var createdRaw interface{}
		var eventType string
		if err := rows.Scan(&createdRaw, &eventType); err != nil {
			return nil, err
		}
		createdAt, err := parseDBTimeValue(createdRaw)
		if err != nil || createdAt.IsZero() {
			continue
		}

		// 对齐到桶
		minute := createdAt.Minute()
		aligned := minute - (minute % bucketMinutes)
		bucketTime := time.Date(createdAt.Year(), createdAt.Month(), createdAt.Day(),
			createdAt.Hour(), aligned, 0, 0, createdAt.Location())
		key := bucketTime.Format("2006-01-02T15:04:05")

		agg, ok := bucketMap[key]
		if !ok {
			agg = &bucketAgg{}
			bucketMap[key] = agg
		}
		switch eventType {
		case "added":
			agg.added++
		case "deleted":
			agg.deleted++
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// 排序输出
	keys := make([]string, 0, len(bucketMap))
	for k := range bucketMap {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	result := make([]AccountEventPoint, 0, len(keys))
	for _, k := range keys {
		agg := bucketMap[k]
		result = append(result, AccountEventPoint{Bucket: k, Added: agg.added, Deleted: agg.deleted})
	}
	return result, nil
}
