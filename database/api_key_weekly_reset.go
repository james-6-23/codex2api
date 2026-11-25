package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
)

// ErrAPIKeyWeeklyQuotaResetRequiresLimits means that a bounded weekly reset
// cannot be granted without positive, finite 7-day and 30-day cost limits.
var ErrAPIKeyWeeklyQuotaResetRequiresLimits = errors.New("weekly quota reset requires positive 7-day and 30-day cost limits")

// APIKeyWeeklyQuotaReset is the durable result of one administrator operation.
// Reusing RequestID returns this result without resetting subsequent usage.
type APIKeyWeeklyQuotaReset struct {
	APIKeyQuotaResetTarget
	RequestID    string
	ResetAt      time.Time
	ExemptedCost float64
	Replayed     bool
}

func (db *DB) ensureAPIKeyWeeklyResetSchema(ctx context.Context) error {
	timestampType := "TIMESTAMPTZ"
	if db.isSQLite() {
		timestampType = "TIMESTAMP"
	}
	statements := []string{
		`CREATE TABLE IF NOT EXISTS api_key_weekly_resets (
			api_key_id BIGINT NOT NULL,
			request_id TEXT NOT NULL,
			reset_at ` + timestampType + ` NOT NULL,
			cost_limit_7d DOUBLE PRECISION NOT NULL,
			exempted_cost DOUBLE PRECISION NOT NULL DEFAULT 0,
			PRIMARY KEY (api_key_id, request_id)
		)`,
		// No usage_logs foreign key: ClearUsageLogs also supports PostgreSQL
		// TRUNCATE. It removes these snapshots in the same transaction.
		`CREATE TABLE IF NOT EXISTS api_key_weekly_reset_usage (
			api_key_id BIGINT NOT NULL,
			usage_log_id BIGINT NOT NULL,
			request_id TEXT NOT NULL,
			exempted_cost DOUBLE PRECISION NOT NULL DEFAULT 0,
			PRIMARY KEY (api_key_id, usage_log_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_api_key_weekly_reset_request
			ON api_key_weekly_reset_usage(api_key_id, request_id)`,
	}
	for _, statement := range statements {
		if _, err := db.conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

// ResetAPIKeyWeeklyQuota restarts only the key-level 7-day cost/token window
// and exempts the covered cost from the rolling 30-day cost limit. Historical
// bills, cumulative quotas, other windows and scope limits are unchanged.
//
// The snapshot contains only committed logs visible to the INSERT statement.
// Late-arriving logs, including logs with the same timestamp, count normally.
// The credit is capped at the current weekly cost limit and ages out with each
// original log, rather than becoming a permanent adjustment to the 30-day cap.
func (db *DB) ResetAPIKeyWeeklyQuota(ctx context.Context, id int64, requestID string) (*APIKeyWeeklyQuotaReset, error) {
	requestID = strings.TrimSpace(requestID)
	if requestID == "" || len(requestID) > 128 {
		return nil, fmt.Errorf("invalid weekly quota reset request ID")
	}
	db.FlushUsageLogs()
	result := &APIKeyWeeklyQuotaReset{RequestID: requestID}
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		if !db.isSQLite() {
			// Writers touch usage_logs before api_keys. Take the compatible read
			// lock in that order too, including when ClearUsageLogs is waiting
			// for its exclusive lock, to avoid a writer/clear/reset lock cycle.
			if _, err := tx.ExecContext(ctx, `LOCK TABLE usage_logs IN ACCESS SHARE MODE`); err != nil {
				return err
			}
		}
		// SQLite's immediate write transaction serializes writers. PostgreSQL
		// additionally locks the key, serializing resets and limit changes.
		query := `SELECT id, key, COALESCE(limits, '{}') FROM api_keys WHERE id = $1`
		if !db.isSQLite() {
			query += ` FOR UPDATE`
		}
		var limitsJSON []byte
		if err := tx.QueryRowContext(ctx, query, id).Scan(&result.ID, &result.Key, &limitsJSON); err != nil {
			return err
		}
		var resetRaw interface{}
		err := tx.QueryRowContext(ctx, `SELECT reset_at, exempted_cost FROM api_key_weekly_resets
			WHERE api_key_id = $1 AND request_id = $2`, id, requestID).Scan(&resetRaw, &result.ExemptedCost)
		if err == nil {
			result.ResetAt, err = parseDBTimeValue(resetRaw)
			result.Replayed = true
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		var limits APIKeyLimits
		if err := json.Unmarshal(limitsJSON, &limits); err != nil {
			return err
		}
		for _, limit := range []float64{limits.CostLimit7d, limits.CostLimit30d} {
			if limit <= 0 || math.IsNaN(limit) || math.IsInf(limit, 0) {
				return ErrAPIKeyWeeklyQuotaResetRequiresLimits
			}
		}
		result.ResetAt = time.Now().UTC().Truncate(time.Microsecond)
		if db.isSQLite() {
			result.ResetAt = result.ResetAt.Truncate(time.Second)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO api_key_weekly_resets
			(api_key_id, request_id, reset_at, cost_limit_7d) VALUES ($1, $2, $3, $4)`,
			id, requestID, db.timeArg(result.ResetAt), limits.CostLimit7d); err != nil {
			return err
		}
		// Allocate the bounded credit oldest-first, using one database snapshot.
		// Zero-cost events are included so their 7-day token usage resets too.
		// The unique (key, log) identity prevents overlap between reset grants.
		if _, err := tx.ExecContext(ctx, `INSERT INTO api_key_weekly_reset_usage
			(api_key_id, usage_log_id, request_id, exempted_cost)
			SELECT api_key_id, id, $2,
				CASE WHEN running_cost <= $3 THEN positive_cost
					 WHEN running_cost - positive_cost < $3 THEN $3 - (running_cost - positive_cost)
					 ELSE 0 END
			FROM (
				SELECT u.api_key_id, u.id,
					CASE WHEN u.user_billed > 0 THEN u.user_billed ELSE 0 END AS positive_cost,
					SUM(CASE WHEN u.user_billed > 0 THEN u.user_billed ELSE 0 END)
						OVER (ORDER BY u.created_at, u.id ROWS UNBOUNDED PRECEDING) AS running_cost
				FROM usage_logs u JOIN api_keys k ON k.id = u.api_key_id
				WHERE u.api_key_id = $1 AND u.created_at >= $4 AND u.created_at <= $5
					AND u.status_code <> 499
					AND (k.last_reset_at IS NULL OR u.created_at >= k.last_reset_at)
					AND NOT EXISTS (SELECT 1 FROM api_key_weekly_reset_usage e
						WHERE e.api_key_id = u.api_key_id AND e.usage_log_id = u.id)
			) eligible`, id, requestID, limits.CostLimit7d,
			db.timeArg(result.ResetAt.Add(-7*24*time.Hour)), db.timeArg(result.ResetAt)); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(SUM(exempted_cost), 0)
			FROM api_key_weekly_reset_usage WHERE api_key_id = $1 AND request_id = $2`,
			id, requestID).Scan(&result.ExemptedCost); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE api_key_weekly_resets SET exempted_cost = $3
			WHERE api_key_id = $1 AND request_id = $2`, id, requestID, result.ExemptedCost)
		return err
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

// Clamp each exemption to its original bill even if an administrator later
// corrects a log. Joining the original rows also makes deleted/expired usage
// incapable of leaving behind spendable credit.
const apiKeyCostExemptionExpr = `CASE WHEN u.user_billed > 0 THEN
	CASE WHEN COALESCE(e.exempted_cost, 0) > u.user_billed THEN u.user_billed
		ELSE COALESCE(e.exempted_cost, 0) END ELSE 0 END`

// apiKeyWindowQuotaSQL is shared by individual enforcement and batch admin
// costs. Calendar-day and request-rate queries pass a zero window.
func apiKeyWindowQuotaSQL(window time.Duration) (joins, filters, credit string) {
	credit = "0"
	if isResettableAPIKeyWindow(window) {
		joins = ` JOIN api_keys k ON k.id = u.api_key_id`
		filters = ` AND (k.last_reset_at IS NULL OR u.created_at >= k.last_reset_at)`
	}
	if window == 7*24*time.Hour {
		filters += ` AND NOT EXISTS (SELECT 1 FROM api_key_weekly_reset_usage e
			WHERE e.api_key_id = u.api_key_id AND e.usage_log_id = u.id)`
	}
	if window == 30*24*time.Hour {
		joins = ` LEFT JOIN api_key_weekly_reset_usage e
			ON e.api_key_id = u.api_key_id AND e.usage_log_id = u.id`
		credit = apiKeyCostExemptionExpr
	}
	return joins, filters, credit
}

func (db *DB) applyAPIKeyWeeklyResetToReport(ctx context.Context, report *APIKeySelfUsageReport, apiKeyID int64, now time.Time) error {
	// Only opted-in keys take this raw 7-day scan; the normal report keeps its
	// hourly rollup path. It preserves exact per-event reset/oldest semantics.
	weekly, err := db.getAPIKeyUsageSince(ctx, apiKeyID, now.Add(-7*24*time.Hour), 7*24*time.Hour)
	if err != nil {
		return err
	}
	report.Windows.Last7d.APIKeyWindowUsage = *weekly
	report.Windows.Last7d.DecayAt = nil
	if weekly.OldestAt != nil {
		decay := weekly.OldestAt.Add(7 * 24 * time.Hour)
		report.Windows.Last7d.DecayAt = &decay
	}
	var credit float64
	err = db.conn.QueryRowContext(ctx, `SELECT COALESCE(SUM(`+apiKeyCostExemptionExpr+`), 0)
		FROM api_key_weekly_reset_usage e JOIN usage_logs u
			ON u.api_key_id = e.api_key_id AND u.id = e.usage_log_id
		WHERE e.api_key_id = $1 AND u.created_at >= $2 AND u.status_code <> 499`,
		apiKeyID, db.timeArg(now.Add(-30*24*time.Hour))).Scan(&credit)
	if err != nil {
		return err
	}
	report.Windows.Last30d.CostExempted = min(max(0, report.Windows.Last30d.UserBilled), credit)
	if report.Windows.Last30d.CostExempted > 0 {
		// The oldest raw bill may be fully exempt: its expiry is not necessarily
		// when the net cost budget next becomes available.
		report.Windows.Last30d.DecayAt = nil
	}
	return nil
}
