package database

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type PromptFilterLog struct {
	ID              int64     `json:"id"`
	CreatedAt       time.Time `json:"created_at"`
	Source          string    `json:"source"`
	Endpoint        string    `json:"endpoint"`
	Protocol        string    `json:"protocol"`
	Provider        string    `json:"provider"`
	Model           string    `json:"model"`
	Action          string    `json:"action"`
	Mode            string    `json:"mode"`
	Score           int       `json:"score"`
	AuditScore      int       `json:"audit_score"`
	Threshold       int       `json:"threshold"`
	PolicyProfile   string    `json:"policy_profile"`
	ReasonCode      string    `json:"reason_code"`
	PrimaryOrigin   string    `json:"primary_origin"`
	StrikeEligible  bool      `json:"strike_eligible"`
	MatchedPatterns string    `json:"matched_patterns"`
	TextPreview     string    `json:"text_preview"`
	MatchContext    string    `json:"match_context"`
	FullText        string    `json:"full_text"`
	APIKeyID        int64     `json:"api_key_id"`
	APIKeyName      string    `json:"api_key_name"`
	APIKeyMasked    string    `json:"api_key_masked"`
	ClientIP        string    `json:"client_ip"`
	ErrorCode       string    `json:"error_code"`
	ReviewModel     string    `json:"review_model"`
	ReviewFlagged   bool      `json:"review_flagged"`
	ReviewError     string    `json:"review_error"`
}

type PromptFilterLogInput struct {
	Source          string
	Endpoint        string
	Protocol        string
	Provider        string
	Model           string
	Action          string
	Mode            string
	Score           int
	AuditScore      int
	Threshold       int
	PolicyProfile   string
	ReasonCode      string
	PrimaryOrigin   string
	StrikeEligible  bool
	MatchedPatterns string
	TextPreview     string
	MatchContext    string
	FullText        string
	APIKeyID        int64
	APIKeyName      string
	APIKeyMasked    string
	ClientIP        string
	ErrorCode       string
	ReviewModel     string
	ReviewFlagged   bool
	ReviewError     string
}

type PromptFilterLogQuery struct {
	Page     int
	PageSize int
	Limit    int
	Source   string
	Action   string
	Endpoint string
	Model    string
	APIKeyID int64
	Query    string
}

func (db *DB) InsertPromptFilterLog(ctx context.Context, input *PromptFilterLogInput) error {
	if db == nil || input == nil {
		return nil
	}
	_, err := db.conn.ExecContext(ctx, `
		INSERT INTO prompt_filter_logs (
			source, endpoint, request_protocol, request_provider, model, action, mode, score, audit_score, threshold_value, policy_profile, reason_code, primary_origin, strike_eligible, matched_patterns, text_preview,
			match_context, api_key_id, api_key_name, api_key_masked, client_ip, error_code, review_model, review_flagged, review_error, full_text
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26)
	`, input.Source, input.Endpoint, input.Protocol, input.Provider, input.Model, input.Action, input.Mode, input.Score, input.AuditScore, input.Threshold,
		input.PolicyProfile, input.ReasonCode, input.PrimaryOrigin, input.StrikeEligible, input.MatchedPatterns, input.TextPreview, input.MatchContext,
		input.APIKeyID, input.APIKeyName, input.APIKeyMasked, input.ClientIP, input.ErrorCode, input.ReviewModel, input.ReviewFlagged, input.ReviewError, input.FullText)
	return err
}

func (db *DB) ListPromptFilterLogs(ctx context.Context, limit int) ([]*PromptFilterLog, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	result, _, err := db.ListPromptFilterLogsPage(ctx, PromptFilterLogQuery{Page: 1, PageSize: limit})
	return result, err
}

func (db *DB) ListPromptFilterLogsPage(ctx context.Context, query PromptFilterLogQuery) ([]*PromptFilterLog, int, error) {
	pageSize := query.PageSize
	if pageSize <= 0 {
		pageSize = query.Limit
	}
	if pageSize <= 0 || pageSize > 500 {
		pageSize = 100
	}
	page := query.Page
	if page <= 0 {
		page = 1
	}

	where, args := promptFilterLogWhere(query)
	countSQL := `SELECT COUNT(*) FROM prompt_filter_logs` + where
	var total int
	if err := db.conn.QueryRowContext(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	args = append(args, pageSize, (page-1)*pageSize)
	rows, err := db.conn.QueryContext(ctx, `
		SELECT id, created_at, COALESCE(source, ''), COALESCE(endpoint, ''), COALESCE(request_protocol, ''), COALESCE(request_provider, ''), COALESCE(model, ''),
		       COALESCE(action, ''), COALESCE(mode, ''), COALESCE(score, 0), COALESCE(audit_score, 0), COALESCE(threshold_value, 0),
		       COALESCE(policy_profile, ''), COALESCE(reason_code, ''), COALESCE(primary_origin, ''), COALESCE(strike_eligible, false),
		       COALESCE(matched_patterns, '[]'), COALESCE(text_preview, ''), COALESCE(match_context, ''), COALESCE(api_key_id, 0),
		       COALESCE(api_key_name, ''), COALESCE(api_key_masked, ''), COALESCE(client_ip, ''), COALESCE(error_code, ''),
		       COALESCE(review_model, ''), COALESCE(review_flagged, false), COALESCE(review_error, ''), COALESCE(full_text, '')
		FROM prompt_filter_logs
		`+where+`
		ORDER BY id DESC
		LIMIT $`+fmt.Sprint(len(args)-1)+` OFFSET $`+fmt.Sprint(len(args))+`
	`, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	logs := make([]*PromptFilterLog, 0)
	for rows.Next() {
		item := &PromptFilterLog{}
		var createdAtRaw interface{}
		if err := rows.Scan(&item.ID, &createdAtRaw, &item.Source, &item.Endpoint, &item.Protocol, &item.Provider, &item.Model, &item.Action, &item.Mode,
			&item.Score, &item.AuditScore, &item.Threshold, &item.PolicyProfile, &item.ReasonCode, &item.PrimaryOrigin, &item.StrikeEligible,
			&item.MatchedPatterns, &item.TextPreview, &item.MatchContext, &item.APIKeyID, &item.APIKeyName,
			&item.APIKeyMasked, &item.ClientIP, &item.ErrorCode, &item.ReviewModel, &item.ReviewFlagged, &item.ReviewError, &item.FullText); err != nil {
			return nil, 0, err
		}
		createdAt, err := parseDBTimeValue(createdAtRaw)
		if err != nil {
			return nil, 0, err
		}
		item.CreatedAt = createdAt
		logs = append(logs, item)
	}
	return logs, total, rows.Err()
}

func promptFilterLogWhere(query PromptFilterLogQuery) (string, []any) {
	clauses := make([]string, 0, 8)
	args := make([]any, 0, 8)
	addExact := func(column, value string) {
		value = strings.TrimSpace(value)
		if value == "" || value == "all" {
			return
		}
		args = append(args, value)
		clauses = append(clauses, fmt.Sprintf("%s = $%d", column, len(args)))
	}
	addExact("source", query.Source)
	addExact("action", query.Action)
	addExact("endpoint", query.Endpoint)
	addExact("model", query.Model)
	if query.APIKeyID > 0 {
		args = append(args, query.APIKeyID)
		clauses = append(clauses, fmt.Sprintf("api_key_id = $%d", len(args)))
	}
	if q := strings.TrimSpace(query.Query); q != "" {
		args = append(args, "%"+strings.ToLower(q)+"%")
		idx := len(args)
		clauses = append(clauses, fmt.Sprintf(`(
			LOWER(COALESCE(text_preview, '')) LIKE $%d OR
			LOWER(COALESCE(match_context, '')) LIKE $%d OR
			LOWER(COALESCE(full_text, '')) LIKE $%d OR
			LOWER(COALESCE(matched_patterns, '')) LIKE $%d OR
			LOWER(COALESCE(error_code, '')) LIKE $%d OR
			LOWER(COALESCE(review_error, '')) LIKE $%d OR
			LOWER(COALESCE(api_key_name, '')) LIKE $%d OR
			LOWER(COALESCE(api_key_masked, '')) LIKE $%d
		)`, idx, idx, idx, idx, idx, idx, idx, idx))
	}
	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

// FindNearestPromptFilterLog 返回与给定时间 at 最接近的一条提示词过滤日志，用于把
// 「使用统计」里的某次报错关联到对应的拦截记录（含完整请求内容）。按 source /
// api_key_id 过滤，时间窗口内取最接近的一条；endpoint 仅作为同等时间下的优先项。
func (db *DB) FindNearestPromptFilterLog(ctx context.Context, at time.Time, source, endpoint string, apiKeyID int64, windowSeconds int) (*PromptFilterLog, error) {
	if db == nil {
		return nil, nil
	}
	if windowSeconds <= 0 {
		windowSeconds = 10
	}
	startArg, endArg := db.timeRangeArgs(at.Add(-time.Duration(windowSeconds)*time.Second), at.Add(time.Duration(windowSeconds)*time.Second))
	clauses := []string{"created_at >= $1", "created_at <= $2"}
	args := []any{startArg, endArg}
	if s := strings.TrimSpace(source); s != "" {
		args = append(args, s)
		clauses = append(clauses, fmt.Sprintf("source = $%d", len(args)))
	}
	if apiKeyID > 0 {
		args = append(args, apiKeyID)
		clauses = append(clauses, fmt.Sprintf("api_key_id = $%d", len(args)))
	}

	rows, err := db.conn.QueryContext(ctx, `
		SELECT id, created_at, COALESCE(source, ''), COALESCE(endpoint, ''), COALESCE(request_protocol, ''), COALESCE(request_provider, ''), COALESCE(model, ''),
		       COALESCE(action, ''), COALESCE(mode, ''), COALESCE(score, 0), COALESCE(audit_score, 0), COALESCE(threshold_value, 0),
		       COALESCE(policy_profile, ''), COALESCE(reason_code, ''), COALESCE(primary_origin, ''), COALESCE(strike_eligible, false),
		       COALESCE(matched_patterns, '[]'), COALESCE(text_preview, ''), COALESCE(match_context, ''), COALESCE(api_key_id, 0),
		       COALESCE(api_key_name, ''), COALESCE(api_key_masked, ''), COALESCE(client_ip, ''), COALESCE(error_code, ''),
		       COALESCE(review_model, ''), COALESCE(review_flagged, false), COALESCE(review_error, ''), COALESCE(full_text, '')
		FROM prompt_filter_logs
		WHERE `+strings.Join(clauses, " AND ")+`
		ORDER BY id DESC
		LIMIT 50
	`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var best *PromptFilterLog
	var bestDelta time.Duration
	for rows.Next() {
		item := &PromptFilterLog{}
		var createdAtRaw interface{}
		if err := rows.Scan(&item.ID, &createdAtRaw, &item.Source, &item.Endpoint, &item.Protocol, &item.Provider, &item.Model, &item.Action, &item.Mode,
			&item.Score, &item.AuditScore, &item.Threshold, &item.PolicyProfile, &item.ReasonCode, &item.PrimaryOrigin, &item.StrikeEligible,
			&item.MatchedPatterns, &item.TextPreview, &item.MatchContext, &item.APIKeyID, &item.APIKeyName,
			&item.APIKeyMasked, &item.ClientIP, &item.ErrorCode, &item.ReviewModel, &item.ReviewFlagged, &item.ReviewError, &item.FullText); err != nil {
			return nil, err
		}
		createdAt, err := parseDBTimeValue(createdAtRaw)
		if err != nil {
			continue
		}
		item.CreatedAt = createdAt
		delta := at.Sub(createdAt)
		if delta < 0 {
			delta = -delta
		}
		// endpoint 一致时给一点优先（减小有效距离），保证同一时刻多条时选对端点。
		if endpoint != "" && item.Endpoint == endpoint {
			if delta >= time.Second {
				delta -= time.Second
			} else {
				delta = 0
			}
		}
		if best == nil || delta < bestDelta {
			best = item
			bestDelta = delta
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return best, nil
}

func (db *DB) ClearPromptFilterLogs(ctx context.Context) error {
	if db == nil {
		return nil
	}
	if db.isSQLite() {
		if _, err := db.conn.ExecContext(ctx, `DELETE FROM prompt_filter_logs`); err != nil {
			return err
		}
		_, err := db.conn.ExecContext(ctx, `DELETE FROM sqlite_sequence WHERE name = 'prompt_filter_logs'`)
		return err
	}
	_, err := db.conn.ExecContext(ctx, `TRUNCATE TABLE prompt_filter_logs RESTART IDENTITY`)
	return err
}
