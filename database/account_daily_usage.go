package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

// AccountDailyUsage 是某账号某一天的官方结算用量快照。
//
// 上游 daily-workspace-usage-counts 可回溯的深度有限（2026-09 实测 ≥84 天，2026-08
// 时只有 7 天），因此这里按天落库累积长期历史。同一天的数值在结算前会变（当天的
// 记录全天持续更新），所以写入是覆盖式 upsert，快照任务每轮回补整个滚动窗口。
//
// 同一行还挂着 daily-token-usage-breakdown 的模型×速度拆分（Breakdown* 三列），
// 由独立的 UpsertAccountDailyBreakdown 写入：两个上游端点各自成功失败，谁到了谁写
// 自己的列，互不覆盖。
type AccountDailyUsage struct {
	AccountID int64  `json:"account_id"`
	Day       string `json:"day"`

	Credits float64 `json:"credits"`
	Users   int     `json:"users"`
	Threads int     `json:"threads"`
	Turns   int     `json:"turns"`

	UncachedInputTokens int64 `json:"uncached_input_tokens"`
	CachedInputTokens   int64 `json:"cached_input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	TotalTokens         int64 `json:"total_tokens"`

	// Settled 为 false 说明抓取时这天还没结算（通常是当天）。
	// 下一轮快照回补同一天时会翻成 true。
	Settled bool `json:"settled"`

	// ClientsJSON / ModelsJSON 原样保存上游的拆分数组，前端直接渲染。
	ClientsJSON string    `json:"clients_json"`
	ModelsJSON  string    `json:"models_json"`
	SyncedAt    time.Time `json:"synced_at"`

	// BreakdownPercent / BreakdownJSON / SurfacesJSON 来自 daily-token-usage-breakdown：
	// 当天各 (model, speed) 与各产品入口的归一化占比。上游按请求区间的峰值归一化，
	// 数值只在同一天内部有意义（份额 = percent / BreakdownPercent），绝不能跨天累加
	// 或跨次同步比较。BreakdownPercent 为 0 表示这天还没同步到拆分。
	BreakdownPercent float64 `json:"breakdown_percent"`
	BreakdownJSON    string  `json:"breakdown_json"`
	SurfacesJSON     string  `json:"surfaces_json"`
}

// AccountDailyUsageInput 是一次 counts upsert 的入参。
type AccountDailyUsageInput struct {
	AccountID           int64
	Day                 string
	Credits             float64
	Users               int
	Threads             int
	Turns               int
	UncachedInputTokens int64
	CachedInputTokens   int64
	OutputTokens        int64
	TotalTokens         int64
	Settled             bool
	ClientsJSON         string
	ModelsJSON          string
}

// AccountDailyBreakdownModel 是落库的单个 (model, speed) 占比条目。
type AccountDailyBreakdownModel struct {
	Model   string  `json:"model"`
	Speed   string  `json:"speed"`
	Percent float64 `json:"percent"`
}

// AccountDailyBreakdownInput 是一次拆分 upsert 的入参。
type AccountDailyBreakdownInput struct {
	AccountID int64
	Day       string
	// Percent 是当天全部 (model, speed) 占比之和，作为份额分母。
	Percent  float64
	Models   []AccountDailyBreakdownModel
	Surfaces map[string]float64
}

// ParseAccountDailyBreakdownModels 解析 BreakdownJSON，坏数据退回空切片。
func ParseAccountDailyBreakdownModels(raw string) []AccountDailyBreakdownModel {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	var out []AccountDailyBreakdownModel
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
		return nil
	}
	return out
}

// ParseAccountDailySurfaces 解析 SurfacesJSON，坏数据退回空表。
func ParseAccountDailySurfaces(raw string) map[string]float64 {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil
	}
	var out map[string]float64
	if err := json.Unmarshal([]byte(trimmed), &out); err != nil {
		return nil
	}
	return out
}

var (
	accountDailyUsageSchemaMu    sync.Mutex
	accountDailyUsageSchemaReady = map[*DB]struct{}{}
)

func (db *DB) ensureAccountDailyUsageTable(ctx context.Context) error {
	if db == nil {
		return errors.New("database unavailable")
	}
	accountDailyUsageSchemaMu.Lock()
	defer accountDailyUsageSchemaMu.Unlock()
	if _, ok := accountDailyUsageSchemaReady[db]; ok {
		return nil
	}
	timeType := "TIMESTAMPTZ"
	boolType := "BOOLEAN"
	if db.isSQLite() {
		timeType = "TIMESTAMP"
		boolType = "INTEGER"
	}
	ddl := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS account_daily_usage (
		account_id BIGINT NOT NULL,
		day VARCHAR(10) NOT NULL,
		credits DOUBLE PRECISION NOT NULL DEFAULT 0,
		users INT NOT NULL DEFAULT 0,
		threads INT NOT NULL DEFAULT 0,
		turns INT NOT NULL DEFAULT 0,
		uncached_input_tokens BIGINT NOT NULL DEFAULT 0,
		cached_input_tokens BIGINT NOT NULL DEFAULT 0,
		output_tokens BIGINT NOT NULL DEFAULT 0,
		total_tokens BIGINT NOT NULL DEFAULT 0,
		settled %s NOT NULL DEFAULT FALSE,
		clients_json TEXT NOT NULL DEFAULT '[]',
		models_json TEXT NOT NULL DEFAULT '[]',
		synced_at %s NOT NULL,
		breakdown_percent DOUBLE PRECISION NOT NULL DEFAULT 0,
		breakdown_json TEXT NOT NULL DEFAULT '[]',
		surfaces_json TEXT NOT NULL DEFAULT '{}',
		PRIMARY KEY (account_id, day)
	)`, boolType, timeType)
	if db.isSQLite() {
		ddl = strings.Replace(ddl, "DEFAULT FALSE", "DEFAULT 0", 1)
	}
	for _, statement := range []string{
		ddl,
		`CREATE INDEX IF NOT EXISTS idx_account_daily_usage_day ON account_daily_usage(day)`,
	} {
		if _, err := db.conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	// 2026-09 新增的拆分三列：老库靠幂等加列补齐。
	breakdownColumns := []struct{ name, def string }{
		{"breakdown_percent", "DOUBLE PRECISION NOT NULL DEFAULT 0"},
		{"breakdown_json", "TEXT NOT NULL DEFAULT '[]'"},
		{"surfaces_json", "TEXT NOT NULL DEFAULT '{}'"},
	}
	for _, column := range breakdownColumns {
		if db.isSQLite() {
			if err := db.ensureSQLiteColumn(ctx, "account_daily_usage", column.name, column.def); err != nil {
				return err
			}
			continue
		}
		if _, err := db.conn.ExecContext(ctx, fmt.Sprintf(`ALTER TABLE account_daily_usage ADD COLUMN IF NOT EXISTS %s %s`, column.name, column.def)); err != nil {
			return err
		}
	}
	accountDailyUsageSchemaReady[db] = struct{}{}
	return nil
}

// UpsertAccountDailyUsage 覆盖式写入一天的 counts 快照。上游同一天的数值会随结算
// 变化，所以固定覆盖而不是累加。不触碰拆分三列：那是 UpsertAccountDailyBreakdown
// 的地盘，两个端点独立同步。
func (db *DB) UpsertAccountDailyUsage(ctx context.Context, input AccountDailyUsageInput) error {
	if err := db.ensureAccountDailyUsageTable(ctx); err != nil {
		return err
	}
	day := strings.TrimSpace(input.Day)
	if input.AccountID <= 0 || day == "" {
		return errors.New("daily usage requires account id and day")
	}
	clients := strings.TrimSpace(input.ClientsJSON)
	if clients == "" {
		clients = "[]"
	}
	models := strings.TrimSpace(input.ModelsJSON)
	if models == "" {
		models = "[]"
	}
	_, err := db.conn.ExecContext(ctx, `INSERT INTO account_daily_usage (
		account_id, day, credits, users, threads, turns,
		uncached_input_tokens, cached_input_tokens, output_tokens, total_tokens,
		settled, clients_json, models_json, synced_at
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
	ON CONFLICT (account_id, day) DO UPDATE SET
		credits=EXCLUDED.credits, users=EXCLUDED.users, threads=EXCLUDED.threads,
		turns=EXCLUDED.turns, uncached_input_tokens=EXCLUDED.uncached_input_tokens,
		cached_input_tokens=EXCLUDED.cached_input_tokens, output_tokens=EXCLUDED.output_tokens,
		total_tokens=EXCLUDED.total_tokens, settled=EXCLUDED.settled,
		clients_json=EXCLUDED.clients_json, models_json=EXCLUDED.models_json,
		synced_at=EXCLUDED.synced_at`,
		input.AccountID, day, input.Credits, input.Users, input.Threads, input.Turns,
		input.UncachedInputTokens, input.CachedInputTokens, input.OutputTokens, input.TotalTokens,
		input.Settled, clients, models, time.Now().UTC(),
	)
	return err
}

// UpsertAccountDailyBreakdown 覆盖式写入一天的模型×速度拆分。只写拆分三列：
// 行不存在时以零 counts 建行（synced_at 取当前时间满足非空约束，counts 到了会覆盖），
// 行已存在时不动 counts 列与 synced_at。
func (db *DB) UpsertAccountDailyBreakdown(ctx context.Context, input AccountDailyBreakdownInput) error {
	if err := db.ensureAccountDailyUsageTable(ctx); err != nil {
		return err
	}
	day := strings.TrimSpace(input.Day)
	if input.AccountID <= 0 || day == "" {
		return errors.New("daily breakdown requires account id and day")
	}
	if input.Percent <= 0 {
		return errors.New("daily breakdown requires a positive percent total")
	}
	models := input.Models
	if models == nil {
		models = []AccountDailyBreakdownModel{}
	}
	modelsJSON, err := json.Marshal(models)
	if err != nil {
		return err
	}
	surfaces := input.Surfaces
	if surfaces == nil {
		surfaces = map[string]float64{}
	}
	surfacesJSON, err := json.Marshal(surfaces)
	if err != nil {
		return err
	}
	_, err = db.conn.ExecContext(ctx, `INSERT INTO account_daily_usage (
		account_id, day, synced_at, breakdown_percent, breakdown_json, surfaces_json
	) VALUES ($1,$2,$3,$4,$5,$6)
	ON CONFLICT (account_id, day) DO UPDATE SET
		breakdown_percent=EXCLUDED.breakdown_percent,
		breakdown_json=EXCLUDED.breakdown_json,
		surfaces_json=EXCLUDED.surfaces_json`,
		input.AccountID, day, time.Now().UTC(), input.Percent, string(modelsJSON), string(surfacesJSON),
	)
	return err
}

// accountDailyUsageCountsEvidence 判断一行是否真的来自 counts 端点。拆分先到时会以零
// counts 建行（credits/turns/tokens 全 0 且未结算），这种行不能算「有官方结算快照」，
// 否则 page-stats 会把它当成 $0 的真实快照、深回补也会被跳过。counts 是稀疏端点，
// 只回有活动的日子，真实 counts 行至少有一项非零。
const accountDailyUsageCountsEvidence = "(settled OR turns>0 OR credits>0 OR total_tokens>0)"

// HasCounts 报告这一行是否带有 counts 端点写入的数据，与 accountDailyUsageCountsEvidence 同义。
func (u *AccountDailyUsage) HasCounts() bool {
	return u != nil && (u.Settled || u.Turns > 0 || u.Credits > 0 || u.TotalTokens > 0)
}

// AccountDailyUsageCoverage 是某账号本地快照的覆盖起点，按端点分开。
type AccountDailyUsageCoverage struct {
	// CountsOldestDay 是最早一条带 counts 数据的日期，空串表示还没有。
	CountsOldestDay string
	// BreakdownOldestDay 是最早一条带拆分的日期，空串表示还没有。
	BreakdownOldestDay string
}

// GetAccountDailyUsageCoverage 返回两个端点各自的本地覆盖起点。同步据此按端点决定
// 是否深回补：只看「有没有行」会被拆分先建的零 counts 行骗过。
func (db *DB) GetAccountDailyUsageCoverage(ctx context.Context, accountID int64) (AccountDailyUsageCoverage, error) {
	var out AccountDailyUsageCoverage
	if err := db.ensureAccountDailyUsageTable(ctx); err != nil {
		return out, err
	}
	if accountID <= 0 {
		return out, errors.New("daily usage requires account id")
	}
	var counts, breakdown sql.NullString
	err := db.conn.QueryRowContext(ctx, `SELECT
		MIN(CASE WHEN `+accountDailyUsageCountsEvidence+` THEN day END),
		MIN(CASE WHEN breakdown_percent>0 THEN day END)
		FROM account_daily_usage WHERE account_id=$1`, accountID).Scan(&counts, &breakdown)
	if err != nil {
		return out, err
	}
	out.CountsOldestDay = strings.TrimSpace(counts.String)
	out.BreakdownOldestDay = strings.TrimSpace(breakdown.String)
	return out, nil
}

const accountDailyUsageSelect = `SELECT account_id, day, credits, users, threads, turns,
	uncached_input_tokens, cached_input_tokens, output_tokens, total_tokens,
	settled, clients_json, models_json, synced_at,
	breakdown_percent, breakdown_json, surfaces_json FROM account_daily_usage`

// ListAccountDailyUsage 返回某账号最近 days 天的快照，按日期升序。
func (db *DB) ListAccountDailyUsage(ctx context.Context, accountID int64, days int) ([]*AccountDailyUsage, error) {
	if err := db.ensureAccountDailyUsageTable(ctx); err != nil {
		return nil, err
	}
	if accountID <= 0 {
		return nil, errors.New("daily usage requires account id")
	}
	if days <= 0 {
		days = 30
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	rows, err := db.conn.QueryContext(ctx,
		accountDailyUsageSelect+` WHERE account_id=$1 AND day>=$2 ORDER BY day ASC`,
		accountID, cutoff,
	)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := make([]*AccountDailyUsage, 0, days)
	for rows.Next() {
		item, scanErr := scanAccountDailyUsage(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// SumAccountDailyUsage 汇总某账号最近 days 天的 credits 与 token 总量，
// 供列表列与统计卡使用（不带客户端/模型拆分，避免解析 JSON）。
// 只统计带 counts 数据的行：拆分先建的零 counts 行不算快照，否则列表会把
// 「counts 从没同步成功」的账号显示成 $0 并停止回补。
func (db *DB) SumAccountDailyUsage(ctx context.Context, accountIDs []int64, days int) (map[int64]AccountDailyUsageTotal, error) {
	if err := db.ensureAccountDailyUsageTable(ctx); err != nil {
		return nil, err
	}
	out := make(map[int64]AccountDailyUsageTotal, len(accountIDs))
	if len(accountIDs) == 0 {
		return out, nil
	}
	if days <= 0 {
		days = 7
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -(days - 1)).Format("2006-01-02")
	placeholders := make([]string, 0, len(accountIDs))
	args := make([]any, 0, len(accountIDs)+1)
	args = append(args, cutoff)
	for i, id := range accountIDs {
		placeholders = append(placeholders, fmt.Sprintf("$%d", i+2))
		args = append(args, id)
	}
	query := fmt.Sprintf(`SELECT account_id, COALESCE(SUM(credits),0), COALESCE(SUM(total_tokens),0), COALESCE(SUM(turns),0)
		FROM account_daily_usage WHERE day>=$1 AND account_id IN (%s) AND %s GROUP BY account_id`,
		strings.Join(placeholders, ","), accountDailyUsageCountsEvidence)
	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id int64
		var total AccountDailyUsageTotal
		if err := rows.Scan(&id, &total.Credits, &total.TotalTokens, &total.Turns); err != nil {
			return nil, err
		}
		out[id] = total
	}
	return out, rows.Err()
}

// SumAccountDailyUsageSince 汇总某账号自 sinceDay（含）起所有带 counts 数据的快照，
// 返回 credits 合计与天数。供「本周期已用」卡片按账号的重置周期起点累加。
func (db *DB) SumAccountDailyUsageSince(ctx context.Context, accountID int64, sinceDay string) (credits float64, days int, err error) {
	if err := db.ensureAccountDailyUsageTable(ctx); err != nil {
		return 0, 0, err
	}
	sinceDay = strings.TrimSpace(sinceDay)
	if accountID <= 0 || sinceDay == "" {
		return 0, 0, errors.New("daily usage sum requires account id and start day")
	}
	err = db.conn.QueryRowContext(ctx, `SELECT COALESCE(SUM(credits),0), COUNT(*) FROM account_daily_usage
		WHERE account_id=$1 AND day>=$2 AND `+accountDailyUsageCountsEvidence, accountID, sinceDay).Scan(&credits, &days)
	if err != nil {
		return 0, 0, err
	}
	return credits, days, nil
}

// AccountDailyUsageTotal 是一个账号在窗口内的汇总。
type AccountDailyUsageTotal struct {
	Credits     float64 `json:"credits"`
	TotalTokens int64   `json:"total_tokens"`
	Turns       int64   `json:"turns"`
}

// PruneAccountDailyUsage 删除早于保留期的快照，避免表无限增长。
func (db *DB) PruneAccountDailyUsage(ctx context.Context, keepDays int) error {
	if err := db.ensureAccountDailyUsageTable(ctx); err != nil {
		return err
	}
	if keepDays <= 0 {
		return nil
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -keepDays).Format("2006-01-02")
	_, err := db.conn.ExecContext(ctx, `DELETE FROM account_daily_usage WHERE day<$1`, cutoff)
	return err
}

// AccountDailySurface 是一个产品入口的占比条目。
type AccountDailySurface struct {
	Surface string
	Percent float64
}

// SortedAccountDailySurfaces 把入口占比表按占比降序、同占比按名字排成稳定列表。
func SortedAccountDailySurfaces(surfaces map[string]float64) []AccountDailySurface {
	out := make([]AccountDailySurface, 0, len(surfaces))
	for name, value := range surfaces {
		out = append(out, AccountDailySurface{Surface: name, Percent: value})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Percent != out[j].Percent {
			return out[i].Percent > out[j].Percent
		}
		return out[i].Surface < out[j].Surface
	})
	return out
}

func scanAccountDailyUsage(scanner interface{ Scan(...any) error }) (*AccountDailyUsage, error) {
	item := &AccountDailyUsage{}
	var syncedAt any
	if err := scanner.Scan(
		&item.AccountID, &item.Day, &item.Credits, &item.Users, &item.Threads, &item.Turns,
		&item.UncachedInputTokens, &item.CachedInputTokens, &item.OutputTokens, &item.TotalTokens,
		&item.Settled, &item.ClientsJSON, &item.ModelsJSON, &syncedAt,
		&item.BreakdownPercent, &item.BreakdownJSON, &item.SurfacesJSON,
	); err != nil {
		return nil, err
	}
	parsed, err := parsePromptRiskTimeValue(syncedAt)
	if err != nil {
		return nil, err
	}
	item.SyncedAt = parsed
	return item, nil
}
