package auth

import (
	"regexp"
	"strings"
	"sync/atomic"
)

// BuiltinClaudeCLIVersion 是编译期内置的 Claude Code CLI 版本下限。
// 生效版本取它与后台同步值中的较大者，远端异常永不导致降级。
const BuiltinClaudeCLIVersion = "2.1.258"

var claudeSyncedCLIVersion atomic.Value // string

// claudeCLIUserAgentVersionPattern 匹配 Claude Code CLI UA 中的版本号段。
var claudeCLIUserAgentVersionPattern = regexp.MustCompile(`(?i)(\bclaude(?:-cli|-code)|\bclaude\s+code)([/\s:_-]*)(?:v?\d+\.\d+\.\d+(?:[-+][0-9A-Za-z.-]+)?)`)

// SetClaudeSyncedCLIVersion 发布后台同步得到的最新版本；非法值归一为空串。
func SetClaudeSyncedCLIVersion(version string) {
	normalized, ok := ParseClaudeClientVersion("claude-cli/" + strings.TrimSpace(version))
	if !ok {
		normalized = ""
	}
	claudeSyncedCLIVersion.Store(normalized)
}

// ClaudeSyncedCLIVersion 返回已同步的规范化版本（空=尚未同步）。
func ClaudeSyncedCLIVersion() string {
	if v, ok := claudeSyncedCLIVersion.Load().(string); ok {
		return v
	}
	return ""
}

// EffectiveClaudeCLIVersion 返回当前生效的 Claude Code CLI 版本：
// max(内置常量, 同步值)。预发布版本永不高于正式版本。
func EffectiveClaudeCLIVersion() string {
	synced := ClaudeSyncedCLIVersion()
	if synced == "" {
		return BuiltinClaudeCLIVersion
	}
	// 预发布版本永不高于正式版本
	if strings.ContainsAny(synced, "-+") {
		return BuiltinClaudeCLIVersion
	}
	if cmp, err := CompareClaudeClientVersions(synced, BuiltinClaudeCLIVersion); err == nil && cmp > 0 {
		return synced
	}
	return BuiltinClaudeCLIVersion
}

// RewriteClaudeCLIUserAgentVersion 只替换 CLI UA 中的版本号段；version 非法返回空串，
// UA 不含 CLI 版本段时原样返回。
func RewriteClaudeCLIUserAgentVersion(userAgent, version string) string {
	version = strings.TrimSpace(version)
	if _, ok := ParseClaudeClientVersion("claude-cli/" + version); !ok {
		return ""
	}
	return claudeCLIUserAgentVersionPattern.ReplaceAllString(userAgent, "${1}${2}"+version)
}
