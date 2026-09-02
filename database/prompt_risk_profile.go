package database

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

const (
	PromptRiskScoringVersion = "prompt-risk-v2"

	PromptRiskSubjectNewAPIUser      = "newapi_user"
	PromptRiskSubjectSession         = "session"
	PromptRiskSubjectAPIKey          = "api_key"
	PromptRiskSubjectClientIP        = "client_ip"
	PromptRiskSubjectUpstreamAccount = "upstream_account"

	PromptRiskLevelLow      = "low"
	PromptRiskLevelObserved = "observed"
	PromptRiskLevelElevated = "elevated"
	PromptRiskLevelHigh     = "high"
	PromptRiskLevelCritical = "critical"

	promptRiskSourceLog      = "prompt_filter_log"
	promptRiskSourceIncident = "prompt_policy_incident"

	promptRiskEventReviewCleared        = "review_cleared"
	promptRiskEventLocalBlock           = "local_block"
	promptRiskEventLocalBlockUnverified = "local_block_unverified"
	promptRiskEventLocalBlockCleared    = "local_block_cleared"
)

type PromptRiskProfile struct {
	SubjectType          string                   `json:"subject_type"`
	SubjectKey           string                   `json:"subject_key"`
	SubjectDisplay       string                   `json:"subject_display"`
	Platform             string                   `json:"platform,omitempty"`
	NewAPIUserID         string                   `json:"newapi_user_id,omitempty"`
	NewAPIUserName       string                   `json:"newapi_user_name,omitempty"`
	NewAPIUserEmail      string                   `json:"newapi_user_email,omitempty"`
	NewAPIUserGroup      string                   `json:"newapi_user_group,omitempty"`
	IsPerson             bool                     `json:"is_person"`
	IdentityConfidence   int                      `json:"identity_confidence"`
	RiskScore            int                      `json:"risk_score"`
	RiskLevel            string                   `json:"risk_level"`
	RecommendedActions   []string                 `json:"recommended_actions"`
	ScoreBreakdown       PromptRiskScoreBreakdown `json:"score_breakdown"`
	HasActivity          bool                     `json:"has_activity"`
	IdentitySource       string                   `json:"identity_source,omitempty"`
	IdentityUpdatedAt    *time.Time               `json:"identity_updated_at,omitempty"`
	LatestAt             time.Time                `json:"latest_at"`
	EventCount           int                      `json:"event_count"`
	Events10m            int                      `json:"events_10m"`
	Events24h            int                      `json:"events_24h"`
	Events7d             int                      `json:"events_7d"`
	Events30d            int                      `json:"events_30d"`
	UpstreamCYCount      int                      `json:"upstream_cy_count"`
	ConfirmedMissCount   int                      `json:"confirmed_miss_count"`
	LocalBlockCount      int                      `json:"local_block_count"`
	LocalWarnCount       int                      `json:"local_warn_count"`
	DistinctFingerprints int                      `json:"distinct_fingerprints"`
	RepeatedFingerprints int                      `json:"repeated_fingerprints"`
	APIKeyID             int64                    `json:"api_key_id,omitempty"`
	APIKeyName           string                   `json:"api_key_name,omitempty"`
	APIKeyMasked         string                   `json:"api_key_masked,omitempty"`
	AccountID            int64                    `json:"account_id,omitempty"`
	AccountName          string                   `json:"account_name,omitempty"`
	AccountEmail         string                   `json:"account_email,omitempty"`
	SessionWindows24h    int                      `json:"session_windows_24h,omitempty"`
	SessionUniqueUsers   int                      `json:"session_unique_users,omitempty"`
	SessionWindowsTotal  int                      `json:"session_windows_total,omitempty"`
	TrustPolicy          *PromptRiskTrustPolicy   `json:"trust_policy,omitempty"`
	ConversationLock     *PromptConversationLock  `json:"conversation_lock,omitempty"`
}

type PromptRiskScoreBreakdown struct {
	LocalSignal        int `json:"local_signal"`
	UpstreamSignal     int `json:"upstream_signal"`
	Recurrence         int `json:"recurrence"`
	IdentityConfidence int `json:"identity_confidence"`
}

type PromptRiskEvent struct {
	ID                   int64     `json:"id"`
	CreatedAt            time.Time `json:"created_at"`
	SourceType           string    `json:"source_type"`
	SourceID             string    `json:"source_id"`
	IncidentID           string    `json:"incident_id,omitempty"`
	PromptFilterLogID    int64     `json:"prompt_filter_log_id,omitempty"`
	RequestCorrelationID string    `json:"request_correlation_id,omitempty"`
	SubjectType          string    `json:"subject_type"`
	SubjectKey           string    `json:"subject_key"`
	SubjectDisplay       string    `json:"subject_display"`
	Platform             string    `json:"platform,omitempty"`
	NewAPIUserID         string    `json:"newapi_user_id,omitempty"`
	NewAPIUserName       string    `json:"newapi_user_name,omitempty"`
	NewAPIUserEmail      string    `json:"newapi_user_email,omitempty"`
	NewAPIUserGroup      string    `json:"newapi_user_group,omitempty"`
	IsPerson             bool      `json:"is_person"`
	IdentityConfidence   int       `json:"identity_confidence"`
	EventKind            string    `json:"event_kind"`
	RequestRiskScore     int       `json:"request_risk_score"`
	EvidenceConfidence   int       `json:"evidence_confidence"`
	ReasonCode           string    `json:"reason_code,omitempty"`
	Action               string    `json:"action,omitempty"`
	LocalOutcome         string    `json:"local_outcome,omitempty"`
	LocalComparison      string    `json:"local_comparison,omitempty"`
	Endpoint             string    `json:"endpoint,omitempty"`
	Model                string    `json:"model,omitempty"`
	PromptFingerprint    string    `json:"prompt_fingerprint,omitempty"`
	PromptPreview        string    `json:"prompt_preview,omitempty"`
	APIKeyID             int64     `json:"api_key_id,omitempty"`
	APIKeyName           string    `json:"api_key_name,omitempty"`
	APIKeyMasked         string    `json:"api_key_masked,omitempty"`
	AccountID            int64     `json:"account_id,omitempty"`
	AccountName          string    `json:"account_name,omitempty"`
}

type PromptRiskProfileQuery struct {
	Page                  int
	PageSize              int
	SubjectType           string
	SubjectKey            string
	Platform              string
	RiskLevel             string
	APIKeyID              int64
	AccountID             int64
	MinScore              int
	Query                 string
	UpstreamCYOnly        bool
	ActivityState         string
	PrioritizeActiveLocks bool
	ActiveLocksOnly       bool
	ConversationLockTTL   time.Duration
	UserCyberCooldownTTL  time.Duration
}

type PromptRiskEventQuery struct {
	Page     int
	PageSize int
}

type PromptRiskIdentityInput struct {
	Platform       string
	ExternalUserID string
	UserName       string
	UserEmail      string
	UserGroup      string
	Source         string
}

type promptRiskIdentity struct {
	SubjectType    string
	SubjectKey     string
	Platform       string
	ExternalUserID string
	UserName       string
	UserEmail      string
	UserGroup      string
	Source         string
	UpdatedAt      time.Time
}

type promptRiskSignal struct {
	SourceType           string
	SourceID             string
	IncidentID           string
	PromptFilterLogID    int64
	RequestCorrelationID string
	CreatedAt            time.Time
	EventKind            string
	RequestRiskScore     int
	EvidenceConfidence   int
	ReasonCode           string
	Action               string
	LocalOutcome         string
	LocalComparison      string
	Endpoint             string
	Model                string
	PromptFingerprint    string
	PromptPreview        string
	APIKeyID             int64
	APIKeyName           string
	APIKeyMasked         string
	AccountID            int64
	AccountName          string
	NewAPIPolicyStatus   string
	NewAPIPlatform       string
	NewAPIUserID         string
	NewAPIUserName       string
	NewAPIUserEmail      string
	NewAPIUserGroup      string
	SessionHash          string
	ClientIPHash         string
}

type promptRiskSubject struct {
	Type               string
	Key                string
	Display            string
	Platform           string
	IsPerson           bool
	IdentityConfidence int
}

type promptRiskAggregate struct {
	Profile            PromptRiskProfile
	FingerprintEvents  int
	PositiveEvents24h  int
	WeightedTotal      int64
	WeightedLocal      int64
	WeightedUpstream   int64
	WeightedUnverified int64
}

func parsePromptRiskTimeValue(value any) (time.Time, error) {
	parsed, err := parseDBTimeValue(value)
	if err == nil {
		return parsed, nil
	}
	var raw string
	switch typed := value.(type) {
	case string:
		raw = typed
	case []byte:
		raw = string(typed)
	default:
		return time.Time{}, err
	}
	raw = strings.TrimSpace(raw)
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999999 -0700 MST",
		"2006-01-02 15:04:05 -0700 MST",
	} {
		if parsed, parseErr := time.Parse(layout, raw); parseErr == nil {
			return parsed, nil
		}
	}
	return time.Time{}, err
}

func (db *DB) ensurePromptRiskEventsTable(ctx context.Context) error {
	if db == nil {
		return errors.New("database is nil")
	}
	boolType := "BOOLEAN"
	idType := "BIGSERIAL PRIMARY KEY"
	timeDefault := "TIMESTAMPTZ DEFAULT NOW()"
	if db.isSQLite() {
		boolType = "INTEGER"
		idType = "INTEGER PRIMARY KEY AUTOINCREMENT"
		timeDefault = "TIMESTAMP DEFAULT CURRENT_TIMESTAMP"
	}
	ddl := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS prompt_risk_events (
		id %s,
		created_at %s,
		source_type VARCHAR(32) NOT NULL,
		source_id VARCHAR(96) NOT NULL,
		incident_id VARCHAR(64) DEFAULT '',
		prompt_filter_log_id BIGINT DEFAULT 0,
		request_correlation_id VARCHAR(64) DEFAULT '',
		subject_type VARCHAR(32) NOT NULL,
		subject_key VARCHAR(160) NOT NULL,
		subject_display VARCHAR(255) DEFAULT '',
		platform VARCHAR(100) DEFAULT '',
		is_person %s DEFAULT FALSE,
		identity_confidence INT DEFAULT 0,
		event_kind VARCHAR(64) NOT NULL,
		request_risk_score INT DEFAULT 0,
		evidence_confidence INT DEFAULT 0,
		reason_code VARCHAR(100) DEFAULT '',
		action VARCHAR(32) DEFAULT '',
		local_outcome VARCHAR(32) DEFAULT '',
		local_comparison VARCHAR(32) DEFAULT '',
		endpoint VARCHAR(256) DEFAULT '',
		model VARCHAR(100) DEFAULT '',
		prompt_fingerprint VARCHAR(64) DEFAULT '',
		prompt_preview TEXT DEFAULT '',
		api_key_id BIGINT DEFAULT 0,
		api_key_name VARCHAR(255) DEFAULT '',
		api_key_masked VARCHAR(64) DEFAULT '',
		account_id BIGINT DEFAULT 0,
		account_name VARCHAR(255) DEFAULT '',
		UNIQUE(source_type, source_id, subject_type, subject_key)
	);
	CREATE TABLE IF NOT EXISTS prompt_risk_event_sources (
		source_type VARCHAR(32) NOT NULL,
		source_id VARCHAR(96) NOT NULL,
		processed_at %s,
		PRIMARY KEY(source_type, source_id)
	);
	CREATE TABLE IF NOT EXISTS prompt_risk_identities (
		subject_type VARCHAR(32) NOT NULL,
		subject_key VARCHAR(160) NOT NULL,
		platform VARCHAR(100) DEFAULT '',
		external_user_id VARCHAR(255) DEFAULT '',
		user_name VARCHAR(128) DEFAULT '',
		user_email VARCHAR(320) DEFAULT '',
		user_group VARCHAR(100) DEFAULT '',
		source VARCHAR(32) DEFAULT '',
		updated_at %s,
		PRIMARY KEY(subject_type, subject_key)
	);`, idType, timeDefault, boolType, timeDefault, timeDefault)
	if _, err := db.conn.ExecContext(ctx, ddl); err != nil {
		return err
	}
	for _, stmt := range []string{
		`CREATE INDEX IF NOT EXISTS idx_prompt_risk_events_subject ON prompt_risk_events(subject_type, subject_key, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_prompt_risk_events_created ON prompt_risk_events(created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_prompt_risk_events_kind ON prompt_risk_events(event_kind, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_prompt_risk_events_incident ON prompt_risk_events(incident_id)`,
		`CREATE INDEX IF NOT EXISTS idx_prompt_risk_events_api_key ON prompt_risk_events(api_key_id, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_prompt_risk_events_account ON prompt_risk_events(account_id, created_at)`,
		`CREATE INDEX IF NOT EXISTS idx_prompt_risk_events_request_match ON prompt_risk_events(request_correlation_id, subject_type, subject_key, event_kind) WHERE request_correlation_id<>''`,
		`CREATE INDEX IF NOT EXISTS idx_prompt_risk_events_fingerprint_match ON prompt_risk_events(prompt_fingerprint, subject_type, subject_key, created_at, event_kind) WHERE request_correlation_id='' AND prompt_fingerprint<>''`,
		`CREATE INDEX IF NOT EXISTS idx_prompt_risk_sources_processed ON prompt_risk_event_sources(processed_at)`,
		`CREATE INDEX IF NOT EXISTS idx_prompt_risk_identities_external ON prompt_risk_identities(platform, external_user_id)`,
		`CREATE INDEX IF NOT EXISTS idx_prompt_risk_identities_updated ON prompt_risk_identities(updated_at)`,
	} {
		if _, err := db.conn.ExecContext(ctx, stmt); err != nil {
			return err
		}
	}
	return nil
}

func promptRiskHash(namespace, value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	digest := sha256.Sum256([]byte(namespace + "\x00" + value))
	return hex.EncodeToString(digest[:16])
}

func promptRiskClamp(value int) int {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func promptRiskSignalForLog(log PromptFilterLog) (promptRiskSignal, bool) {
	source := strings.ToLower(strings.TrimSpace(log.Source))
	reasonCode := strings.ToLower(strings.TrimSpace(log.ReasonCode))
	origin := strings.ToLower(strings.TrimSpace(log.PrimaryOrigin))
	if source != "" && source != "local_filter" {
		return promptRiskSignal{}, false
	}
	if reasonCode == "prompt_policy_shadow_async" || origin == "tool_output" || origin == "tool_arguments" || origin == "session_context" || origin == "history" || origin == "system" || origin == "developer" || origin == "instructions" || origin == "attachment_refs" || origin == "attachment_content" {
		return promptRiskSignal{}, false
	}
	kind := ""
	score := 0
	confidence := 0
	securityContextScore, securityContextObserved := promptRiskSecurityContextScore(log)
	reviewCleared := strings.TrimSpace(log.ReviewModel) != "" && !log.ReviewFlagged && strings.TrimSpace(log.ReviewError) == ""
	switch {
	case log.ReviewFlagged:
		kind, score, confidence = "review_flagged_monitor", 40, 95
	case strings.EqualFold(log.Action, "block") && log.StrikeEligible:
		kind, score, confidence = "local_block_strike", 48, 95
	case strings.EqualFold(log.Action, "allow") && securityContextObserved:
		kind = "local_security_context_observed"
		score = min(10, max(3, max(log.AuditScore, securityContextScore)/5))
		confidence = 35
		if reviewCleared {
			confidence = 25
			if log.ReviewConfidence != nil {
				confidence = max(confidence, min(60, int(math.Round(*log.ReviewConfidence*100))))
			}
		}
	case reviewCleared:
		kind, score, confidence = promptRiskEventReviewCleared, 0, 95
	case strings.EqualFold(log.Action, "block"):
		kind, score, confidence = promptRiskEventLocalBlockUnverified, 8, 35
	case strings.EqualFold(log.Action, "warn"):
		kind, score, confidence = "local_warn", 22, 75
	default:
		return promptRiskSignal{}, false
	}
	if log.ReviewFlagged {
		confidence = max(confidence, 95)
	}
	return promptRiskSignal{
		SourceType: promptRiskSourceLog, SourceID: strconv.FormatInt(log.ID, 10), PromptFilterLogID: log.ID,
		RequestCorrelationID: log.RequestCorrelationID, CreatedAt: log.CreatedAt, EventKind: kind,
		RequestRiskScore: score, EvidenceConfidence: confidence, ReasonCode: log.ReasonCode, Action: log.Action,
		Endpoint: log.Endpoint, Model: log.Model, PromptFingerprint: promptRiskObservedFingerprint(kind, log.TextPreview),
		PromptPreview: log.TextPreview, APIKeyID: log.APIKeyID,
		APIKeyName: log.APIKeyName, APIKeyMasked: log.APIKeyMasked, NewAPIPolicyStatus: log.NewAPIPolicyStatus,
		NewAPIPlatform: log.NewAPIPlatform, NewAPIUserID: log.NewAPIUserID, SessionHash: log.SessionHash,
		ClientIPHash: log.ClientIPHash,
	}, true
}

func promptRiskSecurityContextScore(log PromptFilterLog) (int, bool) {
	if strings.TrimSpace(log.MatchedPatterns) == "" {
		return 0, false
	}
	var matches []struct {
		Category string `json:"category"`
		Weight   int    `json:"weight"`
	}
	if json.Unmarshal([]byte(log.MatchedPatterns), &matches) != nil {
		return 0, false
	}
	score := 0
	for _, match := range matches {
		switch strings.ToLower(strings.TrimSpace(match.Category)) {
		case "prompt_injection", "malware", "evasion", "exploit", "web_attack", "network_attack",
			"credential_attack", "unauthorized_access", "remote_access", "social_engineering",
			"supply_chain", "resource_abuse", "container_security", "cloud_security",
			"wireless_attack", "iot_security", "api_security", "blockchain_security":
			score += max(0, match.Weight)
		}
	}
	return score, score > 0
}

func promptRiskObservedFingerprint(_ string, preview string) string {
	if strings.TrimSpace(preview) == "" {
		return ""
	}
	return promptRiskHash("prompt-risk-preview", preview)
}

func promptRiskSignalForIncident(incident PromptPolicyIncident) promptRiskSignal {
	kind := "upstream_cy_legacy_unknown"
	score, confidence := 8, 25
	switch incident.LocalComparison {
	case PromptPolicyComparisonConfirmedMiss:
		kind, score, confidence = "upstream_cy_confirmed_miss", 52, 92
	case PromptPolicyComparisonLocalDetected:
		kind, score, confidence = "upstream_cy_local_detected", 28, 85
	case PromptPolicyComparisonUpstreamOnly:
		kind, score, confidence = "upstream_cy_upstream_only", 34, 68
	case PromptPolicyComparisonEvidenceUnavailable:
		kind, score, confidence = "upstream_cy_evidence_unavailable", 14, 35
	case PromptPolicyComparisonNotComparable:
		kind, score, confidence = "upstream_cy_not_comparable", 12, 30
	}
	return promptRiskSignal{
		SourceType: promptRiskSourceIncident, SourceID: incident.IncidentID, IncidentID: incident.IncidentID,
		RequestCorrelationID: incident.RequestCorrelationID, CreatedAt: incident.CreatedAt, EventKind: kind,
		RequestRiskScore: score, EvidenceConfidence: confidence, ReasonCode: incident.LocalReasonCode,
		Action: incident.LocalAction, LocalOutcome: incident.LocalOutcome, LocalComparison: incident.LocalComparison,
		Endpoint: incident.Endpoint, Model: incident.Model, PromptFingerprint: incident.PromptFingerprint,
		PromptPreview: incident.PromptPreview, APIKeyID: incident.APIKeyID, APIKeyName: incident.APIKeyName,
		APIKeyMasked: incident.APIKeyMasked, AccountID: incident.AccountID, AccountName: incident.AccountName,
		NewAPIPolicyStatus: incident.NewAPIPolicyStatus, NewAPIPlatform: incident.NewAPIPlatform,
		NewAPIUserID: incident.NewAPIUserID, SessionHash: incident.SessionHash, ClientIPHash: incident.ClientIPHash,
	}
}

func normalizePromptRiskIdentity(input PromptRiskIdentityInput) (promptRiskIdentity, bool) {
	platform := strings.ToLower(truncateCandidateRunes(strings.TrimSpace(input.Platform), 100))
	externalUserID := truncateCandidateRunes(strings.TrimSpace(input.ExternalUserID), 255)
	if platform == "" || externalUserID == "" {
		return promptRiskIdentity{}, false
	}
	return promptRiskIdentity{
		SubjectType:    PromptRiskSubjectNewAPIUser,
		SubjectKey:     PromptRiskNewAPIUserSubjectKey(platform, externalUserID),
		Platform:       platform,
		ExternalUserID: externalUserID,
		UserName:       truncateCandidateRunes(strings.TrimSpace(input.UserName), 128),
		UserEmail:      truncateCandidateRunes(strings.TrimSpace(input.UserEmail), 320),
		UserGroup:      truncateCandidateRunes(strings.TrimSpace(input.UserGroup), 100),
	}, true
}

// PromptRiskNewAPIUserSubjectKey returns the stable privacy-preserving key used
// by both risk events and request-time adaptive trust checks. Raw platform user
// identifiers are never placed in runtime lookup keys.
func PromptRiskNewAPIUserSubjectKey(platform, externalUserID string) string {
	platform = strings.ToLower(truncateCandidateRunes(strings.TrimSpace(platform), 100))
	externalUserID = truncateCandidateRunes(strings.TrimSpace(externalUserID), 255)
	if platform == "" || externalUserID == "" {
		return ""
	}
	return promptRiskHash("newapi-user", platform+"\x00"+externalUserID)
}

func promptRiskIdentityForSignal(signal promptRiskSignal) (promptRiskIdentity, bool) {
	verified := signal.NewAPIPolicyStatus == "verified" || signal.NewAPIPolicyStatus == "signed_response"
	if !verified {
		return promptRiskIdentity{}, false
	}
	return normalizePromptRiskIdentity(PromptRiskIdentityInput{
		Platform:       signal.NewAPIPlatform,
		ExternalUserID: signal.NewAPIUserID,
		UserName:       signal.NewAPIUserName,
		UserEmail:      signal.NewAPIUserEmail,
		UserGroup:      signal.NewAPIUserGroup,
	})
}

func promptRiskIdentityDisplay(identity promptRiskIdentity) string {
	if value := strings.TrimSpace(identity.UserName); value != "" {
		return value
	}
	if value := strings.TrimSpace(identity.UserEmail); value != "" {
		return value
	}
	if value := strings.TrimSpace(identity.ExternalUserID); value != "" {
		return value
	}
	return ""
}

func upsertPromptRiskIdentity(ctx context.Context, exec promptRiskEventExecutor, identity promptRiskIdentity, source string) error {
	if identity.SubjectKey == "" {
		return nil
	}
	source = truncateCandidateRunes(strings.TrimSpace(source), 32)
	if source == "" {
		source = "signed_metadata"
	}
	_, err := exec.ExecContext(ctx, `INSERT INTO prompt_risk_identities (
		subject_type, subject_key, platform, external_user_id, user_name, user_email, user_group, source, updated_at
	) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
	ON CONFLICT(subject_type, subject_key) DO UPDATE SET
		platform=excluded.platform,
		external_user_id=excluded.external_user_id,
		user_name=CASE WHEN excluded.user_name<>'' THEN excluded.user_name ELSE prompt_risk_identities.user_name END,
		user_email=CASE WHEN excluded.user_email<>'' THEN excluded.user_email ELSE prompt_risk_identities.user_email END,
		user_group=CASE WHEN excluded.user_group<>'' THEN excluded.user_group ELSE prompt_risk_identities.user_group END,
		source=excluded.source,
		updated_at=excluded.updated_at`, identity.SubjectType, identity.SubjectKey, identity.Platform, identity.ExternalUserID,
		identity.UserName, identity.UserEmail, identity.UserGroup, source, time.Now().UTC())
	return err
}

func (db *DB) UpsertPromptRiskIdentities(ctx context.Context, inputs []PromptRiskIdentityInput) error {
	if db == nil || len(inputs) == 0 {
		return nil
	}
	if err := db.ensurePromptRiskEventsTable(ctx); err != nil {
		return err
	}
	tx, err := db.conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, input := range inputs {
		identity, ok := normalizePromptRiskIdentity(input)
		if !ok {
			continue
		}
		if err := upsertPromptRiskIdentity(ctx, tx, identity, input.Source); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func promptRiskSubjects(signal promptRiskSignal) []promptRiskSubject {
	items := make([]promptRiskSubject, 0, 5)
	verifiedIdentity := signal.NewAPIPolicyStatus == "verified" || signal.NewAPIPolicyStatus == "signed_response"
	platform := strings.TrimSpace(signal.NewAPIPlatform)
	if verifiedIdentity && strings.TrimSpace(signal.NewAPIUserID) != "" {
		identity, _ := promptRiskIdentityForSignal(signal)
		items = append(items, promptRiskSubject{Type: PromptRiskSubjectNewAPIUser, Key: identity.SubjectKey, Display: promptRiskIdentityDisplay(identity), Platform: platform, IsPerson: true, IdentityConfidence: 100})
	}
	if strings.TrimSpace(signal.SessionHash) != "" {
		confidence := 50
		if verifiedIdentity {
			confidence = 85
		}
		items = append(items, promptRiskSubject{Type: PromptRiskSubjectSession, Key: strings.TrimSpace(signal.SessionHash), Display: "session-" + shortRiskKey(signal.SessionHash), Platform: platform, IdentityConfidence: confidence})
	}
	if signal.APIKeyID > 0 {
		display := strings.TrimSpace(signal.APIKeyName)
		if display == "" {
			display = strings.TrimSpace(signal.APIKeyMasked)
		}
		if display == "" {
			display = "API Key #" + strconv.FormatInt(signal.APIKeyID, 10)
		}
		items = append(items, promptRiskSubject{Type: PromptRiskSubjectAPIKey, Key: strconv.FormatInt(signal.APIKeyID, 10), Display: display, Platform: platform, IdentityConfidence: 60})
	}
	if strings.TrimSpace(signal.ClientIPHash) != "" {
		items = append(items, promptRiskSubject{Type: PromptRiskSubjectClientIP, Key: strings.TrimSpace(signal.ClientIPHash), Display: "IP-" + shortRiskKey(signal.ClientIPHash), Platform: platform, IdentityConfidence: 30})
	}
	if signal.AccountID > 0 {
		display := strings.TrimSpace(signal.AccountName)
		if display == "" {
			display = "Account #" + strconv.FormatInt(signal.AccountID, 10)
		}
		items = append(items, promptRiskSubject{Type: PromptRiskSubjectUpstreamAccount, Key: strconv.FormatInt(signal.AccountID, 10), Display: display, Platform: platform, IdentityConfidence: 25})
	}
	return items
}

func shortRiskKey(value string) string {
	value = strings.TrimSpace(value)
	if len(value) <= 10 {
		return value
	}
	return value[:10]
}

type promptRiskEventExecutor interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func insertPromptRiskSignal(ctx context.Context, exec promptRiskEventExecutor, signal promptRiskSignal) error {
	if signal.SourceType == "" || signal.SourceID == "" {
		return nil
	}
	if signal.CreatedAt.IsZero() {
		signal.CreatedAt = time.Now().UTC()
	}
	if identity, ok := promptRiskIdentityForSignal(signal); ok {
		if err := upsertPromptRiskIdentity(ctx, exec, identity, "signed_metadata"); err != nil {
			return err
		}
	}
	for _, subject := range promptRiskSubjects(signal) {
		if subject.Key == "" {
			continue
		}
		if _, err := exec.ExecContext(ctx, `INSERT INTO prompt_risk_events (
			created_at, source_type, source_id, incident_id, prompt_filter_log_id, request_correlation_id,
			subject_type, subject_key, subject_display, platform, is_person, identity_confidence,
			event_kind, request_risk_score, evidence_confidence, reason_code, action, local_outcome, local_comparison,
			endpoint, model, prompt_fingerprint, prompt_preview, api_key_id, api_key_name, api_key_masked, account_id, account_name
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28)
		ON CONFLICT(source_type, source_id, subject_type, subject_key) DO NOTHING`,
			signal.CreatedAt, signal.SourceType, signal.SourceID, signal.IncidentID, signal.PromptFilterLogID, signal.RequestCorrelationID,
			subject.Type, subject.Key, truncateCandidateRunes(subject.Display, 255), truncateCandidateRunes(subject.Platform, 100),
			subject.IsPerson, promptRiskClamp(subject.IdentityConfidence), signal.EventKind, promptRiskClamp(signal.RequestRiskScore),
			promptRiskClamp(signal.EvidenceConfidence), truncateCandidateRunes(signal.ReasonCode, 100), truncateCandidateRunes(signal.Action, 32),
			truncateCandidateRunes(signal.LocalOutcome, 32), truncateCandidateRunes(signal.LocalComparison, 32),
			truncateCandidateRunes(signal.Endpoint, 256), truncateCandidateRunes(signal.Model, 100), truncateCandidateRunes(signal.PromptFingerprint, 64),
			truncateCandidateRunes(signal.PromptPreview, 500), signal.APIKeyID, truncateCandidateRunes(signal.APIKeyName, 255),
			truncateCandidateRunes(signal.APIKeyMasked, 64), signal.AccountID, truncateCandidateRunes(signal.AccountName, 255)); err != nil {
			return err
		}
		if err := reconcilePromptRiskReviewForSubject(ctx, exec, signal, subject); err != nil {
			return err
		}
	}
	_, err := exec.ExecContext(ctx, `INSERT INTO prompt_risk_event_sources(source_type, source_id) VALUES ($1,$2) ON CONFLICT(source_type, source_id) DO NOTHING`, signal.SourceType, signal.SourceID)
	return err
}

func reconcilePromptRiskReviewForSubject(ctx context.Context, exec promptRiskEventExecutor, signal promptRiskSignal, subject promptRiskSubject) error {
	if signal.EventKind != promptRiskEventReviewCleared && signal.EventKind != promptRiskEventLocalBlockUnverified {
		return nil
	}
	requestID := strings.TrimSpace(signal.RequestCorrelationID)
	fingerprint := strings.TrimSpace(signal.PromptFingerprint)
	if requestID == "" && fingerprint == "" {
		return nil
	}
	if signal.EventKind == promptRiskEventReviewCleared {
		if requestID != "" {
			_, err := exec.ExecContext(ctx, `UPDATE prompt_risk_events SET
				event_kind=$1, request_risk_score=0, evidence_confidence=95
				WHERE subject_type=$2 AND subject_key=$3 AND event_kind IN ($4,$5)
				AND request_correlation_id<>'' AND request_correlation_id=$6`, promptRiskEventLocalBlockCleared, subject.Type, subject.Key,
				promptRiskEventLocalBlock, promptRiskEventLocalBlockUnverified, requestID)
			return err
		}
		windowStart := signal.CreatedAt.Add(-10 * time.Minute)
		windowEnd := signal.CreatedAt.Add(10 * time.Minute)
		_, err := exec.ExecContext(ctx, `UPDATE prompt_risk_events SET
			event_kind=$1, request_risk_score=0, evidence_confidence=95
			WHERE subject_type=$2 AND subject_key=$3 AND event_kind IN ($4,$5)
			AND request_correlation_id='' AND prompt_fingerprint<>'' AND prompt_fingerprint=$6
			AND created_at >= $7 AND created_at <= $8`, promptRiskEventLocalBlockCleared, subject.Type, subject.Key,
			promptRiskEventLocalBlock, promptRiskEventLocalBlockUnverified, fingerprint, windowStart, windowEnd)
		return err
	}
	if requestID != "" {
		_, err := exec.ExecContext(ctx, `UPDATE prompt_risk_events SET
			event_kind=$1, request_risk_score=0, evidence_confidence=95
			WHERE source_type=$2 AND source_id=$3 AND subject_type=$4 AND subject_key=$5
			AND event_kind=$6 AND EXISTS (
				SELECT 1 FROM prompt_risk_events cleared
				WHERE cleared.subject_type=$4 AND cleared.subject_key=$5 AND cleared.event_kind=$7
				AND cleared.request_correlation_id<>'' AND cleared.request_correlation_id=$8
			)`, promptRiskEventLocalBlockCleared, signal.SourceType, signal.SourceID, subject.Type, subject.Key,
			promptRiskEventLocalBlockUnverified, promptRiskEventReviewCleared, requestID)
		return err
	}
	windowStart := signal.CreatedAt.Add(-10 * time.Minute)
	windowEnd := signal.CreatedAt.Add(10 * time.Minute)
	_, err := exec.ExecContext(ctx, `UPDATE prompt_risk_events SET
		event_kind=$1, request_risk_score=0, evidence_confidence=95
		WHERE source_type=$2 AND source_id=$3 AND subject_type=$4 AND subject_key=$5
		AND event_kind=$6 AND EXISTS (
			SELECT 1 FROM prompt_risk_events cleared
			WHERE cleared.subject_type=$4 AND cleared.subject_key=$5 AND cleared.event_kind=$7
			AND cleared.request_correlation_id='' AND cleared.prompt_fingerprint<>'' AND cleared.prompt_fingerprint=$8
			AND cleared.created_at >= $9 AND cleared.created_at <= $10
		)`, promptRiskEventLocalBlockCleared, signal.SourceType, signal.SourceID, subject.Type, subject.Key,
		promptRiskEventLocalBlockUnverified, promptRiskEventReviewCleared, fingerprint, windowStart, windowEnd)
	return err
}

func (db *DB) backfillPromptRiskEvents(ctx context.Context) error {
	cutoff := time.Now().UTC().Add(-30 * 24 * time.Hour)
	if err := db.backfillPromptRiskLogs(ctx, cutoff); err != nil {
		return err
	}
	return db.backfillPromptRiskIncidents(ctx, cutoff)
}

func (db *DB) backfillPromptRiskLogs(ctx context.Context, cutoff time.Time) error {
	for {
		rows, err := db.conn.QueryContext(ctx, `SELECT l.id, l.created_at, COALESCE(l.source,''), COALESCE(l.endpoint,''), COALESCE(l.model,''),
			COALESCE(l.action,''), COALESCE(l.score,0), COALESCE(l.audit_score,0), COALESCE(l.reason_code,''), COALESCE(l.strike_eligible,false),
			COALESCE(l.matched_patterns,'[]'), COALESCE(l.text_preview,''), COALESCE(l.api_key_id,0), COALESCE(l.api_key_name,''),
			COALESCE(l.api_key_masked,''), COALESCE(l.client_ip,''), COALESCE(l.review_model,''), COALESCE(l.review_flagged,false),
			COALESCE(l.review_error,''), COALESCE(l.request_correlation_id,''), COALESCE(l.newapi_policy_status,''),
			COALESCE(l.newapi_platform,''), COALESCE(l.newapi_user_id,''), COALESCE(l.newapi_request_id,''),
			COALESCE(l.newapi_decision_id,''), COALESCE(l.session_hash,'')
		FROM prompt_filter_logs l
		WHERE l.created_at >= $1 AND NOT EXISTS (
			SELECT 1 FROM prompt_risk_event_sources s WHERE s.source_type=$2 AND s.source_id=CAST(l.id AS TEXT)
		) ORDER BY l.id LIMIT 500`, cutoff, promptRiskSourceLog)
		if err != nil {
			return err
		}
		logs := make([]PromptFilterLog, 0, 500)
		for rows.Next() {
			var item PromptFilterLog
			var createdRaw any
			var clientIP string
			if err := rows.Scan(&item.ID, &createdRaw, &item.Source, &item.Endpoint, &item.Model, &item.Action, &item.Score, &item.AuditScore,
				&item.ReasonCode, &item.StrikeEligible, &item.MatchedPatterns, &item.TextPreview, &item.APIKeyID, &item.APIKeyName,
				&item.APIKeyMasked, &clientIP, &item.ReviewModel, &item.ReviewFlagged, &item.ReviewError, &item.RequestCorrelationID,
				&item.NewAPIPolicyStatus, &item.NewAPIPlatform, &item.NewAPIUserID, &item.NewAPIRequestID, &item.NewAPIDecisionID,
				&item.SessionHash); err != nil {
				rows.Close()
				return err
			}
			item.ClientIPHash = promptRiskHash("client-ip", clientIP)
			item.CreatedAt, err = parsePromptRiskTimeValue(createdRaw)
			if err != nil {
				rows.Close()
				return err
			}
			logs = append(logs, item)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if len(logs) == 0 {
			return nil
		}
		if err := db.withSQLiteWriteLock(ctx, func() error {
			tx, err := db.conn.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			for _, item := range logs {
				signal, ok := promptRiskSignalForLog(item)
				if !ok {
					signal = promptRiskSignal{SourceType: promptRiskSourceLog, SourceID: strconv.FormatInt(item.ID, 10), CreatedAt: item.CreatedAt}
				}
				if err := insertPromptRiskSignal(ctx, tx, signal); err != nil {
					return err
				}
			}
			return tx.Commit()
		}); err != nil {
			return err
		}
	}
}

func (db *DB) backfillPromptRiskIncidents(ctx context.Context, cutoff time.Time) error {
	for {
		rows, err := db.conn.QueryContext(ctx, promptPolicyIncidentSelect+` WHERE created_at >= $1 AND NOT EXISTS (
			SELECT 1 FROM prompt_risk_event_sources s WHERE s.source_type=$2 AND s.source_id=prompt_policy_incidents.incident_id
		) ORDER BY id LIMIT 500`, cutoff, promptRiskSourceIncident)
		if err != nil {
			return err
		}
		items := make([]*PromptPolicyIncident, 0, 500)
		for rows.Next() {
			item, scanErr := scanPromptPolicyIncident(rows)
			if scanErr != nil {
				rows.Close()
				return scanErr
			}
			items = append(items, item)
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if len(items) == 0 {
			return nil
		}
		if err := db.withSQLiteWriteLock(ctx, func() error {
			tx, err := db.conn.BeginTx(ctx, nil)
			if err != nil {
				return err
			}
			defer tx.Rollback()
			for _, item := range items {
				if err := insertPromptRiskSignal(ctx, tx, promptRiskSignalForIncident(*item)); err != nil {
					return err
				}
			}
			return tx.Commit()
		}); err != nil {
			return err
		}
	}
}

func (db *DB) loadPromptRiskIdentities(ctx context.Context, subjectKeys []string) (map[string]promptRiskIdentity, error) {
	keys := make([]string, 0, len(subjectKeys))
	seen := make(map[string]struct{}, len(subjectKeys))
	for _, key := range subjectKeys {
		key = strings.TrimSpace(key)
		if key == "" {
			continue
		}
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return map[string]promptRiskIdentity{}, nil
	}
	args := make([]any, 0, len(keys)+1)
	args = append(args, PromptRiskSubjectNewAPIUser)
	placeholders := make([]string, 0, len(keys))
	for _, key := range keys {
		args = append(args, key)
		placeholders = append(placeholders, fmt.Sprintf("$%d", len(args)))
	}
	rows, err := db.conn.QueryContext(ctx, `SELECT subject_type, subject_key, platform, external_user_id, user_name, user_email, user_group, source, updated_at
		FROM prompt_risk_identities WHERE subject_type=$1 AND subject_key IN (`+strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make(map[string]promptRiskIdentity, len(keys))
	for rows.Next() {
		var identity promptRiskIdentity
		var updatedAtRaw any
		if err := rows.Scan(&identity.SubjectType, &identity.SubjectKey, &identity.Platform, &identity.ExternalUserID, &identity.UserName, &identity.UserEmail, &identity.UserGroup, &identity.Source, &updatedAtRaw); err != nil {
			return nil, err
		}
		if updatedAtRaw != nil {
			identity.UpdatedAt, err = parsePromptRiskTimeValue(updatedAtRaw)
			if err != nil {
				return nil, err
			}
		}
		items[identity.SubjectKey] = identity
	}
	return items, rows.Err()
}

func (db *DB) listPromptRiskIdentities(ctx context.Context) ([]promptRiskIdentity, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT subject_type, subject_key, platform, external_user_id, user_name, user_email, user_group, source, updated_at
		FROM prompt_risk_identities WHERE subject_type=$1`, PromptRiskSubjectNewAPIUser)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	items := make([]promptRiskIdentity, 0)
	for rows.Next() {
		var identity promptRiskIdentity
		var updatedAtRaw any
		if err := rows.Scan(&identity.SubjectType, &identity.SubjectKey, &identity.Platform, &identity.ExternalUserID, &identity.UserName, &identity.UserEmail, &identity.UserGroup, &identity.Source, &updatedAtRaw); err != nil {
			return nil, err
		}
		if updatedAtRaw != nil {
			identity.UpdatedAt, err = parsePromptRiskTimeValue(updatedAtRaw)
			if err != nil {
				return nil, err
			}
		}
		items = append(items, identity)
	}
	return items, rows.Err()
}

func applyPromptRiskIdentityToProfile(profile *PromptRiskProfile, identity promptRiskIdentity) {
	if profile == nil || identity.SubjectKey == "" {
		return
	}
	profile.NewAPIUserID = identity.ExternalUserID
	profile.NewAPIUserName = identity.UserName
	profile.NewAPIUserEmail = identity.UserEmail
	profile.NewAPIUserGroup = identity.UserGroup
	profile.IdentitySource = identity.Source
	if !identity.UpdatedAt.IsZero() {
		updatedAt := identity.UpdatedAt
		profile.IdentityUpdatedAt = &updatedAt
	}
	if display := promptRiskIdentityDisplay(identity); display != "" {
		profile.SubjectDisplay = display
	}
	if identity.Platform != "" {
		profile.Platform = identity.Platform
	}
}

func applyPromptRiskIdentityToEvent(event *PromptRiskEvent, identity promptRiskIdentity) {
	if event == nil || identity.SubjectKey == "" {
		return
	}
	event.NewAPIUserID = identity.ExternalUserID
	event.NewAPIUserName = identity.UserName
	event.NewAPIUserEmail = identity.UserEmail
	event.NewAPIUserGroup = identity.UserGroup
	if display := promptRiskIdentityDisplay(identity); display != "" {
		event.SubjectDisplay = display
	}
	if identity.Platform != "" {
		event.Platform = identity.Platform
	}
}

type promptRiskActiveRestriction struct {
	Profile  PromptRiskProfile
	LockedAt time.Time
}

// promptRiskActiveRestrictionSubjects returns the profile subjects currently
// restricted by a live conversation lock or user cooldown. Each entry carries
// a minimal profile so an active restriction remains visible even when its
// originating audit event has already been cleared. The query is intentionally
// read-only: stale rows remain historical evidence and are excluded by the
// configured TTL instead of being updated while the admin list is loading.
func (db *DB) promptRiskActiveRestrictionSubjects(ctx context.Context, conversationTTL, userCooldownTTL time.Duration) (map[string]promptRiskActiveRestriction, error) {
	result := make(map[string]promptRiskActiveRestriction)
	if db == nil {
		return result, nil
	}
	// The lock table is created once during database startup. Do not run DDL
	// from this read path: repeated CREATE/ALTER checks on a busy SQLite instance
	// can contend with OAuth and usage-log writers even when the schema is stable.
	rows, err := db.conn.QueryContext(ctx, `SELECT identity_kind, session_hash, platform, newapi_user_id, reason_code, locked_at
		FROM prompt_conversation_locks WHERE status='active'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := time.Now().UTC()
	for rows.Next() {
		var identityKind, sessionHash, platform, userID, reasonCode string
		var lockedRaw any
		if err := rows.Scan(&identityKind, &sessionHash, &platform, &userID, &reasonCode, &lockedRaw); err != nil {
			return nil, err
		}
		lockedAt, err := parsePromptRiskTimeValue(lockedRaw)
		if err != nil {
			return nil, err
		}
		effectiveTTL := conversationTTL
		if identityKind == PromptConversationLockIdentityFingerprintReplay {
			effectiveTTL = userCooldownTTL
		}
		if sessionHash = strings.ToLower(strings.TrimSpace(sessionHash)); sessionHash != "" &&
			(effectiveTTL <= 0 || lockedAt.After(now.Add(-effectiveTTL))) {
			key := PromptRiskSubjectSession + "\x00" + sessionHash
			confidence := 85
			display := "session-" + shortRiskKey(sessionHash)
			switch identityKind {
			case PromptConversationLockIdentityCodexSession:
				confidence = 50
			case PromptConversationLockIdentityFingerprintReplay:
				// 指纹重放锁只绑定 API Key、客户端 IP 与提示词指纹,不能解释
				// 为人员身份;合成画像仅为了让活动冷却在管理端可见、可解锁。
				confidence = 20
				display = "replay-" + shortRiskKey(sessionHash)
			}
			profile := PromptRiskProfile{
				SubjectType: PromptRiskSubjectSession, SubjectKey: sessionHash,
				SubjectDisplay: display, Platform: platform,
				IdentityConfidence: confidence, RiskLevel: PromptRiskLevelLow,
				RecommendedActions: []string{"observe"}, HasActivity: false, LatestAt: lockedAt,
				ScoreBreakdown: PromptRiskScoreBreakdown{IdentityConfidence: confidence},
			}
			if idPart, ok := strings.CutPrefix(userID, "apikey:"); ok {
				if sep := strings.IndexByte(idPart, ':'); sep >= 0 {
					idPart = idPart[:sep]
				}
				profile.APIKeyID, _ = strconv.ParseInt(idPart, 10, 64)
			}
			if current, ok := result[key]; !ok || lockedAt.After(current.LockedAt) {
				result[key] = promptRiskActiveRestriction{Profile: profile, LockedAt: lockedAt}
			}
		}
		// Only a real upstream CYB expands to user scope. Local deterministic
		// blocks remain attached to the exact session to avoid implicating every
		// conversation of a shared user.
		if identityKind == PromptConversationLockIdentityNewAPI && reasonCode == "upstream_cyber_policy" &&
			(userCooldownTTL <= 0 || lockedAt.After(now.Add(-userCooldownTTL))) {
			if subjectKey := PromptRiskNewAPIUserSubjectKey(platform, userID); subjectKey != "" {
				key := PromptRiskSubjectNewAPIUser + "\x00" + subjectKey
				profile := PromptRiskProfile{
					SubjectType: PromptRiskSubjectNewAPIUser, SubjectKey: subjectKey,
					SubjectDisplay: userID, Platform: platform, NewAPIUserID: userID,
					IsPerson: true, IdentityConfidence: 100, RiskLevel: PromptRiskLevelLow,
					RecommendedActions: []string{"observe"}, HasActivity: false, LatestAt: lockedAt,
					ScoreBreakdown: PromptRiskScoreBreakdown{IdentityConfidence: 100},
				}
				if current, ok := result[key]; !ok || lockedAt.After(current.LockedAt) {
					result[key] = promptRiskActiveRestriction{Profile: profile, LockedAt: lockedAt}
				}
			}
		}
	}
	return result, rows.Err()
}

func promptRiskActiveProfileMatchesQuery(profile PromptRiskProfile, query PromptRiskProfileQuery, includeTextQuery bool) bool {
	if subjectType := strings.TrimSpace(query.SubjectType); subjectType != "" && subjectType != "all" && profile.SubjectType != subjectType {
		return false
	}
	if subjectKey := strings.TrimSpace(query.SubjectKey); subjectKey != "" && profile.SubjectKey != subjectKey {
		return false
	}
	if platform := strings.TrimSpace(query.Platform); platform != "" && profile.Platform != platform {
		return false
	}
	if query.APIKeyID > 0 && profile.APIKeyID != query.APIKeyID {
		return false
	}
	if query.AccountID > 0 || query.MinScore > profile.RiskScore {
		return false
	}
	if level := strings.TrimSpace(query.RiskLevel); level != "" && level != "all" && profile.RiskLevel != level {
		return false
	}
	// 与 SQL 聚合侧的过滤语义保持一致:合成的活动锁画像没有事件计数,
	// 不得泄漏进 cy_only / activity_state 过滤结果。
	if query.UpstreamCYOnly && profile.UpstreamCYCount == 0 {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(query.ActivityState)) {
	case "active":
		if !profile.HasActivity {
			return false
		}
	case "identity_only":
		if profile.HasActivity {
			return false
		}
	}
	if !includeTextQuery {
		return true
	}
	queryText := strings.ToLower(strings.TrimSpace(query.Query))
	if queryText == "" {
		return true
	}
	haystack := strings.ToLower(strings.Join([]string{
		profile.SubjectType, profile.SubjectKey, profile.SubjectDisplay, profile.Platform,
		profile.NewAPIUserID, profile.NewAPIUserName, profile.NewAPIUserEmail, profile.NewAPIUserGroup,
		profile.APIKeyName, profile.APIKeyMasked, strconv.FormatInt(profile.APIKeyID, 10),
	}, "\x00"))
	return strings.Contains(haystack, queryText)
}

func (db *DB) ListPromptRiskProfiles(ctx context.Context, query PromptRiskProfileQuery) ([]*PromptRiskProfile, int, error) {
	if db == nil {
		return nil, 0, nil
	}
	if strings.TrimSpace(query.SubjectType) == PromptRiskSubjectAccountStatus {
		return db.listAccountStatusProfiles(ctx, query)
	}
	if query.Page <= 0 {
		query.Page = 1
	}
	if query.PageSize <= 0 || query.PageSize > 200 {
		query.PageSize = 20
	}
	now := time.Now().UTC()
	cutoff30d, cutoff7d, cutoff24h, cutoff10m := now.Add(-30*24*time.Hour), now.Add(-7*24*time.Hour), now.Add(-24*time.Hour), now.Add(-10*time.Minute)
	clauses := []string{"created_at >= $1"}
	args := []any{cutoff30d, cutoff10m, cutoff24h, cutoff7d}
	if value := strings.TrimSpace(query.SubjectType); value != "" && value != "all" {
		args = append(args, value)
		clauses = append(clauses, fmt.Sprintf("subject_type=$%d", len(args)))
	}
	if value := strings.TrimSpace(query.SubjectKey); value != "" {
		args = append(args, value)
		clauses = append(clauses, fmt.Sprintf("subject_key=$%d", len(args)))
	}
	if value := strings.TrimSpace(query.Platform); value != "" {
		args = append(args, value)
		clauses = append(clauses, fmt.Sprintf("platform=$%d", len(args)))
	}
	if query.APIKeyID > 0 {
		args = append(args, query.APIKeyID)
		clauses = append(clauses, fmt.Sprintf("api_key_id=$%d", len(args)))
	}
	if query.AccountID > 0 {
		args = append(args, query.AccountID)
		clauses = append(clauses, fmt.Sprintf("account_id=$%d", len(args)))
	}
	if value := strings.TrimSpace(query.Query); value != "" {
		args = append(args, "%"+strings.ToLower(value)+"%")
		i := len(args)
		clauses = append(clauses, fmt.Sprintf(`(LOWER(subject_display) LIKE $%d OR LOWER(subject_key) LIKE $%d OR LOWER(platform) LIKE $%d OR LOWER(api_key_name) LIKE $%d OR LOWER(api_key_masked) LIKE $%d OR LOWER(account_name) LIKE $%d OR EXISTS (
			SELECT 1 FROM prompt_risk_identities pri WHERE pri.subject_type=prompt_risk_events.subject_type AND pri.subject_key=prompt_risk_events.subject_key
			AND (LOWER(pri.external_user_id) LIKE $%d OR LOWER(pri.user_name) LIKE $%d OR LOWER(pri.user_email) LIKE $%d OR LOWER(pri.user_group) LIKE $%d)
		))`, i, i, i, i, i, i, i, i, i, i))
	}
	// Keep the shared filter inline. Materializing the full 30-day event rows
	// duplicates a large TEXT-heavy working set before both aggregate passes;
	// SQLite production databases with dense clean-review history can exhaust
	// the admin request budget even though the final profile set is small.
	rows, err := db.conn.QueryContext(ctx, `WITH filtered_events AS NOT MATERIALIZED (
		SELECT * FROM prompt_risk_events WHERE `+strings.Join(clauses, " AND ")+`
	), profile_aggregates AS (
		SELECT subject_type, subject_key,
			MAX(subject_display) AS subject_display, MAX(platform) AS platform, MAX(CASE WHEN is_person THEN 1 ELSE 0 END) AS is_person,
			MAX(identity_confidence) AS identity_confidence, MAX(created_at) AS latest_at,
			COUNT(*) AS event_count, SUM(CASE WHEN created_at >= $2 THEN 1 ELSE 0 END) AS events_10m,
			SUM(CASE WHEN created_at >= $3 THEN 1 ELSE 0 END) AS events_24h,
			SUM(CASE WHEN created_at >= $4 THEN 1 ELSE 0 END) AS events_7d,
			SUM(CASE WHEN request_risk_score>0 AND event_kind NOT IN ('local_audit_hit', 'review_cleared', 'local_security_context_observed', 'local_block', 'local_block_unverified', 'local_block_cleared') AND created_at >= $3 THEN 1 ELSE 0 END) AS positive_events_24h,
			SUM(CASE WHEN event_kind LIKE 'upstream_cy_%' THEN 1 ELSE 0 END) AS upstream_cy_count,
			SUM(CASE WHEN event_kind='upstream_cy_confirmed_miss' THEN 1 ELSE 0 END) AS confirmed_miss_count,
			SUM(CASE WHEN event_kind LIKE 'local_block%' THEN 1 ELSE 0 END) AS local_block_count,
			SUM(CASE WHEN event_kind='local_warn' THEN 1 ELSE 0 END) AS local_warn_count,
			SUM(CASE WHEN prompt_fingerprint<>'' AND event_kind NOT IN ('local_audit_hit', 'review_cleared', 'local_block', 'local_block_unverified', 'local_block_cleared') THEN 1 ELSE 0 END) AS fingerprint_events,
			COUNT(DISTINCT CASE WHEN prompt_fingerprint<>'' AND event_kind NOT IN ('local_audit_hit', 'review_cleared', 'local_block', 'local_block_unverified', 'local_block_cleared') THEN prompt_fingerprint END) AS distinct_fingerprints,
			MAX(api_key_id) AS api_key_id, MAX(api_key_name) AS api_key_name, MAX(api_key_masked) AS api_key_masked,
			MAX(account_id) AS account_id, MAX(account_name) AS account_name,
			SUM(CASE WHEN event_kind IN ('local_audit_hit', 'review_cleared', 'local_block', 'local_block_unverified', 'local_block_cleared') THEN 0 ELSE request_risk_score * evidence_confidence * identity_confidence * CASE WHEN created_at >= $2 THEN 100 WHEN created_at >= $3 THEN 75 WHEN created_at >= $4 THEN 40 ELSE 15 END END) AS weighted_total,
			SUM(CASE WHEN event_kind LIKE 'local_%' AND event_kind NOT IN ('local_audit_hit', 'local_block', 'local_block_unverified', 'local_block_cleared') THEN request_risk_score * evidence_confidence * identity_confidence * CASE WHEN created_at >= $2 THEN 100 WHEN created_at >= $3 THEN 75 WHEN created_at >= $4 THEN 40 ELSE 15 END ELSE 0 END) AS weighted_local,
			SUM(CASE WHEN event_kind LIKE 'upstream_cy_%' THEN request_risk_score * evidence_confidence * identity_confidence * CASE WHEN created_at >= $2 THEN 100 WHEN created_at >= $3 THEN 75 WHEN created_at >= $4 THEN 40 ELSE 15 END ELSE 0 END) AS weighted_upstream
		FROM filtered_events
		GROUP BY subject_type, subject_key
	), ranked_unverified AS (
		SELECT subject_type, subject_key, identity_confidence, created_at, ROW_NUMBER() OVER (
			PARTITION BY subject_type, subject_key,
			CASE WHEN prompt_fingerprint<>'' THEN prompt_fingerprint WHEN prompt_preview<>'' THEN prompt_preview ELSE source_type || ':' || source_id END
			ORDER BY created_at, id
		) AS unverified_rank
		FROM filtered_events WHERE event_kind IN ('local_block','local_block_unverified')
	), unverified_aggregates AS (
		SELECT subject_type, subject_key,
			SUM(CASE WHEN unverified_rank=1 THEN 8 * 35 * identity_confidence * CASE WHEN created_at >= $2 THEN 100 WHEN created_at >= $3 THEN 75 WHEN created_at >= $4 THEN 40 ELSE 15 END ELSE 0 END) AS weighted_unverified
		FROM ranked_unverified
		GROUP BY subject_type, subject_key
	)
	SELECT profile_aggregates.*, COALESCE(unverified_aggregates.weighted_unverified, 0)
	FROM profile_aggregates
	LEFT JOIN unverified_aggregates ON unverified_aggregates.subject_type=profile_aggregates.subject_type
		AND unverified_aggregates.subject_key=profile_aggregates.subject_key`, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	aggregates := make([]promptRiskAggregate, 0)
	aggregateKeys := make(map[string]struct{})
	for rows.Next() {
		var item promptRiskAggregate
		var latestRaw any
		var isPerson int
		if err := rows.Scan(&item.Profile.SubjectType, &item.Profile.SubjectKey, &item.Profile.SubjectDisplay, &item.Profile.Platform,
			&isPerson, &item.Profile.IdentityConfidence, &latestRaw, &item.Profile.EventCount, &item.Profile.Events10m,
			&item.Profile.Events24h, &item.Profile.Events7d, &item.PositiveEvents24h, &item.Profile.UpstreamCYCount, &item.Profile.ConfirmedMissCount,
			&item.Profile.LocalBlockCount, &item.Profile.LocalWarnCount, &item.FingerprintEvents, &item.Profile.DistinctFingerprints,
			&item.Profile.APIKeyID, &item.Profile.APIKeyName, &item.Profile.APIKeyMasked, &item.Profile.AccountID,
			&item.Profile.AccountName, &item.WeightedTotal, &item.WeightedLocal, &item.WeightedUpstream, &item.WeightedUnverified); err != nil {
			return nil, 0, err
		}
		item.Profile.IsPerson = isPerson != 0
		item.Profile.HasActivity = true
		item.Profile.Events30d = item.Profile.EventCount
		item.Profile.RepeatedFingerprints = max(0, item.FingerprintEvents-item.Profile.DistinctFingerprints)
		item.Profile.LatestAt, err = parsePromptRiskTimeValue(latestRaw)
		if err != nil {
			return nil, 0, err
		}
		aggregateKeys[item.Profile.SubjectType+"\x00"+item.Profile.SubjectKey] = struct{}{}
		finalizePromptRiskAggregate(&item)
		if query.UpstreamCYOnly && item.Profile.UpstreamCYCount == 0 {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(query.ActivityState)) {
		case "active":
			if !item.Profile.HasActivity {
				continue
			}
		case "identity_only":
			if item.Profile.HasActivity {
				continue
			}
		}
		if query.MinScore > 0 && item.Profile.RiskScore < query.MinScore {
			continue
		}
		if level := strings.TrimSpace(query.RiskLevel); level != "" && level != "all" && item.Profile.RiskLevel != level {
			continue
		}
		aggregates = append(aggregates, item)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, err
	}
	if err := rows.Close(); err != nil {
		return nil, 0, err
	}
	activeRestrictions := map[string]promptRiskActiveRestriction{}
	pendingActiveProfiles := make([]promptRiskAggregate, 0)
	if query.PrioritizeActiveLocks || query.ActiveLocksOnly {
		activeRestrictions, err = db.promptRiskActiveRestrictionSubjects(ctx, query.ConversationLockTTL, query.UserCyberCooldownTTL)
		if err != nil {
			return nil, 0, err
		}
		existing := make(map[string]struct{}, len(aggregates))
		for i := range aggregates {
			existing[aggregates[i].Profile.SubjectType+"\x00"+aggregates[i].Profile.SubjectKey] = struct{}{}
		}
		for key, restriction := range activeRestrictions {
			if _, ok := existing[key]; ok || !promptRiskActiveProfileMatchesQuery(restriction.Profile, query, false) {
				continue
			}
			pendingActiveProfiles = append(pendingActiveProfiles, promptRiskAggregate{Profile: restriction.Profile})
		}
	}

	identityKeys := make([]string, 0, len(aggregates)+len(pendingActiveProfiles))
	for i := range aggregates {
		if aggregates[i].Profile.SubjectType == PromptRiskSubjectNewAPIUser {
			identityKeys = append(identityKeys, aggregates[i].Profile.SubjectKey)
		}
	}
	for i := range pendingActiveProfiles {
		if pendingActiveProfiles[i].Profile.SubjectType == PromptRiskSubjectNewAPIUser {
			identityKeys = append(identityKeys, pendingActiveProfiles[i].Profile.SubjectKey)
		}
	}
	identities, err := db.loadPromptRiskIdentities(ctx, identityKeys)
	if err != nil {
		return nil, 0, err
	}
	for i := range aggregates {
		applyPromptRiskIdentityToProfile(&aggregates[i].Profile, identities[aggregates[i].Profile.SubjectKey])
	}
	for i := range pendingActiveProfiles {
		applyPromptRiskIdentityToProfile(&pendingActiveProfiles[i].Profile, identities[pendingActiveProfiles[i].Profile.SubjectKey])
		if promptRiskActiveProfileMatchesQuery(pendingActiveProfiles[i].Profile, query, true) {
			aggregates = append(aggregates, pendingActiveProfiles[i])
		}
	}
	includeIdentityDirectory := !query.UpstreamCYOnly && query.ActivityState != "active" && query.APIKeyID == 0 && query.AccountID == 0 &&
		(strings.TrimSpace(query.SubjectType) == "" || strings.TrimSpace(query.SubjectType) == "all" || strings.TrimSpace(query.SubjectType) == PromptRiskSubjectNewAPIUser) &&
		query.MinScore == 0 && (strings.TrimSpace(query.RiskLevel) == "" || strings.TrimSpace(query.RiskLevel) == "all" || strings.TrimSpace(query.RiskLevel) == PromptRiskLevelLow)
	if includeIdentityDirectory {
		directory, listErr := db.listPromptRiskIdentities(ctx)
		if listErr != nil {
			return nil, 0, listErr
		}
		existing := aggregateKeys
		platformFilter := strings.TrimSpace(query.Platform)
		subjectKeyFilter := strings.TrimSpace(query.SubjectKey)
		queryFilter := strings.ToLower(strings.TrimSpace(query.Query))
		for _, identity := range directory {
			if _, ok := existing[identity.SubjectType+"\x00"+identity.SubjectKey]; ok {
				continue
			}
			if platformFilter != "" && identity.Platform != platformFilter {
				continue
			}
			if subjectKeyFilter != "" && identity.SubjectKey != subjectKeyFilter {
				continue
			}
			if queryFilter != "" {
				haystack := strings.ToLower(strings.Join([]string{identity.SubjectKey, identity.Platform, identity.ExternalUserID, identity.UserName, identity.UserEmail, identity.UserGroup}, "\x00"))
				if !strings.Contains(haystack, queryFilter) {
					continue
				}
			}
			profile := PromptRiskProfile{
				SubjectType: PromptRiskSubjectNewAPIUser, SubjectKey: identity.SubjectKey,
				SubjectDisplay: promptRiskIdentityDisplay(identity), Platform: identity.Platform,
				IsPerson: true, IdentityConfidence: 100, RiskLevel: PromptRiskLevelLow,
				RecommendedActions: []string{"observe"}, HasActivity: false, LatestAt: identity.UpdatedAt,
				ScoreBreakdown: PromptRiskScoreBreakdown{IdentityConfidence: 100},
			}
			applyPromptRiskIdentityToProfile(&profile, identity)
			aggregates = append(aggregates, promptRiskAggregate{Profile: profile})
		}
	}
	if query.ActiveLocksOnly {
		filtered := aggregates[:0]
		for i := range aggregates {
			profileKey := aggregates[i].Profile.SubjectType + "\x00" + aggregates[i].Profile.SubjectKey
			if _, active := activeRestrictions[profileKey]; active {
				filtered = append(filtered, aggregates[i])
			}
		}
		aggregates = filtered
	}
	sort.SliceStable(aggregates, func(i, j int) bool {
		leftKey := aggregates[i].Profile.SubjectType + "\x00" + aggregates[i].Profile.SubjectKey
		rightKey := aggregates[j].Profile.SubjectType + "\x00" + aggregates[j].Profile.SubjectKey
		leftRestriction, leftLocked := activeRestrictions[leftKey]
		rightRestriction, rightLocked := activeRestrictions[rightKey]
		if leftLocked != rightLocked {
			return leftLocked
		}
		if leftLocked && !leftRestriction.LockedAt.Equal(rightRestriction.LockedAt) {
			return leftRestriction.LockedAt.After(rightRestriction.LockedAt)
		}
		if aggregates[i].Profile.RiskScore == aggregates[j].Profile.RiskScore {
			if aggregates[i].Profile.LatestAt.Equal(aggregates[j].Profile.LatestAt) {
				return leftKey < rightKey
			}
			return aggregates[i].Profile.LatestAt.After(aggregates[j].Profile.LatestAt)
		}
		return aggregates[i].Profile.RiskScore > aggregates[j].Profile.RiskScore
	})
	total := len(aggregates)
	start := (query.Page - 1) * query.PageSize
	if start >= total {
		return []*PromptRiskProfile{}, total, nil
	}
	end := min(total, start+query.PageSize)
	items := make([]*PromptRiskProfile, 0, end-start)
	for i := start; i < end; i++ {
		profile := aggregates[i].Profile
		items = append(items, &profile)
	}
	return items, total, nil
}

func finalizePromptRiskAggregate(item *promptRiskAggregate) {
	if item == nil {
		return
	}
	points := float64(item.WeightedTotal) / 1_000_000
	base := saturationRiskScore(points)
	unverified := min(14, saturationRiskScore(float64(item.WeightedUnverified)/1_000_000))
	recurrence := min(20, item.Profile.RepeatedFingerprints*3+max(0, item.PositiveEvents24h-1)*2+max(0, item.Profile.ConfirmedMissCount-1)*3)
	item.Profile.RiskScore = min(100, base+recurrence+unverified)
	item.Profile.RiskLevel = promptRiskLevel(item.Profile.RiskScore)
	item.Profile.RecommendedActions = promptRiskRecommendations(item.Profile)
	item.Profile.ScoreBreakdown = PromptRiskScoreBreakdown{
		LocalSignal:        min(100, saturationRiskScore(float64(item.WeightedLocal)/1_000_000)+unverified),
		UpstreamSignal:     saturationRiskScore(float64(item.WeightedUpstream) / 1_000_000),
		Recurrence:         min(100, recurrence*5),
		IdentityConfidence: item.Profile.IdentityConfidence,
	}
}

func saturationRiskScore(points float64) int {
	if points <= 0 {
		return 0
	}
	return promptRiskClamp(int(math.Round(100 * (1 - math.Exp(-points/65)))))
}

func promptRiskLevel(score int) string {
	switch {
	case score >= 80:
		return PromptRiskLevelCritical
	case score >= 60:
		return PromptRiskLevelHigh
	case score >= 35:
		return PromptRiskLevelElevated
	case score >= 15:
		return PromptRiskLevelObserved
	default:
		return PromptRiskLevelLow
	}
}

func promptRiskRecommendations(profile PromptRiskProfile) []string {
	switch profile.RiskLevel {
	case PromptRiskLevelCritical:
		if profile.SubjectType == PromptRiskSubjectUpstreamAccount {
			return []string{"account_rotation_review", "enhanced_review"}
		}
		return []string{"temporary_restriction_review", "rate_limit", "enhanced_review"}
	case PromptRiskLevelHigh:
		if profile.SubjectType == PromptRiskSubjectAPIKey || profile.SubjectType == PromptRiskSubjectClientIP {
			return []string{"require_signed_identity", "rate_limit", "enhanced_review"}
		}
		if profile.SubjectType == PromptRiskSubjectUpstreamAccount {
			return []string{"account_routing_review", "enhanced_review"}
		}
		return []string{"rate_limit", "enhanced_review"}
	case PromptRiskLevelElevated:
		return []string{"enhanced_review", "monitor"}
	case PromptRiskLevelObserved:
		return []string{"monitor"}
	default:
		return []string{"observe"}
	}
}

func (db *DB) GetPromptRiskProfile(ctx context.Context, subjectType, subjectKey string) (*PromptRiskProfile, error) {
	items, _, err := db.ListPromptRiskProfiles(ctx, PromptRiskProfileQuery{Page: 1, PageSize: 1, SubjectType: strings.TrimSpace(subjectType), SubjectKey: strings.TrimSpace(subjectKey)})
	if err != nil {
		return nil, err
	}
	for _, item := range items {
		if item.SubjectKey == strings.TrimSpace(subjectKey) {
			return item, nil
		}
	}
	return nil, sql.ErrNoRows
}

func (db *DB) ListPromptRiskEvents(ctx context.Context, subjectType, subjectKey string, query PromptRiskEventQuery) ([]*PromptRiskEvent, int, error) {
	if query.Page <= 0 {
		query.Page = 1
	}
	if query.PageSize <= 0 || query.PageSize > 200 {
		query.PageSize = 20
	}
	subjectType, subjectKey = strings.TrimSpace(subjectType), strings.TrimSpace(subjectKey)
	var total int
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM prompt_risk_events WHERE subject_type=$1 AND subject_key=$2`, subjectType, subjectKey).Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := db.conn.QueryContext(ctx, `SELECT id, created_at, source_type, source_id, incident_id, prompt_filter_log_id,
		request_correlation_id, subject_type, subject_key, subject_display, platform, is_person, identity_confidence,
		event_kind, request_risk_score, evidence_confidence, reason_code, action, local_outcome, local_comparison,
		endpoint, model, prompt_fingerprint, prompt_preview, api_key_id, api_key_name, api_key_masked, account_id, account_name
	FROM prompt_risk_events WHERE subject_type=$1 AND subject_key=$2 ORDER BY id DESC LIMIT $3 OFFSET $4`,
		subjectType, subjectKey, query.PageSize, (query.Page-1)*query.PageSize)
	if err != nil {
		return nil, 0, err
	}
	items := make([]*PromptRiskEvent, 0, query.PageSize)
	for rows.Next() {
		item := &PromptRiskEvent{}
		var createdRaw any
		if err := rows.Scan(&item.ID, &createdRaw, &item.SourceType, &item.SourceID, &item.IncidentID, &item.PromptFilterLogID,
			&item.RequestCorrelationID, &item.SubjectType, &item.SubjectKey, &item.SubjectDisplay, &item.Platform, &item.IsPerson,
			&item.IdentityConfidence, &item.EventKind, &item.RequestRiskScore, &item.EvidenceConfidence, &item.ReasonCode,
			&item.Action, &item.LocalOutcome, &item.LocalComparison, &item.Endpoint, &item.Model, &item.PromptFingerprint,
			&item.PromptPreview, &item.APIKeyID, &item.APIKeyName, &item.APIKeyMasked, &item.AccountID, &item.AccountName); err != nil {
			return nil, 0, err
		}
		item.CreatedAt, err = parsePromptRiskTimeValue(createdRaw)
		if err != nil {
			return nil, 0, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, 0, err
	}
	if err := rows.Close(); err != nil {
		return nil, 0, err
	}
	if subjectType == PromptRiskSubjectNewAPIUser {
		identities, err := db.loadPromptRiskIdentities(ctx, []string{subjectKey})
		if err != nil {
			return nil, 0, err
		}
		for _, item := range items {
			applyPromptRiskIdentityToEvent(item, identities[subjectKey])
		}
	}
	return items, total, nil
}

func (db *DB) ClearPromptRiskEvents(ctx context.Context) error {
	if db == nil {
		return nil
	}
	if db.isSQLite() {
		if _, err := db.conn.ExecContext(ctx, `DELETE FROM prompt_risk_events; DELETE FROM prompt_risk_event_sources`); err != nil {
			return err
		}
		_, err := db.conn.ExecContext(ctx, `DELETE FROM sqlite_sequence WHERE name='prompt_risk_events'`)
		return err
	}
	_, err := db.conn.ExecContext(ctx, `TRUNCATE TABLE prompt_risk_events, prompt_risk_event_sources RESTART IDENTITY`)
	return err
}
