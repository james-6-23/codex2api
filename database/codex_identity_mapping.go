package database

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"sort"
)

type CodexIdentityMappingPolicy struct {
	Mode   string `json:"mode"`
	Secret string `json:"-"`
}

const CodexIdentityMappingUUIDv7 = "account-uuid7-v2"

type CodexIdentityAliasClaim struct {
	AliasKey  string
	SourceKey string
}

var ErrCodexIdentityAliasCollision = errors.New("codex outbound identity alias collision")

func (db *DB) ensureCodexIdentityMappingTables(ctx context.Context) error {
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS codex_identity_mapping_secret (id INTEGER PRIMARY KEY, secret TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS codex_identity_mapping_policies (root_key TEXT PRIMARY KEY, mode TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS codex_identity_alias_claims (alias_key TEXT PRIMARY KEY, source_key TEXT NOT NULL UNIQUE)`,
		`CREATE TABLE IF NOT EXISTS codex_identity_epochs (identity_key TEXT PRIMARY KEY, state TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS codex_identity_references (reference_key TEXT PRIMARY KEY, state TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS codex_identity_uuid7_values (identity_key TEXT PRIMARY KEY, value TEXT NOT NULL UNIQUE)`,
		`CREATE TABLE IF NOT EXISTS codex_session_context_tokens (token_key TEXT PRIMARY KEY, expires_at BIGINT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_codex_session_context_tokens_expiry ON codex_session_context_tokens(expires_at)`,
	} {
		if _, err := db.conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) ResolveCodexIdentityMapping(ctx context.Context, rootKey string, legacyKeys []string, enabled bool) (CodexIdentityMappingPolicy, error) {
	var policy CodexIdentityMappingPolicy
	if !ValidSessionOperationKey(rootKey) || len(legacyKeys) > 32 {
		return policy, errors.New("invalid codex identity mapping scope")
	}
	for _, key := range legacyKeys {
		if !ValidSessionOperationKey(key) {
			return policy, errors.New("invalid codex legacy identity key")
		}
	}
	err := db.conn.QueryRowContext(ctx, `SELECT p.mode,COALESCE(s.secret,'') FROM codex_identity_mapping_policies p LEFT JOIN codex_identity_mapping_secret s ON s.id=1 WHERE p.root_key=$1`, rootKey).Scan(&policy.Mode, &policy.Secret)
	if !errors.Is(err, sql.ErrNoRows) {
		return policy, err
	}
	err = db.withWriteTx(ctx, func(tx *sql.Tx) error {
		policy.Mode = "preserve"
		if enabled {
			policy.Mode = CodexIdentityMappingUUIDv7
			for _, key := range legacyKeys {
				var exists bool
				if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM codex_identity_claims WHERE identity_key=$1)`, key).Scan(&exists); err != nil {
					return err
				}
				if exists {
					policy.Mode = "preserve"
					break
				}
			}
		}
		var secretExists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM codex_identity_mapping_secret WHERE id=1)`).Scan(&secretExists); err != nil {
			return err
		}
		if !secretExists && policy.Mode != "preserve" {
			var mappingExists bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM codex_identity_mapping_policies WHERE mode IN ('account-suffix-v1','account-uuid7-v2'))`).Scan(&mappingExists); err != nil {
				return err
			}
			if mappingExists {
				return errors.New("codex identity mapping secret is missing; restore the database")
			}
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO codex_identity_mapping_policies(root_key,mode) VALUES ($1,$2) ON CONFLICT(root_key) DO NOTHING`, rootKey, policy.Mode); err != nil {
			return err
		}
		if err := tx.QueryRowContext(ctx, `SELECT mode FROM codex_identity_mapping_policies WHERE root_key=$1`, rootKey).Scan(&policy.Mode); err != nil {
			return err
		}
		if policy.Mode == "preserve" {
			return nil
		}
		if policy.Mode != "account-suffix-v1" && policy.Mode != CodexIdentityMappingUUIDv7 {
			return errors.New("unsupported codex identity mapping policy")
		}
		var secret [32]byte
		if _, err := rand.Read(secret[:]); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO codex_identity_mapping_secret(id,secret) VALUES (1,$1) ON CONFLICT(id) DO NOTHING`, hex.EncodeToString(secret[:])); err != nil {
			return err
		}
		return tx.QueryRowContext(ctx, `SELECT secret FROM codex_identity_mapping_secret WHERE id=1`).Scan(&policy.Secret)
	})
	return policy, err
}

func (db *DB) ClaimCodexIdentityAliases(ctx context.Context, claims []CodexIdentityAliasClaim) error {
	if len(claims) > 32 {
		return errors.New("too many codex identity aliases")
	}
	claims = append([]CodexIdentityAliasClaim(nil), claims...)
	sort.Slice(claims, func(first, second int) bool { return claims[first].AliasKey < claims[second].AliasKey })
	for _, claim := range claims {
		if !ValidSessionOperationKey(claim.AliasKey) || !ValidSessionOperationKey(claim.SourceKey) {
			return errors.New("invalid codex identity alias claim")
		}
	}
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		for _, claim := range claims {
			if _, err := tx.ExecContext(ctx, `INSERT INTO codex_identity_alias_claims(alias_key,source_key) VALUES ($1,$2) ON CONFLICT DO NOTHING`, claim.AliasKey, claim.SourceKey); err != nil {
				return err
			}
			var existing string
			if err := tx.QueryRowContext(ctx, `SELECT source_key FROM codex_identity_alias_claims WHERE alias_key=$1`, claim.AliasKey).Scan(&existing); errors.Is(err, sql.ErrNoRows) {
				return ErrCodexIdentityAliasCollision
			} else if err != nil {
				return err
			}
			if existing != claim.SourceKey {
				return ErrCodexIdentityAliasCollision
			}
		}
		return nil
	})
}
