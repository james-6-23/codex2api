package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	PromptPolicyEvaluationCompleted     = "completed"
	PromptPolicyEvaluationNotRun        = "not_run"
	PromptPolicyEvaluationUnavailable   = "unavailable"
	PromptPolicyEvaluationLegacyUnknown = "legacy_unknown"

	PromptPolicyOutcomeNoHit    = "no_hit"
	PromptPolicyOutcomeAuditHit = "audit_hit"
	PromptPolicyOutcomeWarn     = "warn"
	PromptPolicyOutcomeBlock    = "block"

	PromptPolicyComparisonConfirmedMiss       = "confirmed_miss"
	PromptPolicyComparisonUpstreamOnly        = "upstream_only"
	PromptPolicyComparisonEvidenceUnavailable = "evidence_unavailable"
	PromptPolicyComparisonLocalDetected       = "local_detected"
	PromptPolicyComparisonNotComparable       = "not_comparable"
	PromptPolicyComparisonLegacyUnknown       = "legacy_unknown"
)

type PromptPolicyIncident struct {
	ID                      int64     `json:"id"`
	IncidentID              string    `json:"incident_id"`
	RequestCorrelationID    string    `json:"request_correlation_id,omitempty"`
	CreatedAt               time.Time `json:"created_at"`
	AttemptIndex            int       `json:"attempt_index"`
	Transport               string    `json:"transport"`
	Endpoint                string    `json:"endpoint"`
	Protocol                string    `json:"protocol"`
	Provider                string    `json:"provider"`
	Model                   string    `json:"model"`
	StatusCode              int       `json:"status_code"`
	AccountID               int64     `json:"account_id"`
	AccountName             string    `json:"account_name"`
	AccountPlatform         string    `json:"account_platform"`
	AccountGroupIDs         []int64   `json:"account_group_ids"`
	AccountGroupNames       []string  `json:"account_group_names"`
	APIKeyID                int64     `json:"api_key_id"`
	APIKeyName              string    `json:"api_key_name"`
	APIKeyMasked            string    `json:"api_key_masked"`
	APIKeyAllowedGroupIDs   []int64   `json:"api_key_allowed_group_ids"`
	APIKeyAllowedGroupNames []string  `json:"api_key_allowed_group_names"`
	RoutingSnapshotState    string    `json:"routing_snapshot_state"`
	Platform                string    `json:"platform"`
	NewAPIPolicyStatus      string    `json:"newapi_policy_status,omitempty"`
	NewAPIPlatform          string    `json:"newapi_platform,omitempty"`
	NewAPIUserID            string    `json:"newapi_user_id,omitempty"`
	NewAPIRequestID         string    `json:"newapi_request_id,omitempty"`
	SessionHash             string    `json:"session_hash,omitempty"`
	ClientIPHash            string    `json:"client_ip_hash,omitempty"`
	SourceRef               string    `json:"source_ref,omitempty"`
	UpstreamErrorCode       string    `json:"upstream_error_code"`
	UpstreamError           string    `json:"upstream_error"`
	LocalEvaluationState    string    `json:"local_evaluation_state"`
	LocalOutcome            string    `json:"local_outcome"`
	LocalAction             string    `json:"local_action"`
	LocalScore              *int      `json:"local_score"`
	LocalRawScore           *int      `json:"local_raw_score"`
	LocalAuditScore         *int      `json:"local_audit_score"`
	LocalAuditRawScore      *int      `json:"local_audit_raw_score"`
	LocalThreshold          int       `json:"local_threshold"`
	LocalMode               string    `json:"local_mode"`
	LocalPolicyProfile      string    `json:"local_policy_profile"`
	LocalReasonCode         string    `json:"local_reason_code"`
	LocalReason             string    `json:"local_reason"`
	LocalPrimaryOrigin      string    `json:"local_primary_origin"`
	LocalStrikeEligible     bool      `json:"local_strike_eligible"`
	LocalReviewModel        string    `json:"local_review_model"`
	LocalReviewFlagged      bool      `json:"local_review_flagged"`
	LocalReviewError        string    `json:"local_review_error"`
	LocalMatchedPatterns    string    `json:"local_matched_patterns"`
	PromptFingerprint       string    `json:"prompt_fingerprint"`
	PromptPreview           string    `json:"prompt_preview"`
	PromptText              string    `json:"prompt_text"`
	PromptAvailable         bool      `json:"prompt_available"`
	LocalComparison         string    `json:"local_comparison"`
	CandidateID             int64     `json:"candidate_id,omitempty"`
	CandidateEvidenceID     int64     `json:"candidate_evidence_id,omitempty"`
	LocalMiss               bool      `json:"local_miss"`
}

type PromptPolicyIncidentInput struct {
	IncidentID              string
	RequestCorrelationID    string
	AttemptIndex            int
	Transport               string
	Endpoint                string
	Protocol                string
	Provider                string
	Model                   string
	StatusCode              int
	AccountID               int64
	AccountName             string
	AccountPlatform         string
	AccountGroupIDs         []int64
	AccountGroupNames       []string
	APIKeyID                int64
	APIKeyName              string
	APIKeyMasked            string
	APIKeyAllowedGroupIDs   []int64
	APIKeyAllowedGroupNames []string
	Platform                string
	NewAPIPolicyStatus      string
	NewAPIPlatform          string
	NewAPIUserID            string
	NewAPIUserName          string
	NewAPIUserEmail         string
	NewAPIUserGroup         string
	NewAPIRequestID         string
	SessionHash             string
	ClientIPHash            string
	SourceRef               string
	UpstreamErrorCode       string
	UpstreamError           string
	LocalEvaluationState    string
	LocalOutcome            string
	LocalAction             string
	LocalScore              *int
	LocalRawScore           *int
	LocalAuditScore         *int
	LocalAuditRawScore      *int
	LocalThreshold          int
	LocalMode               string
	LocalPolicyProfile      string
	LocalReasonCode         string
	LocalReason             string
	LocalPrimaryOrigin      string
	LocalStrikeEligible     bool
	LocalReviewModel        string
	LocalReviewFlagged      bool
	LocalReviewError        string
	LocalMatchedPatterns    string
	PromptFingerprint       string
	PromptPreview           string
	PromptText              string
	PromptAvailable         bool
	LocalComparison         string
	ObservedAt              time.Time
}

type PromptPolicyIncidentQuery struct {
	Page            int
	PageSize        int
	Endpoint        string
	Model           string
	APIKeyID        int64
	AccountID       int64
	EvaluationState string
	Outcome         string
	LocalComparison string
	LocalMiss       *bool
	Query           string
}

const promptPolicyIncidentSelect = `SELECT id, incident_id, COALESCE(request_correlation_id, ''), created_at,
	COALESCE(attempt_index, 0), COALESCE(transport, ''), COALESCE(endpoint, ''), COALESCE(request_protocol, ''),
	COALESCE(request_provider, ''), COALESCE(model, ''), COALESCE(status_code, 0), COALESCE(account_id, 0),
	COALESCE(account_name, ''), COALESCE(account_platform, ''), COALESCE(account_group_ids, '[]'), COALESCE(account_group_names, '[]'),
	COALESCE(api_key_id, 0), COALESCE(api_key_name, ''), COALESCE(api_key_masked, ''), COALESCE(api_key_allowed_group_ids, '[]'), COALESCE(api_key_allowed_group_names, '[]'), COALESCE(platform, ''),
	COALESCE(newapi_policy_status, ''), COALESCE(newapi_platform, ''), COALESCE(newapi_user_id, ''), COALESCE(newapi_request_id, ''), COALESCE(session_hash, ''), COALESCE(client_ip_hash, ''),
	COALESCE(source_ref, ''), COALESCE(upstream_error_code, ''), COALESCE(upstream_error, ''),
	COALESCE(local_evaluation_state, ''), COALESCE(local_outcome, ''), COALESCE(local_action, ''),
	local_score, local_raw_score, local_audit_score, local_audit_raw_score, COALESCE(local_threshold, 0),
	COALESCE(local_mode, ''), COALESCE(local_policy_profile, ''), COALESCE(local_reason_code, ''), COALESCE(local_reason, ''),
	COALESCE(local_primary_origin, ''), COALESCE(local_strike_eligible, false), COALESCE(local_review_model, ''),
	COALESCE(local_review_flagged, false), COALESCE(local_review_error, ''), COALESCE(local_matched_patterns, '[]'),
	COALESCE(prompt_fingerprint, ''), COALESCE(prompt_preview, ''), COALESCE(prompt_text, ''), COALESCE(prompt_available, false), COALESCE(local_comparison, ''),
	COALESCE(candidate_id, 0), COALESCE(candidate_evidence_id, 0) FROM prompt_policy_incidents`

func (db *DB) ensurePromptPolicyIncidentsTable(ctx context.Context) error {
	if db == nil {
		return errors.New("database is nil")
	}
	if db.isSQLite() {
		if _, err := db.conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS prompt_policy_incidents (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			incident_id TEXT NOT NULL UNIQUE,
			request_correlation_id TEXT DEFAULT '',
			created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
			attempt_index INTEGER DEFAULT 0,
			transport TEXT DEFAULT '', endpoint TEXT DEFAULT '', request_protocol TEXT DEFAULT '', request_provider TEXT DEFAULT '', model TEXT DEFAULT '',
			status_code INTEGER DEFAULT 0, account_id INTEGER DEFAULT 0, account_name TEXT DEFAULT '', account_platform TEXT DEFAULT '', account_group_ids TEXT DEFAULT '[]', account_group_names TEXT DEFAULT '[]',
			api_key_id INTEGER DEFAULT 0, api_key_name TEXT DEFAULT '', api_key_masked TEXT DEFAULT '', api_key_allowed_group_ids TEXT DEFAULT '[]', api_key_allowed_group_names TEXT DEFAULT '[]', platform TEXT DEFAULT '',
			newapi_policy_status TEXT DEFAULT '', newapi_platform TEXT DEFAULT '', newapi_user_id TEXT DEFAULT '', newapi_request_id TEXT DEFAULT '', session_hash TEXT DEFAULT '', client_ip_hash TEXT DEFAULT '', source_ref TEXT DEFAULT '',
			upstream_error_code TEXT DEFAULT '', upstream_error TEXT DEFAULT '',
			local_evaluation_state TEXT DEFAULT '', local_outcome TEXT DEFAULT '', local_action TEXT DEFAULT '',
			local_score INTEGER NULL, local_raw_score INTEGER NULL, local_audit_score INTEGER NULL, local_audit_raw_score INTEGER NULL,
			local_threshold INTEGER DEFAULT 0, local_mode TEXT DEFAULT '', local_policy_profile TEXT DEFAULT '', local_reason_code TEXT DEFAULT '', local_reason TEXT DEFAULT '', local_primary_origin TEXT DEFAULT '', local_strike_eligible INTEGER DEFAULT 0,
			local_review_model TEXT DEFAULT '', local_review_flagged INTEGER DEFAULT 0, local_review_error TEXT DEFAULT '', local_matched_patterns TEXT DEFAULT '[]',
			prompt_fingerprint TEXT DEFAULT '', prompt_preview TEXT DEFAULT '', prompt_text TEXT DEFAULT '', prompt_available INTEGER DEFAULT 0, local_comparison TEXT DEFAULT '', candidate_id INTEGER DEFAULT 0, candidate_evidence_id INTEGER DEFAULT 0
		)`); err != nil {
			return err
		}
		for _, column := range []struct{ table, name, def string }{
			{"usage_logs", "prompt_policy_incident_id", "TEXT NULL"},
			{"prompt_rule_candidate_evidence", "prompt_policy_incident_id", "TEXT NULL"},
			{"prompt_filter_logs", "request_correlation_id", "TEXT DEFAULT ''"},
			{"prompt_filter_logs", "newapi_policy_status", "TEXT DEFAULT ''"},
			{"prompt_filter_logs", "newapi_platform", "TEXT DEFAULT ''"},
			{"prompt_filter_logs", "newapi_user_id", "TEXT DEFAULT ''"},
			{"prompt_filter_logs", "newapi_request_id", "TEXT DEFAULT ''"},
			{"prompt_filter_logs", "newapi_decision_id", "TEXT DEFAULT ''"},
			{"prompt_filter_logs", "session_hash", "TEXT DEFAULT ''"},
			{"prompt_policy_incidents", "local_reason", "TEXT DEFAULT ''"},
			{"prompt_policy_incidents", "account_name", "TEXT DEFAULT ''"},
			{"prompt_policy_incidents", "account_platform", "TEXT DEFAULT ''"},
			{"prompt_policy_incidents", "account_group_ids", "TEXT DEFAULT '[]'"},
			{"prompt_policy_incidents", "account_group_names", "TEXT DEFAULT '[]'"},
			{"prompt_policy_incidents", "api_key_allowed_group_ids", "TEXT DEFAULT '[]'"},
			{"prompt_policy_incidents", "api_key_allowed_group_names", "TEXT DEFAULT '[]'"},
			{"prompt_policy_incidents", "prompt_available", "INTEGER DEFAULT 0"},
			{"prompt_policy_incidents", "local_comparison", "TEXT DEFAULT ''"},
			{"prompt_policy_incidents", "newapi_policy_status", "TEXT DEFAULT ''"},
			{"prompt_policy_incidents", "newapi_platform", "TEXT DEFAULT ''"},
			{"prompt_policy_incidents", "newapi_user_id", "TEXT DEFAULT ''"},
			{"prompt_policy_incidents", "newapi_request_id", "TEXT DEFAULT ''"},
			{"prompt_policy_incidents", "session_hash", "TEXT DEFAULT ''"},
			{"prompt_policy_incidents", "client_ip_hash", "TEXT DEFAULT ''"},
		} {
			if err := db.ensureSQLiteColumn(ctx, column.table, column.name, column.def); err != nil {
				return err
			}
		}
	} else {
		if _, err := db.conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS prompt_policy_incidents (
			id BIGSERIAL PRIMARY KEY,
			incident_id VARCHAR(64) NOT NULL UNIQUE,
			request_correlation_id VARCHAR(64) DEFAULT '',
			created_at TIMESTAMPTZ DEFAULT NOW(),
			attempt_index INT DEFAULT 0,
			transport VARCHAR(32) DEFAULT '', endpoint VARCHAR(256) DEFAULT '', request_protocol VARCHAR(64) DEFAULT '', request_provider VARCHAR(64) DEFAULT '', model VARCHAR(100) DEFAULT '',
			status_code INT DEFAULT 0, account_id BIGINT DEFAULT 0, account_name VARCHAR(255) DEFAULT '', account_platform VARCHAR(100) DEFAULT '', account_group_ids TEXT DEFAULT '[]', account_group_names TEXT DEFAULT '[]',
			api_key_id BIGINT DEFAULT 0, api_key_name VARCHAR(255) DEFAULT '', api_key_masked VARCHAR(64) DEFAULT '', api_key_allowed_group_ids TEXT DEFAULT '[]', api_key_allowed_group_names TEXT DEFAULT '[]', platform VARCHAR(100) DEFAULT '',
			newapi_policy_status VARCHAR(32) DEFAULT '', newapi_platform VARCHAR(100) DEFAULT '', newapi_user_id VARCHAR(255) DEFAULT '', newapi_request_id VARCHAR(255) DEFAULT '', session_hash VARCHAR(64) DEFAULT '', client_ip_hash VARCHAR(64) DEFAULT '', source_ref TEXT DEFAULT '',
			upstream_error_code VARCHAR(100) DEFAULT '', upstream_error TEXT DEFAULT '',
			local_evaluation_state VARCHAR(32) DEFAULT '', local_outcome VARCHAR(32) DEFAULT '', local_action VARCHAR(32) DEFAULT '',
			local_score INT NULL, local_raw_score INT NULL, local_audit_score INT NULL, local_audit_raw_score INT NULL,
			local_threshold INT DEFAULT 0, local_mode VARCHAR(32) DEFAULT '', local_policy_profile VARCHAR(32) DEFAULT '', local_reason_code VARCHAR(100) DEFAULT '', local_reason TEXT DEFAULT '', local_primary_origin VARCHAR(64) DEFAULT '', local_strike_eligible BOOLEAN DEFAULT FALSE,
			local_review_model VARCHAR(100) DEFAULT '', local_review_flagged BOOLEAN DEFAULT FALSE, local_review_error TEXT DEFAULT '', local_matched_patterns TEXT DEFAULT '[]',
			prompt_fingerprint VARCHAR(64) DEFAULT '', prompt_preview TEXT DEFAULT '', prompt_text TEXT DEFAULT '', prompt_available BOOLEAN DEFAULT FALSE, local_comparison VARCHAR(32) DEFAULT '', candidate_id BIGINT DEFAULT 0, candidate_evidence_id BIGINT DEFAULT 0
		);
		ALTER TABLE usage_logs ADD COLUMN IF NOT EXISTS prompt_policy_incident_id VARCHAR(64) NULL;
		ALTER TABLE prompt_rule_candidate_evidence ADD COLUMN IF NOT EXISTS prompt_policy_incident_id VARCHAR(64) NULL;
		ALTER TABLE prompt_filter_logs ADD COLUMN IF NOT EXISTS request_correlation_id VARCHAR(64) DEFAULT '';
		ALTER TABLE prompt_filter_logs ADD COLUMN IF NOT EXISTS newapi_policy_status VARCHAR(32) DEFAULT '';
		ALTER TABLE prompt_filter_logs ADD COLUMN IF NOT EXISTS newapi_platform VARCHAR(100) DEFAULT '';
		ALTER TABLE prompt_filter_logs ADD COLUMN IF NOT EXISTS newapi_user_id VARCHAR(255) DEFAULT '';
		ALTER TABLE prompt_filter_logs ADD COLUMN IF NOT EXISTS newapi_request_id VARCHAR(255) DEFAULT '';
		ALTER TABLE prompt_filter_logs ADD COLUMN IF NOT EXISTS newapi_decision_id VARCHAR(64) DEFAULT '';
		ALTER TABLE prompt_filter_logs ADD COLUMN IF NOT EXISTS session_hash VARCHAR(64) DEFAULT '';
		ALTER TABLE prompt_policy_incidents ADD COLUMN IF NOT EXISTS local_reason TEXT DEFAULT '';
		ALTER TABLE prompt_policy_incidents ADD COLUMN IF NOT EXISTS account_name VARCHAR(255) DEFAULT '';
		ALTER TABLE prompt_policy_incidents ADD COLUMN IF NOT EXISTS account_platform VARCHAR(100) DEFAULT '';
		ALTER TABLE prompt_policy_incidents ADD COLUMN IF NOT EXISTS account_group_ids TEXT DEFAULT '[]';
		ALTER TABLE prompt_policy_incidents ADD COLUMN IF NOT EXISTS account_group_names TEXT DEFAULT '[]';
		ALTER TABLE prompt_policy_incidents ADD COLUMN IF NOT EXISTS api_key_allowed_group_ids TEXT DEFAULT '[]';
		ALTER TABLE prompt_policy_incidents ADD COLUMN IF NOT EXISTS api_key_allowed_group_names TEXT DEFAULT '[]';
		ALTER TABLE prompt_policy_incidents ADD COLUMN IF NOT EXISTS prompt_available BOOLEAN DEFAULT FALSE;
		ALTER TABLE prompt_policy_incidents ADD COLUMN IF NOT EXISTS local_comparison VARCHAR(32) DEFAULT '';
		ALTER TABLE prompt_policy_incidents ADD COLUMN IF NOT EXISTS newapi_policy_status VARCHAR(32) DEFAULT '';
		ALTER TABLE prompt_policy_incidents ADD COLUMN IF NOT EXISTS newapi_platform VARCHAR(100) DEFAULT '';
		ALTER TABLE prompt_policy_incidents ADD COLUMN IF NOT EXISTS newapi_user_id VARCHAR(255) DEFAULT '';
		ALTER TABLE prompt_policy_incidents ADD COLUMN IF NOT EXISTS newapi_request_id VARCHAR(255) DEFAULT '';
		ALTER TABLE prompt_policy_incidents ADD COLUMN IF NOT EXISTS session_hash VARCHAR(64) DEFAULT '';
		ALTER TABLE prompt_policy_incidents ADD COLUMN IF NOT EXISTS client_ip_hash VARCHAR(64) DEFAULT '';
		`); err != nil {
			return err
		}
	}
	if _, err := db.conn.ExecContext(ctx, `UPDATE prompt_policy_incidents SET prompt_available = CASE WHEN COALESCE(prompt_text, '') <> '' OR COALESCE(prompt_preview, '') <> '' THEN true ELSE false END WHERE COALESCE(local_comparison, '') = ''`); err != nil {
		return err
	}
	if _, err := db.conn.ExecContext(ctx, `UPDATE prompt_policy_incidents SET local_comparison = CASE
		WHEN local_evaluation_state = 'legacy_unknown' THEN 'legacy_unknown'
		WHEN local_evaluation_state <> 'completed' THEN 'not_comparable'
		WHEN local_outcome <> 'no_hit' THEN 'local_detected'
		WHEN prompt_available THEN 'upstream_only'
		ELSE 'evidence_unavailable' END WHERE COALESCE(local_comparison, '') = ''`); err != nil {
		return err
	}
	for _, stmt := range []string{
		`CREATE INDEX IF NOT EXISTS idx_prompt_policy_incidents_request ON prompt_policy_incidents(request_correlation_id, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_prompt_policy_incidents_created ON prompt_policy_incidents(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_prompt_policy_incidents_api_key ON prompt_policy_incidents(api_key_id, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_prompt_policy_incidents_account ON prompt_policy_incidents(account_id, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_prompt_policy_incidents_endpoint ON prompt_policy_incidents(endpoint, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_prompt_policy_incidents_outcome ON prompt_policy_incidents(local_outcome, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_prompt_policy_incidents_comparison ON prompt_policy_incidents(local_comparison, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_usage_logs_policy_incident ON usage_logs(prompt_policy_incident_id)`,
		`CREATE INDEX IF NOT EXISTS idx_prompt_rule_evidence_incident ON prompt_rule_candidate_evidence(prompt_policy_incident_id)`,
	} {
		if _, err := db.conn.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	if err := db.migrateLegacyPromptPolicyIncidents(ctx); err != nil {
		return err
	}
	return db.ensurePromptRiskEventsTable(ctx)
}

func (db *DB) migrateLegacyPromptPolicyIncidents(ctx context.Context) error {
	query := `INSERT INTO prompt_policy_incidents (
		incident_id, created_at, endpoint, request_protocol, request_provider, model, api_key_id, api_key_name, api_key_masked,
		upstream_error_code, upstream_error, local_evaluation_state, local_matched_patterns
	) SELECT 'legacy-' || CAST(id AS TEXT), created_at, endpoint, request_protocol, request_provider, model, api_key_id, api_key_name, api_key_masked,
		error_code, full_text, $1, '[]' FROM prompt_filter_logs WHERE source='upstream_cyber_policy'
	ON CONFLICT(incident_id) DO NOTHING`
	_, err := db.conn.ExecContext(ctx, query, PromptPolicyEvaluationLegacyUnknown)
	return err
}

func normalizePromptPolicyIncidentInput(input PromptPolicyIncidentInput) (PromptPolicyIncidentInput, error) {
	input.IncidentID = strings.TrimSpace(input.IncidentID)
	if input.IncidentID == "" {
		return input, errors.New("incident id is required")
	}
	input.RequestCorrelationID = truncateCandidateRunes(strings.TrimSpace(input.RequestCorrelationID), 64)
	input.Transport = truncateCandidateRunes(strings.TrimSpace(input.Transport), 32)
	input.Endpoint = truncateCandidateRunes(strings.TrimSpace(input.Endpoint), 256)
	input.Protocol = truncateCandidateRunes(strings.TrimSpace(input.Protocol), 64)
	input.Provider = truncateCandidateRunes(strings.TrimSpace(input.Provider), 64)
	input.Model = truncateCandidateRunes(strings.TrimSpace(input.Model), 100)
	input.AccountName = truncateCandidateRunes(strings.TrimSpace(input.AccountName), 255)
	input.AccountPlatform = truncateCandidateRunes(strings.TrimSpace(input.AccountPlatform), 100)
	input.APIKeyName = truncateCandidateRunes(strings.TrimSpace(input.APIKeyName), 255)
	input.APIKeyMasked = truncateCandidateRunes(strings.TrimSpace(input.APIKeyMasked), 64)
	input.Platform = truncateCandidateRunes(strings.TrimSpace(input.Platform), 100)
	input.NewAPIPolicyStatus = truncateCandidateRunes(strings.TrimSpace(input.NewAPIPolicyStatus), 32)
	input.NewAPIPlatform = truncateCandidateRunes(strings.TrimSpace(input.NewAPIPlatform), 100)
	input.NewAPIUserID = truncateCandidateRunes(strings.TrimSpace(input.NewAPIUserID), 255)
	input.NewAPIUserName = truncateCandidateRunes(strings.TrimSpace(input.NewAPIUserName), 128)
	input.NewAPIUserEmail = truncateCandidateRunes(strings.TrimSpace(input.NewAPIUserEmail), 320)
	input.NewAPIUserGroup = truncateCandidateRunes(strings.TrimSpace(input.NewAPIUserGroup), 100)
	input.NewAPIRequestID = truncateCandidateRunes(strings.TrimSpace(input.NewAPIRequestID), 255)
	input.SessionHash = truncateCandidateRunes(strings.TrimSpace(input.SessionHash), 64)
	input.ClientIPHash = truncateCandidateRunes(strings.TrimSpace(input.ClientIPHash), 64)
	input.SourceRef = truncateCandidateRunes(strings.TrimSpace(input.SourceRef), 2000)
	input.UpstreamErrorCode = truncateCandidateRunes(strings.TrimSpace(input.UpstreamErrorCode), 100)
	input.UpstreamError = truncateCandidateRunes(strings.TrimSpace(input.UpstreamError), 8192)
	input.LocalEvaluationState = truncateCandidateRunes(strings.TrimSpace(input.LocalEvaluationState), 32)
	input.LocalOutcome = truncateCandidateRunes(strings.TrimSpace(input.LocalOutcome), 32)
	input.LocalAction = truncateCandidateRunes(strings.TrimSpace(input.LocalAction), 32)
	input.LocalMode = truncateCandidateRunes(strings.TrimSpace(input.LocalMode), 32)
	input.LocalPolicyProfile = truncateCandidateRunes(strings.TrimSpace(input.LocalPolicyProfile), 32)
	input.LocalReasonCode = truncateCandidateRunes(strings.TrimSpace(input.LocalReasonCode), 100)
	input.LocalReason = truncateCandidateRunes(strings.TrimSpace(input.LocalReason), 2000)
	input.LocalPrimaryOrigin = truncateCandidateRunes(strings.TrimSpace(input.LocalPrimaryOrigin), 64)
	input.LocalReviewModel = truncateCandidateRunes(strings.TrimSpace(input.LocalReviewModel), 100)
	input.LocalReviewError = truncateCandidateRunes(strings.TrimSpace(input.LocalReviewError), 2000)
	input.LocalMatchedPatterns = truncateCandidateRunes(strings.TrimSpace(input.LocalMatchedPatterns), 16000)
	if input.LocalMatchedPatterns == "" {
		input.LocalMatchedPatterns = "[]"
	}
	input.PromptFingerprint = truncateCandidateRunes(strings.TrimSpace(input.PromptFingerprint), 64)
	input.PromptPreview = truncateCandidateRunes(strings.TrimSpace(input.PromptPreview), 2000)
	input.PromptText = truncateCandidateRunes(strings.TrimSpace(input.PromptText), 32000)
	if !input.PromptAvailable && (input.PromptPreview != "" || input.PromptText != "") {
		input.PromptAvailable = true
	}
	if strings.TrimSpace(input.LocalComparison) == "" {
		input.LocalComparison = derivePromptPolicyComparison(input.LocalEvaluationState, input.LocalOutcome, input.PromptAvailable)
	}
	input.LocalComparison = truncateCandidateRunes(strings.TrimSpace(input.LocalComparison), 32)
	if input.ObservedAt.IsZero() {
		input.ObservedAt = time.Now().UTC()
	}
	return input, nil
}

func derivePromptPolicyComparison(state, outcome string, promptAvailable bool) string {
	if state == PromptPolicyEvaluationLegacyUnknown {
		return PromptPolicyComparisonLegacyUnknown
	}
	if state != PromptPolicyEvaluationCompleted {
		return PromptPolicyComparisonNotComparable
	}
	if outcome != PromptPolicyOutcomeNoHit {
		return PromptPolicyComparisonLocalDetected
	}
	if !promptAvailable {
		return PromptPolicyComparisonEvidenceUnavailable
	}
	return PromptPolicyComparisonUpstreamOnly
}

func encodePromptPolicyStringSlice(values []string) string {
	if len(values) == 0 {
		return "[]"
	}
	payload, err := json.Marshal(values)
	if err != nil {
		return "[]"
	}
	return string(payload)
}

func decodePromptPolicyStringSlice(raw string) []string {
	var values []string
	if json.Unmarshal([]byte(strings.TrimSpace(raw)), &values) != nil || values == nil {
		return []string{}
	}
	return values
}

type promptPolicyShadowEvidence struct {
	AuditScore      int
	ReasonCode      string
	PrimaryOrigin   string
	MatchedPatterns string
}

func promptPolicyHasMatchedEvidence(raw string) bool {
	var matches []json.RawMessage
	return json.Unmarshal([]byte(strings.TrimSpace(raw)), &matches) == nil && len(matches) > 0
}

func loadPromptPolicyShadowEvidenceTx(ctx context.Context, tx *sql.Tx, correlationID string) (*promptPolicyShadowEvidence, error) {
	correlationID = strings.TrimSpace(correlationID)
	if correlationID == "" {
		return nil, nil
	}
	evidence := &promptPolicyShadowEvidence{}
	err := tx.QueryRowContext(ctx, `SELECT COALESCE(audit_score,0), COALESCE(reason_code,''), COALESCE(primary_origin,''), COALESCE(matched_patterns,'[]')
		FROM prompt_filter_logs WHERE request_correlation_id=$1 AND reason_code='prompt_policy_shadow_async'
		ORDER BY id DESC LIMIT 1`, correlationID).Scan(&evidence.AuditScore, &evidence.ReasonCode, &evidence.PrimaryOrigin, &evidence.MatchedPatterns)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !promptPolicyHasMatchedEvidence(evidence.MatchedPatterns) {
		return nil, nil
	}
	return evidence, nil
}

func mergePromptPolicyShadowEvidence(incident *PromptPolicyIncidentInput, evidence *promptPolicyShadowEvidence) {
	if incident == nil || evidence == nil {
		return
	}
	incident.LocalComparison = PromptPolicyComparisonLocalDetected
	if incident.LocalEvaluationState == PromptPolicyEvaluationCompleted && incident.LocalOutcome == PromptPolicyOutcomeNoHit {
		incident.LocalOutcome = PromptPolicyOutcomeAuditHit
	}
	if incident.LocalAuditScore == nil || evidence.AuditScore > *incident.LocalAuditScore {
		score := evidence.AuditScore
		incident.LocalAuditScore = &score
	}
	incident.LocalMatchedPatterns = evidence.MatchedPatterns
	if strings.TrimSpace(evidence.PrimaryOrigin) != "" {
		incident.LocalPrimaryOrigin = evidence.PrimaryOrigin
	}
	if strings.TrimSpace(evidence.ReasonCode) != "" {
		incident.LocalReasonCode = evidence.ReasonCode
	}
}

func reconcileStoredPromptPolicyIncidentFromShadowTx(ctx context.Context, tx *sql.Tx, input *PromptFilterLogInput) error {
	if input == nil || strings.TrimSpace(input.RequestCorrelationID) == "" || input.ReasonCode != "prompt_policy_shadow_async" || !promptPolicyHasMatchedEvidence(input.MatchedPatterns) {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE prompt_policy_incidents SET
		local_comparison=$1,
		local_outcome=CASE WHEN local_evaluation_state=$2 AND local_outcome=$3 THEN $4 ELSE local_outcome END,
		local_audit_score=CASE WHEN local_audit_score IS NULL OR local_audit_score < $5 THEN $5 ELSE local_audit_score END,
		local_reason_code=$6,
		local_primary_origin=CASE WHEN $7<>'' THEN $7 ELSE local_primary_origin END,
		local_matched_patterns=$8
		WHERE request_correlation_id=$9 AND upstream_error_code='cyber_policy'`,
		PromptPolicyComparisonLocalDetected, PromptPolicyEvaluationCompleted, PromptPolicyOutcomeNoHit, PromptPolicyOutcomeAuditHit,
		input.AuditScore, input.ReasonCode, input.PrimaryOrigin, input.MatchedPatterns, strings.TrimSpace(input.RequestCorrelationID)); err != nil {
		return err
	}
	rows, err := tx.QueryContext(ctx, `SELECT evidence.id, evidence.metadata_json
		FROM prompt_rule_candidate_evidence evidence
		JOIN prompt_policy_incidents incident ON incident.incident_id=evidence.prompt_policy_incident_id
		WHERE incident.request_correlation_id=$1 AND incident.upstream_error_code='cyber_policy'
		  AND evidence.source_kind=$2`, strings.TrimSpace(input.RequestCorrelationID), PromptRuleCandidateSourceUpstreamCyberPolicy)
	if err != nil {
		return err
	}
	type evidenceMetadataUpdate struct {
		id       int64
		metadata string
	}
	updates := make([]evidenceMetadataUpdate, 0, 2)
	for rows.Next() {
		var id int64
		var raw string
		if scanErr := rows.Scan(&id, &raw); scanErr != nil {
			rows.Close()
			return scanErr
		}
		updated, updateErr := mergePromptPolicyCandidateEvidenceMetadata(raw, PromptPolicyOutcomeAuditHit, PromptPolicyComparisonLocalDetected,
			input.AuditScore, input.ReasonCode, input.PrimaryOrigin, input.MatchedPatterns)
		if updateErr != nil {
			rows.Close()
			return updateErr
		}
		updates = append(updates, evidenceMetadataUpdate{id: id, metadata: updated})
	}
	if err = rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, update := range updates {
		if _, err = tx.ExecContext(ctx, `UPDATE prompt_rule_candidate_evidence SET metadata_json=$1 WHERE id=$2`, update.metadata, update.id); err != nil {
			return err
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE prompt_risk_events SET
		event_kind='upstream_cy_local_detected', request_risk_score=28, evidence_confidence=85,
		local_outcome=$1, local_comparison=$2, reason_code=$3
		WHERE source_type=$4 AND source_id IN (
			SELECT incident_id FROM prompt_policy_incidents WHERE request_correlation_id=$5 AND upstream_error_code='cyber_policy'
		)`, PromptPolicyOutcomeAuditHit, PromptPolicyComparisonLocalDetected, input.ReasonCode, promptRiskSourceIncident, strings.TrimSpace(input.RequestCorrelationID))
	return err
}

func mergePromptPolicyCandidateEvidenceMetadata(raw, outcome, comparison string, auditScore int, reasonCode, primaryOrigin, matchedPatterns string) (string, error) {
	metadata := map[string]any{}
	if strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &metadata); err != nil {
			return "", err
		}
	}
	var matches any = []any{}
	if strings.TrimSpace(matchedPatterns) != "" {
		if err := json.Unmarshal([]byte(matchedPatterns), &matches); err != nil {
			return "", err
		}
	}
	metadata["local_outcome"] = outcome
	metadata["local_comparison"] = comparison
	metadata["local_audit_score"] = auditScore
	metadata["local_reason_code"] = reasonCode
	metadata["local_primary_origin"] = primaryOrigin
	metadata["local_matches"] = matches
	learning, _ := metadata["learning_evidence"].(map[string]any)
	if learning == nil {
		learning = map[string]any{"version": 1}
	}
	learning["shadow_audit"] = map[string]any{
		"audit_score": auditScore, "reason_code": reasonCode,
		"primary_origin": primaryOrigin, "matches": matches,
	}
	metadata["learning_evidence"] = learning
	matchCount := 0
	if values, ok := matches.([]any); ok {
		matchCount = len(values)
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return "", err
	}
	if len(encoded) > 64*1024 {
		delete(metadata, "local_matches")
		metadata["local_matches_count"] = matchCount
		learning["shadow_audit"] = map[string]any{
			"audit_score": auditScore, "reason_code": reasonCode,
			"primary_origin": primaryOrigin, "match_count": matchCount,
		}
		encoded, err = json.Marshal(metadata)
		if err != nil {
			return "", err
		}
	}
	if len(encoded) > 64*1024 {
		// Old evidence may already sit at the portable metadata ceiling. Keep the
		// durable learning bundle and the reconciled decision, while dropping
		// redundant incident fields that remain available from the incident row.
		metadata = map[string]any{
			"evidence_quality":     metadata["evidence_quality"],
			"learning_evidence":    learning,
			"local_outcome":        outcome,
			"local_comparison":     comparison,
			"local_audit_score":    auditScore,
			"local_reason_code":    reasonCode,
			"local_primary_origin": primaryOrigin,
			"local_matches_count":  matchCount,
		}
		encoded, err = json.Marshal(metadata)
		if err != nil {
			return "", err
		}
	}
	// JSON 转义(<>& 与控制字符→\u00XX)可把学习包的原始字节预算膨胀至 6 倍,
	// 上面的降级无法保证收敛;继续按编码后体积收缩学习包正文,最终兜底丢弃
	// 学习包。对账与持久化永远不因体积失败——报错会回滚整个日志事务,连带
	// 丢掉安全遥测。
	for len(encoded) > 64*1024 {
		if text, ok := learning["prompt_text"].(string); ok && text != "" {
			if len(text) > 256 {
				learning["prompt_text"] = truncateEvidenceBytesRuneSafe(text, len(text)/2)
			} else {
				delete(learning, "prompt_text")
			}
		} else if _, ok := learning["context"]; ok {
			delete(learning, "context")
		} else if text, ok := learning["upstream_error"].(string); ok && text != "" {
			if len(text) > 256 {
				learning["upstream_error"] = truncateEvidenceBytesRuneSafe(text, len(text)/2)
			} else {
				delete(learning, "upstream_error")
			}
		} else if _, ok := metadata["learning_evidence"]; ok {
			delete(metadata, "learning_evidence")
		} else {
			return "", errors.New("reconciled candidate evidence metadata exceeds 64 KiB")
		}
		encoded, err = json.Marshal(metadata)
		if err != nil {
			return "", err
		}
	}
	return string(encoded), nil
}

func truncateEvidenceBytesRuneSafe(value string, maxBytes int) string {
	if maxBytes <= 0 {
		return ""
	}
	if len(value) <= maxBytes {
		return value
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return strings.TrimSpace(value[:cut])
}

func (db *DB) PersistPromptPolicyIncident(ctx context.Context, rawIncident PromptPolicyIncidentInput, rawCandidate PromptRuleCandidateInput, rawEvidence PromptRuleCandidateEvidenceInput) error {
	if db == nil {
		return errors.New("database is nil")
	}
	incident, err := normalizePromptPolicyIncidentInput(rawIncident)
	if err != nil {
		return err
	}
	candidate, err := normalizePromptRuleCandidateInput(rawCandidate)
	if err != nil {
		return err
	}
	evidence, err := normalizePromptRuleCandidateEvidenceInput(rawEvidence)
	if err != nil {
		return err
	}
	evidence.PromptPolicyIncidentID = incident.IncidentID
	return db.withSQLiteWriteLock(ctx, func() error {
		tx, beginErr := db.conn.BeginTx(ctx, nil)
		if beginErr != nil {
			return beginErr
		}
		defer tx.Rollback()
		if _, execErr := tx.ExecContext(ctx, `INSERT INTO prompt_policy_incidents (
			incident_id, request_correlation_id, created_at, attempt_index, transport, endpoint, request_protocol, request_provider, model,
			status_code, account_id, account_name, account_platform, account_group_ids, account_group_names,
			api_key_id, api_key_name, api_key_masked, api_key_allowed_group_ids, api_key_allowed_group_names, platform,
			newapi_policy_status, newapi_platform, newapi_user_id, newapi_request_id, session_hash, client_ip_hash, source_ref, upstream_error_code, upstream_error,
			local_evaluation_state, local_outcome, local_action, local_score, local_raw_score, local_audit_score, local_audit_raw_score,
			local_threshold, local_mode, local_policy_profile, local_reason_code, local_reason, local_primary_origin, local_strike_eligible,
			local_review_model, local_review_flagged, local_review_error, local_matched_patterns, prompt_fingerprint, prompt_preview, prompt_text, prompt_available, local_comparison
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32,$33,$34,$35,$36,$37,$38,$39,$40,$41,$42,$43,$44,$45,$46,$47,$48,$49,$50,$51,$52,$53)
		ON CONFLICT(incident_id) DO NOTHING`, incident.IncidentID, incident.RequestCorrelationID, incident.ObservedAt, incident.AttemptIndex,
			incident.Transport, incident.Endpoint, incident.Protocol, incident.Provider, incident.Model, incident.StatusCode, incident.AccountID,
			incident.AccountName, incident.AccountPlatform, encodeInt64SliceJSON(incident.AccountGroupIDs), encodePromptPolicyStringSlice(incident.AccountGroupNames),
			incident.APIKeyID, incident.APIKeyName, incident.APIKeyMasked, encodeInt64SliceJSON(incident.APIKeyAllowedGroupIDs), encodePromptPolicyStringSlice(incident.APIKeyAllowedGroupNames), incident.Platform,
			incident.NewAPIPolicyStatus, incident.NewAPIPlatform, incident.NewAPIUserID, incident.NewAPIRequestID, incident.SessionHash, incident.ClientIPHash, incident.SourceRef, incident.UpstreamErrorCode,
			incident.UpstreamError, incident.LocalEvaluationState, incident.LocalOutcome, incident.LocalAction, incident.LocalScore,
			incident.LocalRawScore, incident.LocalAuditScore, incident.LocalAuditRawScore, incident.LocalThreshold, incident.LocalMode,
			incident.LocalPolicyProfile, incident.LocalReasonCode, incident.LocalReason, incident.LocalPrimaryOrigin, incident.LocalStrikeEligible,
			incident.LocalReviewModel, incident.LocalReviewFlagged, incident.LocalReviewError, incident.LocalMatchedPatterns,
			incident.PromptFingerprint, incident.PromptPreview, incident.PromptText, incident.PromptAvailable, incident.LocalComparison); execErr != nil {
			return execErr
		}
		shadowEvidence, shadowErr := loadPromptPolicyShadowEvidenceTx(ctx, tx, incident.RequestCorrelationID)
		if shadowErr != nil {
			return shadowErr
		}
		if shadowEvidence != nil {
			mergePromptPolicyShadowEvidence(&incident, shadowEvidence)
			evidence.MetadataJSON, err = mergePromptPolicyCandidateEvidenceMetadata(evidence.MetadataJSON, incident.LocalOutcome,
				incident.LocalComparison, *incident.LocalAuditScore, incident.LocalReasonCode, incident.LocalPrimaryOrigin, incident.LocalMatchedPatterns)
			if err != nil {
				return err
			}
			if _, execErr := tx.ExecContext(ctx, `UPDATE prompt_policy_incidents SET
				local_comparison=$1, local_outcome=$2, local_audit_score=$3,
				local_reason_code=$4, local_primary_origin=$5, local_matched_patterns=$6
				WHERE incident_id=$7`, incident.LocalComparison, incident.LocalOutcome, incident.LocalAuditScore,
				incident.LocalReasonCode, incident.LocalPrimaryOrigin, incident.LocalMatchedPatterns,
				incident.IncidentID); execErr != nil {
				return execErr
			}
		}
		candidateID, evidenceID, _, stageErr := stagePromptRuleCandidateTx(ctx, tx, candidate, evidence)
		if stageErr != nil {
			return stageErr
		}
		if _, execErr := tx.ExecContext(ctx, `UPDATE prompt_policy_incidents SET candidate_id=$1, candidate_evidence_id=$2 WHERE incident_id=$3`, candidateID, evidenceID, incident.IncidentID); execErr != nil {
			return execErr
		}
		storedIncident := PromptPolicyIncident{
			IncidentID: incident.IncidentID, RequestCorrelationID: incident.RequestCorrelationID, CreatedAt: incident.ObservedAt,
			Endpoint: incident.Endpoint, Model: incident.Model, AccountID: incident.AccountID, AccountName: incident.AccountName,
			APIKeyID: incident.APIKeyID, APIKeyName: incident.APIKeyName, APIKeyMasked: incident.APIKeyMasked,
			NewAPIPolicyStatus: incident.NewAPIPolicyStatus, NewAPIPlatform: incident.NewAPIPlatform, NewAPIUserID: incident.NewAPIUserID,
			NewAPIRequestID: incident.NewAPIRequestID, SessionHash: incident.SessionHash, ClientIPHash: incident.ClientIPHash,
			LocalOutcome: incident.LocalOutcome, LocalAction: incident.LocalAction, LocalReasonCode: incident.LocalReasonCode,
			LocalComparison: incident.LocalComparison, PromptFingerprint: incident.PromptFingerprint, PromptPreview: incident.PromptPreview,
		}
		riskSignal := promptRiskSignalForIncident(storedIncident)
		riskSignal.NewAPIUserName = incident.NewAPIUserName
		riskSignal.NewAPIUserEmail = incident.NewAPIUserEmail
		riskSignal.NewAPIUserGroup = incident.NewAPIUserGroup
		if err := insertPromptRiskSignal(ctx, tx, riskSignal); err != nil {
			return err
		}
		return tx.Commit()
	})
}

func (db *DB) GetPromptPolicyIncident(ctx context.Context, incidentID string) (*PromptPolicyIncident, error) {
	row := db.conn.QueryRowContext(ctx, promptPolicyIncidentSelect+` WHERE incident_id=$1`, strings.TrimSpace(incidentID))
	return scanPromptPolicyIncident(row)
}

// DeletePromptPolicyIncident removes one CY incident from the operational
// history while retaining the candidate/evidence material used for learning.
// References in usage logs and candidate evidence are detached instead of
// leaving links to a deleted incident. Risk profiles are intentionally kept,
// matching the bulk-clear semantics used by the admin UI.
func (db *DB) DeletePromptPolicyIncident(ctx context.Context, incidentID string) error {
	if db == nil {
		return errors.New("database is nil")
	}
	incidentID = strings.TrimSpace(incidentID)
	if incidentID == "" {
		return errors.New("incident id is empty")
	}
	return db.withSQLiteWriteLock(ctx, func() error {
		tx, err := db.conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		var exists int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM prompt_policy_incidents WHERE incident_id=$1`, incidentID).Scan(&exists); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE usage_logs SET prompt_policy_incident_id=NULL WHERE prompt_policy_incident_id=$1`, incidentID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE prompt_rule_candidate_evidence SET prompt_policy_incident_id=NULL WHERE prompt_policy_incident_id=$1`, incidentID); err != nil {
			return err
		}
		// 管理员主动删除 CY 时，其关联的审核日志 / 风险事件 / 来源记录一并清理；
		// 保留策略平时会绕开这些行，只有这里才是它们的出口。
		if err := deletePromptIncidentEvidenceTx(ctx, tx, incidentID); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM prompt_policy_incidents WHERE incident_id=$1`, incidentID); err != nil {
			return err
		}
		return tx.Commit()
	})
}

func (db *DB) ListPromptPolicyIncidentsPage(ctx context.Context, query PromptPolicyIncidentQuery) ([]*PromptPolicyIncident, int, error) {
	if query.Page <= 0 {
		query.Page = 1
	}
	if query.PageSize <= 0 || query.PageSize > 500 {
		query.PageSize = 20
	}
	where, args := promptPolicyIncidentWhere(query)
	var total int
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM prompt_policy_incidents`+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, query.PageSize, (query.Page-1)*query.PageSize)
	rows, err := db.conn.QueryContext(ctx, promptPolicyIncidentSelect+where+` ORDER BY id DESC LIMIT $`+fmt.Sprint(len(args)-1)+` OFFSET $`+fmt.Sprint(len(args)), args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	items := make([]*PromptPolicyIncident, 0)
	for rows.Next() {
		item, scanErr := scanPromptPolicyIncident(rows)
		if scanErr != nil {
			return nil, 0, scanErr
		}
		items = append(items, item)
	}
	return items, total, rows.Err()
}

func promptPolicyIncidentWhere(query PromptPolicyIncidentQuery) (string, []any) {
	clauses := make([]string, 0, 8)
	args := make([]any, 0, 8)
	addExact := func(column, value string) {
		if value = strings.TrimSpace(value); value != "" {
			args = append(args, value)
			clauses = append(clauses, fmt.Sprintf("%s=$%d", column, len(args)))
		}
	}
	addExact("endpoint", query.Endpoint)
	addExact("model", query.Model)
	addExact("local_evaluation_state", query.EvaluationState)
	addExact("local_outcome", query.Outcome)
	addExact("local_comparison", query.LocalComparison)
	if query.APIKeyID > 0 {
		args = append(args, query.APIKeyID)
		clauses = append(clauses, fmt.Sprintf("api_key_id=$%d", len(args)))
	}
	if query.AccountID > 0 {
		args = append(args, query.AccountID)
		clauses = append(clauses, fmt.Sprintf("account_id=$%d", len(args)))
	}
	if query.LocalMiss != nil {
		if *query.LocalMiss {
			clauses = append(clauses, "local_comparison='confirmed_miss'")
		} else {
			clauses = append(clauses, "local_comparison<>'confirmed_miss'")
		}
	}
	if q := strings.TrimSpace(query.Query); q != "" {
		args = append(args, "%"+strings.ToLower(q)+"%")
		i := len(args)
		clauses = append(clauses, fmt.Sprintf(`(LOWER(COALESCE(incident_id,'')) LIKE $%d OR LOWER(COALESCE(request_correlation_id,'')) LIKE $%d OR LOWER(COALESCE(prompt_preview,'')) LIKE $%d OR LOWER(COALESCE(prompt_text,'')) LIKE $%d OR LOWER(COALESCE(local_matched_patterns,'')) LIKE $%d OR LOWER(COALESCE(upstream_error,'')) LIKE $%d OR LOWER(COALESCE(api_key_name,'')) LIKE $%d OR LOWER(COALESCE(account_name,'')) LIKE $%d OR LOWER(COALESCE(account_group_names,'')) LIKE $%d)`, i, i, i, i, i, i, i, i, i))
	}
	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

type promptPolicyIncidentScanner interface{ Scan(dest ...any) error }

func scanPromptPolicyIncident(scanner promptPolicyIncidentScanner) (*PromptPolicyIncident, error) {
	item := &PromptPolicyIncident{}
	var createdAtRaw any
	var accountGroupIDsRaw any
	var accountGroupNamesRaw, apiKeyAllowedGroupNamesRaw string
	var apiKeyAllowedGroupIDsRaw any
	var score, rawScore, auditScore, auditRawScore sql.NullInt64
	if err := scanner.Scan(&item.ID, &item.IncidentID, &item.RequestCorrelationID, &createdAtRaw, &item.AttemptIndex,
		&item.Transport, &item.Endpoint, &item.Protocol, &item.Provider, &item.Model, &item.StatusCode, &item.AccountID,
		&item.AccountName, &item.AccountPlatform, &accountGroupIDsRaw, &accountGroupNamesRaw,
		&item.APIKeyID, &item.APIKeyName, &item.APIKeyMasked, &apiKeyAllowedGroupIDsRaw, &apiKeyAllowedGroupNamesRaw, &item.Platform,
		&item.NewAPIPolicyStatus, &item.NewAPIPlatform, &item.NewAPIUserID, &item.NewAPIRequestID, &item.SessionHash, &item.ClientIPHash, &item.SourceRef, &item.UpstreamErrorCode,
		&item.UpstreamError, &item.LocalEvaluationState, &item.LocalOutcome, &item.LocalAction, &score, &rawScore, &auditScore,
		&auditRawScore, &item.LocalThreshold, &item.LocalMode, &item.LocalPolicyProfile, &item.LocalReasonCode,
		&item.LocalReason, &item.LocalPrimaryOrigin, &item.LocalStrikeEligible, &item.LocalReviewModel, &item.LocalReviewFlagged,
		&item.LocalReviewError, &item.LocalMatchedPatterns, &item.PromptFingerprint, &item.PromptPreview, &item.PromptText, &item.PromptAvailable, &item.LocalComparison,
		&item.CandidateID, &item.CandidateEvidenceID); err != nil {
		return nil, err
	}
	createdAt, err := parseDBTimeValue(createdAtRaw)
	if err != nil {
		return nil, err
	}
	item.CreatedAt = createdAt
	item.AccountGroupIDs = decodeInt64SliceValue(accountGroupIDsRaw)
	item.AccountGroupNames = decodePromptPolicyStringSlice(accountGroupNamesRaw)
	item.APIKeyAllowedGroupIDs = decodeInt64SliceValue(apiKeyAllowedGroupIDsRaw)
	item.APIKeyAllowedGroupNames = decodePromptPolicyStringSlice(apiKeyAllowedGroupNamesRaw)
	if score.Valid {
		v := int(score.Int64)
		item.LocalScore = &v
	}
	if rawScore.Valid {
		v := int(rawScore.Int64)
		item.LocalRawScore = &v
	}
	if auditScore.Valid {
		v := int(auditScore.Int64)
		item.LocalAuditScore = &v
	}
	if auditRawScore.Valid {
		v := int(auditRawScore.Int64)
		item.LocalAuditRawScore = &v
	}
	if item.LocalComparison == "" {
		item.LocalComparison = derivePromptPolicyComparison(item.LocalEvaluationState, item.LocalOutcome, item.PromptAvailable)
	}
	item.LocalMiss = item.LocalComparison == PromptPolicyComparisonConfirmedMiss
	return item, nil
}

func (db *DB) ClearPromptPolicyIncidents(ctx context.Context) error {
	if db == nil {
		return nil
	}
	return db.withSQLiteWriteLock(ctx, func() error {
		tx, err := db.conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		// 清空 CY 同时清空其全部证据链（日志 / 风险事件 / 来源记录）。
		if err := deleteAllPromptIncidentEvidenceTx(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE usage_logs SET prompt_policy_incident_id=NULL WHERE prompt_policy_incident_id IS NOT NULL`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE prompt_rule_candidate_evidence SET prompt_policy_incident_id=NULL WHERE prompt_policy_incident_id IS NOT NULL`); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM prompt_policy_incidents`); err != nil {
			return err
		}
		if db.isSQLite() {
			if _, err := tx.ExecContext(ctx, `DELETE FROM sqlite_sequence WHERE name='prompt_policy_incidents'`); err != nil {
				return err
			}
		}
		return tx.Commit()
	})
}
