package database

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const PromptRiskSubjectAccountStatus = "account_status"

// AccountSessionObservation is purely operational telemetry. It never enters
// prompt-risk scoring, review exemptions, conversation locks, or bans.
type AccountSessionObservation struct {
	AccountID      int64
	SessionHash    string
	NewAPIPlatform string
	NewAPIUserID   string
	NewAPIUserName string
	ObservedAt     time.Time
}

func (db *DB) ensureAccountSessionObservationsTable(ctx context.Context) error {
	if db == nil {
		return nil
	}
	statements := []string{
		`CREATE TABLE IF NOT EXISTS account_session_observations (
			account_id BIGINT NOT NULL,
			session_hash VARCHAR(64) NOT NULL,
			newapi_platform VARCHAR(100) NOT NULL DEFAULT '',
			newapi_user_id VARCHAR(255) NOT NULL DEFAULT '',
			newapi_user_name VARCHAR(255) NOT NULL DEFAULT '',
			first_seen TIMESTAMP NOT NULL,
			last_seen TIMESTAMP NOT NULL,
			PRIMARY KEY(account_id, session_hash)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_account_session_observations_first_seen ON account_session_observations(first_seen)`,
		`CREATE INDEX IF NOT EXISTS idx_account_session_observations_account_first ON account_session_observations(account_id, first_seen)`,
	}
	for _, statement := range statements {
		if _, err := db.conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func applyAccountSessionObservationsWithExec(ctx context.Context, execer sqlExecer, batch []usageLogEntry) error {
	if execer == nil || len(batch) == 0 {
		return nil
	}
	// One request can produce retry/continuation rows. Collapse the batch so a
	// conversation produces at most one upsert per account per flush.
	type observationKey struct {
		accountID   int64
		sessionHash string
	}
	observations := make(map[observationKey]AccountSessionObservation)
	for _, entry := range batch {
		if !entry.RecordSessionObservation || entry.AccountID <= 0 || strings.TrimSpace(entry.SessionHash) == "" {
			continue
		}
		key := observationKey{accountID: entry.AccountID, sessionHash: entry.SessionHash}
		candidate := AccountSessionObservation{
			AccountID: entry.AccountID, SessionHash: entry.SessionHash,
			NewAPIPlatform: entry.NewAPIPlatform, NewAPIUserID: entry.NewAPIUserID,
			NewAPIUserName: entry.NewAPIUserName, ObservedAt: entry.ObservedAt,
		}
		if candidate.ObservedAt.IsZero() {
			candidate.ObservedAt = time.Now().UTC()
		}
		current, exists := observations[key]
		if !exists {
			observations[key] = candidate
			continue
		}
		if candidate.ObservedAt.After(current.ObservedAt) {
			current.ObservedAt = candidate.ObservedAt
		}
		if candidate.NewAPIUserID != "" {
			current.NewAPIPlatform = candidate.NewAPIPlatform
			current.NewAPIUserID = candidate.NewAPIUserID
			if candidate.NewAPIUserName != "" {
				current.NewAPIUserName = candidate.NewAPIUserName
			}
		}
		observations[key] = current
	}
	for _, observation := range observations {
		if _, err := execer.ExecContext(ctx, `INSERT INTO account_session_observations (
			account_id, session_hash, newapi_platform, newapi_user_id, newapi_user_name, first_seen, last_seen
		) VALUES ($1,$2,$3,$4,$5,$6,$6)
		ON CONFLICT(account_id, session_hash) DO UPDATE SET
			first_seen=CASE WHEN excluded.first_seen < account_session_observations.first_seen THEN excluded.first_seen ELSE account_session_observations.first_seen END,
			last_seen=CASE WHEN excluded.last_seen > account_session_observations.last_seen THEN excluded.last_seen ELSE account_session_observations.last_seen END,
			newapi_platform=CASE WHEN excluded.newapi_user_id<>'' THEN excluded.newapi_platform ELSE account_session_observations.newapi_platform END,
			newapi_user_id=CASE WHEN excluded.newapi_user_id<>'' THEN excluded.newapi_user_id ELSE account_session_observations.newapi_user_id END,
			newapi_user_name=CASE WHEN excluded.newapi_user_name<>'' THEN excluded.newapi_user_name ELSE account_session_observations.newapi_user_name END`,
			observation.AccountID, observation.SessionHash,
			strings.TrimSpace(observation.NewAPIPlatform), strings.TrimSpace(observation.NewAPIUserID),
			strings.TrimSpace(observation.NewAPIUserName), observation.ObservedAt.UTC()); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) listAccountStatusProfiles(ctx context.Context, query PromptRiskProfileQuery) ([]*PromptRiskProfile, int, error) {
	if db == nil {
		return nil, 0, nil
	}
	if query.Page <= 0 {
		query.Page = 1
	}
	if query.PageSize <= 0 || query.PageSize > 200 {
		query.PageSize = 20
	}
	clauses := []string{"a.status <> 'deleted'", "COALESCE(a.error_message, '') <> 'deleted'"}
	args := make([]any, 0, 4)
	if query.AccountID > 0 {
		args = append(args, query.AccountID)
		clauses = append(clauses, fmt.Sprintf("a.id=$%d", len(args)))
	}
	if value := strings.TrimSpace(query.SubjectKey); value != "" {
		if accountID, err := strconv.ParseInt(value, 10, 64); err == nil && accountID > 0 {
			args = append(args, accountID)
			clauses = append(clauses, fmt.Sprintf("a.id=$%d", len(args)))
		} else {
			return []*PromptRiskProfile{}, 0, nil
		}
	}
	if value := strings.TrimSpace(query.Query); value != "" {
		args = append(args, "%"+strings.ToLower(value)+"%")
		placeholder := fmt.Sprintf("$%d", len(args))
		clauses = append(clauses, fmt.Sprintf(`(LOWER(COALESCE(a.name,'')) LIKE %s OR LOWER(COALESCE(CAST(a.credentials AS TEXT),'')) LIKE %s OR CAST(a.id AS TEXT) LIKE %s)`, placeholder, placeholder, placeholder))
	}
	where := strings.Join(clauses, " AND ")
	var total int
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM accounts a WHERE `+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	cutoff24h := time.Now().UTC().Add(-24 * time.Hour)
	args = append(args, cutoff24h, query.PageSize, (query.Page-1)*query.PageSize)
	cutoffArg, limitArg, offsetArg := len(args)-2, len(args)-1, len(args)
	rows, err := db.conn.QueryContext(ctx, `SELECT
		a.id, COALESCE(a.name,''), COALESCE(CAST(a.credentials AS TEXT),'{}'),
		COUNT(o.session_hash),
		COALESCE(SUM(CASE WHEN o.first_seen >= $`+strconv.Itoa(cutoffArg)+` THEN 1 ELSE 0 END),0),
		COUNT(DISTINCT CASE WHEN o.newapi_user_id<>'' THEN o.newapi_platform || ':' || o.newapi_user_id END),
		MAX(o.last_seen)
	FROM accounts a
	LEFT JOIN account_session_observations o ON o.account_id=a.id
	WHERE `+where+`
	GROUP BY a.id, a.name, a.credentials
	ORDER BY COUNT(o.session_hash) DESC, a.id ASC
	LIMIT $`+strconv.Itoa(limitArg)+` OFFSET $`+strconv.Itoa(offsetArg), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	profiles := make([]*PromptRiskProfile, 0, query.PageSize)
	for rows.Next() {
		profile := &PromptRiskProfile{SubjectType: PromptRiskSubjectAccountStatus, RiskLevel: PromptRiskLevelLow, RecommendedActions: []string{}}
		var credentialsRaw any
		var latestRaw any
		if err := rows.Scan(&profile.AccountID, &profile.AccountName, &credentialsRaw,
			&profile.SessionWindowsTotal, &profile.SessionWindows24h, &profile.SessionUniqueUsers, &latestRaw); err != nil {
			return nil, 0, err
		}
		profile.SubjectKey = strconv.FormatInt(profile.AccountID, 10)
		profile.SubjectDisplay = profile.AccountName
		profile.AccountEmail = accountEmailFromRawCredentials(credentialsRaw)
		if profile.SubjectDisplay == "" {
			profile.SubjectDisplay = profile.AccountEmail
		}
		if profile.SubjectDisplay == "" {
			profile.SubjectDisplay = "Account #" + profile.SubjectKey
		}
		profile.HasActivity = profile.SessionWindowsTotal > 0
		if latestRaw != nil {
			if latest, parseErr := parseDBTimeValue(latestRaw); parseErr == nil {
				profile.LatestAt = latest
			}
		}
		profiles = append(profiles, profile)
	}
	return profiles, total, rows.Err()
}
