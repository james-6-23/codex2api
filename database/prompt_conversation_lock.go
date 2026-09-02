package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

const (
	PromptConversationLockStatusActive                  = "active"
	PromptConversationLockStatusUnlocked                = "unlocked"
	PromptConversationLockCacheNamespace                = "prompt-conversation-lock"
	PromptConversationRestrictionScopeConversation      = "conversation"
	PromptConversationRestrictionScopeUserCooldown      = "user_cooldown"
	PromptConversationRestrictionScopeFingerprintReplay = "fingerprint_replay"
	// PromptUserCyberCooldownTTL is retained for source compatibility with
	// integrations that referenced the old default. Runtime enforcement reads
	// the configurable prompt-filter setting instead.
	PromptUserCyberCooldownTTL = 30 * time.Minute

	// 锁定身份来源。NewAPI 透传签名是首选,但它不是唯一可信的会话标识:
	// Codex 客户端请求自带 session-id / x-codex-* 标识,配合下游 API Key 足以
	// 稳定标识一个会话。没有 NewAPI 的部署必须也能锁定,否则拦截只能事后止损。
	//
	// 当 identity_kind 为 codex_session 时,newapi_user_id 列存放降级主体
	// (形如 apikey:<id>),platform 列存放固定标识 codex-local。
	// fingerprint_replay 使用同一张锁表,但只绑定 API Key、客户端 IP 哈希
	// 和精确 Prompt 指纹,不能解释为人员身份。
	PromptConversationLockIdentityNewAPI            = "newapi"
	PromptConversationLockIdentityCodexSession      = "codex_session"
	PromptConversationLockIdentityFingerprintReplay = "fingerprint_replay"
)

func normalizePromptConversationLockIdentityKind(kind string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "", PromptConversationLockIdentityNewAPI:
		return PromptConversationLockIdentityNewAPI, true
	case PromptConversationLockIdentityCodexSession:
		return PromptConversationLockIdentityCodexSession, true
	case PromptConversationLockIdentityFingerprintReplay:
		return PromptConversationLockIdentityFingerprintReplay, true
	default:
		return "", false
	}
}

type PromptConversationLock struct {
	ID                 int64      `json:"id"`
	LockKey            string     `json:"lock_key"`
	Status             string     `json:"status"`
	IdentityKind       string     `json:"identity_kind"`
	Platform           string     `json:"platform"`
	NewAPIUserID       string     `json:"newapi_user_id"`
	SessionFingerprint string     `json:"session_fingerprint"`
	SessionHash        string     `json:"session_hash"`
	IncidentID         string     `json:"incident_id,omitempty"`
	DecisionID         string     `json:"decision_id"`
	RequestID          string     `json:"request_id,omitempty"`
	ReasonCode         string     `json:"reason_code"`
	Endpoint           string     `json:"endpoint,omitempty"`
	Model              string     `json:"model,omitempty"`
	TriggerCount       int64      `json:"trigger_count"`
	UnlockCount        int64      `json:"unlock_count"`
	LockedAt           time.Time  `json:"locked_at"`
	UnlockedAt         *time.Time `json:"unlocked_at,omitempty"`
	UnlockReason       string     `json:"unlock_reason,omitempty"`
	RestrictionScope   string     `json:"restriction_scope,omitempty"`
	ExpiresAt          *time.Time `json:"expires_at,omitempty"`
	RemainingSeconds   int64      `json:"remaining_seconds,omitempty"`
	CreatedAt          time.Time  `json:"created_at"`
	UpdatedAt          time.Time  `json:"updated_at"`
}

type PromptConversationLockInput struct {
	LockKey            string
	IdentityKind       string
	Platform           string
	NewAPIUserID       string
	SessionFingerprint string
	SessionHash        string
	IncidentID         string
	DecisionID         string
	RequestID          string
	ReasonCode         string
	Endpoint           string
	Model              string
	LockedAt           time.Time
}

var promptConversationLockSchemaMu sync.Mutex

func (db *DB) ensurePromptConversationLocksTable(ctx context.Context) error {
	if db == nil {
		return errors.New("database unavailable")
	}
	promptConversationLockSchemaMu.Lock()
	defer promptConversationLockSchemaMu.Unlock()
	idType := "BIGSERIAL PRIMARY KEY"
	timeType := "TIMESTAMPTZ"
	if db.isSQLite() {
		idType = "INTEGER PRIMARY KEY AUTOINCREMENT"
		timeType = "TIMESTAMP"
	}
	ddl := fmt.Sprintf(`CREATE TABLE IF NOT EXISTS prompt_conversation_locks (
		id %s,
		lock_key VARCHAR(64) NOT NULL UNIQUE,
		status VARCHAR(24) NOT NULL DEFAULT 'active',
		identity_kind VARCHAR(24) NOT NULL DEFAULT 'newapi',
		platform VARCHAR(100) NOT NULL DEFAULT '',
		newapi_user_id VARCHAR(255) NOT NULL DEFAULT '',
		session_fingerprint VARCHAR(32) NOT NULL DEFAULT '',
		session_hash VARCHAR(64) NOT NULL DEFAULT '',
		incident_id VARCHAR(64) NOT NULL DEFAULT '',
		decision_id VARCHAR(128) NOT NULL DEFAULT '',
		request_id VARCHAR(255) NOT NULL DEFAULT '',
		reason_code VARCHAR(100) NOT NULL DEFAULT '',
		endpoint VARCHAR(255) NOT NULL DEFAULT '',
		model VARCHAR(128) NOT NULL DEFAULT '',
		trigger_count BIGINT NOT NULL DEFAULT 1,
		unlock_count BIGINT NOT NULL DEFAULT 0,
		locked_at %s NOT NULL,
		unlocked_at %s NULL,
		unlock_reason TEXT NOT NULL DEFAULT '',
		created_at %s NOT NULL,
		updated_at %s NOT NULL
	)`, idType, timeType, timeType, timeType, timeType)
	for _, statement := range []string{
		ddl,
		`CREATE INDEX IF NOT EXISTS idx_prompt_conversation_locks_status ON prompt_conversation_locks(status, updated_at)`,
		`CREATE INDEX IF NOT EXISTS idx_prompt_conversation_locks_session ON prompt_conversation_locks(session_hash, status)`,
		`CREATE INDEX IF NOT EXISTS idx_prompt_conversation_locks_user_cooldown ON prompt_conversation_locks(platform, newapi_user_id, status, locked_at)`,
	} {
		if _, err := db.conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	// 滚动升级:已存在的锁表缺少 identity_kind 列。旧数据全部来自 NewAPI 签名
	// 路径,因此默认值保持 newapi,语义与升级前完全一致。
	if db.isSQLite() {
		return db.ensureSQLiteColumn(ctx, "prompt_conversation_locks", "identity_kind", "TEXT NOT NULL DEFAULT 'newapi'")
	}
	_, err := db.conn.ExecContext(ctx, `ALTER TABLE prompt_conversation_locks ADD COLUMN IF NOT EXISTS identity_kind VARCHAR(24) NOT NULL DEFAULT 'newapi'`)
	return err
}

const promptConversationLockSelect = `SELECT id, lock_key, status, identity_kind, platform, newapi_user_id,
	session_fingerprint, session_hash, incident_id, decision_id, request_id, reason_code,
	endpoint, model, trigger_count, unlock_count, locked_at, unlocked_at, unlock_reason,
	created_at, updated_at FROM prompt_conversation_locks`

func scanPromptConversationLock(scanner interface{ Scan(...any) error }) (*PromptConversationLock, error) {
	item := &PromptConversationLock{}
	var lockedAt, unlockedAt, createdAt, updatedAt any
	if err := scanner.Scan(
		&item.ID, &item.LockKey, &item.Status, &item.IdentityKind, &item.Platform, &item.NewAPIUserID,
		&item.SessionFingerprint, &item.SessionHash, &item.IncidentID, &item.DecisionID,
		&item.RequestID, &item.ReasonCode, &item.Endpoint, &item.Model, &item.TriggerCount,
		&item.UnlockCount, &lockedAt, &unlockedAt, &item.UnlockReason, &createdAt, &updatedAt,
	); err != nil {
		return nil, err
	}
	var err error
	if item.LockedAt, err = parsePromptRiskTimeValue(lockedAt); err != nil {
		return nil, err
	}
	if item.CreatedAt, err = parsePromptRiskTimeValue(createdAt); err != nil {
		return nil, err
	}
	if item.UpdatedAt, err = parsePromptRiskTimeValue(updatedAt); err != nil {
		return nil, err
	}
	if unlockedAt != nil {
		if parsed, parseErr := parsePromptRiskTimeValue(unlockedAt); parseErr == nil {
			item.UnlockedAt = &parsed
		}
	}
	return item, nil
}

func normalizePromptConversationLockInput(input PromptConversationLockInput) (PromptConversationLockInput, error) {
	kind, kindOK := normalizePromptConversationLockIdentityKind(input.IdentityKind)
	if !kindOK {
		return PromptConversationLockInput{}, errors.New("unknown prompt conversation lock identity kind")
	}
	input.IdentityKind = kind
	input.LockKey = strings.ToLower(strings.TrimSpace(input.LockKey))
	input.Platform = strings.ToLower(truncateCandidateRunes(strings.TrimSpace(input.Platform), 100))
	input.NewAPIUserID = truncateCandidateRunes(strings.TrimSpace(input.NewAPIUserID), 255)
	input.SessionFingerprint = strings.ToLower(strings.TrimSpace(input.SessionFingerprint))
	input.SessionHash = strings.ToLower(strings.TrimSpace(input.SessionHash))
	input.IncidentID = truncateCandidateRunes(strings.TrimSpace(input.IncidentID), 64)
	input.DecisionID = truncateCandidateRunes(strings.TrimSpace(input.DecisionID), 128)
	input.RequestID = truncateCandidateRunes(strings.TrimSpace(input.RequestID), 255)
	input.ReasonCode = truncateCandidateRunes(strings.TrimSpace(input.ReasonCode), 100)
	input.Endpoint = truncateCandidateRunes(strings.TrimSpace(input.Endpoint), 255)
	input.Model = truncateCandidateRunes(strings.TrimSpace(input.Model), 128)
	invalidSessionIdentity := input.SessionFingerprint == "" && input.SessionHash != "" ||
		input.SessionFingerprint != "" && len(input.SessionFingerprint) != 32
	// 降级的 Codex 会话身份必须携带 32 位指纹，防止空标识锁住共享 API Key。
	// 已验证的 NewAPI 用户级冷却锁有意不绑定会话，因此允许指纹与会话哈希同时为空。
	if input.IdentityKind == PromptConversationLockIdentityCodexSession || input.IdentityKind == PromptConversationLockIdentityFingerprintReplay {
		invalidSessionIdentity = len(input.SessionFingerprint) != 32
	}
	if len(input.LockKey) != 64 || input.Platform == "" || input.NewAPIUserID == "" || input.DecisionID == "" || invalidSessionIdentity {
		return PromptConversationLockInput{}, errors.New("invalid prompt conversation lock identity")
	}
	if input.LockedAt.IsZero() {
		input.LockedAt = time.Now().UTC()
	} else {
		input.LockedAt = input.LockedAt.UTC()
	}
	return input, nil
}

// LockPromptConversation activates a lock for a new verified upstream CYB
// decision. Replaying the same decision is idempotent and cannot re-lock a
// conversation that an administrator has already unlocked.
func (db *DB) LockPromptConversation(ctx context.Context, raw PromptConversationLockInput) (*PromptConversationLock, bool, error) {
	input, err := normalizePromptConversationLockInput(raw)
	if err != nil {
		return nil, false, err
	}
	if err := db.ensurePromptConversationLocksTable(ctx); err != nil {
		return nil, false, err
	}
	now := time.Now().UTC()
	query := `INSERT INTO prompt_conversation_locks (
		lock_key, status, identity_kind, platform, newapi_user_id, session_fingerprint, session_hash,
		incident_id, decision_id, request_id, reason_code, endpoint, model, trigger_count,
		unlock_count, locked_at, unlocked_at, unlock_reason, created_at, updated_at
	) VALUES ($1,'active',$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,1,0,$13,NULL,'',$14,$14)
	ON CONFLICT(lock_key) DO UPDATE SET
		status='active', identity_kind=excluded.identity_kind, platform=excluded.platform,
		newapi_user_id=excluded.newapi_user_id,
		session_fingerprint=excluded.session_fingerprint, session_hash=excluded.session_hash,
		incident_id=excluded.incident_id, decision_id=excluded.decision_id,
		request_id=excluded.request_id, reason_code=excluded.reason_code,
		endpoint=excluded.endpoint, model=excluded.model,
		trigger_count=prompt_conversation_locks.trigger_count+1,
		locked_at=excluded.locked_at, unlocked_at=NULL, unlock_reason='', updated_at=excluded.updated_at
	WHERE prompt_conversation_locks.decision_id<>excluded.decision_id
	RETURNING id, lock_key, status, identity_kind, platform, newapi_user_id, session_fingerprint, session_hash,
		incident_id, decision_id, request_id, reason_code, endpoint, model, trigger_count,
		unlock_count, locked_at, unlocked_at, unlock_reason, created_at, updated_at`
	item, scanErr := scanPromptConversationLock(db.conn.QueryRowContext(ctx, query,
		input.LockKey, input.IdentityKind, input.Platform, input.NewAPIUserID, input.SessionFingerprint, input.SessionHash,
		input.IncidentID, input.DecisionID, input.RequestID, input.ReasonCode, input.Endpoint,
		input.Model, input.LockedAt, now,
	))
	if scanErr == nil {
		return item, true, nil
	}
	if !errors.Is(scanErr, sql.ErrNoRows) {
		return nil, false, scanErr
	}
	item, err = db.GetPromptConversationLock(ctx, input.LockKey)
	return item, false, err
}

func (db *DB) GetPromptConversationLock(ctx context.Context, lockKey string) (*PromptConversationLock, error) {
	if err := db.ensurePromptConversationLocksTable(ctx); err != nil {
		return nil, err
	}
	return scanPromptConversationLock(db.conn.QueryRowContext(ctx, promptConversationLockSelect+` WHERE lock_key=$1`, strings.ToLower(strings.TrimSpace(lockKey))))
}

func (db *DB) GetActivePromptConversationLock(ctx context.Context, lockKey string) (*PromptConversationLock, error) {
	if err := db.ensurePromptConversationLocksTable(ctx); err != nil {
		return nil, err
	}
	return scanPromptConversationLock(db.conn.QueryRowContext(ctx, promptConversationLockSelect+` WHERE lock_key=$1 AND status='active'`, strings.ToLower(strings.TrimSpace(lockKey))))
}

func (db *DB) GetActivePromptConversationLockWithTTL(ctx context.Context, lockKey string, ttl time.Duration) (*PromptConversationLock, error) {
	item, err := db.GetActivePromptConversationLock(ctx, lockKey)
	if err != nil || ttl <= 0 {
		return item, err
	}
	return db.expirePromptConversationLockIfNeeded(ctx, item, ttl, func() (*PromptConversationLock, error) {
		return db.GetActivePromptConversationLock(ctx, lockKey)
	})
}

// GetActivePromptConversationRestriction resolves the strongest active CYB
// restriction for a verified request identity in one database lookup. An exact
// conversation lock takes precedence; otherwise the latest upstream CYB from
// the same verified platform user is returned while the bounded user cooldown
// is live. Local rule locks never expand into cross-session user cooldowns.
func (db *DB) GetActivePromptConversationRestriction(ctx context.Context, lockKey, platform, newAPIUserID string, conversationTTL, userCooldown time.Duration) (*PromptConversationLock, bool, error) {
	if err := db.ensurePromptConversationLocksTable(ctx); err != nil {
		return nil, false, err
	}
	lockKey = strings.ToLower(strings.TrimSpace(lockKey))
	platform = strings.ToLower(strings.TrimSpace(platform))
	newAPIUserID = strings.TrimSpace(newAPIUserID)
	if (lockKey != "" && len(lockKey) != 64) || platform == "" || newAPIUserID == "" {
		return nil, false, sql.ErrNoRows
	}
	now := time.Now().UTC()
	conversationCutoff := time.Unix(0, 0).UTC()
	if conversationTTL > 0 {
		conversationCutoff = now.Add(-conversationTTL)
	}
	userCutoff := now
	if userCooldown > 0 {
		userCutoff = now.Add(-userCooldown)
	}
	query := promptConversationLockSelect + ` WHERE status='active' AND (
		($1<>'' AND lock_key=$1 AND locked_at>$4) OR
		(platform=$2 AND newapi_user_id=$3 AND reason_code='upstream_cyber_policy' AND locked_at>$5)
	) ORDER BY CASE WHEN $1<>'' AND lock_key=$1 THEN 0 ELSE 1 END, locked_at DESC LIMIT 1`
	item, err := scanPromptConversationLock(db.conn.QueryRowContext(ctx, query,
		lockKey, platform, newAPIUserID, conversationCutoff, userCutoff,
	))
	if err != nil {
		return nil, false, err
	}
	return item, lockKey != "" && item.LockKey == lockKey, nil
}

func (db *DB) GetActivePromptConversationLockBySessionHash(ctx context.Context, sessionHash string) (*PromptConversationLock, error) {
	if err := db.ensurePromptConversationLocksTable(ctx); err != nil {
		return nil, err
	}
	return scanPromptConversationLock(db.conn.QueryRowContext(ctx, promptConversationLockSelect+` WHERE session_hash=$1 AND status='active' ORDER BY updated_at DESC LIMIT 1`, strings.ToLower(strings.TrimSpace(sessionHash))))
}

// HasActivePromptFingerprintReplayLocks reports whether any fingerprint replay
// cooldown row is still live within ttl. The relay hot path uses it as a cheap
// existence gate: deriving a replay fingerprint requires a full request
// envelope build, which is wasted work while no cooldown exists anywhere.
func (db *DB) HasActivePromptFingerprintReplayLocks(ctx context.Context, ttl time.Duration) (bool, error) {
	if err := db.ensurePromptConversationLocksTable(ctx); err != nil {
		return false, err
	}
	args := []any{PromptConversationLockIdentityFingerprintReplay}
	query := `SELECT 1 FROM prompt_conversation_locks WHERE status='active' AND identity_kind=$1`
	if ttl > 0 {
		args = append(args, time.Now().UTC().Add(-ttl))
		query += ` AND locked_at>$2`
	}
	var one int
	err := db.conn.QueryRowContext(ctx, query+` LIMIT 1`, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func (db *DB) GetActivePromptConversationLockBySessionHashWithTTL(ctx context.Context, sessionHash string, ttl time.Duration) (*PromptConversationLock, error) {
	item, err := db.GetActivePromptConversationLockBySessionHash(ctx, sessionHash)
	if err != nil || ttl <= 0 {
		return item, err
	}
	return db.expirePromptConversationLockIfNeeded(ctx, item, ttl, func() (*PromptConversationLock, error) {
		return db.GetActivePromptConversationLockBySessionHash(ctx, sessionHash)
	})
}

func (db *DB) expirePromptConversationLockIfNeeded(ctx context.Context, item *PromptConversationLock, ttl time.Duration, reload func() (*PromptConversationLock, error)) (*PromptConversationLock, error) {
	if item == nil || ttl <= 0 {
		return item, nil
	}
	cutoff := time.Now().UTC().Add(-ttl)
	if item.LockedAt.After(cutoff) {
		return item, nil
	}
	result, err := db.conn.ExecContext(ctx, `UPDATE prompt_conversation_locks SET
		status='unlocked', unlock_count=unlock_count+1, unlocked_at=$2,
		unlock_reason=$3, updated_at=$2
		WHERE id=$1 AND status='active' AND locked_at<=$4`,
		item.ID, time.Now().UTC(), "automatic expiry after conversation-lock TTL", cutoff)
	if err != nil {
		return nil, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if rows > 0 {
		return nil, sql.ErrNoRows
	}
	return reload()
}

func (db *DB) UnlockPromptConversation(ctx context.Context, lockKey, reason string) (*PromptConversationLock, error) {
	if err := db.ensurePromptConversationLocksTable(ctx); err != nil {
		return nil, err
	}
	reason = truncateCandidateRunes(strings.TrimSpace(reason), 1000)
	if reason == "" {
		reason = "管理员主动解锁"
	}
	now := time.Now().UTC()
	result, err := db.conn.ExecContext(ctx, `UPDATE prompt_conversation_locks SET
		status='unlocked', unlock_count=unlock_count+1, unlocked_at=$2,
		unlock_reason=$3, updated_at=$2 WHERE lock_key=$1 AND status='active'`,
		strings.ToLower(strings.TrimSpace(lockKey)), now, reason)
	if err != nil {
		return nil, err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return nil, err
	}
	if rows == 0 {
		return nil, sql.ErrNoRows
	}
	return db.GetPromptConversationLock(ctx, lockKey)
}

// UnlockPromptConversationUserCooldown releases every active CYB restriction
// for one verified platform user. Clearing the complete user scope prevents an
// older active conversation row from immediately re-applying the cooldown.
// The returned keys allow callers to invalidate any exact-session cache rows.
func (db *DB) UnlockPromptConversationUserCooldown(ctx context.Context, platform, newAPIUserID, reason string) ([]string, error) {
	if err := db.ensurePromptConversationLocksTable(ctx); err != nil {
		return nil, err
	}
	platform = strings.ToLower(truncateCandidateRunes(strings.TrimSpace(platform), 100))
	newAPIUserID = truncateCandidateRunes(strings.TrimSpace(newAPIUserID), 255)
	reason = truncateCandidateRunes(strings.TrimSpace(reason), 1000)
	if platform == "" || newAPIUserID == "" {
		return nil, sql.ErrNoRows
	}
	if reason == "" {
		reason = "管理员解除用户安全冷却"
	}
	now := time.Now().UTC()
	rows, err := db.conn.QueryContext(ctx, `UPDATE prompt_conversation_locks SET
		status='unlocked', unlock_count=unlock_count+1, unlocked_at=$3,
		unlock_reason=$4, updated_at=$3
		WHERE platform=$1 AND newapi_user_id=$2 AND status='active'
		RETURNING lock_key`, platform, newAPIUserID, now, reason)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	keys := make([]string, 0, 4)
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, sql.ErrNoRows
	}
	return keys, nil
}
