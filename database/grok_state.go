package database

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	GrokFactUser      = "user"
	GrokFactSettings  = "settings"
	GrokFactBilling   = "billing"
	GrokFactAutoTopup = "auto_topup"

	GrokProtocolResponses       = "responses"
	GrokProtocolChatCompletions = "chat_completions"
	GrokProtocolMessages        = "messages"

	dataMigrationGrokStateBackfillV1     = "20260813_grok_state_backfill_v1"
	grokStateHistoricalBackfillBatchSize = 500
)

// GrokAccountFact is one independently refreshed control-plane observation.
// Payload and FieldPresence are deliberately separate: proto/JSON zero values
// must not turn an absent field into an explicit false or zero.
type GrokAccountFact struct {
	AccountID            int64             `json:"account_id"`
	Kind                 string            `json:"kind"`
	CredentialGeneration int64             `json:"credential_generation"`
	Status               string            `json:"status"`
	HTTPStatus           int               `json:"http_status"`
	Source               string            `json:"source"`
	Payload              map[string]any    `json:"payload"`
	FieldPresence        map[string]string `json:"field_presence"`
	ObservedAt           time.Time         `json:"observed_at"`
	ExpiresAt            time.Time         `json:"expires_at"`
	UpdatedAt            time.Time         `json:"updated_at"`
}

type GrokModelCatalogSnapshot struct {
	AccountID            int64     `json:"account_id"`
	Origin               string    `json:"origin"`
	CredentialGeneration int64     `json:"credential_generation"`
	AuthKind             string    `json:"auth_kind"`
	Status               string    `json:"status"`
	HTTPETag             string    `json:"http_etag,omitempty"`
	ETagHint             string    `json:"etag_hint,omitempty"`
	ETagHintObservedAt   time.Time `json:"etag_hint_observed_at,omitempty"`
	ObservedAt           time.Time `json:"observed_at"`
	ExpiresAt            time.Time `json:"expires_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

type GrokModelCatalogItem struct {
	AccountID               int64             `json:"account_id"`
	Origin                  string            `json:"origin"`
	ModelID                 string            `json:"model_id"`
	CredentialGeneration    int64             `json:"credential_generation"`
	DisplayName             string            `json:"display_name,omitempty"`
	Description             string            `json:"description,omitempty"`
	BaseURL                 string            `json:"base_url,omitempty"`
	APIBaseURL              string            `json:"api_base_url,omitempty"`
	APIBackend              string            `json:"api_backend,omitempty"`
	ContextWindow           int64             `json:"context_window,omitempty"`
	MaxOutputTokens         int64             `json:"max_output_tokens,omitempty"`
	ReasoningEffort         string            `json:"reasoning_effort,omitempty"`
	ReasoningEfforts        []string          `json:"reasoning_efforts,omitempty"`
	SupportsReasoningEffort bool              `json:"supports_reasoning_effort"`
	SupportsBackendSearch   bool              `json:"supports_backend_search"`
	StreamToolCalls         bool              `json:"stream_tool_calls"`
	SupportedInAPI          bool              `json:"supported_in_api"`
	Hidden                  bool              `json:"hidden"`
	ExtraHeaders            map[string]string `json:"extra_headers,omitempty"`
	FieldPresence           map[string]string `json:"field_presence,omitempty"`
	FirstSeenAt             time.Time         `json:"first_seen_at"`
}

type GrokModelCatalog struct {
	Snapshot GrokModelCatalogSnapshot `json:"snapshot"`
	Items    []GrokModelCatalogItem   `json:"items"`
}

type GrokModelCapability struct {
	AccountID            int64     `json:"account_id"`
	ModelID              string    `json:"model_id"`
	Origin               string    `json:"origin"`
	Protocol             string    `json:"protocol"`
	CredentialGeneration int64     `json:"credential_generation"`
	Status               string    `json:"status"`
	HTTPStatus           int       `json:"http_status"`
	ProviderCode         string    `json:"provider_code,omitempty"`
	Source               string    `json:"source"`
	RetryAfterSeconds    int64     `json:"retry_after_seconds,omitempty"`
	ObservedAt           time.Time `json:"observed_at"`
	ExpiresAt            time.Time `json:"expires_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

// GrokAccountIdentitySummary contains only non-secret labels needed by the
// admin details view. None of the credential documents or token material is
// returned by GetGrokAccountState.
type GrokAccountIdentitySummary struct {
	CredentialFamilyID string `json:"credential_family_id"`
	ArchivePlan        string `json:"archive_plan,omitempty"`
	ArchivePlanSource  string `json:"archive_plan_source,omitempty"`
	JWTTier            string `json:"jwt_tier,omitempty"`
	JWTTierTrust       string `json:"jwt_tier_trust,omitempty"`
}

type GrokAccountState struct {
	AccountID            int64                      `json:"account_id"`
	CredentialGeneration int64                      `json:"credential_generation"`
	Identity             GrokAccountIdentitySummary `json:"identity"`
	Facts                map[string]GrokAccountFact `json:"facts"`
	Catalogs             []GrokModelCatalog         `json:"catalogs"`
	Capabilities         []GrokModelCapability      `json:"capabilities"`
}

// ensureGrokStateSchema keeps schema creation separate from the potentially
// expensive historical data migration. The completion marker makes normal
// startups DDL-only after the one-time backfill has finished.
func (db *DB) ensureGrokStateSchema(ctx context.Context) error {
	if db == nil || db.conn == nil {
		return errors.New("database is not initialized")
	}
	if err := db.ensureGrokStateTables(ctx); err != nil {
		return err
	}
	return db.ensureGrokStateHistoricalBackfill(ctx)
}

// ensureGrokStateTables is intentionally additive. Older binaries can keep
// reading credentials while the new tables become authoritative.
func (db *DB) ensureGrokStateTables(ctx context.Context) error {
	if db.isSQLite() {
		if err := db.ensureSQLiteColumn(ctx, "accounts", "credential_generation", "INTEGER NOT NULL DEFAULT 1"); err != nil {
			return err
		}
		if err := db.ensureSQLiteColumn(ctx, "accounts", "credential_family_id", "TEXT NOT NULL DEFAULT ''"); err != nil {
			return err
		}
		_, err := db.conn.ExecContext(ctx, `
			CREATE TABLE IF NOT EXISTS grok_account_fact_snapshots (
				account_id INTEGER NOT NULL, fact_kind TEXT NOT NULL,
				credential_generation INTEGER NOT NULL, status TEXT NOT NULL DEFAULT 'unknown',
				http_status INTEGER NOT NULL DEFAULT 0, source TEXT NOT NULL DEFAULT '',
				payload_json TEXT NOT NULL DEFAULT '{}', field_presence_json TEXT NOT NULL DEFAULT '{}',
				observed_at TIMESTAMP NOT NULL, expires_at TIMESTAMP NOT NULL,
				updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
				PRIMARY KEY (account_id, fact_kind)
			);
			CREATE TABLE IF NOT EXISTS grok_model_catalog_snapshots (
				account_id INTEGER NOT NULL, origin TEXT NOT NULL,
				credential_generation INTEGER NOT NULL, auth_kind TEXT NOT NULL DEFAULT '',
				status TEXT NOT NULL DEFAULT 'unknown', http_etag TEXT NOT NULL DEFAULT '',
				etag_hint TEXT NOT NULL DEFAULT '', etag_hint_observed_at TIMESTAMP NULL,
				observed_at TIMESTAMP NOT NULL, expires_at TIMESTAMP NOT NULL,
				updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
				PRIMARY KEY (account_id, origin)
			);
			CREATE TABLE IF NOT EXISTS grok_model_catalog_items (
				account_id INTEGER NOT NULL, origin TEXT NOT NULL, model_id TEXT NOT NULL,
				credential_generation INTEGER NOT NULL, display_name TEXT NOT NULL DEFAULT '',
				description TEXT NOT NULL DEFAULT '', base_url TEXT NOT NULL DEFAULT '',
				api_base_url TEXT NOT NULL DEFAULT '', api_backend TEXT NOT NULL DEFAULT '',
				context_window INTEGER NOT NULL DEFAULT 0, max_output_tokens INTEGER NOT NULL DEFAULT 0,
				reasoning_effort TEXT NOT NULL DEFAULT '', reasoning_efforts_json TEXT NOT NULL DEFAULT '[]',
				supports_reasoning_effort INTEGER NOT NULL DEFAULT 0,
				supports_backend_search INTEGER NOT NULL DEFAULT 0, stream_tool_calls INTEGER NOT NULL DEFAULT 0,
				supported_in_api INTEGER NOT NULL DEFAULT 1, hidden INTEGER NOT NULL DEFAULT 0,
				extra_headers_json TEXT NOT NULL DEFAULT '{}', field_presence_json TEXT NOT NULL DEFAULT '{}',
				first_seen_at TIMESTAMP NOT NULL, PRIMARY KEY (account_id, origin, model_id)
			);
			CREATE TABLE IF NOT EXISTS grok_model_capabilities (
				account_id INTEGER NOT NULL, model_id TEXT NOT NULL, origin TEXT NOT NULL, protocol TEXT NOT NULL,
				credential_generation INTEGER NOT NULL, status TEXT NOT NULL DEFAULT 'untested',
				http_status INTEGER NOT NULL DEFAULT 0, provider_code TEXT NOT NULL DEFAULT '',
				source TEXT NOT NULL DEFAULT '', retry_after_seconds INTEGER NOT NULL DEFAULT 0,
				observed_at TIMESTAMP NOT NULL, expires_at TIMESTAMP NOT NULL,
				updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
				PRIMARY KEY (account_id, model_id, origin, protocol)
			);
			CREATE TABLE IF NOT EXISTS grok_credential_identity_claims (
				identity_key TEXT NOT NULL PRIMARY KEY, account_id INTEGER NOT NULL,
				created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
			);
			CREATE TABLE IF NOT EXISTS grok_state_migration_progress (
				version TEXT NOT NULL PRIMARY KEY, phase TEXT NOT NULL DEFAULT 'families',
				last_account_id INTEGER NOT NULL DEFAULT 0, completed_at TIMESTAMP NULL,
				updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
			);
			CREATE INDEX IF NOT EXISTS idx_grok_facts_expires ON grok_account_fact_snapshots(expires_at);
			CREATE INDEX IF NOT EXISTS idx_grok_catalog_items_model ON grok_model_catalog_items(model_id, account_id);
			CREATE INDEX IF NOT EXISTS idx_grok_capabilities_expires ON grok_model_capabilities(expires_at);
			CREATE INDEX IF NOT EXISTS idx_grok_identity_claims_account ON grok_credential_identity_claims(account_id);
		`)
		if err != nil {
			return err
		}
	} else {
		_, err := db.conn.ExecContext(ctx, `
			ALTER TABLE accounts ADD COLUMN IF NOT EXISTS credential_generation BIGINT NOT NULL DEFAULT 1;
			ALTER TABLE accounts ADD COLUMN IF NOT EXISTS credential_family_id TEXT NOT NULL DEFAULT '';
			CREATE TABLE IF NOT EXISTS grok_account_fact_snapshots (
				account_id BIGINT NOT NULL, fact_kind TEXT NOT NULL,
				credential_generation BIGINT NOT NULL, status TEXT NOT NULL DEFAULT 'unknown',
				http_status INT NOT NULL DEFAULT 0, source TEXT NOT NULL DEFAULT '',
				payload_json JSONB NOT NULL DEFAULT '{}'::jsonb,
				field_presence_json JSONB NOT NULL DEFAULT '{}'::jsonb,
				observed_at TIMESTAMPTZ NOT NULL, expires_at TIMESTAMPTZ NOT NULL,
				updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY (account_id, fact_kind)
			);
			CREATE TABLE IF NOT EXISTS grok_model_catalog_snapshots (
				account_id BIGINT NOT NULL, origin TEXT NOT NULL,
				credential_generation BIGINT NOT NULL, auth_kind TEXT NOT NULL DEFAULT '',
				status TEXT NOT NULL DEFAULT 'unknown', http_etag TEXT NOT NULL DEFAULT '',
				etag_hint TEXT NOT NULL DEFAULT '', etag_hint_observed_at TIMESTAMPTZ NULL,
				observed_at TIMESTAMPTZ NOT NULL, expires_at TIMESTAMPTZ NOT NULL,
				updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(), PRIMARY KEY (account_id, origin)
			);
			CREATE TABLE IF NOT EXISTS grok_model_catalog_items (
				account_id BIGINT NOT NULL, origin TEXT NOT NULL, model_id TEXT NOT NULL,
				credential_generation BIGINT NOT NULL, display_name TEXT NOT NULL DEFAULT '',
				description TEXT NOT NULL DEFAULT '', base_url TEXT NOT NULL DEFAULT '',
				api_base_url TEXT NOT NULL DEFAULT '', api_backend TEXT NOT NULL DEFAULT '',
				context_window BIGINT NOT NULL DEFAULT 0, max_output_tokens BIGINT NOT NULL DEFAULT 0,
				reasoning_effort TEXT NOT NULL DEFAULT '', reasoning_efforts_json JSONB NOT NULL DEFAULT '[]'::jsonb,
				supports_reasoning_effort BOOLEAN NOT NULL DEFAULT FALSE,
				supports_backend_search BOOLEAN NOT NULL DEFAULT FALSE, stream_tool_calls BOOLEAN NOT NULL DEFAULT FALSE,
				supported_in_api BOOLEAN NOT NULL DEFAULT TRUE, hidden BOOLEAN NOT NULL DEFAULT FALSE,
				extra_headers_json JSONB NOT NULL DEFAULT '{}'::jsonb,
				field_presence_json JSONB NOT NULL DEFAULT '{}'::jsonb,
				first_seen_at TIMESTAMPTZ NOT NULL, PRIMARY KEY (account_id, origin, model_id)
			);
			CREATE TABLE IF NOT EXISTS grok_model_capabilities (
				account_id BIGINT NOT NULL, model_id TEXT NOT NULL, origin TEXT NOT NULL, protocol TEXT NOT NULL,
				credential_generation BIGINT NOT NULL, status TEXT NOT NULL DEFAULT 'untested',
				http_status INT NOT NULL DEFAULT 0, provider_code TEXT NOT NULL DEFAULT '',
				source TEXT NOT NULL DEFAULT '', retry_after_seconds BIGINT NOT NULL DEFAULT 0,
				observed_at TIMESTAMPTZ NOT NULL, expires_at TIMESTAMPTZ NOT NULL,
				updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
				PRIMARY KEY (account_id, model_id, origin, protocol)
			);
			CREATE TABLE IF NOT EXISTS grok_credential_identity_claims (
				identity_key TEXT NOT NULL PRIMARY KEY, account_id BIGINT NOT NULL,
				created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
			);
			CREATE TABLE IF NOT EXISTS grok_state_migration_progress (
				version TEXT NOT NULL PRIMARY KEY, phase TEXT NOT NULL DEFAULT 'families',
				last_account_id BIGINT NOT NULL DEFAULT 0, completed_at TIMESTAMPTZ NULL,
				updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
			);
			CREATE INDEX IF NOT EXISTS idx_grok_facts_expires ON grok_account_fact_snapshots(expires_at);
			CREATE INDEX IF NOT EXISTS idx_grok_catalog_items_model ON grok_model_catalog_items(model_id, account_id);
			CREATE INDEX IF NOT EXISTS idx_grok_capabilities_expires ON grok_model_capabilities(expires_at);
			CREATE INDEX IF NOT EXISTS idx_grok_identity_claims_account ON grok_credential_identity_claims(account_id);
		`)
		if err != nil {
			return err
		}
	}
	return nil
}

// ensureGrokStateHistoricalBackfill commits bounded, cursor-tracked batches.
// A crash repeats at most one idempotent batch, while the final marker makes
// every later startup skip the historical account scan.
func (db *DB) ensureGrokStateHistoricalBackfill(ctx context.Context) error {
	if err := db.ensureDataMigrationsTable(ctx); err != nil {
		return err
	}
	var completed int
	if err := db.conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM data_migrations WHERE version=$1`, dataMigrationGrokStateBackfillV1).Scan(&completed); err != nil {
		return fmt.Errorf("check Grok state backfill marker: %w", err)
	}
	if completed != 0 {
		return nil
	}
	if _, err := db.conn.ExecContext(ctx, `
		INSERT INTO grok_state_migration_progress(version,phase,last_account_id,updated_at)
		VALUES($1,'families',0,CURRENT_TIMESTAMP) ON CONFLICT(version) DO NOTHING`, dataMigrationGrokStateBackfillV1); err != nil {
		return fmt.Errorf("initialize Grok state migration progress: %w", err)
	}
	for {
		done, err := db.runGrokStateHistoricalBackfillBatch(ctx)
		if err != nil {
			return err
		}
		if done {
			return nil
		}
	}
}

func (db *DB) runGrokStateHistoricalBackfillBatch(ctx context.Context) (bool, error) {
	done := false
	err := db.withSQLiteWriteLock(ctx, func() error {
		tx, err := db.conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		progressQuery := `SELECT phase,last_account_id,completed_at FROM grok_state_migration_progress WHERE version=$1`
		if !db.isSQLite() {
			progressQuery += ` FOR UPDATE`
		}
		var phase string
		var lastAccountID int64
		var completedAt sql.NullTime
		if err := tx.QueryRowContext(ctx, progressQuery, dataMigrationGrokStateBackfillV1).Scan(&phase, &lastAccountID, &completedAt); err != nil {
			return fmt.Errorf("read Grok state migration progress: %w", err)
		}
		if completedAt.Valid {
			done = true
			return tx.Commit()
		}

		switch phase {
		case "families":
			processed, err := db.backfillCredentialFamiliesBatch(ctx, tx, grokStateHistoricalBackfillBatchSize)
			if err != nil {
				return fmt.Errorf("backfill Grok credential families: %w", err)
			}
			if processed == 0 {
				if _, err := tx.ExecContext(ctx, `UPDATE grok_state_migration_progress SET phase='identities',last_account_id=0,updated_at=CURRENT_TIMESTAMP WHERE version=$1`, dataMigrationGrokStateBackfillV1); err != nil {
					return err
				}
			}
		case "identities":
			processed, nextID, err := db.backfillGrokCredentialIdentityClaimsBatch(ctx, tx, lastAccountID, grokStateHistoricalBackfillBatchSize)
			if err != nil {
				return fmt.Errorf("backfill Grok credential identity claims: %w", err)
			}
			if processed > 0 {
				if _, err := tx.ExecContext(ctx, `UPDATE grok_state_migration_progress SET last_account_id=$1,updated_at=CURRENT_TIMESTAMP WHERE version=$2`, nextID, dataMigrationGrokStateBackfillV1); err != nil {
					return err
				}
			} else {
				if _, err := tx.ExecContext(ctx, `UPDATE grok_state_migration_progress SET completed_at=CURRENT_TIMESTAMP,updated_at=CURRENT_TIMESTAMP WHERE version=$1`, dataMigrationGrokStateBackfillV1); err != nil {
					return err
				}
				if _, err := tx.ExecContext(ctx, `INSERT INTO data_migrations(version,applied_at) VALUES($1,CURRENT_TIMESTAMP) ON CONFLICT(version) DO NOTHING`, dataMigrationGrokStateBackfillV1); err != nil {
					return err
				}
				done = true
			}
		default:
			return fmt.Errorf("unknown Grok state migration phase %q", phase)
		}
		return tx.Commit()
	})
	return done, err
}

func credentialFamilyCandidate(credentials map[string]any) string {
	if explicit, _ := credentials["credential_family_id"].(string); strings.TrimSpace(explicit) != "" {
		return strings.TrimSpace(explicit)
	}
	get := func(keys ...string) string {
		for _, key := range keys {
			if value, ok := credentials[key].(string); ok && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
		return ""
	}
	isGrok := strings.EqualFold(get("upstream_type"), "grok")
	authKind := "oauth"
	if get("api_key") != "" {
		authKind = "api_key"
	}
	issuer := strings.ToLower(strings.TrimRight(get("grok_oidc_issuer"), "/"))
	if isGrok && issuer == "" && authKind == "oauth" {
		issuer = "https://auth.x.ai"
	}
	origin := get("base_url")
	if isGrok && origin == "" {
		if authKind == "api_key" {
			origin = "https://api.x.ai/v1"
		} else {
			origin = "https://cli-chat-proxy.grok.com/v1"
		}
	}
	if parsed, err := url.Parse(origin); err == nil && parsed.Hostname() != "" {
		host := strings.ToLower(parsed.Hostname())
		if port := parsed.Port(); port != "" && port != "443" && port != "80" {
			host += ":" + port
		}
		origin = strings.ToLower(parsed.Scheme) + "://" + host
	} else {
		origin = strings.ToLower(strings.TrimRight(origin, "/"))
	}
	principal := get("grok_principal_id", "account_id", "chatgpt_account_id", "user_id")
	identity := strings.Join([]string{
		"grok-credential-family-v1", authKind, issuer, get("grok_client_id"),
		strings.ToLower(get("grok_principal_type")), principal, origin,
	}, "\x00")
	if principal == "" {
		fallback := get("api_key", "refresh_token", "access_token")
		if fallback == "" {
			return ""
		}
		identity += "\x00fallback\x00" + fallback
	}
	if !isGrok && strings.TrimSpace(principal) == "" && get("api_key", "refresh_token", "access_token") == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:])
}

func (db *DB) backfillCredentialFamilies(ctx context.Context) error {
	rows, err := db.conn.QueryContext(ctx, `SELECT id, credentials FROM accounts WHERE COALESCE(credential_family_id, '') = ''`)
	if err != nil {
		return err
	}
	type pending struct {
		id     int64
		family string
	}
	var updates []pending
	for rows.Next() {
		var id int64
		var raw any
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return err
		}
		family := credentialFamilyCandidate(decodeCredentials(raw))
		if family == "" {
			family = "cf_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		}
		updates = append(updates, pending{id: id, family: family})
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, update := range updates {
		if _, err := db.conn.ExecContext(ctx, `UPDATE accounts SET credential_family_id = $1 WHERE id = $2 AND COALESCE(credential_family_id, '') = ''`, update.family, update.id); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) backfillCredentialFamiliesBatch(ctx context.Context, tx *sql.Tx, limit int) (int, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id,credentials FROM accounts
		WHERE COALESCE(credential_family_id,'')=''
		ORDER BY id LIMIT $1`, limit)
	if err != nil {
		return 0, err
	}
	type pending struct {
		id     int64
		family string
	}
	updates := make([]pending, 0, limit)
	for rows.Next() {
		var id int64
		var raw any
		if err := rows.Scan(&id, &raw); err != nil {
			rows.Close()
			return 0, err
		}
		family := credentialFamilyCandidate(decodeCredentials(raw))
		if family == "" {
			family = "cf_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		}
		updates = append(updates, pending{id: id, family: family})
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	stmt, err := tx.PrepareContext(ctx, `UPDATE accounts SET credential_family_id=$1 WHERE id=$2 AND COALESCE(credential_family_id,'')=''`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()
	for _, update := range updates {
		if _, err := stmt.ExecContext(ctx, update.family, update.id); err != nil {
			return 0, err
		}
	}
	return len(updates), nil
}

func grokCredentialIdentityKey(kind, value string) string {
	kind = strings.ToLower(strings.TrimSpace(kind))
	value = strings.TrimSpace(value)
	if kind == "" || value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("grok-credential-identity-v1\x00" + kind + "\x00" + value))
	return kind + ":" + hex.EncodeToString(sum[:])
}

func grokCredentialIdentityKeys(credentials map[string]any, familyID string) []string {
	get := func(keys ...string) string {
		for _, key := range keys {
			if value, ok := credentials[key].(string); ok && strings.TrimSpace(value) != "" {
				return strings.TrimSpace(value)
			}
		}
		return ""
	}
	keys := make([]string, 0, 2)
	if principal := get("grok_principal_id", "account_id", "chatgpt_account_id", "user_id"); principal != "" {
		issuer := strings.ToLower(strings.TrimRight(get("grok_oidc_issuer"), "/"))
		if issuer == "" {
			issuer = "https://auth.x.ai"
		}
		principalType := strings.ToLower(get("grok_principal_type"))
		keys = append(keys, grokCredentialIdentityKey("principal", issuer+"\x00"+principalType+"\x00"+principal))
	}
	if familyID = strings.TrimSpace(familyID); familyID != "" {
		keys = append(keys, grokCredentialIdentityKey("family", familyID))
	}
	return keys
}

// InsertGrokAccountIfAbsent atomically reserves both the stable credential
// family and the OAuth principal before publishing a newly imported account.
// duplicateAccountID is non-zero when either identity already belongs to an
// existing account (including one in the recycle bin).
func (db *DB) InsertGrokAccountIfAbsent(ctx context.Context, name string, credentials map[string]any, proxyURL string, enabled bool) (accountID, duplicateAccountID int64, err error) {
	if db == nil || db.conn == nil {
		return 0, 0, errors.New("database is not initialized")
	}
	credentialCopy := make(map[string]any, len(credentials)+1)
	for key, value := range credentials {
		credentialCopy[key] = value
	}
	familyID := credentialFamilyCandidate(credentialCopy)
	if familyID == "" {
		familyID = "cf_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	}
	credentialCopy["credential_family_id"] = familyID
	identityKeys := grokCredentialIdentityKeys(credentialCopy, familyID)
	if len(identityKeys) == 0 {
		return 0, 0, errors.New("grok credential has no stable identity")
	}
	encoded, err := json.Marshal(encryptSensitiveCredentials(credentialCopy))
	if err != nil {
		return 0, 0, err
	}

	err = db.withSQLiteWriteLock(ctx, func() error {
		tx, beginErr := db.conn.BeginTx(ctx, nil)
		if beginErr != nil {
			return beginErr
		}
		defer tx.Rollback()

		if db.isSQLite() {
			result, insertErr := tx.ExecContext(ctx, `INSERT INTO accounts(name,platform,type,credentials,proxy_url,enabled) VALUES($1,'xai','grok',$2,$3,$4)`, name, encoded, proxyURL, enabled)
			if insertErr != nil {
				return insertErr
			}
			accountID, insertErr = result.LastInsertId()
			if insertErr != nil {
				return insertErr
			}
		} else if insertErr := tx.QueryRowContext(ctx, `INSERT INTO accounts(name,platform,type,credentials,proxy_url,enabled) VALUES($1,'xai','grok',$2::jsonb,$3,$4) RETURNING id`, name, encoded, proxyURL, enabled).Scan(&accountID); insertErr != nil {
			return insertErr
		}
		if _, updateErr := tx.ExecContext(ctx, `UPDATE accounts SET credential_family_id=$1 WHERE id=$2`, familyID, accountID); updateErr != nil {
			return updateErr
		}

		for _, identityKey := range identityKeys {
			result, claimErr := tx.ExecContext(ctx, `INSERT INTO grok_credential_identity_claims(identity_key,account_id) VALUES($1,$2) ON CONFLICT(identity_key) DO NOTHING`, identityKey, accountID)
			if claimErr != nil {
				return claimErr
			}
			claimed, rowsErr := result.RowsAffected()
			if rowsErr != nil {
				return rowsErr
			}
			if claimed != 0 {
				continue
			}
			if queryErr := tx.QueryRowContext(ctx, `SELECT account_id FROM grok_credential_identity_claims WHERE identity_key=$1`, identityKey).Scan(&duplicateAccountID); queryErr != nil {
				return queryErr
			}
			accountID = 0
			return nil
		}
		return tx.Commit()
	})
	return accountID, duplicateAccountID, err
}

// GrokReauthResult describes how a duplicate-identity re-authorization landed.
type GrokReauthResult struct {
	// Revived 表示该账号原先在回收站,本次重授权已将其复活。
	Revived bool
}

// ReauthGrokAccount 把一次新授权的凭据合并进已持有该凭据身份的账号:
// 回收站账号复活(status/deleted_at/enabled/冷却全部重置),正常账号原地更新
// token(status=error 时顺带清错)。name/proxyURL 仅在非空时覆盖既有值。
// 新凭据派生出的全部身份键都必须归属 accountID(缺失的键补登记),任一键
// 属于其他账号时中止——两份身份不能被静默合并。
func (db *DB) ReauthGrokAccount(ctx context.Context, accountID int64, credentials map[string]any, name, proxyURL string) (GrokReauthResult, error) {
	var result GrokReauthResult
	if db == nil || db.conn == nil {
		return result, errors.New("database is not initialized")
	}
	if accountID <= 0 {
		return result, errors.New("invalid account id")
	}
	err := db.withSQLiteWriteLock(ctx, func() error {
		tx, beginErr := db.conn.BeginTx(ctx, nil)
		if beginErr != nil {
			return beginErr
		}
		defer tx.Rollback()

		selectQuery := `SELECT credentials, status, COALESCE(error_message,''), deleted_at IS NOT NULL FROM accounts WHERE id=$1`
		if !db.isSQLite() {
			selectQuery += ` FOR UPDATE`
		}
		var currentRaw any
		var status, errorMessage string
		var deleted bool
		if scanErr := tx.QueryRowContext(ctx, selectQuery, accountID).Scan(&currentRaw, &status, &errorMessage, &deleted); scanErr != nil {
			return scanErr
		}
		existing := decodeCredentials(currentRaw)
		if !strings.EqualFold(strings.TrimSpace(credentialStringFromMap(existing, "upstream_type")), "grok") {
			return fmt.Errorf("账号 %d 不是 Grok 账号,不能承接 Grok 重授权", accountID)
		}
		inRecycleBin := deleted || strings.EqualFold(strings.TrimSpace(status), "deleted") || strings.EqualFold(strings.TrimSpace(errorMessage), "deleted")

		merged := mergeCredentialMaps(existing, credentials)
		// credentialFamilyCandidate 优先取既有 credential_family_id,family 跨 RT 轮转稳定。
		familyID := credentialFamilyCandidate(merged)
		if familyID == "" {
			familyID = "cf_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		}
		merged["credential_family_id"] = familyID

		for _, identityKey := range grokCredentialIdentityKeys(merged, familyID) {
			if _, claimErr := tx.ExecContext(ctx, `INSERT INTO grok_credential_identity_claims(identity_key,account_id) VALUES($1,$2) ON CONFLICT(identity_key) DO NOTHING`, identityKey, accountID); claimErr != nil {
				return claimErr
			}
			var holder int64
			if queryErr := tx.QueryRowContext(ctx, `SELECT account_id FROM grok_credential_identity_claims WHERE identity_key=$1`, identityKey).Scan(&holder); queryErr != nil {
				return queryErr
			}
			if holder != accountID {
				return fmt.Errorf("凭据身份已被账号 %d 占用,无法合并到账号 %d,请先处理冲突账号", holder, accountID)
			}
		}

		encoded, marshalErr := json.Marshal(encryptSensitiveCredentials(merged))
		if marshalErr != nil {
			return marshalErr
		}
		credentialsExpr := "$1"
		if !db.isSQLite() {
			credentialsExpr = "$1::jsonb"
		}
		if _, updateErr := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE accounts SET credentials=%s, credential_family_id=$2, updated_at=CURRENT_TIMESTAMP WHERE id=$3`, credentialsExpr), encoded, familyID, accountID); updateErr != nil {
			return updateErr
		}
		if name = strings.TrimSpace(name); name != "" {
			if _, updateErr := tx.ExecContext(ctx, `UPDATE accounts SET name=$1 WHERE id=$2`, name, accountID); updateErr != nil {
				return updateErr
			}
		}
		if proxyURL = strings.TrimSpace(proxyURL); proxyURL != "" {
			if _, updateErr := tx.ExecContext(ctx, `UPDATE accounts SET proxy_url=$1 WHERE id=$2`, proxyURL, accountID); updateErr != nil {
				return updateErr
			}
		}
		if inRecycleBin {
			if _, updateErr := tx.ExecContext(ctx, `UPDATE accounts SET status='active', error_message='', deleted_at=NULL, enabled=$1, cooldown_until=NULL, cooldown_reason='' WHERE id=$2`, true, accountID); updateErr != nil {
				return updateErr
			}
			result.Revived = true
		} else if strings.EqualFold(strings.TrimSpace(status), "error") {
			// 与 Codex 重导入语义一致:仅清鉴权类错误态,不动合法的限速冷却。
			if _, updateErr := tx.ExecContext(ctx, `UPDATE accounts SET status='active', error_message='' WHERE id=$1`, accountID); updateErr != nil {
				return updateErr
			}
		}
		return tx.Commit()
	})
	return result, err
}

// backfillGrokCredentialIdentityClaims assigns each historical identity to its
// oldest account. INSERT ... ON CONFLICT makes the migration safe when an old
// deployment already contains duplicates: those rows remain available for
// operators to reconcile, while all new imports are atomically fenced.
func (db *DB) backfillGrokCredentialIdentityClaims(ctx context.Context) error {
	rows, err := db.conn.QueryContext(ctx, `SELECT id,credentials,credential_family_id FROM accounts ORDER BY id`)
	if err != nil {
		return err
	}
	type historicalIdentity struct {
		accountID int64
		keys      []string
	}
	identities := make([]historicalIdentity, 0)
	for rows.Next() {
		var accountID int64
		var raw any
		var familyID string
		if err := rows.Scan(&accountID, &raw, &familyID); err != nil {
			rows.Close()
			return err
		}
		credentials := decodeCredentials(raw)
		if !strings.EqualFold(strings.TrimSpace(credentialStringFromMap(credentials, "upstream_type")), "grok") {
			continue
		}
		identities = append(identities, historicalIdentity{accountID: accountID, keys: grokCredentialIdentityKeys(credentials, familyID)})
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, identity := range identities {
		for _, key := range identity.keys {
			if _, err := db.conn.ExecContext(ctx, `INSERT INTO grok_credential_identity_claims(identity_key,account_id) VALUES($1,$2) ON CONFLICT(identity_key) DO NOTHING`, key, identity.accountID); err != nil {
				return err
			}
		}
	}
	return nil
}

func (db *DB) backfillGrokCredentialIdentityClaimsBatch(ctx context.Context, tx *sql.Tx, afterID int64, limit int) (processed int, nextID int64, err error) {
	query := `SELECT id,credentials,credential_family_id FROM accounts WHERE id>$1 ORDER BY id LIMIT $2`
	if !db.isSQLite() {
		query = `SELECT id,credentials,credential_family_id FROM accounts WHERE id>$1 AND LOWER(COALESCE(credentials->>'upstream_type',''))='grok' ORDER BY id LIMIT $2`
	}
	rows, err := tx.QueryContext(ctx, query, afterID, limit)
	if err != nil {
		return 0, afterID, err
	}
	type identity struct {
		accountID int64
		keys      []string
	}
	identities := make([]identity, 0, limit)
	for rows.Next() {
		var accountID int64
		var raw any
		var familyID string
		if err := rows.Scan(&accountID, &raw, &familyID); err != nil {
			rows.Close()
			return 0, afterID, err
		}
		nextID = accountID
		processed++
		credentials := decodeCredentials(raw)
		if db.isSQLite() && !strings.EqualFold(strings.TrimSpace(credentialStringFromMap(credentials, "upstream_type")), "grok") {
			continue
		}
		identities = append(identities, identity{accountID: accountID, keys: grokCredentialIdentityKeys(credentials, familyID)})
	}
	if err := rows.Close(); err != nil {
		return 0, afterID, err
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO grok_credential_identity_claims(identity_key,account_id) VALUES($1,$2) ON CONFLICT(identity_key) DO NOTHING`)
	if err != nil {
		return 0, afterID, err
	}
	defer stmt.Close()
	for _, item := range identities {
		for _, key := range item.keys {
			if _, err := stmt.ExecContext(ctx, key, item.accountID); err != nil {
				return 0, afterID, err
			}
		}
	}
	return processed, nextID, nil
}

func (db *DB) GetAccountCredentialState(ctx context.Context, accountID int64) (generation int64, familyID string, err error) {
	err = db.conn.QueryRowContext(ctx, `SELECT credential_generation, credential_family_id FROM accounts WHERE id = $1`, accountID).Scan(&generation, &familyID)
	return
}

// EnsureAccountCredentialFamilyID only fills an empty family. A credential
// family is stable across token rotation and is never reassigned in place.
func (db *DB) EnsureAccountCredentialFamilyID(ctx context.Context, accountID int64, candidate string) (string, error) {
	candidate = strings.TrimSpace(candidate)
	if candidate == "" {
		candidate = "cf_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	}
	if _, err := db.conn.ExecContext(ctx, `UPDATE accounts SET credential_family_id=$1 WHERE id=$2 AND COALESCE(credential_family_id,'')=''`, candidate, accountID); err != nil {
		return "", err
	}
	var family string
	if err := db.conn.QueryRowContext(ctx, `SELECT credential_family_id FROM accounts WHERE id=$1`, accountID).Scan(&family); err != nil {
		return "", err
	}
	return family, nil
}

// UpdateAccountCredentialsCAS merges a refreshed identity only if it still
// belongs to expectedGeneration. A successful write advances the generation;
// callers must publish tokens to memory/cache only after applied=true.
func (db *DB) UpdateAccountCredentialsCAS(ctx context.Context, accountID, expectedGeneration int64, updates map[string]any) (newGeneration int64, applied bool, err error) {
	err = db.withSQLiteWriteLock(ctx, func() error {
		tx, beginErr := db.conn.BeginTx(ctx, nil)
		if beginErr != nil {
			return beginErr
		}
		defer tx.Rollback()
		query := `SELECT credentials, credential_generation, credential_family_id FROM accounts WHERE id=$1 AND status <> 'deleted'`
		if !db.isSQLite() {
			query += ` FOR UPDATE`
		}
		var raw any
		var current int64
		var familyID string
		if scanErr := tx.QueryRowContext(ctx, query, accountID).Scan(&raw, &current, &familyID); scanErr != nil {
			return scanErr
		}
		if current != expectedGeneration {
			newGeneration = current
			return nil
		}
		merged := mergeCredentialMaps(decodeCredentials(raw), updates)
		familyID = strings.TrimSpace(familyID)
		if familyID == "" {
			familyID = credentialFamilyCandidate(merged)
		}
		if familyID == "" {
			familyID = "cf_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		}
		// Keep the compatibility JSON field synchronized with the canonical
		// column in the same write that publishes the rotated credential.
		merged["credential_family_id"] = familyID
		encoded, marshalErr := json.Marshal(encryptSensitiveCredentials(merged))
		if marshalErr != nil {
			return marshalErr
		}
		updateQuery := `UPDATE accounts SET credentials=$1, credential_family_id=$2, credential_generation=credential_generation+1, updated_at=CURRENT_TIMESTAMP WHERE id=$3 AND credential_generation=$4`
		if !db.isSQLite() {
			updateQuery = `UPDATE accounts SET credentials=$1::jsonb, credential_family_id=$2, credential_generation=credential_generation+1, updated_at=NOW() WHERE id=$3 AND credential_generation=$4`
		}
		res, execErr := tx.ExecContext(ctx, updateQuery, encoded, familyID, accountID, expectedGeneration)
		if execErr != nil {
			return execErr
		}
		n, rowsErr := res.RowsAffected()
		if rowsErr != nil {
			return rowsErr
		}
		if n == 0 {
			return nil
		}
		// 例行 AT/RT 刷新(本 CAS 的唯一调用方,受 family lease 串行化)不更换
		// 上游身份:目录/能力/控制面事实描述的是上游账号而非某一枚 token。若让
		// 它们随代作废,每个 token 刷新周期都会触发"全账号 × 全模型 × 3 协议"
		// 的真实推理能力重探(不受探测开关控制),形成成本放大器。在同一事务内
		// 把上一代观测盖章到新代;身份替换(重新导入)不走本函数,其失效 fence
		// 依旧生效。各观测自身的 expires_at 不变,新鲜度语义不受影响。
		for _, table := range []string{
			"grok_account_fact_snapshots",
			"grok_model_catalog_snapshots",
			"grok_model_catalog_items",
			"grok_model_capabilities",
		} {
			if _, carryErr := tx.ExecContext(ctx,
				`UPDATE `+table+` SET credential_generation=$1 WHERE account_id=$2 AND credential_generation=$3`,
				expectedGeneration+1, accountID, expectedGeneration); carryErr != nil {
				return carryErr
			}
		}
		applied = true
		newGeneration = expectedGeneration + 1
		return tx.Commit()
	})
	return
}

// ReplaceAccountCredentialsCAS atomically replaces an account identity. Unlike
// routine token refresh, it updates the canonical family column and does not
// carry generation-fenced provider observations forward.
func (db *DB) ReplaceAccountCredentialsCAS(ctx context.Context, accountID, expectedGeneration int64, familyID string, updates map[string]any) (newGeneration int64, applied bool, err error) {
	if db == nil || db.conn == nil || accountID <= 0 || expectedGeneration <= 0 || len(updates) == 0 {
		return 0, false, nil
	}
	err = db.withSQLiteWriteLock(ctx, func() error {
		tx, beginErr := db.conn.BeginTx(ctx, nil)
		if beginErr != nil {
			return beginErr
		}
		defer tx.Rollback()

		query := `SELECT credentials, credential_generation FROM accounts WHERE id=$1 AND status <> 'deleted'`
		if !db.isSQLite() {
			query += ` FOR UPDATE`
		}
		var raw any
		var current int64
		if scanErr := tx.QueryRowContext(ctx, query, accountID).Scan(&raw, &current); scanErr != nil {
			return scanErr
		}
		if current != expectedGeneration {
			newGeneration = current
			return nil
		}
		merged := mergeCredentialMaps(decodeCredentials(raw), updates)
		familyID = strings.TrimSpace(familyID)
		if familyID == "" {
			familyID = credentialFamilyCandidate(merged)
		}
		if familyID == "" {
			familyID = "cf_" + strings.ReplaceAll(uuid.NewString(), "-", "")
		}
		merged["credential_family_id"] = familyID
		encoded, marshalErr := json.Marshal(encryptSensitiveCredentials(merged))
		if marshalErr != nil {
			return marshalErr
		}
		updateQuery := `UPDATE accounts SET credentials=$1, credential_family_id=$2, credential_generation=credential_generation+1, updated_at=CURRENT_TIMESTAMP WHERE id=$3 AND credential_generation=$4`
		if !db.isSQLite() {
			updateQuery = `UPDATE accounts SET credentials=$1::jsonb, credential_family_id=$2, credential_generation=credential_generation+1, updated_at=NOW() WHERE id=$3 AND credential_generation=$4`
		}
		res, execErr := tx.ExecContext(ctx, updateQuery, encoded, familyID, accountID, expectedGeneration)
		if execErr != nil {
			return execErr
		}
		rows, rowsErr := res.RowsAffected()
		if rowsErr != nil {
			return rowsErr
		}
		if rows == 0 {
			return nil
		}
		applied = true
		newGeneration = expectedGeneration + 1
		return tx.Commit()
	})
	return
}

// MergeAccountCredentialsForGeneration merges non-identity compatibility
// fields only while the account still belongs to expectedGeneration. Unlike
// UpdateAccountCredentialsCAS it deliberately does not advance the generation:
// callers use it to dual-write additive Grok state into the legacy credentials
// document after the generation-fenced fact/catalog transaction succeeds.
//
// The key allowlist is intentionally enforced here (rather than relying on
// callers) so a future refactor cannot accidentally publish token, principal,
// origin, or rich catalog metadata through this compatibility path.
func (db *DB) MergeAccountCredentialsForGeneration(ctx context.Context, accountID, expectedGeneration int64, updates map[string]any) (applied bool, err error) {
	if db == nil || db.conn == nil || accountID <= 0 || expectedGeneration <= 0 || len(updates) == 0 {
		return false, nil
	}
	allowed := map[string]struct{}{
		"models": {}, "grok_billing_detail": {},
		"grok_weekly_usage_percent": {}, "grok_weekly_period_end": {},
		"grok_monthly_usage_percent": {}, "grok_monthly_limit_cents": {},
		"grok_monthly_used_cents": {}, "grok_monthly_period_end": {},
		"grok_usage_updated_at":  {},
		"antigravity_sync_error": {}, "antigravity_sync_warning": {}, "antigravity_last_sync_attempt_at": {},
		"antigravity_permanent_refresh_error": {},
		"antigravity_catalog_source":          {}, "antigravity_catalog_verified": {},
		"antigravity_capabilities": {}, "antigravity_capability_last_probe_at": {},
	}
	filtered := make(map[string]any, len(updates))
	for key, value := range updates {
		if _, ok := allowed[key]; ok {
			filtered[key] = value
		}
	}
	if len(filtered) == 0 {
		return false, nil
	}

	err = db.withSQLiteWriteLock(ctx, func() error {
		tx, beginErr := db.conn.BeginTx(ctx, nil)
		if beginErr != nil {
			return beginErr
		}
		defer tx.Rollback()
		query := `SELECT credentials, credential_generation FROM accounts WHERE id=$1 AND status <> 'deleted'`
		if !db.isSQLite() {
			query += ` FOR UPDATE`
		}
		var raw any
		var current int64
		if scanErr := tx.QueryRowContext(ctx, query, accountID).Scan(&raw, &current); scanErr != nil {
			return scanErr
		}
		if current != expectedGeneration {
			return nil
		}
		encoded, marshalErr := json.Marshal(encryptSensitiveCredentials(mergeCredentialMaps(decodeCredentials(raw), filtered)))
		if marshalErr != nil {
			return marshalErr
		}
		update := `UPDATE accounts SET credentials=$1, updated_at=CURRENT_TIMESTAMP WHERE id=$2 AND credential_generation=$3`
		if !db.isSQLite() {
			update = `UPDATE accounts SET credentials=$1::jsonb, updated_at=NOW() WHERE id=$2 AND credential_generation=$3`
		}
		result, execErr := tx.ExecContext(ctx, update, encoded, accountID, expectedGeneration)
		if execErr != nil {
			return execErr
		}
		rows, rowsErr := result.RowsAffected()
		if rowsErr != nil {
			return rowsErr
		}
		if rows == 0 {
			return nil
		}
		applied = true
		return tx.Commit()
	})
	return applied, err
}

func (db *DB) verifyGrokGenerationTx(ctx context.Context, tx *sql.Tx, accountID, generation int64) (bool, error) {
	var current int64
	query := `SELECT credential_generation FROM accounts WHERE id=$1`
	if !db.isSQLite() {
		query += ` FOR UPDATE`
	}
	if err := tx.QueryRowContext(ctx, query, accountID).Scan(&current); err != nil {
		return false, err
	}
	return current == generation, nil
}

func jsonObject(value any) ([]byte, error) {
	if value == nil {
		return []byte("{}"), nil
	}
	return json.Marshal(value)
}

func (db *DB) UpsertGrokAccountFact(ctx context.Context, fact GrokAccountFact) (bool, error) {
	fact.Kind = strings.ToLower(strings.TrimSpace(fact.Kind))
	if fact.AccountID <= 0 || fact.CredentialGeneration <= 0 || fact.Kind == "" {
		return false, errors.New("invalid grok fact identity")
	}
	payload, err := jsonObject(fact.Payload)
	if err != nil {
		return false, err
	}
	presence, err := jsonObject(fact.FieldPresence)
	if err != nil {
		return false, err
	}
	if fact.ObservedAt.IsZero() {
		fact.ObservedAt = time.Now()
	}
	if fact.ExpiresAt.IsZero() {
		fact.ExpiresAt = fact.ObservedAt.Add(5 * time.Minute)
	}
	applied := false
	err = db.withSQLiteWriteLock(ctx, func() error {
		tx, e := db.conn.BeginTx(ctx, nil)
		if e != nil {
			return e
		}
		defer tx.Rollback()
		ok, e := db.verifyGrokGenerationTx(ctx, tx, fact.AccountID, fact.CredentialGeneration)
		if e != nil || !ok {
			return e
		}
		query := `INSERT INTO grok_account_fact_snapshots
			(account_id,fact_kind,credential_generation,status,http_status,source,payload_json,field_presence_json,observed_at,expires_at,updated_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,CURRENT_TIMESTAMP)
			ON CONFLICT(account_id,fact_kind) DO UPDATE SET credential_generation=excluded.credential_generation,status=excluded.status,
			http_status=excluded.http_status,source=excluded.source,payload_json=excluded.payload_json,
			field_presence_json=excluded.field_presence_json,observed_at=excluded.observed_at,expires_at=excluded.expires_at,updated_at=CURRENT_TIMESTAMP`
		if !db.isSQLite() {
			query = strings.Replace(query, "$7,$8,$9", "$7::jsonb,$8::jsonb,$9", 1)
		}
		if _, e = tx.ExecContext(ctx, query, fact.AccountID, fact.Kind, fact.CredentialGeneration, fact.Status, fact.HTTPStatus, fact.Source, payload, presence, db.timeArg(fact.ObservedAt), db.timeArg(fact.ExpiresAt)); e != nil {
			return e
		}
		if e = tx.Commit(); e == nil {
			applied = true
		}
		return e
	})
	return applied, err
}

// UpsertGrokAccountFactAndExpireCapabilities atomically publishes a new
// control-plane fact and expires every native capability for the same account
// generation. Subscription/access/billing changes must not leave a window in
// which the DB exposes the new fact alongside old successful probes.
func (db *DB) UpsertGrokAccountFactAndExpireCapabilities(ctx context.Context, fact GrokAccountFact, expiredAt time.Time) (bool, error) {
	fact.Kind = strings.ToLower(strings.TrimSpace(fact.Kind))
	if fact.AccountID <= 0 || fact.CredentialGeneration <= 0 || fact.Kind == "" {
		return false, errors.New("invalid grok fact identity")
	}
	payload, err := jsonObject(fact.Payload)
	if err != nil {
		return false, err
	}
	presence, err := jsonObject(fact.FieldPresence)
	if err != nil {
		return false, err
	}
	if fact.ObservedAt.IsZero() {
		fact.ObservedAt = time.Now()
	}
	if fact.ExpiresAt.IsZero() {
		fact.ExpiresAt = fact.ObservedAt.Add(5 * time.Minute)
	}
	if expiredAt.IsZero() {
		expiredAt = fact.ObservedAt
	}
	applied := false
	err = db.withSQLiteWriteLock(ctx, func() error {
		tx, e := db.conn.BeginTx(ctx, nil)
		if e != nil {
			return e
		}
		defer tx.Rollback()
		ok, e := db.verifyGrokGenerationTx(ctx, tx, fact.AccountID, fact.CredentialGeneration)
		if e != nil || !ok {
			return e
		}
		query := `INSERT INTO grok_account_fact_snapshots
			(account_id,fact_kind,credential_generation,status,http_status,source,payload_json,field_presence_json,observed_at,expires_at,updated_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,CURRENT_TIMESTAMP)
			ON CONFLICT(account_id,fact_kind) DO UPDATE SET credential_generation=excluded.credential_generation,status=excluded.status,
			http_status=excluded.http_status,source=excluded.source,payload_json=excluded.payload_json,
			field_presence_json=excluded.field_presence_json,observed_at=excluded.observed_at,expires_at=excluded.expires_at,updated_at=CURRENT_TIMESTAMP`
		if !db.isSQLite() {
			query = strings.Replace(query, "$7,$8,$9", "$7::jsonb,$8::jsonb,$9", 1)
		}
		if _, e = tx.ExecContext(ctx, query, fact.AccountID, fact.Kind, fact.CredentialGeneration, fact.Status, fact.HTTPStatus, fact.Source, payload, presence, db.timeArg(fact.ObservedAt), db.timeArg(fact.ExpiresAt)); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `UPDATE grok_model_capabilities SET expires_at=$1,updated_at=CURRENT_TIMESTAMP
			WHERE account_id=$2 AND credential_generation=$3`, db.timeArg(expiredAt), fact.AccountID, fact.CredentialGeneration); e != nil {
			return e
		}
		if e = tx.Commit(); e == nil {
			applied = true
		}
		return e
	})
	return applied, err
}

func (db *DB) ReplaceGrokModelCatalog(ctx context.Context, snapshot GrokModelCatalogSnapshot, items []GrokModelCatalogItem) (bool, error) {
	snapshot.Origin = strings.TrimRight(strings.TrimSpace(snapshot.Origin), "/")
	if snapshot.AccountID <= 0 || snapshot.CredentialGeneration <= 0 || snapshot.Origin == "" {
		return false, errors.New("invalid grok catalog identity")
	}
	if snapshot.ObservedAt.IsZero() {
		snapshot.ObservedAt = time.Now()
	}
	if snapshot.ExpiresAt.IsZero() {
		snapshot.ExpiresAt = snapshot.ObservedAt.Add(5 * time.Minute)
	}
	applied := false
	err := db.withSQLiteWriteLock(ctx, func() error {
		tx, e := db.conn.BeginTx(ctx, nil)
		if e != nil {
			return e
		}
		defer tx.Rollback()
		ok, e := db.verifyGrokGenerationTx(ctx, tx, snapshot.AccountID, snapshot.CredentialGeneration)
		if e != nil || !ok {
			return e
		}
		var existing GrokModelCatalogSnapshot
		var existingSnapshot bool
		e = tx.QueryRowContext(ctx, `SELECT credential_generation,auth_kind,status,http_etag
			FROM grok_model_catalog_snapshots WHERE account_id=$1 AND origin=$2`, snapshot.AccountID, snapshot.Origin).
			Scan(&existing.CredentialGeneration, &existing.AuthKind, &existing.Status, &existing.HTTPETag)
		switch {
		case e == nil:
			existingSnapshot = true
		case errors.Is(e, sql.ErrNoRows):
			e = nil
		default:
			return e
		}

		upsert := `INSERT INTO grok_model_catalog_snapshots(account_id,origin,credential_generation,auth_kind,status,http_etag,etag_hint,etag_hint_observed_at,observed_at,expires_at,updated_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,CURRENT_TIMESTAMP)
			ON CONFLICT(account_id,origin) DO UPDATE SET credential_generation=excluded.credential_generation,auth_kind=excluded.auth_kind,
			status=excluded.status,http_etag=excluded.http_etag,etag_hint=excluded.etag_hint,etag_hint_observed_at=excluded.etag_hint_observed_at,
			observed_at=excluded.observed_at,expires_at=excluded.expires_at,updated_at=CURRENT_TIMESTAMP`
		var hintAt any
		if !snapshot.ETagHintObservedAt.IsZero() {
			hintAt = db.timeArg(snapshot.ETagHintObservedAt)
		}

		// A failed fetch is an attempt, not a new catalog observation. Keep the
		// last successful snapshot and its items byte-for-byte so repeated
		// failures cannot slide the one-hour stale-if-error baseline. updated_at
		// records that an attempt happened without changing routing facts.
		if !strings.EqualFold(strings.TrimSpace(snapshot.Status), "ok") {
			if existingSnapshot && existing.CredentialGeneration == snapshot.CredentialGeneration && strings.EqualFold(existing.Status, "ok") {
				if _, e = tx.ExecContext(ctx, `UPDATE grok_model_catalog_snapshots SET updated_at=CURRENT_TIMESTAMP
					WHERE account_id=$1 AND origin=$2 AND credential_generation=$3`, snapshot.AccountID, snapshot.Origin, snapshot.CredentialGeneration); e != nil {
					return e
				}
			}
			if e = tx.Commit(); e == nil {
				applied = true
			}
			return e
		}

		firstSeen := map[string]time.Time{}
		rows, e := tx.QueryContext(ctx, `SELECT account_id,origin,model_id,credential_generation,display_name,description,base_url,api_base_url,api_backend,context_window,max_output_tokens,reasoning_effort,reasoning_efforts_json,supports_reasoning_effort,supports_backend_search,stream_tool_calls,supported_in_api,hidden,extra_headers_json,field_presence_json,first_seen_at
			FROM grok_model_catalog_items WHERE account_id=$1 AND origin=$2 ORDER BY LOWER(model_id),model_id`, snapshot.AccountID, snapshot.Origin)
		if e != nil {
			return e
		}
		existingItems, e := scanGrokCatalogItems(rows)
		if e != nil {
			return e
		}
		for _, item := range existingItems {
			firstSeen[strings.ToLower(item.ModelID)] = item.FirstSeenAt
		}
		normalizedItems := make([]GrokModelCatalogItem, 0, len(items))
		seen := map[string]struct{}{}
		for _, item := range items {
			item.ModelID = strings.TrimSpace(item.ModelID)
			key := strings.ToLower(item.ModelID)
			if item.ModelID == "" {
				continue
			}
			if _, dup := seen[key]; dup {
				continue
			}
			seen[key] = struct{}{}
			item.AccountID = snapshot.AccountID
			item.Origin = snapshot.Origin
			item.CredentialGeneration = snapshot.CredentialGeneration
			if item.FirstSeenAt.IsZero() {
				item.FirstSeenAt = firstSeen[key]
			}
			if item.FirstSeenAt.IsZero() {
				item.FirstSeenAt = snapshot.ObservedAt
			}
			normalizedItems = append(normalizedItems, item)
		}
		catalogChanged := !existingSnapshot ||
			existing.CredentialGeneration != snapshot.CredentialGeneration ||
			!strings.EqualFold(strings.TrimSpace(existing.AuthKind), strings.TrimSpace(snapshot.AuthKind)) ||
			!strings.EqualFold(strings.TrimSpace(existing.Status), "ok") ||
			existing.HTTPETag != snapshot.HTTPETag ||
			grokCatalogContentSignature(existingItems) != grokCatalogContentSignature(normalizedItems)
		if _, e = tx.ExecContext(ctx, upsert, snapshot.AccountID, snapshot.Origin, snapshot.CredentialGeneration, snapshot.AuthKind, snapshot.Status, snapshot.HTTPETag, snapshot.ETagHint, hintAt, db.timeArg(snapshot.ObservedAt), db.timeArg(snapshot.ExpiresAt)); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, `DELETE FROM grok_model_catalog_items WHERE account_id=$1 AND origin=$2`, snapshot.AccountID, snapshot.Origin); e != nil {
			return e
		}
		for _, item := range normalizedItems {
			reasoning, _ := json.Marshal(item.ReasoningEfforts)
			headers, _ := json.Marshal(item.ExtraHeaders)
			presence, _ := json.Marshal(item.FieldPresence)
			insert := `INSERT INTO grok_model_catalog_items(account_id,origin,model_id,credential_generation,display_name,description,base_url,api_base_url,api_backend,context_window,max_output_tokens,reasoning_effort,reasoning_efforts_json,supports_reasoning_effort,supports_backend_search,stream_tool_calls,supported_in_api,hidden,extra_headers_json,field_presence_json,first_seen_at)
				VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)`
			if !db.isSQLite() {
				insert = strings.Replace(insert, "$13,$14", "$13::jsonb,$14", 1)
				insert = strings.Replace(insert, "$19,$20", "$19::jsonb,$20::jsonb", 1)
			}
			if _, e = tx.ExecContext(ctx, insert, item.AccountID, item.Origin, item.ModelID, item.CredentialGeneration, item.DisplayName, item.Description, item.BaseURL, item.APIBaseURL, item.APIBackend, item.ContextWindow, item.MaxOutputTokens, item.ReasoningEffort, reasoning, item.SupportsReasoningEffort, item.SupportsBackendSearch, item.StreamToolCalls, item.SupportedInAPI, item.Hidden, headers, presence, db.timeArg(item.FirstSeenAt)); e != nil {
				return e
			}
		}
		// A successful 200 only invalidates native conclusions when the catalog
		// identity or parsed content actually changed. Timestamps alone, 304s,
		// and failed fetch attempts leave successful capabilities intact.
		if catalogChanged {
			// Capability origin is the model's effective BaseURL/APIBaseURL and may
			// differ from the origin used to fetch /models. Conservatively expire
			// every conclusion for this credential generation after a material
			// catalog replacement.
			if _, e = tx.ExecContext(ctx, `DELETE FROM grok_model_capabilities WHERE account_id=$1 AND credential_generation=$2`, snapshot.AccountID, snapshot.CredentialGeneration); e != nil {
				return e
			}
		}
		if e = tx.Commit(); e == nil {
			applied = true
		}
		return e
	})
	return applied, err
}

func (db *DB) TouchGrokModelCatalogNotModified(ctx context.Context, accountID int64, origin string, generation int64, observedAt, expiresAt time.Time) (bool, error) {
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	if expiresAt.IsZero() {
		expiresAt = observedAt.Add(5 * time.Minute)
	}
	res, err := db.conn.ExecContext(ctx, `UPDATE grok_model_catalog_snapshots SET status='ok',observed_at=$1,expires_at=$2,updated_at=CURRENT_TIMESTAMP
		WHERE account_id=$3 AND origin=$4 AND credential_generation=$5 AND EXISTS(SELECT 1 FROM accounts WHERE id=$3 AND credential_generation=$5)`, db.timeArg(observedAt), db.timeArg(expiresAt), accountID, strings.TrimRight(strings.TrimSpace(origin), "/"), generation)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// UpdateGrokModelsETagHint stores the inference response hint without ever
// overwriting the HTTP catalog ETag used by If-None-Match.
func (db *DB) UpdateGrokModelsETagHint(ctx context.Context, accountID int64, origin string, generation int64, hint string, observedAt time.Time) (bool, error) {
	if db == nil || db.conn == nil {
		return false, errors.New("database is not initialized")
	}
	if accountID <= 0 || generation <= 0 {
		return false, errors.New("invalid grok catalog identity")
	}
	if observedAt.IsZero() {
		observedAt = time.Now()
	}
	hint = strings.TrimSpace(hint)
	if hint == "" || len(hint) > 512 || strings.ContainsAny(hint, "\r\n\x00") {
		return false, nil
	}
	// A changed hint means the catalog may have changed. Expire its normal freshness so
	// the read-only maintenance loop refetches /models, while retaining the old
	// items for the separate one-hour stale-if-error routing window. HTTP ETag
	// is deliberately untouched and remains the next If-None-Match value. An
	// identical hint only refreshes its observation timestamp; it must not keep
	// an otherwise fresh catalog permanently expired after a successful sync.
	origin = strings.TrimRight(strings.TrimSpace(origin), "/")
	if origin == "" {
		return false, errors.New("invalid grok catalog identity")
	}
	applied := false
	err := db.withSQLiteWriteLock(ctx, func() error {
		tx, err := db.conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		generationMatches, err := db.verifyGrokGenerationTx(ctx, tx, accountID, generation)
		if err != nil || !generationMatches {
			return err
		}
		res, err := tx.ExecContext(ctx, `UPDATE grok_model_catalog_snapshots SET etag_hint=$1,etag_hint_observed_at=$2,
			expires_at=CASE WHEN etag_hint<>$1 AND expires_at>$2 THEN $2 ELSE expires_at END,updated_at=CURRENT_TIMESTAMP
			WHERE account_id=$3 AND origin=$4 AND credential_generation=$5`, hint, db.timeArg(observedAt), accountID, origin, generation)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		applied = n > 0
		return nil
	})
	return applied, err
}

func (db *DB) UpsertGrokModelCapability(ctx context.Context, cap GrokModelCapability) (bool, error) {
	cap.ModelID = strings.TrimSpace(cap.ModelID)
	cap.Origin = strings.TrimRight(strings.TrimSpace(cap.Origin), "/")
	cap.Protocol = strings.ToLower(strings.TrimSpace(cap.Protocol))
	if cap.AccountID <= 0 || cap.CredentialGeneration <= 0 || cap.ModelID == "" || cap.Origin == "" || cap.Protocol == "" {
		return false, errors.New("invalid grok capability identity")
	}
	if cap.ObservedAt.IsZero() {
		cap.ObservedAt = time.Now()
	}
	if cap.ExpiresAt.IsZero() {
		cap.ExpiresAt = cap.ObservedAt.Add(24 * time.Hour)
	}
	applied := false
	err := db.withSQLiteWriteLock(ctx, func() error {
		tx, e := db.conn.BeginTx(ctx, nil)
		if e != nil {
			return e
		}
		defer tx.Rollback()
		ok, e := db.verifyGrokGenerationTx(ctx, tx, cap.AccountID, cap.CredentialGeneration)
		if e != nil || !ok {
			return e
		}
		_, e = tx.ExecContext(ctx, `INSERT INTO grok_model_capabilities(account_id,model_id,origin,protocol,credential_generation,status,http_status,provider_code,source,retry_after_seconds,observed_at,expires_at,updated_at)
			VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,CURRENT_TIMESTAMP)
			ON CONFLICT(account_id,model_id,origin,protocol) DO UPDATE SET credential_generation=excluded.credential_generation,status=excluded.status,http_status=excluded.http_status,provider_code=excluded.provider_code,source=excluded.source,retry_after_seconds=excluded.retry_after_seconds,observed_at=excluded.observed_at,expires_at=excluded.expires_at,updated_at=CURRENT_TIMESTAMP`, cap.AccountID, cap.ModelID, cap.Origin, cap.Protocol, cap.CredentialGeneration, cap.Status, cap.HTTPStatus, cap.ProviderCode, cap.Source, cap.RetryAfterSeconds, db.timeArg(cap.ObservedAt), db.timeArg(cap.ExpiresAt))
		if e != nil {
			return e
		}
		if e = tx.Commit(); e == nil {
			applied = true
		}
		return e
	})
	return applied, err
}

// ExpireGrokModelCapabilities advances matching capability observations to an
// expired state without deleting their diagnostic history. Empty model,
// origin, or protocol values are wildcards. The account generation predicate
// fences late runtime failures in the same way as capability probes and
// control-plane snapshots.
func (db *DB) ExpireGrokModelCapabilities(ctx context.Context, accountID, generation int64, modelID, origin, protocol string, expiredAt time.Time) (int64, error) {
	if db == nil || db.conn == nil {
		return 0, errors.New("database is not initialized")
	}
	if accountID <= 0 || generation <= 0 {
		return 0, errors.New("invalid grok capability identity")
	}
	if expiredAt.IsZero() {
		expiredAt = time.Now()
	}

	args := []any{db.timeArg(expiredAt), accountID, generation}
	conditions := []string{
		"account_id=$2",
		"credential_generation=$3",
	}
	appendFilter := func(column, value string, foldCase bool) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		args = append(args, value)
		placeholder := fmt.Sprintf("$%d", len(args))
		if foldCase {
			conditions = append(conditions, fmt.Sprintf("LOWER(%s)=LOWER(%s)", column, placeholder))
		} else {
			conditions = append(conditions, fmt.Sprintf("%s=%s", column, placeholder))
		}
	}
	appendFilter("model_id", modelID, true)
	appendFilter("origin", strings.TrimRight(strings.TrimSpace(origin), "/"), true)
	appendFilter("protocol", strings.ToLower(strings.TrimSpace(protocol)), false)

	var affected int64
	err := db.withSQLiteWriteLock(ctx, func() error {
		tx, err := db.conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()
		generationMatches, err := db.verifyGrokGenerationTx(ctx, tx, accountID, generation)
		if err != nil || !generationMatches {
			return err
		}
		result, err := tx.ExecContext(ctx, `UPDATE grok_model_capabilities
			SET expires_at=$1,updated_at=CURRENT_TIMESTAMP WHERE `+strings.Join(conditions, " AND "), args...)
		if err != nil {
			return err
		}
		affected, err = result.RowsAffected()
		if err != nil {
			return err
		}
		return tx.Commit()
	})
	return affected, err
}

func decodeJSONMap(raw any) map[string]any {
	out := map[string]any{}
	_ = json.Unmarshal(bytesFromDBValue(raw), &out)
	return out
}
func decodeJSONStringMap(raw any) map[string]string {
	out := map[string]string{}
	_ = json.Unmarshal(bytesFromDBValue(raw), &out)
	return out
}
func decodeJSONStringSlice(raw any) []string {
	var out []string
	_ = json.Unmarshal(bytesFromDBValue(raw), &out)
	return out
}

type grokCatalogRows interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close() error
}

func scanGrokCatalogItems(rows grokCatalogRows) ([]GrokModelCatalogItem, error) {
	defer rows.Close()
	var out []GrokModelCatalogItem
	for rows.Next() {
		var item GrokModelCatalogItem
		var reasoning, headers, presence, first any
		if err := rows.Scan(&item.AccountID, &item.Origin, &item.ModelID, &item.CredentialGeneration, &item.DisplayName, &item.Description, &item.BaseURL, &item.APIBaseURL, &item.APIBackend, &item.ContextWindow, &item.MaxOutputTokens, &item.ReasoningEffort, &reasoning, &item.SupportsReasoningEffort, &item.SupportsBackendSearch, &item.StreamToolCalls, &item.SupportedInAPI, &item.Hidden, &headers, &presence, &first); err != nil {
			return nil, err
		}
		item.ReasoningEfforts = decodeJSONStringSlice(reasoning)
		item.ExtraHeaders = decodeJSONStringMap(headers)
		item.FieldPresence = decodeJSONStringMap(presence)
		item.FirstSeenAt, _ = parseDBTimeValue(first)
		out = append(out, item)
	}
	return out, rows.Err()
}

// grokCatalogContentSignature compares the persisted, routing-relevant
// catalog representation. Observation/first-seen timestamps and account
// identity live outside the provider content and intentionally do not affect
// capability invalidation.
func grokCatalogContentSignature(items []GrokModelCatalogItem) string {
	type content struct {
		ModelID                 string            `json:"model_id"`
		DisplayName             string            `json:"display_name"`
		Description             string            `json:"description"`
		BaseURL                 string            `json:"base_url"`
		APIBaseURL              string            `json:"api_base_url"`
		APIBackend              string            `json:"api_backend"`
		ContextWindow           int64             `json:"context_window"`
		MaxOutputTokens         int64             `json:"max_output_tokens"`
		ReasoningEffort         string            `json:"reasoning_effort"`
		ReasoningEfforts        []string          `json:"reasoning_efforts"`
		SupportsReasoningEffort bool              `json:"supports_reasoning_effort"`
		SupportsBackendSearch   bool              `json:"supports_backend_search"`
		StreamToolCalls         bool              `json:"stream_tool_calls"`
		SupportedInAPI          bool              `json:"supported_in_api"`
		Hidden                  bool              `json:"hidden"`
		ExtraHeaders            map[string]string `json:"extra_headers"`
		FieldPresence           map[string]string `json:"field_presence"`
	}
	normalized := make([]content, 0, len(items))
	for _, item := range items {
		normalized = append(normalized, content{
			ModelID: item.ModelID, DisplayName: item.DisplayName, Description: item.Description,
			BaseURL: item.BaseURL, APIBaseURL: item.APIBaseURL, APIBackend: item.APIBackend,
			ContextWindow: item.ContextWindow, MaxOutputTokens: item.MaxOutputTokens,
			ReasoningEffort: item.ReasoningEffort, ReasoningEfforts: item.ReasoningEfforts,
			SupportsReasoningEffort: item.SupportsReasoningEffort, SupportsBackendSearch: item.SupportsBackendSearch,
			StreamToolCalls: item.StreamToolCalls, SupportedInAPI: item.SupportedInAPI, Hidden: item.Hidden,
			ExtraHeaders: item.ExtraHeaders, FieldPresence: item.FieldPresence,
		})
	}
	sort.Slice(normalized, func(i, j int) bool {
		left, right := strings.ToLower(normalized[i].ModelID), strings.ToLower(normalized[j].ModelID)
		if left == right {
			return normalized[i].ModelID < normalized[j].ModelID
		}
		return left < right
	})
	encoded, _ := json.Marshal(normalized)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func (db *DB) GetGrokAccountFact(ctx context.Context, accountID int64, kind string) (*GrokAccountFact, error) {
	row := db.conn.QueryRowContext(ctx, `SELECT account_id,fact_kind,credential_generation,status,http_status,source,payload_json,field_presence_json,observed_at,expires_at,updated_at FROM grok_account_fact_snapshots WHERE account_id=$1 AND fact_kind=$2`, accountID, strings.ToLower(strings.TrimSpace(kind)))
	f := &GrokAccountFact{}
	var payload, presence, observed, expires, updated any
	if err := row.Scan(&f.AccountID, &f.Kind, &f.CredentialGeneration, &f.Status, &f.HTTPStatus, &f.Source, &payload, &presence, &observed, &expires, &updated); err != nil {
		return nil, err
	}
	f.Payload = decodeJSONMap(payload)
	f.FieldPresence = decodeJSONStringMap(presence)
	var err error
	if f.ObservedAt, err = parseDBTimeValue(observed); err != nil {
		return nil, err
	}
	if f.ExpiresAt, err = parseDBTimeValue(expires); err != nil {
		return nil, err
	}
	if f.UpdatedAt, err = parseDBTimeValue(updated); err != nil {
		return nil, err
	}
	return f, nil
}

func (db *DB) GetGrokModelCatalog(ctx context.Context, accountID int64, origin string) (*GrokModelCatalogSnapshot, []GrokModelCatalogItem, error) {
	origin = strings.TrimRight(strings.TrimSpace(origin), "/")
	s := &GrokModelCatalogSnapshot{}
	var hintAt, observed, expires, updated any
	err := db.conn.QueryRowContext(ctx, `SELECT account_id,origin,credential_generation,auth_kind,status,http_etag,etag_hint,etag_hint_observed_at,observed_at,expires_at,updated_at FROM grok_model_catalog_snapshots WHERE account_id=$1 AND origin=$2`, accountID, origin).Scan(&s.AccountID, &s.Origin, &s.CredentialGeneration, &s.AuthKind, &s.Status, &s.HTTPETag, &s.ETagHint, &hintAt, &observed, &expires, &updated)
	if err != nil {
		return nil, nil, err
	}
	if hintAt != nil {
		s.ETagHintObservedAt, _ = parseDBTimeValue(hintAt)
	}
	s.ObservedAt, _ = parseDBTimeValue(observed)
	s.ExpiresAt, _ = parseDBTimeValue(expires)
	s.UpdatedAt, _ = parseDBTimeValue(updated)
	items, err := db.getGrokCatalogItems(ctx, accountID, origin)
	return s, items, err
}

func (db *DB) getGrokCatalogItems(ctx context.Context, accountID int64, origin string) ([]GrokModelCatalogItem, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT account_id,origin,model_id,credential_generation,display_name,description,base_url,api_base_url,api_backend,context_window,max_output_tokens,reasoning_effort,reasoning_efforts_json,supports_reasoning_effort,supports_backend_search,stream_tool_calls,supported_in_api,hidden,extra_headers_json,field_presence_json,first_seen_at FROM grok_model_catalog_items WHERE account_id=$1 AND origin=$2 ORDER BY LOWER(model_id),model_id`, accountID, origin)
	if err != nil {
		return nil, err
	}
	return scanGrokCatalogItems(rows)
}

func (db *DB) GetGrokModelCapabilities(ctx context.Context, accountID int64) ([]GrokModelCapability, error) {
	rows, err := db.conn.QueryContext(ctx, `SELECT account_id,model_id,origin,protocol,credential_generation,status,http_status,provider_code,source,retry_after_seconds,observed_at,expires_at,updated_at FROM grok_model_capabilities WHERE account_id=$1 ORDER BY LOWER(model_id),origin,protocol`, accountID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GrokModelCapability
	for rows.Next() {
		var c GrokModelCapability
		var observed, expires, updated any
		if err = rows.Scan(&c.AccountID, &c.ModelID, &c.Origin, &c.Protocol, &c.CredentialGeneration, &c.Status, &c.HTTPStatus, &c.ProviderCode, &c.Source, &c.RetryAfterSeconds, &observed, &expires, &updated); err != nil {
			return nil, err
		}
		c.ObservedAt, _ = parseDBTimeValue(observed)
		c.ExpiresAt, _ = parseDBTimeValue(expires)
		c.UpdatedAt, _ = parseDBTimeValue(updated)
		out = append(out, c)
	}
	return out, rows.Err()
}

func (db *DB) GetGrokAccountState(ctx context.Context, accountID int64) (*GrokAccountState, error) {
	state := &GrokAccountState{AccountID: accountID, Facts: map[string]GrokAccountFact{}}
	var raw any
	if err := db.conn.QueryRowContext(ctx, `SELECT credential_generation,credential_family_id,credentials FROM accounts WHERE id=$1`, accountID).Scan(&state.CredentialGeneration, &state.Identity.CredentialFamilyID, &raw); err != nil {
		return nil, err
	}
	credentials := decodeCredentials(raw)
	state.Identity.JWTTier = strings.TrimSpace(credentialStringFromMap(credentials, "jwt_plan_type"))
	state.Identity.ArchivePlan = strings.TrimSpace(credentialStringFromMap(credentials, "archive_plan_type"))
	if state.Identity.ArchivePlan != "" {
		state.Identity.ArchivePlanSource = "import_file"
	} else if legacy := strings.TrimSpace(credentialStringFromMap(credentials, "plan_type")); legacy != "" {
		// New refreshes dual-write the unverified JWT tier to plan_type for
		// compatibility with old runtimes. Do not relabel that same value as an
		// archive import fact. A distinct legacy value remains visible for old
		// rows which predate archive_plan_type.
		if state.Identity.JWTTier == "" || !strings.EqualFold(legacy, state.Identity.JWTTier) {
			state.Identity.ArchivePlan = legacy
			state.Identity.ArchivePlanSource = "legacy_plan_type"
		}
	}
	if state.Identity.JWTTier != "" {
		state.Identity.JWTTierTrust = "unverified"
		if trusted, ok := credentials["jwt_plan_trusted"].(bool); ok && trusted {
			state.Identity.JWTTierTrust = "verified"
		} else if verified, ok := credentials["jwt_plan_verified"].(bool); ok && verified {
			// Compatibility with the short-lived transition field. New writers
			// persist jwt_plan_trusted; neither field is inferred from JWT claims.
			state.Identity.JWTTierTrust = "verified"
		}
	}
	rows, err := db.conn.QueryContext(ctx, `SELECT fact_kind,credential_generation,status,http_status,source,payload_json,field_presence_json,observed_at,expires_at,updated_at FROM grok_account_fact_snapshots WHERE account_id=$1 ORDER BY fact_kind`, accountID)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		f := GrokAccountFact{AccountID: accountID}
		var payload, presence, observed, expires, updated any
		if err = rows.Scan(&f.Kind, &f.CredentialGeneration, &f.Status, &f.HTTPStatus, &f.Source, &payload, &presence, &observed, &expires, &updated); err != nil {
			rows.Close()
			return nil, err
		}
		f.Payload = decodeJSONMap(payload)
		f.FieldPresence = decodeJSONStringMap(presence)
		f.ObservedAt, _ = parseDBTimeValue(observed)
		f.ExpiresAt, _ = parseDBTimeValue(expires)
		f.UpdatedAt, _ = parseDBTimeValue(updated)
		state.Facts[f.Kind] = f
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	catRows, err := db.conn.QueryContext(ctx, `SELECT origin FROM grok_model_catalog_snapshots WHERE account_id=$1 ORDER BY origin`, accountID)
	if err != nil {
		return nil, err
	}
	var origins []string
	for catRows.Next() {
		var origin string
		if err = catRows.Scan(&origin); err != nil {
			catRows.Close()
			return nil, err
		}
		origins = append(origins, origin)
	}
	catRows.Close()
	for _, origin := range origins {
		s, items, e := db.GetGrokModelCatalog(ctx, accountID, origin)
		if e != nil {
			return nil, e
		}
		state.Catalogs = append(state.Catalogs, GrokModelCatalog{Snapshot: *s, Items: items})
	}
	state.Capabilities, err = db.GetGrokModelCapabilities(ctx, accountID)
	if err != nil {
		return nil, err
	}
	return state, nil
}

func (db *DB) deleteGrokAccountStateTx(ctx context.Context, tx *sql.Tx, accountPredicate string, args ...any) error {
	for _, table := range []string{"grok_model_capabilities", "grok_model_catalog_items", "grok_model_catalog_snapshots", "grok_account_fact_snapshots", "grok_credential_identity_claims"} {
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("DELETE FROM %s WHERE account_id %s", table, accountPredicate), args...); err != nil {
			return err
		}
	}
	return nil
}

// SortGrokCatalogItems provides a shared deterministic order for callers that
// synthesize model lists from multiple account catalogs.
func SortGrokCatalogItems(items []GrokModelCatalogItem) {
	sort.SliceStable(items, func(i, j int) bool {
		a, b := strings.ToLower(items[i].ModelID), strings.ToLower(items[j].ModelID)
		if a == b {
			return items[i].ModelID < items[j].ModelID
		}
		return a < b
	})
}
