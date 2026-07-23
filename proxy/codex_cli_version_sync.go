package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/codex2api/database"
)

// CodexReleasesLatestURL 是 openai/codex 官方最新稳定 release 的 GitHub API 端点。
// GitHub 的 "latest" 天然排除 prerelease/draft（如 0.145.0-alpha.1），只返回正式版。
const CodexReleasesLatestURL = "https://api.github.com/repos/openai/codex/releases/latest"

// codexReleasesLatestURLForTest 允许测试替换默认 URL。生产代码不要赋值。
var codexReleasesLatestURLForTest = ""

// CodexCLIVersionSyncDisabled 报告是否通过环境变量关闭了 CLI 版本自动同步。
// CODEX_DISABLE_CLI_VERSION_SYNC=1（或 true）时关闭后台定时与启动同步；
// 管理端「立即同步」按钮不受影响。
func CodexCLIVersionSyncDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_DISABLE_CLI_VERSION_SYNC"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// CodexCLIVersionSyncResult 是一次版本同步的结果投影。
type CodexCLIVersionSyncResult struct {
	// FetchedVersion 是本次从上游解析到的最新版本（可能为空，表示解析失败）。
	FetchedVersion string `json:"fetched_version"`
	// EffectiveVersion 是同步后当前生效的模拟版本（内置常量与同步值的较大者）。
	EffectiveVersion string `json:"effective_version"`
	// BuiltinVersion 是编译期内置的版本常量。
	BuiltinVersion string `json:"builtin_version"`
	// Updated 表示本次是否真的抬升了生效版本并已持久化。
	Updated bool `json:"updated"`
}

// FetchLatestCodexCLIVersion 从 openai/codex releases 拉取最新稳定版本号（如 "0.144.1"）。
// 优先取 release 的 name（上游填的是干净版本号），回退解析 tag_name（rust-v0.144.1）。
func FetchLatestCodexCLIVersion(ctx context.Context, proxyURL string) (string, error) {
	endpoint := CodexReleasesLatestURL
	if codexReleasesLatestURLForTest != "" {
		endpoint = codexReleasesLatestURLForTest
	}

	reqCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("build codex releases request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "codex2api")

	client := &http.Client{Transport: newCodexStandardTransport(proxyURL), Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", fmt.Errorf("codex releases request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("codex releases upstream status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload struct {
		Name    string `json:"name"`
		TagName string `json:"tag_name"`
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", fmt.Errorf("read codex releases response: %w", err)
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", fmt.Errorf("parse codex releases response: %w", err)
	}

	version := extractCodexCLIVersion(payload.Name)
	if version == "" {
		version = extractCodexCLIVersion(payload.TagName)
	}
	if version == "" {
		return "", fmt.Errorf("no valid version in codex release (name=%q tag=%q)", payload.Name, payload.TagName)
	}
	return version, nil
}

// extractCodexCLIVersion 从 release 的 name / tag_name 里提取干净语义版本。
// 接受 "0.144.1"、"v0.144.1"、"rust-v0.144.1" 等形态；校验为合法 Codex 版本才返回。
func extractCodexCLIVersion(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if idx := strings.LastIndexByte(raw, '-'); idx >= 0 {
		raw = raw[idx+1:]
	}
	raw = normalizeCodexClientVersionText(raw)
	if raw == "" || !validCodexClientVersionString(raw) {
		return ""
	}
	return raw
}

// SyncCodexCLIVersion 拉取上游最新版本并在其高于当前生效版本时持久化到系统设置，
// 随后刷新 RuntimeSettings。绝不写入低于内置常量的值（远端异常不会导致降级）。
func SyncCodexCLIVersion(ctx context.Context, db *database.DB, proxyURL string) (*CodexCLIVersionSyncResult, error) {
	result := &CodexCLIVersionSyncResult{
		BuiltinVersion:   latestCodexCLIVersion,
		EffectiveVersion: effectiveLatestCodexCLIVersion(),
	}
	if db == nil {
		return result, fmt.Errorf("数据库不可用，无法同步 Codex CLI 版本")
	}

	fetched, err := FetchLatestCodexCLIVersion(ctx, proxyURL)
	if err != nil {
		return result, err
	}
	result.FetchedVersion = fetched

	// 仅当拉取值高于内置常量时才有意义（否则运行时会自动回落内置常量）。
	if cmp, ok := compareCodexClientVersions(fetched, latestCodexCLIVersion); !ok || cmp <= 0 {
		result.EffectiveVersion = effectiveLatestCodexCLIVersion()
		return result, nil
	}

	settings, err := db.GetSystemSettings(ctx)
	if err != nil {
		return result, err
	}
	if settings == nil {
		settings = &database.SystemSettings{}
	}
	if strings.TrimSpace(settings.CodexSyncedCLIVersion) == fetched {
		result.EffectiveVersion = effectiveLatestCodexCLIVersion()
		return result, nil
	}

	if err := db.UpdateCodexSyncedCLIVersion(ctx, fetched); err != nil {
		return result, err
	}
	UpdateRuntimeSettings(func(settings RuntimeSettings) RuntimeSettings {
		settings.CodexSyncedCLIVersion = fetched
		return settings
	})

	result.Updated = true
	result.EffectiveVersion = effectiveLatestCodexCLIVersion()
	return result, nil
}

// StartCodexCLIVersionSync 在后台按系统设置的间隔周期同步 Codex CLI 版本，并在启动时先同步一次。
// 开关(CodexCLIVersionSyncEnabled)与间隔(CodexCLIVersionSyncIntervalHours)在每个周期读取；
// 新间隔从下一轮计时生效，无需重启。环境变量 CODEX_DISABLE_CLI_VERSION_SYNC 为硬开关，优先级最高。
// proxyResolver 允许调用方注入出站代理（可为 nil）。
func StartCodexCLIVersionSync(ctx context.Context, db *database.DB, proxyResolver func() string) {
	if db == nil || CodexCLIVersionSyncDisabled() {
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
		syncCtx, cancel := context.WithTimeout(runCtx, 30*time.Second)
		defer cancel()
		if res, err := SyncCodexCLIVersion(syncCtx, db, resolveProxy()); err != nil {
			fmt.Printf("[codex-cli-version-sync] 同步失败（不影响服务）: %v\n", err)
		} else if res.Updated {
			fmt.Printf("[codex-cli-version-sync] 模拟版本已更新至 %s\n", res.EffectiveVersion)
		}
	}

	// 当前配置的同步间隔（小时→时长），钳到 [1h, 720h]。
	currentInterval := func() time.Duration {
		hours := CurrentRuntimeSettings().CodexCLIVersionSyncIntervalHours
		if hours <= 0 {
			hours = 12
		}
		if hours > 720 {
			hours = 720
		}
		return time.Duration(hours) * time.Hour
	}

	db.RunBackgroundTask(func(lifecycle context.Context) {
		taskCtx, taskCancel := context.WithCancel(lifecycle)
		stopParent := context.AfterFunc(ctx, taskCancel)
		defer func() {
			stopParent()
			taskCancel()
		}()
		if CurrentRuntimeSettings().CodexCLIVersionSyncEnabled {
			runOnce(taskCtx)
		}
		for {
			select {
			case <-taskCtx.Done():
				return
			case <-time.After(currentInterval()):
				if CurrentRuntimeSettings().CodexCLIVersionSyncEnabled {
					runOnce(taskCtx)
				}
			}
		}
	})
}
