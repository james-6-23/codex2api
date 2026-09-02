package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Codex 邀请快照的两种类型。资格与已发记录的变化速度不同，各自独立过期。
const (
	CodexInviteSnapshotEligibility = "eligibility"
	CodexInviteSnapshotTracking    = "tracking"
)

// CodexInviteSnapshot 是一次成功的上游邀请查询结果，按账号 + 类型 + 作用域留存。
//
// Scope 是「同一份结果的完整标识」，由调用方按类型拼：资格用 program|entrypoint，
// 已发记录还要带上 period|limit——limit=10 与 limit=100 是两份不同的数据，只按
// program 做键会让前者被当成后者返回。
//
// 只有成功的查询才写进来：上游这两个端点挂在 Cloudflare bot 管理后面，挑战页
// 借用 403 与「无资格」同码，把失败也存成快照会让一次挑战抹掉最后一份可用配额。
// CredentialGeneration 参与写入条件，账号重新授权后旧快照读不出来也压不住新值。
type CodexInviteSnapshot struct {
	AccountID            int64
	Kind                 string
	Scope                string
	CredentialGeneration int64
	HTTPStatus           int
	Payload              json.RawMessage
	ObservedAt           time.Time
	ExpiresAt            time.Time
	UpdatedAt            time.Time
}

// Expired 判断快照是否已过软 TTL。expires_at 是显式列而不是由 observed_at 现算，
// 便于在配额变化时（例如刚发过邀请）直接把某一行提前置为过期。
func (s *CodexInviteSnapshot) Expired(now time.Time) bool {
	return s == nil || s.ExpiresAt.IsZero() || !now.Before(s.ExpiresAt)
}

var (
	codexInviteSnapshotInitMu sync.Mutex
	codexInviteSnapshotReady  = make(map[*DB]bool)
)

func normalizeCodexInviteKind(kind string) string {
	kind = strings.ToLower(strings.TrimSpace(kind))
	if kind == CodexInviteSnapshotTracking {
		return CodexInviteSnapshotTracking
	}
	return CodexInviteSnapshotEligibility
}

func (db *DB) ensureCodexInviteSnapshotTable(ctx context.Context) error {
	if db == nil || db.conn == nil {
		return fmt.Errorf("数据库不可用")
	}
	codexInviteSnapshotInitMu.Lock()
	defer codexInviteSnapshotInitMu.Unlock()
	if codexInviteSnapshotReady[db] {
		return nil
	}

	ddl := `CREATE TABLE IF NOT EXISTS codex_invite_snapshots (
		account_id BIGINT NOT NULL,
		snapshot_kind TEXT NOT NULL,
		scope TEXT NOT NULL DEFAULT '',
		credential_generation BIGINT NOT NULL DEFAULT 1,
		http_status INT NOT NULL DEFAULT 0,
		payload_json JSONB NOT NULL DEFAULT '{}'::jsonb,
		observed_at TIMESTAMPTZ NOT NULL,
		expires_at TIMESTAMPTZ NOT NULL,
		updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
		PRIMARY KEY (account_id, snapshot_kind, scope)
	)`
	if db.isSQLite() {
		ddl = `CREATE TABLE IF NOT EXISTS codex_invite_snapshots (
			account_id INTEGER NOT NULL,
			snapshot_kind TEXT NOT NULL,
			scope TEXT NOT NULL DEFAULT '',
			credential_generation INTEGER NOT NULL DEFAULT 1,
			http_status INTEGER NOT NULL DEFAULT 0,
			payload_json TEXT NOT NULL DEFAULT '{}',
			observed_at TIMESTAMP NOT NULL,
			expires_at TIMESTAMP NOT NULL,
			updated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			PRIMARY KEY (account_id, snapshot_kind, scope)
		)`
	}
	if _, err := db.conn.ExecContext(ctx, ddl); err != nil {
		return err
	}
	if _, err := db.conn.ExecContext(ctx,
		`CREATE INDEX IF NOT EXISTS idx_codex_invite_snapshots_expires ON codex_invite_snapshots(expires_at)`); err != nil {
		return err
	}

	codexInviteSnapshotReady[db] = true
	return nil
}

// GetCodexInviteSnapshot 读取一份邀请快照。没有记录时返回 (nil, nil)——「还没查过」
// 是正常状态，不该逼调用方去分辨 sql.ErrNoRows。过期与否由调用方按自己的 TTL 判定。
func (db *DB) GetCodexInviteSnapshot(ctx context.Context, accountID int64, kind, scope string) (*CodexInviteSnapshot, error) {
	if err := db.ensureCodexInviteSnapshotTable(ctx); err != nil {
		return nil, err
	}
	kind = normalizeCodexInviteKind(kind)
	scope = strings.TrimSpace(scope)

	snap := &CodexInviteSnapshot{}
	var payload, observed, expires, updated any
	err := db.conn.QueryRowContext(ctx, `SELECT account_id,snapshot_kind,scope,credential_generation,http_status,payload_json,observed_at,expires_at,updated_at
		FROM codex_invite_snapshots WHERE account_id=$1 AND snapshot_kind=$2 AND scope=$3`,
		accountID, kind, scope).Scan(&snap.AccountID, &snap.Kind, &snap.Scope,
		&snap.CredentialGeneration, &snap.HTTPStatus, &payload, &observed, &expires, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}

	snap.Payload = json.RawMessage(bytesFromDBValue(payload))
	if snap.ObservedAt, err = parseDBTimeValue(observed); err != nil {
		return nil, err
	}
	if snap.ExpiresAt, err = parseDBTimeValue(expires); err != nil {
		return nil, err
	}
	if snap.UpdatedAt, err = parseDBTimeValue(updated); err != nil {
		return nil, err
	}
	return snap, nil
}

// UpsertCodexInviteSnapshot 写入一份成功观测。DO UPDATE 带 credential_generation
// 单调条件：重新授权后仍在途的旧探针回来时，压不掉新一代凭据写下的快照。
func (db *DB) UpsertCodexInviteSnapshot(ctx context.Context, snap *CodexInviteSnapshot) error {
	if snap == nil {
		return fmt.Errorf("快照为空")
	}
	if err := db.ensureCodexInviteSnapshotTable(ctx); err != nil {
		return err
	}

	payload := snap.Payload
	if len(payload) == 0 || !json.Valid(payload) {
		payload = json.RawMessage(`{}`)
	}
	generation := snap.CredentialGeneration
	if generation <= 0 {
		generation = 1
	}
	observedAt := snap.ObservedAt
	if observedAt.IsZero() {
		observedAt = time.Now()
	}

	query := `INSERT INTO codex_invite_snapshots
		(account_id,snapshot_kind,scope,credential_generation,http_status,payload_json,observed_at,expires_at,updated_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8,CURRENT_TIMESTAMP)
		ON CONFLICT(account_id,snapshot_kind,scope) DO UPDATE SET
		credential_generation=excluded.credential_generation,http_status=excluded.http_status,
		payload_json=excluded.payload_json,observed_at=excluded.observed_at,
		expires_at=excluded.expires_at,updated_at=CURRENT_TIMESTAMP
		WHERE codex_invite_snapshots.credential_generation <= excluded.credential_generation`
	if !db.isSQLite() {
		query = strings.Replace(query, "$6,$7", "$6::jsonb,$7", 1)
	}

	_, err := db.conn.ExecContext(ctx, query,
		snap.AccountID, normalizeCodexInviteKind(snap.Kind), strings.TrimSpace(snap.Scope),
		generation, snap.HTTPStatus, string(payload),
		db.timeArg(observedAt), db.timeArg(snap.ExpiresAt))
	return err
}

// DeleteCodexInviteSnapshots 清掉一个账号的全部邀请快照。发送邀请成功后调用：
// 配额刚被本网关消耗掉，任何缓存值都已经是错的，留着比没有更糟。
func (db *DB) DeleteCodexInviteSnapshots(ctx context.Context, accountID int64) error {
	if err := db.ensureCodexInviteSnapshotTable(ctx); err != nil {
		return err
	}
	_, err := db.conn.ExecContext(ctx, `DELETE FROM codex_invite_snapshots WHERE account_id=$1`, accountID)
	return err
}

// PurgeExpiredCodexInviteSnapshots 删除早于 before 的过期行，供后台清理调用。
// 返回删除行数；驱动不支持 RowsAffected 时返回 0，不算错误。
func (db *DB) PurgeExpiredCodexInviteSnapshots(ctx context.Context, before time.Time) (int64, error) {
	if err := db.ensureCodexInviteSnapshotTable(ctx); err != nil {
		return 0, err
	}
	res, err := db.conn.ExecContext(ctx,
		`DELETE FROM codex_invite_snapshots WHERE expires_at < $1`, db.timeArg(before))
	if err != nil {
		return 0, err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return affected, nil
}
