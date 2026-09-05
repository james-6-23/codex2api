package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"
)

const (
	proxyRiskScoringProviderScamalytics = "scamalytics"
	proxyRiskScoringStatusSuccess       = "success"
	proxyRiskScoringStatusError         = "error"
	proxyRiskScoringStatusSkipped       = "skipped"
	proxyRiskScoringMaxRawBytes         = 256 * 1024
	proxyRiskScoringMaxFeaturesBytes    = 128 * 1024
	proxyRiskScoringDefaultTimeout      = 8
	proxyRiskScoringDefaultConcurrency  = 3
	proxyRiskScoringDefaultCacheTTL     = 3600
	proxyRiskScoringDefaultHost         = "api11.scamalytics.com"
)

// ProxyRiskScoringProfile is an operator-managed scoring service profile.
// Credentials are never serialized by the admin API; callers should use the
// masked response type in admin/proxy_risk_scoring.go.
type ProxyRiskScoringProfile struct {
	ID       int64  `json:"id"`
	Name     string `json:"name"`
	Provider string `json:"provider"`
	Enabled  bool   `json:"enabled"`
	Priority int    `json:"priority"`
	// BaseURL and AccessToken are retained only to read profiles created by the
	// earlier external-wrapper prototype. The embedded engine never uses them
	// and the admin API does not expose or accept them.
	BaseURL            string     `json:"-"`
	AccessToken        string     `json:"-"`
	ScamalyticsHost    string     `json:"scamalytics_host"`
	ScamalyticsUser    string     `json:"scamalytics_user"`
	ScamalyticsKey     string     `json:"-"`
	TimeoutSeconds     int        `json:"timeout_seconds"`
	Concurrency        int        `json:"concurrency"`
	RequestDelayMS     int        `json:"request_delay_ms"`
	CacheTTLSeconds    int        `json:"cache_ttl_seconds"`
	MaxChecksPerJob    int        `json:"max_checks_per_job"`
	DailyCheckLimit    int        `json:"daily_check_limit"`
	CreditReserve      int64      `json:"credit_reserve"`
	AllowForceRefresh  bool       `json:"allow_force_refresh"`
	ResolveHostnames   bool       `json:"resolve_hostnames"`
	AllowPrivateTarget bool       `json:"allow_private_targets"`
	DocsURL            string     `json:"docs_url"`
	TutorialURL        string     `json:"tutorial_url"`
	DailyUsedDate      string     `json:"daily_used_date"`
	DailyUsedCount     int        `json:"daily_used_count"`
	CreditsRemaining   *int64     `json:"credits_remaining,omitempty"`
	CreditsUsed        *int64     `json:"credits_used,omitempty"`
	CreditResetAt      *time.Time `json:"credit_reset_at,omitempty"`
	LastQuotaCheckedAt *time.Time `json:"last_quota_checked_at,omitempty"`
	LastError          string     `json:"last_error,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

// NormalizeProxyRiskScoringProfile validates only operator-controlled values.
// Credentials and documentation URLs are intentionally not populated with
// built-in secrets or provider-specific quotas.
func NormalizeProxyRiskScoringProfile(profile ProxyRiskScoringProfile) ProxyRiskScoringProfile {
	profile.Name = strings.TrimSpace(profile.Name)
	if profile.Name == "" {
		profile.Name = "proxy-risk-profile"
	}
	profile.Provider = strings.ToLower(strings.TrimSpace(profile.Provider))
	if profile.Provider == "" {
		profile.Provider = proxyRiskScoringProviderScamalytics
	}
	profile.BaseURL = strings.TrimRight(strings.TrimSpace(profile.BaseURL), "/")
	profile.ScamalyticsHost = strings.ToLower(strings.TrimSpace(profile.ScamalyticsHost))
	if profile.ScamalyticsHost == "" {
		profile.ScamalyticsHost = proxyRiskScoringDefaultHost
	}
	profile.ScamalyticsUser = strings.TrimSpace(profile.ScamalyticsUser)
	profile.DocsURL = strings.TrimSpace(profile.DocsURL)
	profile.TutorialURL = strings.TrimSpace(profile.TutorialURL)
	if profile.Priority < 0 {
		profile.Priority = 0
	}
	if profile.TimeoutSeconds <= 0 {
		profile.TimeoutSeconds = proxyRiskScoringDefaultTimeout
	}
	if profile.TimeoutSeconds > 120 {
		profile.TimeoutSeconds = 120
	}
	if profile.Concurrency <= 0 {
		profile.Concurrency = proxyRiskScoringDefaultConcurrency
	}
	if profile.Concurrency > 64 {
		profile.Concurrency = 64
	}
	if profile.RequestDelayMS < 0 {
		profile.RequestDelayMS = 0
	}
	if profile.RequestDelayMS > 60_000 {
		profile.RequestDelayMS = 60_000
	}
	if profile.CacheTTLSeconds <= 0 {
		profile.CacheTTLSeconds = proxyRiskScoringDefaultCacheTTL
	}
	if profile.CacheTTLSeconds > 30*24*60*60 {
		profile.CacheTTLSeconds = 30 * 24 * 60 * 60
	}
	if profile.MaxChecksPerJob < 0 {
		profile.MaxChecksPerJob = 0
	}
	if profile.DailyCheckLimit < 0 {
		profile.DailyCheckLimit = 0
	}
	if profile.CreditReserve < 0 {
		profile.CreditReserve = 0
	}
	profile.LastError = strings.TrimSpace(profile.LastError)
	return profile
}

// ProxyRiskScoreSnapshot is an immutable reference-only observation for one
// proxy and one scoring profile. Score is nullable: unknown is not zero.
type ProxyRiskScoreSnapshot struct {
	ID              int64      `json:"id"`
	ProxyID         int64      `json:"proxy_id"`
	ProfileID       int64      `json:"profile_id"`
	Provider        string     `json:"provider"`
	ResolvedIP      string     `json:"resolved_ip"`
	Score           *int       `json:"score"`
	RiskLevel       string     `json:"risk_level"`
	Recommendation  string     `json:"recommendation"`
	ProxyType       string     `json:"proxy_type,omitempty"`
	IsVPN           bool       `json:"is_vpn"`
	IsTOR           bool       `json:"is_tor"`
	IsDatacenter    bool       `json:"is_datacenter"`
	IsBlacklisted   bool       `json:"is_blacklisted"`
	BlacklistSource []string   `json:"blacklist_sources,omitempty"`
	ISP             string     `json:"isp,omitempty"`
	Country         string     `json:"country,omitempty"`
	LatencyMS       int        `json:"latency_ms"`
	Status          string     `json:"status"`
	Error           string     `json:"error,omitempty"`
	FeaturesJSON    string     `json:"features_json,omitempty"`
	RawResponseJSON string     `json:"raw_response_json,omitempty"`
	CheckedAt       time.Time  `json:"checked_at"`
	ExpiresAt       *time.Time `json:"expires_at,omitempty"`
}

const sqliteProxyRiskScoringDDL = `
CREATE TABLE IF NOT EXISTS proxy_risk_scoring_profiles (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 name TEXT NOT NULL UNIQUE,
 provider TEXT NOT NULL DEFAULT 'scamalytics',
 enabled INTEGER NOT NULL DEFAULT 0,
 priority INTEGER NOT NULL DEFAULT 0,
 base_url TEXT NOT NULL DEFAULT '',
 access_token TEXT NOT NULL DEFAULT '',
 scamalytics_host TEXT NOT NULL DEFAULT '',
 scamalytics_user TEXT NOT NULL DEFAULT '',
 scamalytics_key TEXT NOT NULL DEFAULT '',
 timeout_seconds INTEGER NOT NULL DEFAULT 8,
 concurrency INTEGER NOT NULL DEFAULT 3,
 request_delay_ms INTEGER NOT NULL DEFAULT 0,
 cache_ttl_seconds INTEGER NOT NULL DEFAULT 3600,
 max_checks_per_job INTEGER NOT NULL DEFAULT 0,
 daily_check_limit INTEGER NOT NULL DEFAULT 0,
 credit_reserve INTEGER NOT NULL DEFAULT 0,
 allow_force_refresh INTEGER NOT NULL DEFAULT 0,
 resolve_hostnames INTEGER NOT NULL DEFAULT 0,
 allow_private_targets INTEGER NOT NULL DEFAULT 0,
 docs_url TEXT NOT NULL DEFAULT '',
 tutorial_url TEXT NOT NULL DEFAULT '',
 daily_used_date TEXT NOT NULL DEFAULT '',
 daily_used_count INTEGER NOT NULL DEFAULT 0,
 credits_remaining INTEGER NULL,
 credits_used INTEGER NULL,
 credit_reset_at TIMESTAMP NULL,
 last_quota_checked_at TIMESTAMP NULL,
 last_error TEXT NOT NULL DEFAULT '',
 created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
 updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_proxy_risk_profile_enabled ON proxy_risk_scoring_profiles(enabled, priority, id);
`

const postgresProxyRiskScoringDDL = `
CREATE TABLE IF NOT EXISTS proxy_risk_scoring_profiles (
 id BIGSERIAL PRIMARY KEY,
 name VARCHAR(120) NOT NULL UNIQUE,
 provider VARCHAR(64) NOT NULL DEFAULT 'scamalytics',
 enabled BOOLEAN NOT NULL DEFAULT FALSE,
 priority INTEGER NOT NULL DEFAULT 0,
 base_url VARCHAR(512) NOT NULL DEFAULT '',
 access_token TEXT NOT NULL DEFAULT '',
 scamalytics_host VARCHAR(512) NOT NULL DEFAULT '',
 scamalytics_user VARCHAR(255) NOT NULL DEFAULT '',
 scamalytics_key TEXT NOT NULL DEFAULT '',
 timeout_seconds INTEGER NOT NULL DEFAULT 8,
 concurrency INTEGER NOT NULL DEFAULT 3,
 request_delay_ms INTEGER NOT NULL DEFAULT 0,
 cache_ttl_seconds INTEGER NOT NULL DEFAULT 3600,
 max_checks_per_job INTEGER NOT NULL DEFAULT 0,
 daily_check_limit INTEGER NOT NULL DEFAULT 0,
 credit_reserve BIGINT NOT NULL DEFAULT 0,
 allow_force_refresh BOOLEAN NOT NULL DEFAULT FALSE,
 resolve_hostnames BOOLEAN NOT NULL DEFAULT FALSE,
 allow_private_targets BOOLEAN NOT NULL DEFAULT FALSE,
 docs_url VARCHAR(1024) NOT NULL DEFAULT '',
 tutorial_url VARCHAR(1024) NOT NULL DEFAULT '',
 daily_used_date VARCHAR(16) NOT NULL DEFAULT '',
 daily_used_count INTEGER NOT NULL DEFAULT 0,
 credits_remaining BIGINT NULL,
 credits_used BIGINT NULL,
 credit_reset_at TIMESTAMP NULL,
 last_quota_checked_at TIMESTAMP NULL,
 last_error TEXT NOT NULL DEFAULT '',
 created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
 updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_proxy_risk_profile_enabled ON proxy_risk_scoring_profiles(enabled, priority, id);
`

const sqliteProxyRiskScoringSnapshotsDDL = `
CREATE TABLE IF NOT EXISTS proxy_risk_score_snapshots (
 id INTEGER PRIMARY KEY AUTOINCREMENT,
 proxy_id INTEGER NOT NULL,
 profile_id INTEGER NOT NULL,
 provider TEXT NOT NULL DEFAULT '',
 resolved_ip TEXT NOT NULL DEFAULT '',
 score INTEGER NULL,
 risk_level TEXT NOT NULL DEFAULT '',
 recommendation TEXT NOT NULL DEFAULT '',
 proxy_type TEXT NOT NULL DEFAULT '',
 is_vpn INTEGER NOT NULL DEFAULT 0,
 is_tor INTEGER NOT NULL DEFAULT 0,
 is_datacenter INTEGER NOT NULL DEFAULT 0,
 is_blacklisted INTEGER NOT NULL DEFAULT 0,
 blacklist_sources TEXT NOT NULL DEFAULT '[]',
 isp TEXT NOT NULL DEFAULT '',
 country TEXT NOT NULL DEFAULT '',
 latency_ms INTEGER NOT NULL DEFAULT 0,
 status TEXT NOT NULL DEFAULT 'error',
 error TEXT NOT NULL DEFAULT '',
 features_json TEXT NOT NULL DEFAULT '{}',
 raw_response_json TEXT NOT NULL DEFAULT '',
 checked_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
 expires_at TIMESTAMP NULL
);
CREATE INDEX IF NOT EXISTS idx_proxy_risk_snapshot_latest ON proxy_risk_score_snapshots(proxy_id, profile_id, checked_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_proxy_risk_snapshot_profile_time ON proxy_risk_score_snapshots(profile_id, checked_at DESC, id DESC);
`

const postgresProxyRiskScoringSnapshotsDDL = `
CREATE TABLE IF NOT EXISTS proxy_risk_score_snapshots (
 id BIGSERIAL PRIMARY KEY,
 proxy_id BIGINT NOT NULL,
 profile_id BIGINT NOT NULL,
 provider VARCHAR(64) NOT NULL DEFAULT '',
 resolved_ip VARCHAR(64) NOT NULL DEFAULT '',
 score INTEGER NULL,
 risk_level VARCHAR(32) NOT NULL DEFAULT '',
 recommendation VARCHAR(32) NOT NULL DEFAULT '',
 proxy_type VARCHAR(32) NOT NULL DEFAULT '',
 is_vpn BOOLEAN NOT NULL DEFAULT FALSE,
 is_tor BOOLEAN NOT NULL DEFAULT FALSE,
 is_datacenter BOOLEAN NOT NULL DEFAULT FALSE,
 is_blacklisted BOOLEAN NOT NULL DEFAULT FALSE,
 blacklist_sources TEXT NOT NULL DEFAULT '[]',
 isp VARCHAR(512) NOT NULL DEFAULT '',
 country VARCHAR(128) NOT NULL DEFAULT '',
 latency_ms INTEGER NOT NULL DEFAULT 0,
 status VARCHAR(32) NOT NULL DEFAULT 'error',
 error TEXT NOT NULL DEFAULT '',
 features_json TEXT NOT NULL DEFAULT '{}',
 raw_response_json TEXT NOT NULL DEFAULT '',
 checked_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
 expires_at TIMESTAMP NULL
);
CREATE INDEX IF NOT EXISTS idx_proxy_risk_snapshot_latest ON proxy_risk_score_snapshots(proxy_id, profile_id, checked_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_proxy_risk_snapshot_profile_time ON proxy_risk_score_snapshots(profile_id, checked_at DESC, id DESC);
`

func (db *DB) ensureProxyRiskScoringTables(ctx context.Context) error {
	if db == nil || db.conn == nil {
		return errors.New("database is not initialized")
	}
	ddl := postgresProxyRiskScoringDDL + postgresProxyRiskScoringSnapshotsDDL
	if db.isSQLite() {
		ddl = sqliteProxyRiskScoringDDL + sqliteProxyRiskScoringSnapshotsDDL
	}
	for _, stmt := range strings.Split(ddl, ";") {
		stmt = strings.TrimSpace(stmt)
		if stmt == "" {
			continue
		}
		if _, err := db.conn.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func proxyRiskScoringSecret(field, value string) string {
	return encryptCredentialValue("proxy_risk_"+field, strings.TrimSpace(value))
}

func proxyRiskScoringReveal(field, value string) string {
	return decryptCredentialValue("proxy_risk_"+field, value)
}

func proxyRiskProfileArgs(profile ProxyRiskScoringProfile) []any {
	profile = NormalizeProxyRiskScoringProfile(profile)
	return []any{
		profile.Name, profile.Provider, profile.Enabled, profile.Priority, profile.BaseURL,
		proxyRiskScoringSecret("access_token", profile.AccessToken), profile.ScamalyticsHost,
		proxyRiskScoringSecret("scamalytics_user", profile.ScamalyticsUser), proxyRiskScoringSecret("scamalytics_key", profile.ScamalyticsKey),
		profile.TimeoutSeconds, profile.Concurrency, profile.RequestDelayMS, profile.CacheTTLSeconds,
		profile.MaxChecksPerJob, profile.DailyCheckLimit, profile.CreditReserve, profile.AllowForceRefresh,
		profile.ResolveHostnames, profile.AllowPrivateTarget, profile.DocsURL, profile.TutorialURL,
	}
}

func (db *DB) CreateProxyRiskScoringProfile(ctx context.Context, profile *ProxyRiskScoringProfile) (int64, error) {
	if profile == nil {
		return 0, errors.New("profile is nil")
	}
	args := proxyRiskProfileArgs(*profile)
	query := `INSERT INTO proxy_risk_scoring_profiles (name,provider,enabled,priority,base_url,access_token,scamalytics_host,scamalytics_user,scamalytics_key,timeout_seconds,concurrency,request_delay_ms,cache_ttl_seconds,max_checks_per_job,daily_check_limit,credit_reserve,allow_force_refresh,resolve_hostnames,allow_private_targets,docs_url,tutorial_url) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)`
	if db.isSQLite() {
		query = strings.ReplaceAll(query, "$", "?")
	}
	var id int64
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		if db.isSQLite() {
			result, err := tx.ExecContext(ctx, query, args...)
			if err != nil {
				return err
			}
			id, err = result.LastInsertId()
			return err
		}
		return tx.QueryRowContext(ctx, query+" RETURNING id", args...).Scan(&id)
	})
	if err != nil {
		return 0, err
	}
	profile.ID = id
	return id, nil
}

func (db *DB) UpdateProxyRiskScoringProfile(ctx context.Context, profile *ProxyRiskScoringProfile) error {
	if profile == nil || profile.ID <= 0 {
		return errors.New("profile is invalid")
	}
	args := proxyRiskProfileArgs(*profile)
	args = append(args, profile.ID)
	query := `UPDATE proxy_risk_scoring_profiles SET name=$1,provider=$2,enabled=$3,priority=$4,base_url=$5,access_token=$6,scamalytics_host=$7,scamalytics_user=$8,scamalytics_key=$9,timeout_seconds=$10,concurrency=$11,request_delay_ms=$12,cache_ttl_seconds=$13,max_checks_per_job=$14,daily_check_limit=$15,credit_reserve=$16,allow_force_refresh=$17,resolve_hostnames=$18,allow_private_targets=$19,docs_url=$20,tutorial_url=$21,updated_at=CURRENT_TIMESTAMP WHERE id=$22`
	if db.isSQLite() {
		query = strings.ReplaceAll(query, "$", "?")
	}
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, query, args...)
		if err != nil {
			return err
		}
		if affected, _ := result.RowsAffected(); affected == 0 {
			return sql.ErrNoRows
		}
		return nil
	})
}

func (db *DB) DeleteProxyRiskScoringProfile(ctx context.Context, id int64) error {
	if id <= 0 {
		return errors.New("profile id is invalid")
	}
	query := `DELETE FROM proxy_risk_scoring_profiles WHERE id=$1`
	if db.isSQLite() {
		query = `DELETE FROM proxy_risk_scoring_profiles WHERE id=?`
	}
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, query, id)
		if err != nil {
			return err
		}
		if affected, _ := result.RowsAffected(); affected == 0 {
			return sql.ErrNoRows
		}
		return nil
	})
}

func (db *DB) ListProxyRiskScoringProfiles(ctx context.Context) ([]ProxyRiskScoringProfile, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT id,name,provider,enabled,priority,base_url,access_token,scamalytics_host,scamalytics_user,scamalytics_key,timeout_seconds,concurrency,request_delay_ms,cache_ttl_seconds,max_checks_per_job,daily_check_limit,credit_reserve,allow_force_refresh,resolve_hostnames,allow_private_targets,docs_url,tutorial_url,daily_used_date,daily_used_count,credits_remaining,credits_used,credit_reset_at,last_quota_checked_at,last_error,created_at,updated_at FROM proxy_risk_scoring_profiles ORDER BY enabled DESC, priority ASC, id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	profiles := make([]ProxyRiskScoringProfile, 0)
	for rows.Next() {
		profile, err := scanProxyRiskScoringProfile(rows)
		if err != nil {
			return nil, err
		}
		profiles = append(profiles, profile)
	}
	return profiles, rows.Err()
}

func (db *DB) GetProxyRiskScoringProfile(ctx context.Context, id int64) (*ProxyRiskScoringProfile, error) {
	if id <= 0 {
		return nil, errors.New("profile id is invalid")
	}
	row := db.conn.QueryRowContext(ctx, `SELECT id,name,provider,enabled,priority,base_url,access_token,scamalytics_host,scamalytics_user,scamalytics_key,timeout_seconds,concurrency,request_delay_ms,cache_ttl_seconds,max_checks_per_job,daily_check_limit,credit_reserve,allow_force_refresh,resolve_hostnames,allow_private_targets,docs_url,tutorial_url,daily_used_date,daily_used_count,credits_remaining,credits_used,credit_reset_at,last_quota_checked_at,last_error,created_at,updated_at FROM proxy_risk_scoring_profiles WHERE id=$1`, id)
	if db.isSQLite() {
		row = db.conn.QueryRowContext(ctx, `SELECT id,name,provider,enabled,priority,base_url,access_token,scamalytics_host,scamalytics_user,scamalytics_key,timeout_seconds,concurrency,request_delay_ms,cache_ttl_seconds,max_checks_per_job,daily_check_limit,credit_reserve,allow_force_refresh,resolve_hostnames,allow_private_targets,docs_url,tutorial_url,daily_used_date,daily_used_count,credits_remaining,credits_used,credit_reset_at,last_quota_checked_at,last_error,created_at,updated_at FROM proxy_risk_scoring_profiles WHERE id=?`, id)
	}
	profile, err := scanProxyRiskScoringProfile(row)
	if err != nil {
		return nil, err
	}
	return &profile, nil
}

type proxyRiskScanner interface{ Scan(...any) error }

func scanProxyRiskScoringProfile(scanner proxyRiskScanner) (ProxyRiskScoringProfile, error) {
	var profile ProxyRiskScoringProfile
	var accessToken, scamUser, scamKey string
	var remaining, used sql.NullInt64
	var resetAt, quotaAt, createdAt, updatedAt any
	if err := scanner.Scan(&profile.ID, &profile.Name, &profile.Provider, &profile.Enabled, &profile.Priority, &profile.BaseURL, &accessToken, &profile.ScamalyticsHost, &scamUser, &scamKey, &profile.TimeoutSeconds, &profile.Concurrency, &profile.RequestDelayMS, &profile.CacheTTLSeconds, &profile.MaxChecksPerJob, &profile.DailyCheckLimit, &profile.CreditReserve, &profile.AllowForceRefresh, &profile.ResolveHostnames, &profile.AllowPrivateTarget, &profile.DocsURL, &profile.TutorialURL, &profile.DailyUsedDate, &profile.DailyUsedCount, &remaining, &used, &resetAt, &quotaAt, &profile.LastError, &createdAt, &updatedAt); err != nil {
		return profile, err
	}
	profile.AccessToken = proxyRiskScoringReveal("access_token", accessToken)
	profile.ScamalyticsUser = proxyRiskScoringReveal("scamalytics_user", scamUser)
	profile.ScamalyticsKey = proxyRiskScoringReveal("scamalytics_key", scamKey)
	if remaining.Valid {
		value := remaining.Int64
		profile.CreditsRemaining = &value
	}
	if used.Valid {
		value := used.Int64
		profile.CreditsUsed = &value
	}
	if parsed, err := parseDBTimeValue(resetAt); err == nil && !parsed.IsZero() {
		profile.CreditResetAt = &parsed
	}
	if parsed, err := parseDBTimeValue(quotaAt); err == nil && !parsed.IsZero() {
		profile.LastQuotaCheckedAt = &parsed
	}
	profile.CreatedAt, _ = parseDBTimeValue(createdAt)
	profile.UpdatedAt, _ = parseDBTimeValue(updatedAt)
	return NormalizeProxyRiskScoringProfile(profile), nil
}

func (db *DB) ReserveProxyRiskScoringCheck(ctx context.Context, id int64, now time.Time) (bool, int, error) {
	if id <= 0 {
		return false, 0, errors.New("profile id is invalid")
	}
	day := now.UTC().Format("2006-01-02")
	query := `UPDATE proxy_risk_scoring_profiles SET daily_used_count=CASE WHEN daily_used_date=$1 THEN daily_used_count+1 ELSE 1 END,daily_used_date=$1,updated_at=CURRENT_TIMESTAMP WHERE id=$2 AND (daily_check_limit<=0 OR daily_used_date<>$1 OR daily_used_count<daily_check_limit)`
	if db.isSQLite() {
		query = strings.ReplaceAll(query, "$", "?")
	}
	allowed := false
	count := 0
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		result, err := tx.ExecContext(ctx, query, day, id)
		if err != nil {
			return err
		}
		if affected, _ := result.RowsAffected(); affected == 0 {
			return nil
		}
		allowed = true
		selectQuery := `SELECT daily_used_count FROM proxy_risk_scoring_profiles WHERE id=$1`
		if db.isSQLite() {
			selectQuery = `SELECT daily_used_count FROM proxy_risk_scoring_profiles WHERE id=?`
		}
		return tx.QueryRowContext(ctx, selectQuery, id).Scan(&count)
	})
	return allowed, count, err
}

func (db *DB) UpdateProxyRiskScoringQuota(ctx context.Context, id int64, remaining, used *int64, resetAt *time.Time, lastError string) error {
	if id <= 0 {
		return errors.New("profile id is invalid")
	}
	query := `UPDATE proxy_risk_scoring_profiles SET credits_remaining=$1,credits_used=$2,credit_reset_at=$3,last_quota_checked_at=$4,last_error=$5,updated_at=CURRENT_TIMESTAMP WHERE id=$6`
	if db.isSQLite() {
		query = strings.ReplaceAll(query, "$", "?")
	}
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, query, nullableInt64(remaining), nullableInt64(used), nullableTime(db, resetAt), db.timeArg(time.Now()), strings.TrimSpace(lastError), id)
		return err
	})
}

func nullableInt64(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullableTime(db *DB, value *time.Time) any {
	if value == nil {
		return nil
	}
	return db.timeArg(*value)
}

func (db *DB) InsertProxyRiskScoreSnapshot(ctx context.Context, snapshot *ProxyRiskScoreSnapshot) error {
	if snapshot == nil || snapshot.ProxyID <= 0 || snapshot.ProfileID <= 0 {
		return errors.New("snapshot is invalid")
	}
	snapshot.Provider = strings.TrimSpace(snapshot.Provider)
	snapshot.ResolvedIP = strings.TrimSpace(snapshot.ResolvedIP)
	snapshot.RiskLevel = strings.TrimSpace(snapshot.RiskLevel)
	snapshot.Recommendation = strings.TrimSpace(snapshot.Recommendation)
	snapshot.Status = strings.TrimSpace(snapshot.Status)
	if snapshot.Status == "" {
		snapshot.Status = proxyRiskScoringStatusError
	}
	if len(snapshot.FeaturesJSON) > proxyRiskScoringMaxFeaturesBytes {
		snapshot.FeaturesJSON = snapshot.FeaturesJSON[:proxyRiskScoringMaxFeaturesBytes]
	}
	if len(snapshot.RawResponseJSON) > proxyRiskScoringMaxRawBytes {
		snapshot.RawResponseJSON = snapshot.RawResponseJSON[:proxyRiskScoringMaxRawBytes]
	}
	if snapshot.CheckedAt.IsZero() {
		snapshot.CheckedAt = time.Now().UTC()
	}
	args := []any{snapshot.ProxyID, snapshot.ProfileID, snapshot.Provider, snapshot.ResolvedIP, nullableScore(snapshot.Score), snapshot.RiskLevel, snapshot.Recommendation, snapshot.ProxyType, snapshot.IsVPN, snapshot.IsTOR, snapshot.IsDatacenter, snapshot.IsBlacklisted, jsonString(snapshot.BlacklistSource, "[]"), snapshot.ISP, snapshot.Country, snapshot.LatencyMS, snapshot.Status, snapshot.Error, snapshot.FeaturesJSON, snapshot.RawResponseJSON, db.timeArg(snapshot.CheckedAt), nullableTime(db, snapshot.ExpiresAt)}
	query := `INSERT INTO proxy_risk_score_snapshots (proxy_id,profile_id,provider,resolved_ip,score,risk_level,recommendation,proxy_type,is_vpn,is_tor,is_datacenter,is_blacklisted,blacklist_sources,isp,country,latency_ms,status,error,features_json,raw_response_json,checked_at,expires_at) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22)`
	if db.isSQLite() {
		query = strings.ReplaceAll(query, "$", "?")
	}
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		if db.isSQLite() {
			result, err := tx.ExecContext(ctx, query, args...)
			if err != nil {
				return err
			}
			snapshot.ID, _ = result.LastInsertId()
			return nil
		}
		return tx.QueryRowContext(ctx, query+" RETURNING id", args...).Scan(&snapshot.ID)
	})
}

func nullableScore(score *int) any {
	if score == nil {
		return nil
	}
	return *score
}

func jsonString(value []string, fallback string) string {
	encoded, err := json.Marshal(value)
	if err != nil || len(encoded) == 0 {
		return fallback
	}
	return string(encoded)
}

func buildProxyRiskScoreIDList(ids []int64) (string, []any) {
	placeholders := make([]string, 0, len(ids))
	args := make([]any, 0, len(ids))
	for index, id := range ids {
		placeholders = append(placeholders, fmt.Sprintf("$%d", index+1))
		args = append(args, id)
	}
	return strings.Join(placeholders, ","), args
}

func (db *DB) ListLatestProxyRiskScores(ctx context.Context, proxyIDs []int64) (map[int64]*ProxyRiskScoreSnapshot, error) {
	if len(proxyIDs) == 0 {
		return map[int64]*ProxyRiskScoreSnapshot{}, nil
	}
	placeholders, args := buildProxyRiskScoreIDList(proxyIDs)
	if db.isSQLite() {
		placeholders = strings.ReplaceAll(placeholders, "$", "?")
	}
	query := fmt.Sprintf(`SELECT s.id,s.proxy_id,s.profile_id,s.provider,s.resolved_ip,s.score,s.risk_level,s.recommendation,s.proxy_type,s.is_vpn,s.is_tor,s.is_datacenter,s.is_blacklisted,s.blacklist_sources,s.isp,s.country,s.latency_ms,s.status,s.error,s.features_json,s.raw_response_json,s.checked_at,s.expires_at FROM proxy_risk_score_snapshots s WHERE s.proxy_id IN (%s) AND s.id=(SELECT latest.id FROM proxy_risk_score_snapshots latest WHERE latest.proxy_id=s.proxy_id AND latest.profile_id=s.profile_id ORDER BY latest.checked_at DESC,latest.id DESC LIMIT 1)`, placeholders)
	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int64]*ProxyRiskScoreSnapshot)
	for rows.Next() {
		snapshot, err := scanProxyRiskScoreSnapshot(rows)
		if err != nil {
			return nil, err
		}
		if current, exists := out[snapshot.ProxyID]; !exists || snapshot.CheckedAt.After(current.CheckedAt) {
			out[snapshot.ProxyID] = &snapshot
		}
	}
	return out, rows.Err()
}

func (db *DB) ListProxyRiskScoreHistory(ctx context.Context, proxyID, profileID int64, page, pageSize int) ([]ProxyRiskScoreSnapshot, int64, error) {
	if proxyID <= 0 || profileID <= 0 {
		return nil, 0, errors.New("proxy or profile id is invalid")
	}
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 500 {
		pageSize = 50
	}
	countQuery := `SELECT COUNT(*) FROM proxy_risk_score_snapshots WHERE proxy_id=$1 AND profile_id=$2`
	listQuery := `SELECT id,proxy_id,profile_id,provider,resolved_ip,score,risk_level,recommendation,proxy_type,is_vpn,is_tor,is_datacenter,is_blacklisted,blacklist_sources,isp,country,latency_ms,status,error,features_json,raw_response_json,checked_at,expires_at FROM proxy_risk_score_snapshots WHERE proxy_id=$1 AND profile_id=$2 ORDER BY checked_at DESC,id DESC OFFSET $3 LIMIT $4`
	listArgs := []any{proxyID, profileID, (page - 1) * pageSize, pageSize}
	if db.isSQLite() {
		countQuery = strings.ReplaceAll(countQuery, "$", "?")
		listQuery = `SELECT id,proxy_id,profile_id,provider,resolved_ip,score,risk_level,recommendation,proxy_type,is_vpn,is_tor,is_datacenter,is_blacklisted,blacklist_sources,isp,country,latency_ms,status,error,features_json,raw_response_json,checked_at,expires_at FROM proxy_risk_score_snapshots WHERE proxy_id=? AND profile_id=? ORDER BY checked_at DESC,id DESC LIMIT ? OFFSET ?`
		listArgs = []any{proxyID, profileID, pageSize, (page - 1) * pageSize}
	}
	var total int64
	if err := db.conn.QueryRowContext(ctx, countQuery, proxyID, profileID).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := db.conn.QueryContext(ctx, listQuery, listArgs...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := make([]ProxyRiskScoreSnapshot, 0)
	for rows.Next() {
		item, err := scanProxyRiskScoreSnapshot(rows)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	return items, total, rows.Err()
}

func scanProxyRiskScoreSnapshot(scanner proxyRiskScanner) (ProxyRiskScoreSnapshot, error) {
	var snapshot ProxyRiskScoreSnapshot
	var score sql.NullInt64
	var blacklistRaw string
	var checkedRaw, expiresRaw any
	if err := scanner.Scan(&snapshot.ID, &snapshot.ProxyID, &snapshot.ProfileID, &snapshot.Provider, &snapshot.ResolvedIP, &score, &snapshot.RiskLevel, &snapshot.Recommendation, &snapshot.ProxyType, &snapshot.IsVPN, &snapshot.IsTOR, &snapshot.IsDatacenter, &snapshot.IsBlacklisted, &blacklistRaw, &snapshot.ISP, &snapshot.Country, &snapshot.LatencyMS, &snapshot.Status, &snapshot.Error, &snapshot.FeaturesJSON, &snapshot.RawResponseJSON, &checkedRaw, &expiresRaw); err != nil {
		return snapshot, err
	}
	if score.Valid {
		value := int(score.Int64)
		snapshot.Score = &value
	}
	_ = json.Unmarshal([]byte(blacklistRaw), &snapshot.BlacklistSource)
	if snapshot.BlacklistSource == nil {
		snapshot.BlacklistSource = []string{}
	}
	snapshot.CheckedAt, _ = parseDBTimeValue(checkedRaw)
	if parsed, err := parseDBTimeValue(expiresRaw); err == nil && !parsed.IsZero() {
		snapshot.ExpiresAt = &parsed
	}
	return snapshot, nil
}

// ResolveProxyRiskScoringHost returns the host component without proxy user
// info. The admin adapter performs the public-IP policy before calling it.
func ResolveProxyRiskScoringHost(rawURL string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Hostname() == "" {
		return "", errors.New("proxy URL host is invalid")
	}
	return u.Hostname(), nil
}

func IsPublicProxyRiskScoringIP(ip net.IP) bool {
	if ip == nil {
		return false
	}
	if ip4 := ip.To4(); ip4 == nil {
		return false
	} else {
		ip = ip4
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() || ip.IsMulticast() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return false
	}
	return !(ip[0] == 0 || ip[0] >= 224 || (ip[0] == 100 && ip[1] >= 64 && ip[1] <= 127) ||
		(ip[0] == 192 && ip[1] == 0) || (ip[0] == 198 && ip[1] >= 18 && ip[1] <= 19) ||
		(ip[0] == 198 && ip[1] == 51 && ip[2] == 100) || (ip[0] == 203 && ip[1] == 0 && ip[2] == 113))
}

func ProxyRiskScoringProviderName() string  { return proxyRiskScoringProviderScamalytics }
func ProxyRiskScoringStatusSuccess() string { return proxyRiskScoringStatusSuccess }
func ProxyRiskScoringStatusError() string   { return proxyRiskScoringStatusError }
func ProxyRiskScoringStatusSkipped() string { return proxyRiskScoringStatusSkipped }
