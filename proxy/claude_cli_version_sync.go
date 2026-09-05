package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

const (
	// ClaudeReleasesLatestURL 是 anthropics/claude-code 最新正式 release 的 GitHub API 端点。
	ClaudeReleasesLatestURL = "https://api.github.com/repos/anthropics/claude-code/releases/latest"
	// ClaudeNpmDistTagsURL 是 npm 上 @anthropic-ai/claude-code 的 dist-tags 端点（GitHub 失败时回退）。
	ClaudeNpmDistTagsURL = "https://registry.npmjs.org/-/package/@anthropic-ai/claude-code/dist-tags"
)

// 测试接缝；生产代码不要赋值。
var (
	claudeReleasesLatestURLForTest = ""
	claudeNpmDistTagsURLForTest    = ""
)

// SetClaudeVersionSourceURLsForTest 覆盖 GitHub/npm 版本源端点。仅供测试使用；
// 生产代码不要调用。传空串恢复默认端点。
func SetClaudeVersionSourceURLsForTest(github, npm string) {
	claudeReleasesLatestURLForTest = github
	claudeNpmDistTagsURLForTest = npm
}

// ClaudeCLIVersionSyncDisabled 报告是否通过 CLAUDE_DISABLE_CLI_VERSION_SYNC 关闭了联网同步。
// 关闭后仍会在启动时用当前生效版本做一次本地指纹回写（不联网）；管理端「立即同步」不受影响。
func ClaudeCLIVersionSyncDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CLAUDE_DISABLE_CLI_VERSION_SYNC"))) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
}

// ClaudeCLIVersionSyncResult 是一次同步的结果投影。
type ClaudeCLIVersionSyncResult struct {
	FetchedVersion    string `json:"fetched_version"`
	EffectiveVersion  string `json:"effective_version"`
	BuiltinVersion    string `json:"builtin_version"`
	Updated           bool   `json:"updated"`
	AccountsRefreshed int    `json:"accounts_refreshed"`
}

// extractClaudeCLIVersion 接受 "2.1.258" / "v2.1.258"，丢弃预发布后缀；非法返回空串。
func extractClaudeCLIVersion(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(strings.TrimPrefix(raw, "v"), "V")
	if idx := strings.IndexAny(raw, "-+"); idx >= 0 {
		raw = raw[:idx]
	}
	if raw == "" {
		return ""
	}
	version, ok := auth.ParseClaudeClientVersion("claude-cli/" + raw)
	if !ok {
		return ""
	}
	return version
}

func fetchClaudeJSON(ctx context.Context, endpoint string, transport http.RoundTripper, github bool, out interface{}) error {
	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "codex2api")
	if github {
		req.Header.Set("Accept", "application/vnd.github+json")
		ApplyGithubAuth(req)
	}
	client := &http.Client{Transport: transport, Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	return json.Unmarshal(body, out)
}

func fetchClaudeVersionFromGithub(ctx context.Context, proxyURL string) (string, error) {
	endpoint := ClaudeReleasesLatestURL
	if claudeReleasesLatestURLForTest != "" {
		endpoint = claudeReleasesLatestURLForTest
	}
	var payload struct {
		Name    string `json:"name"`
		TagName string `json:"tag_name"`
	}
	if err := fetchClaudeJSON(ctx, endpoint, newCodexStandardTransport(GithubProxyOrDefault(endpoint, proxyURL)), true, &payload); err != nil {
		return "", err
	}
	if v := extractClaudeCLIVersion(payload.TagName); v != "" {
		return v, nil
	}
	if v := extractClaudeCLIVersion(payload.Name); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("no valid version in release (name=%q tag=%q)", payload.Name, payload.TagName)
}

func fetchClaudeVersionFromNpm(ctx context.Context, proxyURL string) (string, error) {
	endpoint := ClaudeNpmDistTagsURL
	if claudeNpmDistTagsURLForTest != "" {
		endpoint = claudeNpmDistTagsURLForTest
	}
	var payload struct {
		Latest string `json:"latest"`
	}
	if err := fetchClaudeJSON(ctx, endpoint, newCodexStandardTransport(proxyURL), false, &payload); err != nil {
		return "", err
	}
	if v := extractClaudeCLIVersion(payload.Latest); v != "" {
		return v, nil
	}
	return "", fmt.Errorf("no valid version in dist-tags (latest=%q)", payload.Latest)
}

// FetchLatestClaudeCLIVersion 先查 GitHub releases/latest，失败再查 npm dist-tags。
func FetchLatestClaudeCLIVersion(ctx context.Context, proxyURL string) (string, error) {
	version, ghErr := fetchClaudeVersionFromGithub(ctx, proxyURL)
	if ghErr == nil {
		return version, nil
	}
	version, npmErr := fetchClaudeVersionFromNpm(ctx, proxyURL)
	if npmErr == nil {
		return version, nil
	}
	return "", fmt.Errorf("claude cli version fetch failed: github: %v; npm: %v", ghErr, npmErr)
}

func claudeHeadersPersister(db *database.DB) auth.ClaudeCustomHeadersPersister {
	if db == nil {
		return nil // 必须返回接口 nil，而不是 nil 指针
	}
	return db
}

// SyncClaudeCLIVersion 拉取最新版本，高于当前生效版本时持久化并发布，随后回写所有账号指纹。
func SyncClaudeCLIVersion(ctx context.Context, db *database.DB, store *auth.Store, proxyURL string) (*ClaudeCLIVersionSyncResult, error) {
	result := &ClaudeCLIVersionSyncResult{
		BuiltinVersion:   auth.BuiltinClaudeCLIVersion,
		EffectiveVersion: auth.EffectiveClaudeCLIVersion(),
	}
	fetched, err := FetchLatestClaudeCLIVersion(ctx, proxyURL)
	if err != nil {
		return result, err
	}
	result.FetchedVersion = fetched
	if cmp, cmpErr := auth.CompareClaudeClientVersions(fetched, result.EffectiveVersion); cmpErr == nil && cmp > 0 {
		if db != nil {
			if err := db.UpdateClaudeSyncedCLIVersion(ctx, fetched); err != nil {
				return result, err
			}
		}
		auth.SetClaudeSyncedCLIVersion(fetched)
		result.Updated = true
	}
	result.EffectiveVersion = auth.EffectiveClaudeCLIVersion()
	refreshed, refreshErr := auth.RefreshClaudeFingerprintVersions(ctx, store, claudeHeadersPersister(db), result.EffectiveVersion)
	result.AccountsRefreshed = refreshed
	return result, refreshErr
}

// StartClaudeCLIVersionSync 启动时先用生效版本做一次本地指纹回写（不联网），
// 然后按 ClaudeConfig 的开关与间隔定时联网同步。
// 本地回写在后台任务内执行，即使 CLAUDE_DISABLE_CLI_VERSION_SYNC 关闭了联网
// 同步也照常运行；只有联网的 runOnce 与定时循环受该开关约束。
func StartClaudeCLIVersionSync(ctx context.Context, db *database.DB, store *auth.Store, proxyResolver func() string) {
	if db == nil || store == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	resolveProxy := func() string {
		if proxyResolver == nil {
			return ""
		}
		return proxyResolver()
	}
	runOnce := func(runCtx context.Context) {
		syncCtx, cancel := context.WithTimeout(runCtx, 45*time.Second)
		defer cancel()
		res, err := SyncClaudeCLIVersion(syncCtx, db, store, resolveProxy())
		if err != nil {
			log.Printf("[claude-cli-version-sync] 同步失败（不影响服务）: %v", err)
			return
		}
		if res.Updated || res.AccountsRefreshed > 0 {
			log.Printf("[claude-cli-version-sync] 生效版本 %s，回写账号 %d 个", res.EffectiveVersion, res.AccountsRefreshed)
		}
	}
	currentInterval := func() time.Duration {
		return time.Duration(store.ClaudeCLIVersionSyncIntervalHours()) * time.Hour
	}
	db.RunBackgroundTask(func(lifecycle context.Context) {
		taskCtx, taskCancel := context.WithCancel(lifecycle)
		stopParent := context.AfterFunc(ctx, taskCancel)
		defer func() {
			stopParent()
			taskCancel()
		}()

		{
			refreshCtx, cancel := context.WithTimeout(taskCtx, 30*time.Second)
			if n, err := auth.RefreshClaudeFingerprintVersions(refreshCtx, store, db, auth.EffectiveClaudeCLIVersion()); err != nil {
				log.Printf("[claude-cli-version-sync] 启动指纹版本回写部分失败: %v", err)
			} else if n > 0 {
				log.Printf("[claude-cli-version-sync] 启动时已回写 %d 个 Claude 账号指纹版本至 %s", n, auth.EffectiveClaudeCLIVersion())
			}
			cancel()
		}
		if ClaudeCLIVersionSyncDisabled() {
			return
		}

		if store.ClaudeCLIVersionSyncEnabled() {
			runOnce(taskCtx)
		}
		for {
			select {
			case <-taskCtx.Done():
				return
			case <-time.After(currentInterval()):
				if store.ClaudeCLIVersionSyncEnabled() {
					runOnce(taskCtx)
				}
			}
		}
	})
}
