package auth

import (
	"context"
	"fmt"
	"log"
	"strings"
)

// ClaudeCustomHeadersPersister 把账号指纹头持久化到凭据存储（由 database.DB 实现）。
type ClaudeCustomHeadersPersister interface {
	UpdateAccountCustomHeaders(ctx context.Context, id int64, headers map[string]string) error
}

// RefreshClaudeFingerprintUserAgent 在指纹 UA 版本低于 targetVersion 时返回只改了版本段的副本。
// UA 缺失、无法识别为 CLI、或版本不低于目标时返回 (原 map, false)。
func RefreshClaudeFingerprintUserAgent(headers map[string]string, targetVersion string) (map[string]string, bool) {
	uaKey := ""
	for key := range headers {
		if strings.EqualFold(strings.TrimSpace(key), "user-agent") {
			uaKey = key
			break
		}
	}
	if uaKey == "" {
		return headers, false
	}
	current, ok := ParseClaudeClientVersion(headers[uaKey])
	if !ok {
		return headers, false
	}
	if cmp, err := CompareClaudeClientVersions(current, targetVersion); err != nil || cmp >= 0 {
		return headers, false
	}
	rewritten := RewriteClaudeCLIUserAgentVersion(headers[uaKey], targetVersion)
	if rewritten == "" {
		return headers, false
	}
	next := cloneStringMap(headers)
	next[uaKey] = rewritten
	return next, true
}

// RefreshClaudeFingerprintVersions 把所有 Claude 账号的指纹 UA 版本抬到 version。
// 返回实际改写的账号数与首个持久化错误；单账号失败不影响其它账号。
// 每个账号最多重试一次内存 CAS（见 refreshClaudeFingerprintAccountWithRetry）；
// 两次都撞并发修改则放弃本轮，不计入已更新，也不视为错误。
func RefreshClaudeFingerprintVersions(ctx context.Context, store *Store, persister ClaudeCustomHeadersPersister, version string) (int, error) {
	target, ok := ParseClaudeClientVersion("claude-cli/" + strings.TrimSpace(version))
	if !ok {
		return 0, fmt.Errorf("invalid Claude CLI version %q", version)
	}
	if store == nil {
		return 0, nil
	}
	store.mu.RLock()
	accounts := append([]*Account(nil), store.accounts...)
	store.mu.RUnlock()

	updated := 0
	var firstErr error
	for _, acc := range accounts {
		if acc == nil {
			continue
		}
		acc.mu.RLock()
		isClaude := strings.EqualFold(strings.TrimSpace(acc.UpstreamType), UpstreamClaude)
		headers := cloneStringMap(acc.CustomHeaders)
		dbID := acc.DBID
		acc.mu.RUnlock()
		if !isClaude {
			continue
		}
		applied, err := refreshClaudeFingerprintAccountWithRetry(ctx, acc, dbID, headers, target, persister)
		if applied {
			updated++
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return updated, firstErr
}

// refreshClaudeFingerprintAccountWithRetry bumps one Claude account's
// fingerprint UA version, persisting to the DB and applying to memory under
// a bounded compare-and-swap retry loop.
//
// There is no reconciliation loop for Claude accounts (unlike
// openai_responses accounts — see store.go's dispatch-state reconciliation,
// which never touches Claude custom headers), so a skipped in-memory write
// would persist as a memory/DB divergence until the next sync run or a
// process restart. To avoid that, on a CAS mismatch we recompute `next` from
// a fresh snapshot (which already includes whatever the concurrent writer
// set) and retry the persist + CAS once more.
func refreshClaudeFingerprintAccountWithRetry(ctx context.Context, acc *Account, dbID int64, headers map[string]string, target string, persister ClaudeCustomHeadersPersister) (bool, error) {
	const maxAttempts = 2
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		next, changed := RefreshClaudeFingerprintUserAgent(headers, target)
		if !changed {
			return false, nil
		}
		if persister != nil {
			if err := persister.UpdateAccountCustomHeaders(ctx, dbID, next); err != nil {
				log.Printf("[claude-cli-version-sync] 账号 %d 指纹版本回写失败: %v", dbID, err)
				return false, fmt.Errorf("account %d: %w", dbID, err)
			}
		}
		acc.mu.Lock()
		if stringMapEqual(acc.CustomHeaders, headers) {
			acc.CustomHeaders = next
			acc.mu.Unlock()
			return true, nil
		}
		// Concurrent writer changed CustomHeaders between our snapshot and
		// this lock. Re-snapshot and retry from the newer value so both the
		// DB and memory end up reflecting the concurrent edit plus the
		// version bump.
		freshHeaders := cloneStringMap(acc.CustomHeaders)
		acc.mu.Unlock()
		headers = freshHeaders
	}
	log.Printf("[claude-cli-version-sync] 账号 %d 指纹在回写期间被并发修改两次，放弃本轮", dbID)
	return false, nil
}
