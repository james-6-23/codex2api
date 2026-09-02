package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
)

// ListAccountListProjection returns only non-secret fields needed to build the
// short-lived admin list snapshot. In particular it never selects or decodes
// the complete credentials document.
func (db *DB) ListAccountListProjection(ctx context.Context, channel string) ([]*AccountRow, error) {
	where := `status <> 'deleted' AND COALESCE(error_message, '') <> 'deleted'`
	upstreamExpr := `LOWER(COALESCE(credentials->>'upstream_type', ''))`
	fromClause := `FROM accounts
		CROSS JOIN LATERAL jsonb_to_record(accounts.credentials) AS account_public(
			upstream_type text, email text, base_url text, plan_type text,
			models jsonb, api_key text, refresh_token text, scheduler_priority text,
			avatar_url text, verified_email boolean, project_id text,
			antigravity_sync_error text, antigravity_sync_warning text,
			antigravity_permissions text, antigravity_entitlements text, antigravity_quota text,
			claude_usage_probe_at text, claude_usage_probe_error text
		)`
	credentialColumns := `
		COALESCE(account_public.upstream_type, ''),
		COALESCE(account_public.email, ''),
		COALESCE(account_public.base_url, ''),
		COALESCE(account_public.plan_type, ''),
		COALESCE(account_public.models, '[]'::jsonb)::text,
		COALESCE(account_public.api_key, '') <> '',
		COALESCE(account_public.refresh_token, '') <> '',
		COALESCE(account_public.scheduler_priority, ''),
		COALESCE(account_public.avatar_url, ''),
		COALESCE(account_public.verified_email, false),
		COALESCE(account_public.project_id, ''),
		COALESCE(account_public.antigravity_sync_error, ''),
		COALESCE(account_public.antigravity_sync_warning, ''),
		COALESCE(NULLIF(account_public.antigravity_permissions, ''), account_public.antigravity_entitlements, ''),
		COALESCE(account_public.antigravity_quota, ''),
		COALESCE(account_public.claude_usage_probe_at, ''),
		COALESCE(account_public.claude_usage_probe_error, '')`
	if db.isSQLite() {
		upstreamExpr = `LOWER(COALESCE(json_extract(credentials, '$.upstream_type'), ''))`
		fromClause = `FROM accounts`
		credentialColumns = `
			COALESCE(json_extract(credentials, '$.upstream_type'), ''),
			COALESCE(json_extract(credentials, '$.email'), ''),
			COALESCE(json_extract(credentials, '$.base_url'), ''),
			COALESCE(json_extract(credentials, '$.plan_type'), ''),
			COALESCE(json_extract(credentials, '$.models'), '[]'),
			CASE WHEN COALESCE(json_extract(credentials, '$.api_key'), '') <> '' THEN 1 ELSE 0 END,
			CASE WHEN COALESCE(json_extract(credentials, '$.refresh_token'), '') <> '' THEN 1 ELSE 0 END,
			COALESCE(CAST(json_extract(credentials, '$.scheduler_priority') AS TEXT), ''),
			COALESCE(json_extract(credentials, '$.avatar_url'), ''),
			CASE WHEN COALESCE(json_extract(credentials, '$.verified_email'), 0) <> 0 THEN 1 ELSE 0 END,
			COALESCE(json_extract(credentials, '$.project_id'), ''),
			COALESCE(json_extract(credentials, '$.antigravity_sync_error'), ''),
			COALESCE(json_extract(credentials, '$.antigravity_sync_warning'), ''),
			COALESCE(NULLIF(json_extract(credentials, '$.antigravity_permissions'), ''), json_extract(credentials, '$.antigravity_entitlements'), '{}'),
			COALESCE(json_extract(credentials, '$.antigravity_quota'), '{}'),
			COALESCE(json_extract(credentials, '$.claude_usage_probe_at'), ''),
			COALESCE(json_extract(credentials, '$.claude_usage_probe_error'), '')`
	}
	where += accountChannelFilterSQL(channel, upstreamExpr)
	query := `SELECT id, name, type, proxy_url, status, cooldown_reason, cooldown_until,
		COALESCE(error_message, ''), COALESCE(enabled, true), COALESCE(locked, false),
		score_bias_override, base_concurrency_override, COALESCE(tags, '[]'), created_at, updated_at,
		COALESCE(credential_generation, 1), COALESCE(credential_family_id, ''),` + credentialColumns + `
		` + fromClause + ` WHERE ` + where + ` ORDER BY id`
	rows, err := db.conn.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("查询账号列表投影失败: %w", err)
	}
	defer rows.Close()
	result := make([]*AccountRow, 0)
	for rows.Next() {
		row, err := scanAccountListProjection(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

type accountProjectionScanner interface {
	Scan(dest ...interface{}) error
}

func scanAccountListProjection(scanner accountProjectionScanner) (*AccountRow, error) {
	row := &AccountRow{}
	var cooldownRaw, tagsRaw, createdRaw, updatedRaw interface{}
	var upstreamType, email, baseURL, planType, schedulerPriority string
	var avatarURL, projectID string
	var antigravitySyncError, antigravitySyncWarning, antigravityPermissions, antigravityQuota string
	var claudeUsageProbeAt, claudeUsageProbeError string
	var modelsRaw interface{}
	var hasAPIKey, hasRefreshToken, verifiedEmail bool
	if err := scanner.Scan(
		&row.ID, &row.Name, &row.Type, &row.ProxyURL, &row.Status, &row.CooldownReason, &cooldownRaw,
		&row.ErrorMessage, &row.Enabled, &row.Locked, &row.ScoreBiasOverride, &row.BaseConcurrencyOverride,
		&tagsRaw, &createdRaw, &updatedRaw, &row.CredentialGeneration, &row.CredentialFamilyID,
		&upstreamType, &email, &baseURL, &planType, &modelsRaw,
		&hasAPIKey, &hasRefreshToken, &schedulerPriority,
		&avatarURL, &verifiedEmail, &projectID,
		&antigravitySyncError, &antigravitySyncWarning, &antigravityPermissions, &antigravityQuota,
		&claudeUsageProbeAt, &claudeUsageProbeError,
	); err != nil {
		return nil, fmt.Errorf("扫描账号列表投影失败: %w", err)
	}
	row.Tags = decodeTagsValue(tagsRaw)
	var err error
	row.CooldownUntil, err = parseDBNullTimeValue(cooldownRaw)
	if err != nil {
		return nil, fmt.Errorf("解析 cooldown_until 失败: %w", err)
	}
	row.CreatedAt, err = parseDBTimeValue(createdRaw)
	if err != nil {
		return nil, fmt.Errorf("解析 created_at 失败: %w", err)
	}
	row.UpdatedAt, err = parseDBTimeValue(updatedRaw)
	if err != nil {
		return nil, fmt.Errorf("解析 updated_at 失败: %w", err)
	}
	row.Credentials = map[string]interface{}{
		"upstream_type": upstreamType,
		"email":         email,
		"base_url":      baseURL,
		"plan_type":     planType,
	}
	// 调度优先级参与列表排序(issue 截图反馈:排序不生效),投影缺了它会让
	// 快照全员按 0 打平、退化成 ID 序。以文本取出交给 GetCredentialInt64 解析。
	if trimmed := strings.TrimSpace(schedulerPriority); trimmed != "" {
		row.Credentials["scheduler_priority"] = trimmed
	}
	if trimmed := strings.TrimSpace(avatarURL); trimmed != "" {
		row.Credentials["avatar_url"] = trimmed
	}
	if verifiedEmail {
		row.Credentials["verified_email"] = true
	}
	if trimmed := strings.TrimSpace(projectID); trimmed != "" {
		row.Credentials["project_id"] = trimmed
	}
	if trimmed := strings.TrimSpace(antigravitySyncError); trimmed != "" {
		row.Credentials["antigravity_sync_error"] = trimmed
	}
	if trimmed := strings.TrimSpace(antigravitySyncWarning); trimmed != "" {
		row.Credentials["antigravity_sync_warning"] = trimmed
	}
	if trimmed := strings.TrimSpace(antigravityPermissions); trimmed != "" && trimmed != "{}" {
		row.Credentials["antigravity_permissions"] = trimmed
	}
	if trimmed := strings.TrimSpace(antigravityQuota); trimmed != "" && trimmed != "{}" {
		row.Credentials["antigravity_quota"] = trimmed
	}
	if trimmed := strings.TrimSpace(claudeUsageProbeAt); trimmed != "" {
		row.Credentials["claude_usage_probe_at"] = trimmed
	}
	if trimmed := strings.TrimSpace(claudeUsageProbeError); trimmed != "" {
		row.Credentials["claude_usage_probe_error"] = trimmed
	}
	if models := decodeProjectionStringSlice(modelsRaw); len(models) > 0 {
		row.Credentials["models"] = models
	}
	if hasAPIKey {
		row.Credentials["api_key"] = "configured"
	}
	if hasRefreshToken {
		row.Credentials["refresh_token"] = "configured"
	}
	return row, nil
}

func decodeProjectionStringSlice(value interface{}) []string {
	var raw []byte
	switch typed := value.(type) {
	case string:
		raw = []byte(typed)
	case []byte:
		raw = typed
	case nil:
		return nil
	default:
		raw, _ = json.Marshal(typed)
	}
	var values []string
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil
	}
	return values
}

// ListActiveByIDs fetches the complete base rows for one selected page in a
// single query. It is intentionally capped by the caller's page size (<=500).
func (db *DB) ListActiveByIDs(ctx context.Context, ids []int64) ([]*AccountRow, error) {
	ids = positiveUniqueIDs(ids)
	if len(ids) == 0 {
		return []*AccountRow{}, nil
	}
	args := make([]interface{}, 0, len(ids))
	placeholders := make([]string, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
		placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
	}
	query := `SELECT id, name, platform, type, credentials, proxy_url, status, cooldown_reason,
		cooldown_until, error_message, COALESCE(enabled, true), COALESCE(locked, false),
		COALESCE(credit_enabled, false), COALESCE(credit_skip_usage_window, false),
		COALESCE(skip_warm_tier, false), score_bias_override, base_concurrency_override,
		COALESCE(tags, '[]'), COALESCE(note, ''), created_at, updated_at,
		COALESCE(credential_generation, 1), COALESCE(credential_family_id, '')
		FROM accounts WHERE status <> 'deleted' AND COALESCE(error_message, '') <> 'deleted'
		AND id IN (` + strings.Join(placeholders, ",") + `) ORDER BY id`
	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("批量查询账号失败: %w", err)
	}
	defer rows.Close()
	result := make([]*AccountRow, 0, len(ids))
	for rows.Next() {
		row := &AccountRow{}
		var credentialsRaw, cooldownRaw, tagsRaw, createdRaw, updatedRaw interface{}
		if err := rows.Scan(
			&row.ID, &row.Name, &row.Platform, &row.Type, &credentialsRaw, &row.ProxyURL, &row.Status,
			&row.CooldownReason, &cooldownRaw, &row.ErrorMessage, &row.Enabled, &row.Locked,
			&row.CreditEnabled, &row.CreditSkipUsageWindow, &row.SkipWarmTier, &row.ScoreBiasOverride,
			&row.BaseConcurrencyOverride, &tagsRaw, &row.Note, &createdRaw, &updatedRaw,
			&row.CredentialGeneration, &row.CredentialFamilyID,
		); err != nil {
			return nil, fmt.Errorf("扫描账号行失败: %w", err)
		}
		row.Credentials = decodeCredentials(credentialsRaw)
		row.Tags = decodeTagsValue(tagsRaw)
		row.CooldownUntil, err = parseDBNullTimeValue(cooldownRaw)
		if err != nil {
			return nil, err
		}
		row.CreatedAt, err = parseDBTimeValue(createdRaw)
		if err != nil {
			return nil, err
		}
		row.UpdatedAt, err = parseDBTimeValue(updatedRaw)
		if err != nil {
			return nil, err
		}
		result = append(result, row)
	}
	return result, rows.Err()
}

var _ accountProjectionScanner = (*sql.Row)(nil)
