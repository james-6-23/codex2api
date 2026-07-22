package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/codex2api/internal/openaiidentity"
)

const (
	dataMigrationOAuthIdentityDedupeV1 = "20260616_oauth_identity_dedupe_v1"
	// v2: 身份别名补充 user_id。个人账号(无工作区 account_id)此前只按凭证原文
	// 去重，AT 轮换后产生的重复账号 v1 清不掉；v2 把 email+user_id 也纳入别名
	// 后重跑一次合并。
	dataMigrationOAuthIdentityDedupeV2 = "20260702_oauth_identity_dedupe_v2"
	// usage_logs.channel 回填：按 accounts.platform 推导渠道（xai→grok，其余→codex）；
	// 账号已删的历史行默认 codex（Grok 上线前的流量全部是 codex）。
	dataMigrationUsageLogChannelV1   = "20260721_usage_log_channel_backfill_v1"
	dataMigrationWorkspaceIdentityV3 = "20260722_workspace_identity_v3"
	dataMigrationTimeout             = 5 * time.Minute
)

type oauthIdentityDedupeAccount struct {
	id          int64
	credentials map[string]interface{}
	enabled     bool
	locked      bool
	createdAt   time.Time
	updatedAt   time.Time
}

func (db *DB) runDataMigrations(ctx context.Context) error {
	if err := db.ensureDataMigrationsTable(ctx); err != nil {
		return err
	}
	if err := db.runDataMigrationOnce(ctx, dataMigrationOAuthIdentityDedupeV1, db.dedupeOAuthIdentityAccounts); err != nil {
		return err
	}
	if err := db.runDataMigrationOnce(ctx, dataMigrationOAuthIdentityDedupeV2, db.dedupeOAuthIdentityAccounts); err != nil {
		return err
	}
	if err := db.runDataMigrationOnce(ctx, dataMigrationUsageLogChannelV1, db.backfillUsageLogChannel); err != nil {
		return err
	}
	return db.runDataMigrationOnce(ctx, dataMigrationWorkspaceIdentityV3, db.migrateWorkspaceIdentityV3)
}

// backfillUsageLogChannel 给存量 usage_logs 回填 channel：先按现存账号的 platform
// 标记 grok，其余空值统一置 codex（Grok 渠道上线前的历史流量全部走 Codex）。
func (db *DB) backfillUsageLogChannel(ctx context.Context, tx *sql.Tx) error {
	if db.isSQLite() {
		if _, err := tx.ExecContext(ctx, `
			UPDATE usage_logs SET channel = 'grok'
			WHERE COALESCE(channel, '') = ''
			  AND account_id IN (SELECT id FROM accounts WHERE platform = 'xai')`); err != nil {
			return fmt.Errorf("回填 grok 渠道: %w", err)
		}
	} else {
		if _, err := tx.ExecContext(ctx, `
			UPDATE usage_logs SET channel = 'grok'
			FROM accounts a
			WHERE COALESCE(usage_logs.channel, '') = ''
			  AND usage_logs.account_id = a.id
			  AND a.platform = 'xai'`); err != nil {
			return fmt.Errorf("回填 grok 渠道: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE usage_logs SET channel = 'codex'
		WHERE COALESCE(channel, '') = ''`); err != nil {
		return fmt.Errorf("回填 codex 渠道: %w", err)
	}
	return nil
}

func (db *DB) runDataMigrationsWithTimeout() error {
	ctx, cancel := context.WithTimeout(context.Background(), dataMigrationTimeout)
	defer cancel()
	return db.runDataMigrations(ctx)
}

func (db *DB) ensureDataMigrationsTable(ctx context.Context) error {
	_, err := db.conn.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS data_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ DEFAULT CURRENT_TIMESTAMP
		)
	`)
	if err != nil {
		return fmt.Errorf("创建 data_migrations 表失败: %w", err)
	}
	return nil
}

func (db *DB) runDataMigrationOnce(ctx context.Context, version string, migrate func(context.Context, *sql.Tx) error) error {
	return db.withSQLiteWriteLock(ctx, func() error {
		tx, err := db.conn.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback()

		res, err := tx.ExecContext(ctx, `
			INSERT INTO data_migrations (version, applied_at)
			VALUES ($1, CURRENT_TIMESTAMP)
			ON CONFLICT(version) DO NOTHING
		`, version)
		if err != nil {
			return fmt.Errorf("记录 data migration %s 失败: %w", version, err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return tx.Commit()
		}

		if err := migrate(ctx, tx); err != nil {
			return fmt.Errorf("执行 data migration %s 失败: %w", version, err)
		}
		return tx.Commit()
	})
}

func (db *DB) dedupeOAuthIdentityAccounts(ctx context.Context, tx *sql.Tx) error {
	accounts, err := db.listOAuthIdentityDedupeAccounts(ctx, tx)
	if err != nil {
		return err
	}
	return db.dedupeIdentityAccounts(
		ctx,
		tx,
		accounts,
		oauthIdentityDedupeAliases,
		"oauth_identity_dedupe_v1",
		dataMigrationOAuthIdentityDedupeV1,
	)
}

func (db *DB) migrateWorkspaceIdentityV3(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, credentials
		FROM accounts
		WHERE type = 'oauth'
		  AND status <> 'deleted'
		  AND COALESCE(error_message, '') <> 'deleted'
	`)
	if err != nil {
		return err
	}
	defer rows.Close()

	type update struct {
		id          int64
		credentials map[string]interface{}
	}
	var updates []update
	for rows.Next() {
		var id int64
		var raw interface{}
		if err := rows.Scan(&id, &raw); err != nil {
			return err
		}
		credentials := decodeCredentials(raw)
		workspaceID := ""
		email := ""
		for _, tokenKey := range []string{"id_token", "access_token"} {
			if token := credentialStringFromMap(credentials, tokenKey); token != "" {
				if claims, parseErr := openaiidentity.ParseJWT(token); parseErr == nil {
					if workspaceID == "" {
						workspaceID = claims.WorkspaceID()
					}
					if email == "" {
						email = strings.TrimSpace(claims.Email)
						if email == "" && claims.OpenAIProfile != nil {
							email = strings.TrimSpace(claims.OpenAIProfile.Email)
						}
					}
					if workspaceID != "" && email != "" {
						break
					}
				}
			}
		}
		storedWorkspaceID := openaiidentity.NormalizeWorkspaceID(credentialStringFromMap(credentials, "workspace_id"))
		storedEmail := strings.TrimSpace(credentialStringFromMap(credentials, "email"))
		// 迁移只补充 JWT 能确认的 workspace_id；解析不到时保留历史值，
		// 包括 user-* 等旧字段，避免扩大迁移的修改范围。
		workspaceNeedsUpdate := workspaceID != "" && workspaceID != storedWorkspaceID
		if !workspaceNeedsUpdate && (email == "" || storedEmail != "") {
			continue
		}
		if workspaceNeedsUpdate {
			credentials["workspace_id"] = workspaceID
		}
		if storedEmail == "" && email != "" {
			credentials["email"] = email
		}
		updates = append(updates, update{id: id, credentials: credentials})
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, item := range updates {
		encoded, err := json.Marshal(item.credentials)
		if err != nil {
			return fmt.Errorf("序列化账号 %d credentials 失败: %w", item.id, err)
		}
		// 回填不刷新 updated_at，避免迁移改变重复账号的胜出顺序。
		query := `UPDATE accounts SET credentials = $1 WHERE id = $2`
		if !db.isSQLite() {
			query = `UPDATE accounts SET credentials = $1::jsonb WHERE id = $2`
		}
		if _, err := tx.ExecContext(ctx, query, encoded, item.id); err != nil {
			return fmt.Errorf("回填账号 %d workspace_id 失败: %w", item.id, err)
		}
	}
	if err := db.dedupeWorkspaceIdentityAccounts(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, oauthIdentityUniqueIndexSQL(db.isSQLite())); err != nil {
		return fmt.Errorf("创建 OAuth 身份唯一索引失败: %w", err)
	}
	return nil
}

func (db *DB) dedupeWorkspaceIdentityAccounts(ctx context.Context, tx *sql.Tx) error {
	accounts, err := db.listWorkspaceIdentityDedupeAccounts(ctx, tx)
	if err != nil {
		return err
	}
	return db.dedupeIdentityAccounts(
		ctx,
		tx,
		accounts,
		workspaceIdentityDedupeAliases,
		"workspace_identity_v3",
		dataMigrationWorkspaceIdentityV3,
	)
}

func (db *DB) dedupeIdentityAccounts(
	ctx context.Context,
	tx *sql.Tx,
	accounts []oauthIdentityDedupeAccount,
	aliasesFor func(map[string]interface{}) []string,
	eventSource string,
	migrationVersion string,
) error {
	parent := make([]int, len(accounts))
	eligible := make([]bool, len(accounts))
	for i := range parent {
		parent[i] = i
	}
	var find func(int) int
	find = func(i int) int {
		if parent[i] != i {
			parent[i] = find(parent[i])
		}
		return parent[i]
	}
	union := func(a, b int) {
		rootA := find(a)
		rootB := find(b)
		if rootA != rootB {
			parent[rootB] = rootA
		}
	}

	aliasOwner := make(map[string]int)
	for i, account := range accounts {
		aliases := aliasesFor(account.credentials)
		if len(aliases) == 0 {
			continue
		}
		eligible[i] = true
		for _, alias := range aliases {
			if owner, ok := aliasOwner[alias]; ok {
				union(i, owner)
				continue
			}
			aliasOwner[alias] = i
		}
	}

	groups := make(map[int][]oauthIdentityDedupeAccount)
	for i, account := range accounts {
		if !eligible[i] {
			continue
		}
		groups[find(i)] = append(groups[find(i)], account)
	}

	var loserIDs []int64
	duplicateGroups := 0
	for _, group := range groups {
		if len(group) < 2 {
			continue
		}
		duplicateGroups++
		sort.SliceStable(group, func(i, j int) bool {
			return oauthIdentityDedupeWinnerLess(group[i], group[j])
		})
		for _, loser := range group[1:] {
			loserIDs = append(loserIDs, loser.id)
		}
	}
	if len(loserIDs) == 0 {
		return nil
	}

	sort.Slice(loserIDs, func(i, j int) bool { return loserIDs[i] < loserIDs[j] })
	if err := softDeleteAccountsTx(ctx, tx, loserIDs); err != nil {
		return err
	}
	if err := insertAccountEventsTx(ctx, tx, loserIDs, "deleted", eventSource); err != nil {
		return err
	}
	log.Printf("[data_migration] %s: 发现 %d 组重复 OAuth 身份，已软删除 %d 个重复账号", migrationVersion, duplicateGroups, len(loserIDs))
	return nil
}

func (db *DB) listOAuthIdentityDedupeAccounts(ctx context.Context, tx *sql.Tx) ([]oauthIdentityDedupeAccount, error) {
	return db.listOAuthIdentityDedupeAccountsFiltered(ctx, tx, false)
}

func (db *DB) listWorkspaceIdentityDedupeAccounts(ctx context.Context, tx *sql.Tx) ([]oauthIdentityDedupeAccount, error) {
	return db.listOAuthIdentityDedupeAccountsFiltered(ctx, tx, true)
}

func (db *DB) listOAuthIdentityDedupeAccountsFiltered(ctx context.Context, tx *sql.Tx, onlyOAuth bool) ([]oauthIdentityDedupeAccount, error) {
	query := `
		SELECT id, credentials, COALESCE(enabled, true), COALESCE(locked, false), created_at, updated_at
		FROM accounts
		WHERE status <> 'deleted' AND COALESCE(error_message, '') <> 'deleted'
	`
	if onlyOAuth {
		query += ` AND type = 'oauth'`
	}
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var accounts []oauthIdentityDedupeAccount
	for rows.Next() {
		var account oauthIdentityDedupeAccount
		var rawCredentials interface{}
		var createdRaw interface{}
		var updatedRaw interface{}
		if err := rows.Scan(
			&account.id,
			&rawCredentials,
			&account.enabled,
			&account.locked,
			&createdRaw,
			&updatedRaw,
		); err != nil {
			return nil, err
		}
		account.credentials = decodeCredentials(rawCredentials)
		account.createdAt, err = parseDBTimeValue(createdRaw)
		if err != nil {
			return nil, fmt.Errorf("解析账号 %d created_at 失败: %w", account.id, err)
		}
		account.updatedAt, err = parseDBTimeValue(updatedRaw)
		if err != nil {
			return nil, fmt.Errorf("解析账号 %d updated_at 失败: %w", account.id, err)
		}
		accounts = append(accounts, account)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return accounts, nil
}

func oauthIdentityDedupeAliases(credentials map[string]interface{}) []string {
	// 用户勾选"允许重复添加"强制导入的副本带 allow_duplicate 标记，
	// 是故意保留的重复（如同一账号配不同代理），不得参与合并。
	if strings.EqualFold(strings.TrimSpace(credentialStringFromMap(credentials, "allow_duplicate")), "true") {
		return nil
	}
	email := strings.ToLower(strings.TrimSpace(credentialStringFromMap(credentials, "email")))
	if email == "" {
		return nil
	}
	seen := make(map[string]struct{}, 2)
	// user_id 也是身份别名：个人账号可能没有工作区 account_id，且旧版 wham
	// 回填曾把 user_id 写进 account_id 字段，两种形态要能合并到同一组。
	for _, key := range []string{"account_id", "chatgpt_account_id", "user_id"} {
		accountID := strings.TrimSpace(credentialStringFromMap(credentials, key))
		if accountID == "" {
			continue
		}
		seen[email+"\x00"+accountID] = struct{}{}
	}
	aliases := make([]string, 0, len(seen))
	for alias := range seen {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	return aliases
}

func workspaceIdentityDedupeAliases(credentials map[string]interface{}) []string {
	email := strings.ToLower(strings.TrimSpace(credentialStringFromMap(credentials, "email")))
	if email == "" {
		return nil
	}
	workspaceID := openaiidentity.NormalizeWorkspaceID(credentialStringFromMap(credentials, "workspace_id"))
	if workspaceID == "" {
		return nil
	}
	return []string{email + "\x00" + workspaceID}
}

func credentialStringFromMap(credentials map[string]interface{}, key string) string {
	if credentials == nil {
		return ""
	}
	value, ok := credentials[key]
	if !ok || value == nil {
		return ""
	}
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return fmt.Sprintf("%v", typed)
	default:
		return ""
	}
}

func oauthIdentityDedupeWinnerLess(a, b oauthIdentityDedupeAccount) bool {
	if !a.updatedAt.Equal(b.updatedAt) {
		return a.updatedAt.After(b.updatedAt)
	}
	if scoreA, scoreB := oauthIdentityCredentialScore(a.credentials), oauthIdentityCredentialScore(b.credentials); scoreA != scoreB {
		return scoreA > scoreB
	}
	if a.enabled != b.enabled {
		return a.enabled
	}
	if a.locked != b.locked {
		return !a.locked
	}
	if !a.createdAt.Equal(b.createdAt) {
		return a.createdAt.After(b.createdAt)
	}
	return a.id > b.id
}

func oauthIdentityCredentialScore(credentials map[string]interface{}) int {
	score := 0
	if strings.TrimSpace(credentialStringFromMap(credentials, "access_token")) != "" {
		score += 4
	}
	if strings.TrimSpace(credentialStringFromMap(credentials, "refresh_token")) != "" {
		score += 2
	}
	if strings.TrimSpace(credentialStringFromMap(credentials, "session_token")) != "" {
		score++
	}
	return score
}

func softDeleteAccountsTx(ctx context.Context, tx *sql.Tx, ids []int64) error {
	const batchSize = 500
	for i := 0; i < len(ids); i += batchSize {
		end := i + batchSize
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[i:end]
		placeholders := make([]string, len(batch))
		args := make([]interface{}, 0, len(batch))
		for j, id := range batch {
			placeholders[j] = fmt.Sprintf("$%d", j+1)
			args = append(args, id)
		}
		query := fmt.Sprintf(`
			UPDATE accounts
			SET status = 'deleted',
				error_message = '',
				cooldown_reason = '',
				cooldown_until = NULL,
				deleted_at = CURRENT_TIMESTAMP,
				updated_at = CURRENT_TIMESTAMP
			WHERE status <> 'deleted' AND id IN (%s)
		`, strings.Join(placeholders, ","))
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("软删除重复账号失败: %w", err)
		}

		query = fmt.Sprintf(`DELETE FROM account_group_members WHERE account_id IN (%s)`, strings.Join(placeholders, ","))
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("清理重复账号分组关系失败: %w", err)
		}
	}
	return nil
}

func insertAccountEventsTx(ctx context.Context, tx *sql.Tx, ids []int64, eventType, source string) error {
	const batchSize = 500
	for i := 0; i < len(ids); i += batchSize {
		end := i + batchSize
		if end > len(ids) {
			end = len(ids)
		}
		batch := ids[i:end]
		placeholders := make([]string, len(batch))
		args := make([]interface{}, 0, len(batch)+2)
		args = append(args, eventType, source)
		for j, id := range batch {
			paramIdx := j + 3
			placeholders[j] = fmt.Sprintf("($%d, $1, $2)", paramIdx)
			args = append(args, id)
		}
		query := fmt.Sprintf(`INSERT INTO account_events (account_id, event_type, source) VALUES %s`, strings.Join(placeholders, ","))
		if _, err := tx.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("记录重复账号清理事件失败: %w", err)
		}
	}
	return nil
}
