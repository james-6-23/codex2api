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
	// account_groups.channel 归类:成员全为 Grok 账号的存量分组标记为 grok 渠道,
	// 其余(含空组/混合组)保持 codex。此后分组按渠道隔离,写入路径强校验。
	dataMigrationGroupChannelV1 = "20260807_account_group_channel_v1"
	// Claude 原生渠道上线后的存量回填：只修复能从当前账号、端点或模型可靠
	// 识别的记录；不把混合分组或历史不明请求强行改写成 Claude。
	dataMigrationClaudeProviderV1 = "20260829_claude_provider_backfill_v1"
	dataMigrationTimeout          = 5 * time.Minute
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
	if err := db.runDataMigrationOnce(ctx, dataMigrationWorkspaceIdentityV3, db.migrateWorkspaceIdentityV3); err != nil {
		return err
	}
	if err := db.runDataMigrationOnce(ctx, dataMigrationGroupChannelV1, db.classifyAccountGroupChannels); err != nil {
		return err
	}
	return db.runDataMigrationOnce(ctx, dataMigrationClaudeProviderV1, db.backfillClaudeProviderData)
}

// classifyAccountGroupChannels 把成员清一色是 Grok 账号的存量分组归到 grok 渠道。
// 混合组保持 codex 且成员不动(只在后续写入时强校验),避免迁移悄悄拆散生产分组。
func (db *DB) classifyAccountGroupChannels(ctx context.Context, tx *sql.Tx) error {
	upstreamTypeExpr := `LOWER(COALESCE(a.credentials->>'upstream_type', ''))`
	if db.isSQLite() {
		upstreamTypeExpr = `LOWER(COALESCE(json_extract(a.credentials, '$.upstream_type'), ''))`
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE account_groups SET channel = 'grok'
		WHERE COALESCE(channel, 'codex') <> 'grok' AND id IN (
			SELECT m.group_id
			FROM account_group_members m
			JOIN accounts a ON a.id = m.account_id
			WHERE a.status <> 'deleted' AND COALESCE(a.error_message, '') <> 'deleted'
			GROUP BY m.group_id
			HAVING COUNT(*) = SUM(CASE WHEN `+upstreamTypeExpr+` = 'grok' THEN 1 ELSE 0 END)
		)`)
	if err != nil {
		return fmt.Errorf("归类 grok 分组: %w", err)
	}
	if affected, err := res.RowsAffected(); err == nil && affected > 0 {
		log.Printf("[data_migration] %s: %d 个存量分组归类为 grok 渠道", dataMigrationGroupChannelV1, affected)
	}
	return nil
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

// backfillClaudeProviderData repairs two conservative pieces of provider
// metadata for databases that predate the native Claude channel. Usage rows are
// updated only when their endpoint/model clearly identifies Anthropic Messages
// traffic and the credential generation still matches the account (or is a
// legacy zero). Pure/mixed groups are left untouched; only groups whose active
// members are all Claude accounts are promoted from the legacy Codex channel.
func (db *DB) backfillClaudeProviderData(ctx context.Context, tx *sql.Tx) error {
	upstreamTypeExpr := `LOWER(COALESCE(a.credentials->>'upstream_type', ''))`
	if db.isSQLite() {
		upstreamTypeExpr = `LOWER(COALESCE(json_extract(a.credentials, '$.upstream_type'), ''))`
	}
	// Do not update usage_logs with a correlated account subquery. On a large
	// history that shape forces a full usage_logs scan (and a repeated accounts
	// scan) while the migration holds the startup write transaction. Resolve the
	// small set of Claude account generations once, then update by the existing
	// account_id/credential_generation indexes in bounded batches.
	accountRows, err := tx.QueryContext(ctx, `
		SELECT id, COALESCE(credential_generation, 0)
		FROM accounts a
		WHERE `+upstreamTypeExpr+` = 'claude'
		ORDER BY id`)
	if err != nil {
		return fmt.Errorf("读取 Claude 账号代际: %w", err)
	}
	const batchSize = 500
	zeroGenerationIDs := make([]int64, 0)
	idsByGeneration := make(map[int64][]int64)
	for accountRows.Next() {
		var id, generation int64
		if err := accountRows.Scan(&id, &generation); err != nil {
			accountRows.Close()
			return fmt.Errorf("读取 Claude 账号代际: %w", err)
		}
		if generation <= 0 {
			zeroGenerationIDs = append(zeroGenerationIDs, id)
			continue
		}
		idsByGeneration[generation] = append(idsByGeneration[generation], id)
	}
	if err := accountRows.Err(); err != nil {
		accountRows.Close()
		return fmt.Errorf("读取 Claude 账号代际: %w", err)
	}
	if err := accountRows.Close(); err != nil {
		return fmt.Errorf("关闭 Claude 账号代际游标: %w", err)
	}

	updateUsageBatch := func(ids []int64, generation *int64) (int64, error) {
		var affectedTotal int64
		for start := 0; start < len(ids); start += batchSize {
			end := start + batchSize
			if end > len(ids) {
				end = len(ids)
			}
			batch := ids[start:end]
			placeholders := dbPlaceholders(db.isSQLite(), 1, len(batch))
			args := argsFromInt64s(batch)
			generationPredicate := "COALESCE(credential_generation, 0) = 0"
			if generation != nil {
				args = append(args, *generation)
				generationPlaceholder := "?"
				if !db.isSQLite() {
					generationPlaceholder = fmt.Sprintf("$%d", len(args))
				}
				generationPredicate = "credential_generation = " + generationPlaceholder
			}
			usageQuery := fmt.Sprintf(`
				UPDATE usage_logs
				SET channel = 'claude'
				WHERE COALESCE(channel, '') IN ('', 'codex')
				  AND account_id IN (%s)
				  AND (LOWER(COALESCE(endpoint, '')) LIKE '/v1/messages%%'
				       OR LOWER(COALESCE(model, '')) LIKE 'claude-%%')
				  AND %s`, strings.Join(placeholders, ","), generationPredicate)
			res, err := tx.ExecContext(ctx, usageQuery, args...)
			if err != nil {
				return affectedTotal, fmt.Errorf("回填 Claude usage_logs 渠道: %w", err)
			}
			if affected, err := res.RowsAffected(); err == nil {
				affectedTotal += affected
			}
		}
		return affectedTotal, nil
	}

	var usageAffected int64
	if affected, err := updateUsageBatch(zeroGenerationIDs, nil); err != nil {
		return err
	} else {
		usageAffected += affected
	}
	generations := make([]int64, 0, len(idsByGeneration))
	for generation := range idsByGeneration {
		generations = append(generations, generation)
	}
	sort.Slice(generations, func(i, j int) bool { return generations[i] < generations[j] })
	for _, generation := range generations {
		if affected, err := updateUsageBatch(idsByGeneration[generation], &generation); err != nil {
			return err
		} else {
			usageAffected += affected
		}
	}
	if usageAffected > 0 {
		log.Printf("[data_migration] %s: %d 条 usage_logs 回填为 Claude", dataMigrationClaudeProviderV1, usageAffected)
	}

	groupQuery := `
		UPDATE account_groups SET channel = 'claude'
		WHERE COALESCE(channel, 'codex') = 'codex' AND id IN (
			SELECT m.group_id
			FROM account_group_members m
			JOIN accounts a ON a.id = m.account_id
			WHERE a.status <> 'deleted' AND COALESCE(a.error_message, '') <> 'deleted'
			GROUP BY m.group_id
			HAVING COUNT(*) > 0 AND COUNT(*) = SUM(CASE WHEN ` + upstreamTypeExpr + ` = 'claude' THEN 1 ELSE 0 END)
		)`
	if res, err := tx.ExecContext(ctx, groupQuery); err != nil {
		return fmt.Errorf("归类 Claude 分组: %w", err)
	} else if affected, err := res.RowsAffected(); err == nil && affected > 0 {
		log.Printf("[data_migration] %s: %d 个存量分组归类为 Claude 渠道", dataMigrationClaudeProviderV1, affected)
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
		aliases := oauthIdentityDedupeAliases(account.credentials)
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
	if err := insertAccountEventsTx(ctx, tx, loserIDs, "deleted", "oauth_identity_dedupe_v1"); err != nil {
		return err
	}
	log.Printf("[data_migration] %s: 发现 %d 组重复 OAuth 身份，已软删除 %d 个重复账号", dataMigrationOAuthIdentityDedupeV1, duplicateGroups, len(loserIDs))
	return nil
}

// migrateWorkspaceIdentityV3 backfills active OAuth accounts from JWTs and
// deduplicates only non-empty email + workspace_id pairs.
func (db *DB) migrateWorkspaceIdentityV3(ctx context.Context, tx *sql.Tx) error {
	accounts, err := db.listOAuthIdentityDedupeAccounts(ctx, tx)
	if err != nil {
		return err
	}

	groups := make(map[string][]oauthIdentityDedupeAccount)
	for i := range accounts {
		account := &accounts[i]
		if !workspaceIdentityMigrationEligible(account.credentials) {
			continue
		}
		email := strings.TrimSpace(credentialStringFromMap(account.credentials, "email"))
		if email != "" && strings.TrimSpace(credentialStringFromMap(account.credentials, "workspace_id")) == "" {
			accessToken := strings.TrimSpace(credentialStringFromMap(account.credentials, "access_token"))
			if strings.EqualFold(strings.TrimSpace(credentialStringFromMap(account.credentials, "access_token_type")), "codex_at") ||
				strings.HasPrefix(accessToken, "at-") {
				accessToken = ""
			}
			tokenEmail, workspaceID := openaiidentity.TokenIdentity(
				credentialStringFromMap(account.credentials, "id_token"),
				accessToken,
			)
			if workspaceID != "" && strings.EqualFold(tokenEmail, email) {
				account.credentials["workspace_id"] = workspaceID
				encoded, err := json.Marshal(encryptSensitiveCredentials(account.credentials))
				if err != nil {
					return err
				}
				query := `UPDATE accounts SET credentials = $1 WHERE id = $2`
				if !db.isSQLite() {
					query = `UPDATE accounts SET credentials = $1::jsonb WHERE id = $2`
				}
				if _, err := tx.ExecContext(ctx, query, encoded, account.id); err != nil {
					return err
				}
			}
		}
		if key := workspaceIdentityDedupeKey(account.credentials); key != "" {
			groups[key] = append(groups[key], *account)
		}
	}
	var loserIDs []int64
	for _, group := range groups {
		if len(group) < 2 {
			continue
		}
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
	if err := insertAccountEventsTx(ctx, tx, loserIDs, "deleted", "workspace_identity_v3"); err != nil {
		return err
	}
	log.Printf("[data_migration] %s: 已软删除 %d 个重复账号", dataMigrationWorkspaceIdentityV3, len(loserIDs))
	return nil
}

func workspaceIdentityMigrationEligible(credentials map[string]interface{}) bool {
	if strings.EqualFold(strings.TrimSpace(credentialStringFromMap(credentials, "auth_mode")), "agentIdentity") {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(credentialStringFromMap(credentials, "upstream_type"))) {
	case "grok", "openai_responses":
		return false
	default:
		return true
	}
}

func workspaceIdentityDedupeKey(credentials map[string]interface{}) string {
	email := strings.ToLower(strings.TrimSpace(credentialStringFromMap(credentials, "email")))
	workspaceID := openaiidentity.NormalizeWorkspaceID(credentialStringFromMap(credentials, "workspace_id"))
	if email == "" || workspaceID == "" {
		return ""
	}
	return email + "\x00" + workspaceID
}

func (db *DB) listOAuthIdentityDedupeAccounts(ctx context.Context, tx *sql.Tx) ([]oauthIdentityDedupeAccount, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT id, credentials, COALESCE(enabled, true), COALESCE(locked, false), created_at, updated_at
		FROM accounts
		WHERE status <> 'deleted' AND COALESCE(error_message, '') <> 'deleted'
	`)
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

		// Keep the duplicate account's last group snapshot for historical usage
		// attribution. The account is already excluded from the active pool.
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
