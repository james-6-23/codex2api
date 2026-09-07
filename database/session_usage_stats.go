package database

import (
	"context"
	"strconv"
	"strings"
	"time"
)

type SessionUsageStats struct {
	WindowCount            int      `json:"window_count"`
	AverageDurationSeconds *float64 `json:"average_duration_seconds"`
}

type AccountSessionSummary struct {
	AccountCount           int        `json:"account_count"`
	AverageWindows24h      float64    `json:"average_windows_24h"`
	AverageUniqueUsers     float64    `json:"average_unique_users"`
	AverageWindowsTotal    float64    `json:"average_windows_total"`
	AverageDurationSeconds *float64   `json:"average_duration_seconds"`
	LatestAt               *time.Time `json:"latest_at,omitempty"`
}

func (db *DB) sessionObservationDurationSQL() string {
	difference := `(julianday(o.last_seen) - julianday(o.first_seen)) * 86400.0`
	if db.driver == "postgres" {
		difference = `EXTRACT(EPOCH FROM (o.last_seen - o.first_seen))`
	}
	return `CASE WHEN o.session_hash IS NULL THEN NULL WHEN o.last_seen > o.first_seen THEN ` + difference + ` ELSE 0.0 END`
}

func (db *DB) GetNewAPIUserSessionUsage(ctx context.Context, platform, userID string) (*SessionUsageStats, error) {
	result := &SessionUsageStats{}
	if db == nil || strings.TrimSpace(platform) == "" || strings.TrimSpace(userID) == "" {
		return result, nil
	}
	err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*), AVG(`+db.sessionObservationDurationSQL()+`)
		FROM account_session_usage_periods o WHERE o.newapi_platform=$1 AND o.newapi_user_id=$2`,
		strings.TrimSpace(platform), strings.TrimSpace(userID)).Scan(&result.WindowCount, &result.AverageDurationSeconds)
	return result, err
}

func (db *DB) GetAccountSessionSummary(ctx context.Context, query PromptRiskProfileQuery) (*AccountSessionSummary, error) {
	result := &AccountSessionSummary{}
	if db == nil {
		return result, nil
	}
	where, args, valid := accountStatusProfileFilter(query)
	if !valid {
		return result, nil
	}
	args = append(args, time.Now().UTC().Add(-24*time.Hour))
	cutoff := "$" + strconv.Itoa(len(args))
	var latestRaw any
	err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(AVG(windows_24h),0), COALESCE(AVG(unique_users),0), COALESCE(AVG(windows_total),0),
		SUM(duration_seconds) * 1.0 / NULLIF(SUM(duration_samples),0), MAX(latest_at)
		FROM (SELECT a.id, COUNT(o.session_hash) AS windows_total,
			SUM(CASE WHEN o.first_seen >= `+cutoff+` THEN 1 ELSE 0 END) AS windows_24h,
			COUNT(DISTINCT CASE WHEN o.newapi_user_id<>'' THEN o.newapi_platform || ':' || o.newapi_user_id END) AS unique_users,
			(SELECT SUM(`+db.sessionObservationDurationSQL()+`) FROM account_session_usage_periods o WHERE o.account_id=a.id) AS duration_seconds,
			(SELECT COUNT(`+db.sessionObservationDurationSQL()+`) FROM account_session_usage_periods o WHERE o.account_id=a.id) AS duration_samples,
			MAX(o.last_seen) AS latest_at
			FROM accounts a LEFT JOIN account_session_observations o ON o.account_id=a.id
			WHERE `+where+` GROUP BY a.id) account_stats`, args...).Scan(
		&result.AccountCount, &result.AverageWindows24h, &result.AverageUniqueUsers,
		&result.AverageWindowsTotal, &result.AverageDurationSeconds, &latestRaw)
	if err != nil {
		return nil, err
	}
	if latestRaw != nil {
		latest, parseErr := parseDBTimeValue(latestRaw)
		if parseErr != nil {
			return nil, parseErr
		}
		result.LatestAt = &latest
	}
	return result, nil
}
