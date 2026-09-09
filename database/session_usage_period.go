package database

import (
	"context"
	"strings"
	"time"
)

func (db *DB) applyAccountSessionUsagePeriodsWithExec(ctx context.Context, execer sqlExecer, batch []usageLogEntry) error {
	type periodKey struct {
		accountID int64
		periodID  string
	}
	type usagePeriod struct {
		sessionHash   string
		platform      string
		userID        string
		startedAt     time.Time
		endedAt       time.Time
		eligibleAfter int64
		success       bool
	}
	periods := make(map[periodKey]usagePeriod)
	for _, entry := range batch {
		if entry.AccountID <= 0 || entry.SessionUsagePeriodID == "" || entry.SessionHash == "" || entry.SessionUsageStartedAt.IsZero() || entry.ObservedAt.Before(entry.SessionUsageStartedAt) {
			continue
		}
		key := periodKey{accountID: entry.AccountID, periodID: entry.SessionUsagePeriodID}
		period, exists := periods[key]
		if !exists {
			period = usagePeriod{sessionHash: entry.SessionHash, startedAt: entry.SessionUsageStartedAt, endedAt: entry.ObservedAt}
		}
		if entry.ObservedAt.After(period.endedAt) {
			period.endedAt = entry.ObservedAt
		}
		if entry.SessionUsageStartedAt.Before(period.startedAt) {
			period.startedAt = entry.SessionUsageStartedAt
		}
		if strings.TrimSpace(entry.NewAPIUserID) != "" {
			period.platform = strings.TrimSpace(entry.NewAPIPlatform)
			period.userID = strings.TrimSpace(entry.NewAPIUserID)
		}
		if entry.SessionUsageIdleSeconds > 0 {
			period.eligibleAfter = max(period.eligibleAfter, entry.ObservedAt.Add(time.Duration(entry.SessionUsageIdleSeconds)*time.Second).UnixMilli())
			period.success = period.success || (entry.StatusCode >= 200 && entry.StatusCode < 300 && entry.ErrorMessage == "")
		}
		periods[key] = period
	}
	for key, period := range periods {
		var startedArg, endedArg any = period.startedAt.UTC(), period.endedAt.UTC()
		if db.isSQLite() {
			startedArg = period.startedAt.UTC().Format("2006-01-02 15:04:05.000000000")
			endedArg = period.endedAt.UTC().Format("2006-01-02 15:04:05.000000000")
		}
		_, err := execer.ExecContext(ctx, `INSERT INTO account_session_usage_periods
			(account_id, period_id, session_hash, newapi_platform, newapi_user_id, first_seen, last_seen)
			VALUES ($1,$2,$3,$4,$5,$6,$7)
			ON CONFLICT(account_id, period_id) DO UPDATE SET
			first_seen=CASE WHEN excluded.first_seen < account_session_usage_periods.first_seen THEN excluded.first_seen ELSE account_session_usage_periods.first_seen END,
			last_seen=CASE WHEN excluded.last_seen > account_session_usage_periods.last_seen THEN excluded.last_seen ELSE account_session_usage_periods.last_seen END,
			newapi_platform=CASE WHEN excluded.newapi_user_id<>'' THEN excluded.newapi_platform ELSE account_session_usage_periods.newapi_platform END,
			newapi_user_id=CASE WHEN excluded.newapi_user_id<>'' THEN excluded.newapi_user_id ELSE account_session_usage_periods.newapi_user_id END`,
			key.accountID, key.periodID, period.sessionHash, period.platform, period.userID, startedArg, endedArg)
		if err != nil {
			return err
		}
		if period.eligibleAfter > 0 {
			if period.success {
				_, err = execer.ExecContext(ctx, `INSERT INTO account_session_usage_successes(account_id,period_id,eligible_after_ms) VALUES($1,$2,$3)
					ON CONFLICT(account_id,period_id) DO UPDATE SET eligible_after_ms=CASE WHEN excluded.eligible_after_ms > account_session_usage_successes.eligible_after_ms THEN excluded.eligible_after_ms ELSE account_session_usage_successes.eligible_after_ms END`, key.accountID, key.periodID, period.eligibleAfter)
			} else {
				_, err = execer.ExecContext(ctx, `UPDATE account_session_usage_successes SET eligible_after_ms=CASE WHEN eligible_after_ms<$3 THEN $3 ELSE eligible_after_ms END WHERE account_id=$1 AND period_id=$2`, key.accountID, key.periodID, period.eligibleAfter)
			}
			if err != nil {
				return err
			}
		}
	}
	return nil
}
