package database

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

type SessionErrorIdentity struct {
	Key         string `json:"key"`
	Kind        string `json:"kind"`
	Platform    string `json:"platform"`
	UserID      string `json:"user_id"`
	UserLabel   string `json:"user_label,omitempty"`
	SessionID   string `json:"session_id,omitempty"`
	Fingerprint string `json:"root_fingerprint,omitempty"`
	ParentKey   string `json:"parent_key,omitempty"`
}

type SessionErrorEvent struct {
	Identity        SessionErrorIdentity `json:"identity"`
	CreatedAt       time.Time            `json:"created_at"`
	RequestID       string               `json:"request_id"`
	NewAPIRequestID string               `json:"newapi_request_id,omitempty"`
	Model           string               `json:"model,omitempty"`
	Code            string               `json:"code"`
	Message         string               `json:"message"`
	Endpoint        string               `json:"endpoint"`
	Transport       string               `json:"transport"`
	AccountID       int64                `json:"account_id,omitempty"`
	RequestType     string               `json:"request_type,omitempty"`
}

type SessionErrorRow struct {
	Identity       SessionErrorIdentity `json:"identity"`
	AccountName    string               `json:"account_name,omitempty"`
	AccountEmail   string               `json:"account_email,omitempty"`
	Count          int64                `json:"count"`
	FirstAt        time.Time            `json:"first_at"`
	LastAt         time.Time            `json:"last_at"`
	Latest         SessionErrorEvent    `json:"latest"`
	Locked         bool                 `json:"locked"`
	LockedBy       string               `json:"locked_by,omitempty"`
	LineageInvalid bool                 `json:"lineage_invalid,omitempty"`
}

type SessionErrorQuery struct {
	UserID, SessionID, Cursor string
	LockState                 string
	LockedOnly                bool
	Limit                     int
}

type SessionErrorPage struct {
	Items      []SessionErrorRow          `json:"items"`
	NextCursor string                     `json:"next_cursor,omitempty"`
	Groups     int64                      `json:"groups"`
	Errors     int64                      `json:"errors"`
	Collector  ServiceErrorCollectorStats `json:"collector"`
}

type sessionBlacklistResult struct {
	LockedBy string
	Until    time.Time
}

type sessionBlacklistCache struct {
	mu      sync.Mutex
	entries map[string]sessionBlacklistResult
	parents map[string]string
	version uint64
}

var ErrSessionLineageConflict = errors.New("session lineage changed or exceeded the safe depth")

func ValidSessionOperationKey(key string) bool {
	decoded, err := hex.DecodeString(key)
	return len(key) == 64 && len(decoded) == 32 && err == nil && key == strings.ToLower(key)
}

func (db *DB) ensureSessionErrorSchema(ctx context.Context) error {
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS session_error_stats (session_key TEXT PRIMARY KEY, user_id TEXT NOT NULL, session_id TEXT NOT NULL, first_at BIGINT NOT NULL, last_at BIGINT NOT NULL, error_count BIGINT NOT NULL, identity_data TEXT NOT NULL, latest_data TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_session_error_stats_last ON session_error_stats(last_at, session_key)`,
		`CREATE INDEX IF NOT EXISTS idx_session_error_stats_user ON session_error_stats(user_id, last_at)`,
		`CREATE INDEX IF NOT EXISTS idx_session_error_stats_session ON session_error_stats(session_id, last_at)`,
		`CREATE TABLE IF NOT EXISTS session_blacklist (session_key TEXT PRIMARY KEY, user_id TEXT NOT NULL, session_id TEXT NOT NULL, identity_data TEXT NOT NULL, locked INTEGER NOT NULL, updated_at BIGINT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_session_blacklist_active ON session_blacklist(locked, updated_at, session_key)`,
		`CREATE TABLE IF NOT EXISTS session_identity_links (session_key TEXT PRIMARY KEY, parent_key TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS session_policy_lock_identities (session_key TEXT NOT NULL, lock_kind TEXT NOT NULL, lock_key TEXT NOT NULL, PRIMARY KEY(session_key,lock_kind,lock_key))`,
	} {
		if _, err := db.conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) RecordSessionParent(ctx context.Context, key, parent string) error {
	if !ValidSessionOperationKey(key) || !ValidSessionOperationKey(parent) || key == parent {
		return ErrSessionLineageConflict
	}
	db.sessionBlacklist.mu.Lock()
	known, exists := db.sessionBlacklist.parents[key]
	db.sessionBlacklist.mu.Unlock()
	if exists {
		if known != parent {
			return ErrSessionLineageConflict
		}
		return nil
	}
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO session_identity_links(session_key,parent_key) VALUES ($1,$2) ON CONFLICT(session_key) DO NOTHING`, key, parent); err != nil {
			return err
		}
		var stored string
		if err := tx.QueryRowContext(ctx, `SELECT parent_key FROM session_identity_links WHERE session_key=$1`, key).Scan(&stored); err != nil {
			return err
		}
		if stored != parent {
			return ErrSessionLineageConflict
		}
		return nil
	})
	if err == nil {
		db.sessionBlacklist.mu.Lock()
		if db.sessionBlacklist.parents == nil || len(db.sessionBlacklist.parents) >= 4096 {
			db.sessionBlacklist.parents = make(map[string]string)
		}
		db.sessionBlacklist.parents[key] = parent
		db.sessionBlacklist.entries = nil
		db.sessionBlacklist.version++
		db.sessionBlacklist.mu.Unlock()
	}
	return err
}

func (db *DB) SessionBlacklistStatus(ctx context.Context, key, parent string) (string, error) {
	if !ValidSessionOperationKey(key) || parent != "" && !ValidSessionOperationKey(parent) {
		return "", ErrSessionLineageConflict
	}
	cacheKey := key + ":" + parent
	db.sessionBlacklist.mu.Lock()
	cached, found := db.sessionBlacklist.entries[cacheKey]
	version := db.sessionBlacklist.version
	db.sessionBlacklist.mu.Unlock()
	if found && time.Now().Before(cached.Until) {
		return cached.LockedBy, nil
	}
	if parent != "" {
		if err := db.RecordSessionParent(ctx, key, parent); err != nil {
			return "", err
		}
		db.sessionBlacklist.mu.Lock()
		version = db.sessionBlacklist.version
		db.sessionBlacklist.mu.Unlock()
	}
	results, err := db.sessionBlacklistStatuses(ctx, []string{key})
	if err != nil {
		return "", err
	}
	result := results[key]
	if result == "lineage_invalid" {
		return "", ErrSessionLineageConflict
	}
	db.sessionBlacklist.mu.Lock()
	if version == db.sessionBlacklist.version {
		if db.sessionBlacklist.entries == nil || len(db.sessionBlacklist.entries) >= 4096 {
			db.sessionBlacklist.entries = make(map[string]sessionBlacklistResult)
		}
		db.sessionBlacklist.entries[cacheKey] = sessionBlacklistResult{LockedBy: result, Until: time.Now().Add(2 * time.Second)}
	}
	db.sessionBlacklist.mu.Unlock()
	return result, nil
}

func (db *DB) sessionBlacklistStatuses(ctx context.Context, keys []string) (map[string]string, error) {
	result := make(map[string]string)
	if len(keys) == 0 {
		return result, nil
	}
	seeds := make([]string, 0, len(keys))
	args := make([]any, 0, len(keys))
	for _, key := range keys {
		args = append(args, key)
		placeholder := fmt.Sprintf("$%d", len(args))
		seeds = append(seeds, "SELECT CAST("+placeholder+" AS TEXT), CAST("+placeholder+" AS TEXT), 0")
	}
	query := `WITH RECURSIVE ancestry(start_key,session_key,depth) AS (` + strings.Join(seeds, " UNION ALL ") + ` UNION ALL SELECT ancestry.start_key, links.parent_key, ancestry.depth+1 FROM session_identity_links links JOIN ancestry ON links.session_key=ancestry.session_key WHERE ancestry.depth < 64) SELECT ancestry.start_key, ancestry.session_key, ancestry.depth, COALESCE(blacklist.locked,0) FROM ancestry LEFT JOIN session_blacklist blacklist ON blacklist.session_key=ancestry.session_key WHERE blacklist.locked=1 OR ancestry.depth=64 ORDER BY ancestry.depth`
	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var start, ancestor string
		var depth, locked int
		if err := rows.Scan(&start, &ancestor, &depth, &locked); err != nil {
			return nil, err
		}
		if depth == 64 && locked == 0 {
			if result[start] == "" {
				result[start] = "lineage_invalid"
			}
			continue
		}
		if result[start] == "" {
			result[start] = ancestor
		}
	}
	return result, rows.Err()
}

func (db *DB) SetSessionBlacklist(ctx context.Context, keys []string, locked bool) error {
	if len(keys) == 0 || len(keys) > 100 {
		return fmt.Errorf("select between 1 and 100 sessions")
	}
	seen := make(map[string]bool)
	for _, key := range keys {
		if !ValidSessionOperationKey(key) || seen[key] {
			return fmt.Errorf("invalid or duplicate session key")
		}
		seen[key] = true
	}
	lockValue := 0
	if locked {
		lockValue = 1
	}
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		for _, key := range keys {
			var userID, sessionID, payload string
			queryErr := tx.QueryRowContext(ctx, `SELECT user_id,session_id,identity_data FROM session_error_stats WHERE session_key=$1 UNION ALL SELECT user_id,session_id,identity_data FROM session_blacklist WHERE session_key=$1 LIMIT 1`, key).Scan(&userID, &sessionID, &payload)
			if queryErr != nil {
				return queryErr
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO session_blacklist(session_key,user_id,session_id,identity_data,locked,updated_at) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(session_key) DO UPDATE SET locked=excluded.locked, updated_at=excluded.updated_at`, key, userID, sessionID, payload, lockValue, time.Now().UnixMilli()); err != nil {
				return err
			}
		}
		return nil
	})
	if err == nil {
		db.sessionBlacklist.mu.Lock()
		db.sessionBlacklist.entries = nil
		db.sessionBlacklist.version++
		db.sessionBlacklist.mu.Unlock()
	}
	return err
}

func (db *DB) ListSessionErrors(ctx context.Context, filter SessionErrorQuery) (SessionErrorPage, error) {
	page := SessionErrorPage{Items: []SessionErrorRow{}, Collector: db.SessionErrorCollectorStats()}
	if filter.LockState != "" && filter.LockState != "all" && filter.LockState != "locked" && filter.LockState != "unlocked" {
		return page, fmt.Errorf("invalid session lock state")
	}
	cursor, err := decodeServiceErrorCursor(filter.Cursor)
	if err != nil {
		return page, err
	}
	if filter.Limit < 1 || filter.Limit > 100 {
		filter.Limit = 20
	}
	from := `session_error_stats stats`
	stamp, key := "stats.last_at", "stats.session_key"
	projection := `stats.identity_data,stats.error_count,stats.first_at,stats.last_at,stats.latest_data,stats.last_at,stats.session_key`
	identitySource := "stats"
	conditions := []string{"1=1"}
	if filter.LockedOnly {
		from = `session_blacklist blacklist LEFT JOIN session_error_stats stats ON stats.session_key=blacklist.session_key`
		stamp, key, identitySource = "blacklist.updated_at", "blacklist.session_key", "blacklist"
		projection = `blacklist.identity_data,COALESCE(stats.error_count,0),COALESCE(stats.first_at,0),COALESCE(stats.last_at,0),COALESCE(stats.latest_data,'{}'),blacklist.updated_at,blacklist.session_key`
		conditions = append(conditions, "blacklist.locked=1")
	}
	args := []any{}
	for _, item := range []struct{ column, value string }{{"user_id", filter.UserID}, {"session_id", filter.SessionID}} {
		if item.value != "" {
			args = append(args, item.value)
			conditions = append(conditions, fmt.Sprintf("%s.%s=$%d", identitySource, item.column, len(args)))
		}
	}
	where := strings.Join(conditions, " AND ")
	prefix := ""
	if !filter.LockedOnly && (filter.LockState == "locked" || filter.LockState == "unlocked") {
		prefix = `WITH RECURSIVE session_ancestors(start_key,session_key,depth) AS (
			SELECT stats.session_key,links.parent_key,1 FROM session_error_stats stats
			JOIN session_identity_links links ON links.session_key=stats.session_key WHERE ` + where + `
			UNION ALL SELECT ancestry.start_key,links.parent_key,ancestry.depth+1
			FROM session_ancestors ancestry JOIN session_identity_links links ON links.session_key=ancestry.session_key WHERE ancestry.depth<64
		), filtered_locked_sessions(session_key) AS (
			SELECT session_key FROM session_blacklist WHERE locked=1
			UNION SELECT ancestry.start_key FROM session_ancestors ancestry
			LEFT JOIN session_blacklist blacklist ON blacklist.session_key=ancestry.session_key
			WHERE blacklist.locked=1 OR ancestry.depth=64
		) `
		operator := "IN"
		if filter.LockState == "unlocked" {
			operator = "NOT IN"
		}
		where += ` AND stats.session_key ` + operator + ` (SELECT session_key FROM filtered_locked_sessions)`
	}
	if err := db.conn.QueryRowContext(ctx, prefix+`SELECT COUNT(*),COALESCE(SUM(stats.error_count),0) FROM `+from+` WHERE `+where, args...).Scan(&page.Groups, &page.Errors); err != nil {
		return page, err
	}
	if cursor.ID != "" {
		args = append(args, cursor.CreatedAt, cursor.ID)
		where += fmt.Sprintf(" AND (%s<$%d OR (%s=$%d AND %s<$%d))", stamp, len(args)-1, stamp, len(args)-1, key, len(args))
	}
	args = append(args, filter.Limit+1)
	query := prefix + `SELECT ` + projection + ` FROM ` + from + ` WHERE ` + where + ` ORDER BY ` + stamp + ` DESC,` + key + fmt.Sprintf(" DESC LIMIT $%d", len(args))
	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return page, err
	}
	keys := []string{}
	cursors := []serviceErrorCursor{}
	for rows.Next() {
		var row SessionErrorRow
		var identity, latest, rowKey string
		var first, last, rowStamp int64
		if err := rows.Scan(&identity, &row.Count, &first, &last, &latest, &rowStamp, &rowKey); err != nil {
			rows.Close()
			return page, err
		}
		if json.Unmarshal([]byte(identity), &row.Identity) != nil || json.Unmarshal([]byte(latest), &row.Latest) != nil {
			rows.Close()
			return page, fmt.Errorf("invalid session error snapshot")
		}
		if first > 0 {
			row.FirstAt = time.UnixMilli(first).UTC()
		}
		if last > 0 {
			row.LastAt = time.UnixMilli(last).UTC()
		}
		page.Items = append(page.Items, row)
		keys = append(keys, rowKey)
		cursors = append(cursors, serviceErrorCursor{CreatedAt: rowStamp, ID: rowKey})
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return page, err
	}
	if len(page.Items) > filter.Limit {
		page.Items, keys, cursors = page.Items[:filter.Limit], keys[:filter.Limit], cursors[:filter.Limit]
		payload, _ := json.Marshal(cursors[len(cursors)-1])
		page.NextCursor = base64.RawURLEncoding.EncodeToString(payload)
	}
	statuses, err := db.sessionBlacklistStatuses(ctx, keys)
	if err != nil {
		return page, err
	}
	for index := range page.Items {
		page.Items[index].LockedBy = statuses[keys[index]]
		page.Items[index].Locked = statuses[keys[index]] != ""
		if statuses[keys[index]] == "lineage_invalid" {
			page.Items[index].LineageInvalid = true
			page.Items[index].LockedBy = ""
		}
	}
	if err := db.populateSessionErrorAccounts(ctx, page.Items); err != nil {
		return page, err
	}
	return page, nil
}

func (db *DB) populateSessionErrorAccounts(ctx context.Context, items []SessionErrorRow) error {
	ids := make([]int64, 0, len(items))
	for _, item := range items {
		ids = append(ids, item.Latest.AccountID)
	}
	ids = positiveUniqueIDs(ids)
	if len(ids) == 0 {
		return nil
	}
	query := `SELECT id, COALESCE(name,''), COALESCE(CAST(credentials AS TEXT),'{}') FROM accounts WHERE id IN (` + strings.Join(dbPlaceholders(db.isSQLite(), 1, len(ids)), ",") + `)`
	rows, err := db.conn.QueryContext(ctx, query, argsFromInt64s(ids)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	labels := make(map[int64][2]string, len(ids))
	for rows.Next() {
		var id int64
		var name, credentials string
		if err := rows.Scan(&id, &name, &credentials); err != nil {
			return err
		}
		labels[id] = [2]string{serviceErrorString(name, 160), serviceErrorString(accountEmailFromRawCredentials(credentials), 256)}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for index := range items {
		label := labels[items[index].Latest.AccountID]
		items[index].AccountName, items[index].AccountEmail = label[0], label[1]
	}
	return nil
}
