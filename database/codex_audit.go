package database

import (
	"context"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"
)

type CodexAuditQuery struct {
	Start         time.Time
	End           time.Time
	BucketMinutes int
	Limit         int
}

type CodexAuditReport struct {
	WindowStart        time.Time                   `json:"window_start"`
	WindowEnd          time.Time                   `json:"window_end"`
	GeneratedAt        time.Time                   `json:"generated_at"`
	Verdict            string                      `json:"verdict"`
	Summary            CodexAuditSummary           `json:"summary"`
	PromptFilter       []CodexAuditPromptFilterRow `json:"prompt_filter"`
	Usage              CodexAuditUsageSummary      `json:"usage"`
	Timeline           []CodexAuditTimelinePoint   `json:"timeline"`
	Models             []CodexAuditModelRow        `json:"models"`
	SuspiciousSamples  []*PromptFilterLog          `json:"suspicious_samples"`
	ProbeObserved      []CodexAuditProbeRow        `json:"probe_observed"`
	ProbeShortCircuits []CodexAuditProbeRow        `json:"probe_short_circuits"`
	PolicyErrors       []*UsageLog                 `json:"policy_errors"`
	SlowRequests       []*UsageLog                 `json:"slow_requests"`
	Notes              []string                    `json:"notes"`
}

type CodexAuditSummary struct {
	PromptLogs                 int64 `json:"prompt_logs"`
	PromptBlocks               int64 `json:"prompt_blocks"`
	ReviewFlagged              int64 `json:"review_flagged"`
	ReviewErrors               int64 `json:"review_errors"`
	HighScoreAllowed           int64 `json:"high_score_allowed"`
	SemanticDisagreements      int64 `json:"semantic_disagreements"`
	SemanticDisagreementBlocks int64 `json:"semantic_disagreement_blocks"`
	UpstreamCyberPolicy        int64 `json:"upstream_cyber_policy"`
	ProbeObserved              int64 `json:"probe_observed"`
	ProbeShortCircuits         int64 `json:"probe_short_circuits"`
}

type CodexAuditPromptFilterRow struct {
	Source        string `json:"source"`
	Action        string `json:"action"`
	Mode          string `json:"mode"`
	ReviewModel   string `json:"review_model"`
	ReviewFlagged bool   `json:"review_flagged"`
	Count         int64  `json:"count"`
	MinScore      int    `json:"min_score"`
	MaxScore      int    `json:"max_score"`
	ReviewErrors  int64  `json:"review_errors"`
}

type CodexAuditUsageSummary struct {
	Requests          int64   `json:"requests"`
	Errors4xx         int64   `json:"errors_4xx"`
	Errors5xx         int64   `json:"errors_5xx"`
	WebSocketRequests int64   `json:"websocket_requests"`
	WebSocketRatio    float64 `json:"websocket_ratio"`
	PolicyLikeErrors  int64   `json:"policy_like_errors"`
	FirstTokenSamples int64   `json:"first_token_samples"`
	FirstTokenMinMS   int     `json:"first_token_min_ms"`
	FirstTokenP50MS   int     `json:"first_token_p50_ms"`
	FirstTokenP95MS   int     `json:"first_token_p95_ms"`
	FirstTokenMaxMS   int     `json:"first_token_max_ms"`
}

type CodexAuditTimelinePoint struct {
	Bucket              time.Time `json:"bucket"`
	Requests            int64     `json:"requests"`
	PromptBlocks        int64     `json:"prompt_blocks"`
	ReviewFlagged       int64     `json:"review_flagged"`
	UpstreamCyberPolicy int64     `json:"upstream_cyber_policy"`
	Errors4xx           int64     `json:"errors_4xx"`
	Errors5xx           int64     `json:"errors_5xx"`
	FirstTokenP95MS     int       `json:"first_token_p95_ms"`
}

type CodexAuditModelRow struct {
	Model           string `json:"model"`
	Requests        int64  `json:"requests"`
	Errors4xx       int64  `json:"errors_4xx"`
	Errors5xx       int64  `json:"errors_5xx"`
	WebSocket       int64  `json:"websocket"`
	FirstTokenP95MS int    `json:"first_token_p95_ms"`
}

type CodexAuditProbeRow struct {
	APIKeyID     int64     `json:"api_key_id"`
	APIKeyName   string    `json:"api_key_name"`
	APIKeyMasked string    `json:"api_key_masked"`
	Endpoint     string    `json:"endpoint"`
	Model        string    `json:"model"`
	Count        int64     `json:"count"`
	FirstSeen    time.Time `json:"first_seen"`
	LastSeen     time.Time `json:"last_seen"`
	SpanSeconds  float64   `json:"span_seconds"`
}

func (db *DB) BuildCodexAuditReport(ctx context.Context, query CodexAuditQuery) (*CodexAuditReport, error) {
	if db == nil {
		return nil, fmt.Errorf("database is nil")
	}
	now := time.Now()
	end := query.End
	if end.IsZero() {
		end = now
	}
	start := query.Start
	if start.IsZero() {
		start = end.Add(-30 * time.Minute)
	}
	if !start.Before(end) {
		start = end.Add(-30 * time.Minute)
	}
	if query.BucketMinutes <= 0 {
		query.BucketMinutes = 5
	}
	if query.BucketMinutes > 1440 {
		query.BucketMinutes = 1440
	}
	if query.Limit <= 0 || query.Limit > 100 {
		query.Limit = 20
	}

	report := &CodexAuditReport{
		WindowStart: start,
		WindowEnd:   end,
		GeneratedAt: now,
		Notes: []string{
			"Sub2 bridge account state is not queried from inside codex2api; use the external s12 audit workflow when bridge schedulability must be confirmed.",
		},
	}
	var err error
	if report.PromptFilter, err = db.codexAuditPromptFilterRows(ctx, start, end); err != nil {
		return nil, err
	}
	if report.Usage, err = db.codexAuditUsageSummary(ctx, start, end); err != nil {
		return nil, err
	}
	if report.Timeline, err = db.codexAuditTimeline(ctx, start, end, query.BucketMinutes); err != nil {
		return nil, err
	}
	if report.Models, err = db.codexAuditModels(ctx, start, end, query.Limit); err != nil {
		return nil, err
	}
	if report.SuspiciousSamples, err = db.codexAuditSuspiciousSamples(ctx, start, end, query.Limit); err != nil {
		return nil, err
	}
	if report.ProbeObserved, err = db.codexAuditProbeObserved(ctx, start, end, query.Limit); err != nil {
		return nil, err
	}
	if report.ProbeShortCircuits, err = db.codexAuditProbeShortCircuits(ctx, start, end, query.Limit); err != nil {
		return nil, err
	}
	if report.PolicyErrors, err = db.codexAuditUsageSamples(ctx, start, end, query.Limit, "policy"); err != nil {
		return nil, err
	}
	if report.SlowRequests, err = db.codexAuditUsageSamples(ctx, start, end, query.Limit, "slow"); err != nil {
		return nil, err
	}
	report.Summary = summarizeCodexAudit(report)
	report.Verdict = codexAuditVerdict(report)
	return report, nil
}

func (db *DB) codexAuditPromptFilterRows(ctx context.Context, start, end time.Time) ([]CodexAuditPromptFilterRow, error) {
	startArg, endArg := db.timeRangeArgs(start, end)
	rows, err := db.conn.QueryContext(ctx, `
		SELECT COALESCE(source, ''), COALESCE(action, ''), COALESCE(mode, ''), COALESCE(review_model, ''),
		       COALESCE(review_flagged, false), COUNT(*), COALESCE(MIN(score), 0), COALESCE(MAX(score), 0),
		       COALESCE(SUM(CASE WHEN COALESCE(review_error, '') <> '' THEN 1 ELSE 0 END), 0)
		FROM prompt_filter_logs
		WHERE created_at >= $1 AND created_at <= $2
		GROUP BY 1, 2, 3, 4, 5
		ORDER BY COUNT(*) DESC, 1, 2
	`, startArg, endArg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]CodexAuditPromptFilterRow, 0)
	for rows.Next() {
		var item CodexAuditPromptFilterRow
		if err := rows.Scan(&item.Source, &item.Action, &item.Mode, &item.ReviewModel, &item.ReviewFlagged, &item.Count, &item.MinScore, &item.MaxScore, &item.ReviewErrors); err != nil {
			return nil, err
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (db *DB) codexAuditUsageSummary(ctx context.Context, start, end time.Time) (CodexAuditUsageSummary, error) {
	startArg, endArg := db.timeRangeArgs(start, end)
	var summary CodexAuditUsageSummary
	err := db.conn.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN status_code BETWEEN 400 AND 499 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN status_code >= 500 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN COALESCE(via_websocket, false) THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN lower(COALESCE(error_message, '')) LIKE '%cyber%'
		                           OR lower(COALESCE(error_message, '')) LIKE '%policy%'
		                           OR lower(COALESCE(error_message, '')) LIKE '%violat%'
		                           OR lower(COALESCE(error_message, '')) LIKE '%safety%'
		                           OR lower(COALESCE(upstream_error_kind, '')) LIKE '%policy%'
		                           OR lower(COALESCE(upstream_error_kind, '')) LIKE '%cyber%'
		                           OR lower(COALESCE(upstream_error_kind, '')) LIKE '%violat%'
		                           OR lower(COALESCE(upstream_error_kind, '')) LIKE '%safety%'
		                      THEN 1 ELSE 0 END), 0),
		       COALESCE(MIN(CASE WHEN first_token_ms > 0 THEN first_token_ms END), 0),
		       COALESCE(MAX(first_token_ms), 0)
		FROM usage_logs
		WHERE created_at >= $1 AND created_at <= $2
	`, startArg, endArg).Scan(&summary.Requests, &summary.Errors4xx, &summary.Errors5xx, &summary.WebSocketRequests, &summary.PolicyLikeErrors, &summary.FirstTokenMinMS, &summary.FirstTokenMaxMS)
	if err != nil {
		return summary, err
	}
	if summary.Requests > 0 {
		summary.WebSocketRatio = float64(summary.WebSocketRequests) / float64(summary.Requests)
	}
	tokens, err := db.codexAuditFirstTokenValues(ctx, start, end, "")
	if err != nil {
		return summary, err
	}
	summary.FirstTokenSamples = int64(len(tokens))
	summary.FirstTokenP50MS = percentileInt(tokens, 0.50)
	summary.FirstTokenP95MS = percentileInt(tokens, 0.95)
	return summary, nil
}

func (db *DB) codexAuditFirstTokenValues(ctx context.Context, start, end time.Time, model string) ([]int, error) {
	startArg, endArg := db.timeRangeArgs(start, end)
	args := []any{startArg, endArg}
	where := "created_at >= $1 AND created_at <= $2 AND first_token_ms > 0"
	if strings.TrimSpace(model) != "" {
		args = append(args, model)
		where += fmt.Sprintf(" AND COALESCE(NULLIF(effective_model, ''), model, '') = $%d", len(args))
	}
	rows, err := db.conn.QueryContext(ctx, `SELECT first_token_ms FROM usage_logs WHERE `+where+` ORDER BY first_token_ms ASC LIMIT 20000`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]int, 0)
	for rows.Next() {
		var v int
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		values = append(values, v)
	}
	return values, rows.Err()
}

func (db *DB) codexAuditTimeline(ctx context.Context, start, end time.Time, bucketMinutes int) ([]CodexAuditTimelinePoint, error) {
	buckets := map[int64]*CodexAuditTimelinePoint{}
	bucketOf := func(t time.Time) int64 {
		sec := int64(bucketMinutes) * 60
		if sec <= 0 {
			sec = 300
		}
		return t.Unix() / sec * sec
	}
	startArg, endArg := db.timeRangeArgs(start, end)
	rows, err := db.conn.QueryContext(ctx, `
		SELECT created_at, status_code, COALESCE(first_token_ms, 0)
		FROM usage_logs
		WHERE created_at >= $1 AND created_at <= $2
		ORDER BY created_at ASC
		LIMIT 100000
	`, startArg, endArg)
	if err != nil {
		return nil, err
	}
	firstTokens := map[int64][]int{}
	for rows.Next() {
		var raw any
		var status, ft int
		if err := rows.Scan(&raw, &status, &ft); err != nil {
			rows.Close()
			return nil, err
		}
		created, err := parseDBTimeValue(raw)
		if err != nil {
			rows.Close()
			return nil, err
		}
		key := bucketOf(created)
		point := buckets[key]
		if point == nil {
			point = &CodexAuditTimelinePoint{Bucket: time.Unix(key, 0)}
			buckets[key] = point
		}
		point.Requests++
		if status >= 500 {
			point.Errors5xx++
		} else if status >= 400 {
			point.Errors4xx++
		}
		if ft > 0 {
			firstTokens[key] = append(firstTokens[key], ft)
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	pRows, err := db.conn.QueryContext(ctx, `
		SELECT created_at, COALESCE(action, ''), COALESCE(review_flagged, false), COALESCE(source, '')
		FROM prompt_filter_logs
		WHERE created_at >= $1 AND created_at <= $2
		ORDER BY created_at ASC
		LIMIT 100000
	`, startArg, endArg)
	if err != nil {
		return nil, err
	}
	defer pRows.Close()
	for pRows.Next() {
		var raw any
		var action, source string
		var reviewFlagged bool
		if err := pRows.Scan(&raw, &action, &reviewFlagged, &source); err != nil {
			return nil, err
		}
		created, err := parseDBTimeValue(raw)
		if err != nil {
			return nil, err
		}
		key := bucketOf(created)
		point := buckets[key]
		if point == nil {
			point = &CodexAuditTimelinePoint{Bucket: time.Unix(key, 0)}
			buckets[key] = point
		}
		if action == "block" {
			point.PromptBlocks++
		}
		if reviewFlagged {
			point.ReviewFlagged++
		}
		if source == "upstream_cyber_policy" {
			point.UpstreamCyberPolicy++
		}
	}
	if err := pRows.Err(); err != nil {
		return nil, err
	}

	keys := make([]int64, 0, len(buckets))
	for key := range buckets {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	result := make([]CodexAuditTimelinePoint, 0, len(keys))
	for _, key := range keys {
		point := *buckets[key]
		point.FirstTokenP95MS = percentileInt(firstTokens[key], 0.95)
		result = append(result, point)
	}
	return result, nil
}

func (db *DB) codexAuditModels(ctx context.Context, start, end time.Time, limit int) ([]CodexAuditModelRow, error) {
	startArg, endArg := db.timeRangeArgs(start, end)
	rows, err := db.conn.QueryContext(ctx, `
		SELECT COALESCE(NULLIF(effective_model, ''), model, '') AS m,
		       COUNT(*),
		       COALESCE(SUM(CASE WHEN status_code BETWEEN 400 AND 499 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN status_code >= 500 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN COALESCE(via_websocket, false) THEN 1 ELSE 0 END), 0)
		FROM usage_logs
		WHERE created_at >= $1 AND created_at <= $2
		GROUP BY 1
		ORDER BY COUNT(*) DESC, 1
		LIMIT $3
	`, startArg, endArg, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]CodexAuditModelRow, 0)
	for rows.Next() {
		var item CodexAuditModelRow
		if err := rows.Scan(&item.Model, &item.Requests, &item.Errors4xx, &item.Errors5xx, &item.WebSocket); err != nil {
			return nil, err
		}
		values, err := db.codexAuditFirstTokenValues(ctx, start, end, item.Model)
		if err != nil {
			return nil, err
		}
		item.FirstTokenP95MS = percentileInt(values, 0.95)
		result = append(result, item)
	}
	return result, rows.Err()
}

func (db *DB) codexAuditSuspiciousSamples(ctx context.Context, start, end time.Time, limit int) ([]*PromptFilterLog, error) {
	startArg, endArg := db.timeRangeArgs(start, end)
	rows, err := db.conn.QueryContext(ctx, `
		SELECT id, created_at, COALESCE(source, ''), COALESCE(endpoint, ''), COALESCE(model, ''),
		       COALESCE(action, ''), COALESCE(mode, ''), COALESCE(score, 0), COALESCE(threshold_value, 0),
		       COALESCE(matched_patterns, '[]'), COALESCE(text_preview, ''), COALESCE(api_key_id, 0),
		       COALESCE(api_key_name, ''), COALESCE(api_key_masked, ''), COALESCE(client_ip, ''), COALESCE(error_code, ''),
		       COALESCE(review_model, ''), COALESCE(review_flagged, false), COALESCE(review_error, ''), COALESCE(full_text, '')
		FROM prompt_filter_logs
		WHERE created_at >= $1 AND created_at <= $2
		  AND (
		    action = 'block'
		    OR source IN ('upstream_cyber_policy', 'semantic_review_disagreement')
		    OR COALESCE(review_error, '') <> ''
		    OR (action = 'allow' AND COALESCE(review_model, '') <> '' AND COALESCE(review_flagged, false) = false AND score >= 50)
		  )
		ORDER BY CASE
		    WHEN source = 'upstream_cyber_policy' THEN 0
		    WHEN source = 'semantic_review_disagreement' THEN 1
		    WHEN action = 'block' THEN 2
		    WHEN COALESCE(review_error, '') <> '' THEN 3
		    ELSE 4
		  END,
		  score DESC,
		  created_at DESC
		LIMIT $3
	`, startArg, endArg, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPromptFilterLogs(rows)
}

func (db *DB) codexAuditProbeObserved(ctx context.Context, start, end time.Time, limit int) ([]CodexAuditProbeRow, error) {
	startArg, endArg := db.timeRangeArgs(start, end)
	rows, err := db.conn.QueryContext(ctx, `
		SELECT COALESCE(api_key_id, 0), COALESCE(api_key_name, ''), COALESCE(api_key_masked, ''),
		       COALESCE(endpoint, ''), COALESCE(model, ''), COUNT(*), MIN(created_at), MAX(created_at)
		FROM prompt_filter_logs
		WHERE created_at >= $1 AND created_at <= $2 AND source = 'local_probe_observed'
		GROUP BY 1, 2, 3, 4, 5
		ORDER BY COUNT(*) DESC, MIN(created_at)
		LIMIT $3
	`, startArg, endArg, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCodexAuditProbeRows(rows)
}

func (db *DB) codexAuditProbeShortCircuits(ctx context.Context, start, end time.Time, limit int) ([]CodexAuditProbeRow, error) {
	startArg, endArg := db.timeRangeArgs(start, end)
	rows, err := db.conn.QueryContext(ctx, `
		SELECT COALESCE(api_key_id, 0), COALESCE(api_key_name, ''), COALESCE(api_key_masked, ''),
		       COALESCE(inbound_endpoint, endpoint, ''), COALESCE(NULLIF(effective_model, ''), model, ''), COUNT(*), MIN(created_at), MAX(created_at)
		FROM usage_logs
		WHERE created_at >= $1 AND created_at <= $2 AND upstream_error_kind = 'local_probe_short_circuit'
		GROUP BY 1, 2, 3, 4, 5
		ORDER BY COUNT(*) DESC, MIN(created_at)
		LIMIT $3
	`, startArg, endArg, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanCodexAuditProbeRows(rows)
}

func scanCodexAuditProbeRows(rows scannerRows) ([]CodexAuditProbeRow, error) {
	result := make([]CodexAuditProbeRow, 0)
	for rows.Next() {
		var item CodexAuditProbeRow
		var firstRaw, lastRaw any
		if err := rows.Scan(&item.APIKeyID, &item.APIKeyName, &item.APIKeyMasked, &item.Endpoint, &item.Model, &item.Count, &firstRaw, &lastRaw); err != nil {
			return nil, err
		}
		first, err := parseDBTimeValue(firstRaw)
		if err != nil {
			return nil, err
		}
		last, err := parseDBTimeValue(lastRaw)
		if err != nil {
			return nil, err
		}
		item.FirstSeen = first
		item.LastSeen = last
		item.SpanSeconds = last.Sub(first).Seconds()
		result = append(result, item)
	}
	return result, rows.Err()
}

func (db *DB) codexAuditUsageSamples(ctx context.Context, start, end time.Time, limit int, kind string) ([]*UsageLog, error) {
	if kind == "policy" {
		return db.codexAuditPolicyErrorSamples(ctx, start, end, limit)
	}

	filter := UsageLogFilter{
		Start:    start,
		End:      end,
		Page:     1,
		PageSize: limit,
	}
	switch kind {
	case "slow":
		filter.PageSize = limit
	default:
		filter.PageSize = limit
	}
	page, err := db.ListUsageLogsByTimeRangePaged(ctx, filter)
	if err != nil {
		return nil, err
	}
	logs := page.Logs
	sort.Slice(logs, func(i, j int) bool {
		return logs[i].FirstTokenMs > logs[j].FirstTokenMs
	})
	if len(logs) > limit {
		logs = logs[:limit]
	}
	return logs, nil
}

func (db *DB) codexAuditPolicyErrorSamples(ctx context.Context, start, end time.Time, limit int) ([]*UsageLog, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	where, args := db.buildUsageLogWhere(UsageLogFilter{
		Start:           start,
		End:             end,
		ErrorOnly:       true,
		IncludeCanceled: true,
	})
	where += ` AND (
		LOWER(COALESCE(u.error_message, '')) LIKE '%policy%'
		OR LOWER(COALESCE(u.error_message, '')) LIKE '%cyber%'
		OR LOWER(COALESCE(u.error_message, '')) LIKE '%violat%'
		OR LOWER(COALESCE(u.error_message, '')) LIKE '%safety%'
		OR LOWER(COALESCE(u.upstream_error_kind, '')) LIKE '%policy%'
		OR LOWER(COALESCE(u.upstream_error_kind, '')) LIKE '%cyber%'
		OR LOWER(COALESCE(u.upstream_error_kind, '')) LIKE '%violat%'
		OR LOWER(COALESCE(u.upstream_error_kind, '')) LIKE '%safety%'
	)`
	limitArg := fmt.Sprintf("$%d", len(args)+1)
	args = append(args, limit)

	query := `SELECT u.id, u.account_id, COALESCE(u.client_ip, ''), u.endpoint, u.model, COALESCE(u.effective_model, ''), u.prompt_tokens, u.completion_tokens, u.total_tokens, u.status_code, u.duration_ms,
			COALESCE(u.input_tokens, 0), COALESCE(u.output_tokens, 0), COALESCE(u.reasoning_tokens, 0),
			COALESCE(u.first_token_ms, 0), COALESCE(u.reasoning_effort, ''), COALESCE(u.inbound_endpoint, ''),
			COALESCE(u.upstream_endpoint, ''), COALESCE(u.stream, false), COALESCE(u.compact, false), COALESCE(u.via_websocket, false), COALESCE(u.cached_tokens, 0), COALESCE(u.service_tier, ''),
			COALESCE(u.requested_service_tier, ''), COALESCE(u.actual_service_tier, ''), COALESCE(u.billing_service_tier, ''),
			COALESCE(u.api_key_id, 0), COALESCE(u.api_key_name, ''), COALESCE(u.api_key_masked, ''),
			COALESCE(u.image_count, 0), COALESCE(u.image_width, 0), COALESCE(u.image_height, 0), COALESCE(u.image_bytes, 0),
			COALESCE(u.image_format, ''), COALESCE(u.image_size, ''),
			COALESCE(u.account_billed, 0), COALESCE(u.user_billed, 0),
			COALESCE(u.is_retry_attempt, false), COALESCE(u.attempt_index, 0), COALESCE(u.upstream_error_kind, ''), COALESCE(u.error_message, ''),
			COALESCE(CAST(a.credentials AS TEXT), '{}'), u.created_at
		FROM usage_logs u
		LEFT JOIN accounts a ON u.account_id = a.id
		WHERE ` + where + ` ORDER BY u.created_at DESC LIMIT ` + limitArg

	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	logs := make([]*UsageLog, 0)
	for rows.Next() {
		l := &UsageLog{}
		var credentialRaw interface{}
		var createdAtRaw interface{}
		if err := rows.Scan(&l.ID, &l.AccountID, &l.ClientIP, &l.Endpoint, &l.Model, &l.EffectiveModel, &l.PromptTokens, &l.CompletionTokens, &l.TotalTokens, &l.StatusCode, &l.DurationMs,
			&l.InputTokens, &l.OutputTokens, &l.ReasoningTokens, &l.FirstTokenMs, &l.ReasoningEffort, &l.InboundEndpoint, &l.UpstreamEndpoint, &l.Stream, &l.Compact, &l.ViaWebsocket, &l.CachedTokens,
			&l.ServiceTier, &l.RequestedServiceTier, &l.ActualServiceTier, &l.BillingServiceTier, &l.APIKeyID, &l.APIKeyName, &l.APIKeyMasked, &l.ImageCount, &l.ImageWidth, &l.ImageHeight, &l.ImageBytes, &l.ImageFormat, &l.ImageSize,
			&l.AccountBilled, &l.UserBilled, &l.IsRetryAttempt, &l.AttemptIndex, &l.UpstreamErrorKind, &l.ErrorMessage, &credentialRaw, &createdAtRaw); err != nil {
			return nil, err
		}
		l.AccountEmail = accountEmailFromRawCredentials(credentialRaw)
		l.CreatedAt, err = parseDBTimeValue(createdAtRaw)
		if err != nil {
			return nil, err
		}
		l.populateBillingBreakdown()
		logs = append(logs, l)
	}
	return logs, rows.Err()
}

type scannerRows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
}

func scanPromptFilterLogs(rows scannerRows) ([]*PromptFilterLog, error) {
	result := make([]*PromptFilterLog, 0)
	for rows.Next() {
		item := &PromptFilterLog{}
		var createdAtRaw any
		if err := rows.Scan(&item.ID, &createdAtRaw, &item.Source, &item.Endpoint, &item.Model, &item.Action, &item.Mode,
			&item.Score, &item.Threshold, &item.MatchedPatterns, &item.TextPreview, &item.APIKeyID, &item.APIKeyName,
			&item.APIKeyMasked, &item.ClientIP, &item.ErrorCode, &item.ReviewModel, &item.ReviewFlagged, &item.ReviewError, &item.FullText); err != nil {
			return nil, err
		}
		createdAt, err := parseDBTimeValue(createdAtRaw)
		if err != nil {
			return nil, err
		}
		item.CreatedAt = createdAt
		result = append(result, item)
	}
	return result, rows.Err()
}

func summarizeCodexAudit(report *CodexAuditReport) CodexAuditSummary {
	var summary CodexAuditSummary
	for _, row := range report.PromptFilter {
		summary.PromptLogs += row.Count
		if row.Action == "block" {
			summary.PromptBlocks += row.Count
		}
		if row.ReviewFlagged {
			summary.ReviewFlagged += row.Count
		}
		summary.ReviewErrors += row.ReviewErrors
		if row.Source == "upstream_cyber_policy" {
			summary.UpstreamCyberPolicy += row.Count
		}
		if row.Source == "semantic_review_disagreement" {
			summary.SemanticDisagreements += row.Count
			if row.Action == "block" {
				summary.SemanticDisagreementBlocks += row.Count
			}
		}
		if row.Source == "local_probe_observed" {
			summary.ProbeObserved += row.Count
		}
		if row.Action == "allow" && row.ReviewModel != "" && !row.ReviewFlagged && row.MaxScore >= 50 {
			summary.HighScoreAllowed += row.Count
		}
	}
	for _, row := range report.ProbeShortCircuits {
		summary.ProbeShortCircuits += row.Count
	}
	return summary
}

func codexAuditVerdict(report *CodexAuditReport) string {
	switch {
	case report.Summary.UpstreamCyberPolicy > 0:
		return "suspected_miss"
	case report.Summary.ReviewErrors > 0:
		return "review_error_risk"
	case report.Usage.Errors5xx > 0:
		return "operational_issue"
	case report.Summary.PromptBlocks > 0:
		return "blocked_activity"
	default:
		return "normal"
	}
}

func percentileInt(values []int, p float64) int {
	if len(values) == 0 {
		return 0
	}
	sort.Ints(values)
	if p <= 0 {
		return values[0]
	}
	if p >= 1 {
		return values[len(values)-1]
	}
	pos := p * float64(len(values)-1)
	lower := int(math.Floor(pos))
	upper := int(math.Ceil(pos))
	if lower == upper {
		return values[lower]
	}
	weight := pos - float64(lower)
	return int(math.Round(float64(values[lower])*(1-weight) + float64(values[upper])*weight))
}
