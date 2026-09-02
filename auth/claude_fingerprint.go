package auth

// Claude Code 客户端指纹。
//
// 目的:让每个 Claude 账号对外呈现一套**稳定且各不相同**的真实 Claude Code CLI
// 身份(UA / x-app / x-stainless-*),对抗 Anthropic 的一致性风控——最容易被标记的
// 不是某个具体值,而是"同一账号身份忽变"。指纹在导入账号时生成一次并持久化到
// credentials.custom_headers,之后每次上游请求原样套用。
//
// 值域取自真实 Claude Code / @anthropic-ai SDK 在链路上出现过的组合,随机挑选但一旦
// 落库即固定。真实 Claude Code 客户端直连时,其自带的这些头会被优先保留(见
// proxy 层 applyClaudeMessagesHeaders),仅在缺失时才用这里合成的指纹补齐。

import (
	"crypto/rand"
	"math/big"
	"strings"
	"time"
)

// 真实取值池(保持精简、贴近近期版本)。
var (
	claudeCLIVersions = []string{"2.1.220", "2.1.219", "2.1.205", "2.0.14"}
	claudeSDKVersions = []string{"0.68.0", "0.65.0", "0.63.1", "0.60.0"}
	claudeNodeRuntime = []string{"v22.14.0", "v22.11.0", "v20.18.1", "v20.17.0"}
	claudeStainlessOS = []string{"MacOS", "Linux", "Windows"}
	claudeArchByOS    = map[string][]string{
		"MacOS":   {"arm64", "x64"},
		"Linux":   {"x64", "arm64"},
		"Windows": {"x64"},
	}
)

// ClaudeFingerprint 是一套稳定的 Claude Code CLI 身份。
type ClaudeFingerprint struct {
	UserAgent               string `json:"user_agent"`
	XApp                    string `json:"x_app"`
	StainlessLang           string `json:"x_stainless_lang"`
	StainlessPackageVersion string `json:"x_stainless_package_version"`
	StainlessOS             string `json:"x_stainless_os"`
	StainlessArch           string `json:"x_stainless_arch"`
	StainlessRuntime        string `json:"x_stainless_runtime"`
	StainlessRuntimeVersion string `json:"x_stainless_runtime_version"`
	// Timezone 是账号绑定的 IANA 时区(如 Asia/Shanghai),用于身份一致性;
	// 空表示不指定。
	Timezone string `json:"timezone,omitempty"`
}

func claudePick(pool []string) string {
	if len(pool) == 0 {
		return ""
	}
	n, err := rand.Int(rand.Reader, big.NewInt(int64(len(pool))))
	if err != nil {
		return pool[0]
	}
	return pool[n.Int64()]
}

// GenerateClaudeFingerprint 生成一套稳定指纹。timezone 为空时不设置(留给调用方决定
// 是否用全局默认)。非空时会校验为合法 IANA 时区,非法则丢弃。
func GenerateClaudeFingerprint(timezone string) ClaudeFingerprint {
	cliVer := claudePick(claudeCLIVersions)
	os := claudePick(claudeStainlessOS)
	arch := claudePick(claudeArchByOS[os])
	fp := ClaudeFingerprint{
		UserAgent:               "claude-cli/" + cliVer + " (external, cli)",
		XApp:                    "cli",
		StainlessLang:           "js",
		StainlessPackageVersion: claudePick(claudeSDKVersions),
		StainlessOS:             os,
		StainlessArch:           arch,
		StainlessRuntime:        "node",
		StainlessRuntimeVersion: claudePick(claudeNodeRuntime),
	}
	if tz := strings.TrimSpace(timezone); tz != "" {
		if _, err := time.LoadLocation(tz); err == nil {
			fp.Timezone = tz
		}
	}
	return fp
}

// Headers 返回该指纹对应的请求头(键为规范化的头名)。仅返回 x-stainless / x-app /
// user-agent 这类身份头;Authorization / anthropic-* 由调用方另行设置。
func (f ClaudeFingerprint) Headers() map[string]string {
	h := map[string]string{}
	if f.UserAgent != "" {
		h["User-Agent"] = f.UserAgent
	}
	if f.XApp != "" {
		h["X-App"] = f.XApp
	}
	if f.StainlessLang != "" {
		h["X-Stainless-Lang"] = f.StainlessLang
	}
	if f.StainlessPackageVersion != "" {
		h["X-Stainless-Package-Version"] = f.StainlessPackageVersion
	}
	if f.StainlessOS != "" {
		h["X-Stainless-OS"] = f.StainlessOS
	}
	if f.StainlessArch != "" {
		h["X-Stainless-Arch"] = f.StainlessArch
	}
	if f.StainlessRuntime != "" {
		h["X-Stainless-Runtime"] = f.StainlessRuntime
	}
	if f.StainlessRuntimeVersion != "" {
		h["X-Stainless-Runtime-Version"] = f.StainlessRuntimeVersion
	}
	return h
}

// ClaudeIdentityHeaderNames 是"客户端身份"类头名(小写),用于在透传时判断入站真实
// 客户端是否已自带身份、以及需要用指纹补齐哪些。
var ClaudeIdentityHeaderNames = []string{
	"user-agent",
	"x-app",
	"x-stainless-lang",
	"x-stainless-package-version",
	"x-stainless-os",
	"x-stainless-arch",
	"x-stainless-runtime",
	"x-stainless-runtime-version",
}
