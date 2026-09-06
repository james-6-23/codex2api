package auth

// Claude 账号的凭据形态(auth_kind)。
//
// 同一个 upstream_type=claude 账号允许三种凭据:
//   - oauth:完整 Claude Code OAuth,access_token + refresh_token,AT 临期自动续期;
//   - setup_token:官方 `claude setup-token` 同款长效令牌(sk-ant-oat01-…),仅
//     user:inference scope,有效期 1 年,没有 refresh_token,到期只能重新授权。
//   - api_key:API 服务的长期密钥,配合 claude_base_url 使用,没有 RT 或过期时间。
//
// 形态写在 credentials.claude_auth_kind;未声明时保留历史 OAuth 推断规则,
// 因此旧数据不需要迁移。

import (
	"strings"
	"time"
)

const (
	// ClaudeAuthKindCredentialKey 是 credentials 中记录凭据形态的键。
	ClaudeAuthKindCredentialKey = "claude_auth_kind"
	// ClaudeAuthKindOAuth 是可刷新的完整 OAuth 凭据。
	ClaudeAuthKindOAuth = "oauth"
	// ClaudeAuthKindSetupToken 是长效 Setup Token(无 RT)。
	ClaudeAuthKindSetupToken   = "setup_token"
	ClaudeAuthKindAPIKey       = "api_key"
	ClaudeBaseURLCredentialKey = "claude_base_url"
	// ClaudeSetupTokenPrefix 是官方 Setup Token(亦即 OAuth access token)的固定前缀。
	ClaudeSetupTokenPrefix = "sk-ant-oat01-"
	// ClaudeRefreshTokenPrefix 是 OAuth refresh token 的固定前缀。裸 RT 不能直接推理,
	// 入库前要先走 refresh_token 授权换出 AT,并按 oauth 形态保存。
	ClaudeRefreshTokenPrefix = "sk-ant-ort01-"
	// ClaudeSetupTokenLifetime 是 Setup Token 的默认有效期(与交换时请求的 expires_in 一致)。
	ClaudeSetupTokenLifetime = time.Duration(ClaudeSetupTokenExpiresInSeconds) * time.Second
)

// IsValidClaudeAuthKind 判断 auth_kind 取值是否合法(空值合法,表示按 RT 推断)。
func IsValidClaudeAuthKind(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", ClaudeAuthKindOAuth, ClaudeAuthKindSetupToken, ClaudeAuthKindAPIKey:
		return true
	}
	return false
}

// NormalizeClaudeAuthKind 归一化凭据形态:显式合法值优先;未声明时一律视为 oauth
// (历史账号全部是带 RT 的 OAuth 凭据,新写入的 Setup Token 总会带显式标记)。
// hasRefreshToken 目前仅作语义占位:无 RT 的未声明凭据同样按 oauth 处理,由调用方
// 决定是否拒绝(createClaudeAccount 会拒绝无 RT 的 oauth)。
func NormalizeClaudeAuthKind(raw string, hasRefreshToken bool) string {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case ClaudeAuthKindOAuth:
		return ClaudeAuthKindOAuth
	case ClaudeAuthKindSetupToken:
		return ClaudeAuthKindSetupToken
	case ClaudeAuthKindAPIKey:
		return ClaudeAuthKindAPIKey
	}
	_ = hasRefreshToken
	return ClaudeAuthKindOAuth
}

// InferClaudeAuthKind 在 NormalizeClaudeAuthKind 之上多一条令牌形状启发:未声明形态、
// 没有 RT、且 AT 形如官方 Setup Token(sk-ant-oat01-)时判为 setup_token。用于从
// 数据库行还原运行时形态,让手工写库/旧版导入的长效令牌也能被正确识别。
func InferClaudeAuthKind(raw, accessToken, refreshToken string) string {
	if kind := strings.ToLower(strings.TrimSpace(raw)); kind == ClaudeAuthKindOAuth || kind == ClaudeAuthKindSetupToken || kind == ClaudeAuthKindAPIKey {
		return kind
	}
	if strings.TrimSpace(refreshToken) == "" && LooksLikeClaudeSetupToken(accessToken) {
		return ClaudeAuthKindSetupToken
	}
	return ClaudeAuthKindOAuth
}

// LooksLikeClaudeSetupToken 判断字符串是否形如官方 Setup Token。
func LooksLikeClaudeSetupToken(token string) bool {
	token = strings.TrimSpace(token)
	return len(token) > len(ClaudeSetupTokenPrefix) && strings.HasPrefix(token, ClaudeSetupTokenPrefix)
}

// LooksLikeClaudeRefreshToken 判断字符串是否形如 OAuth refresh token。
func LooksLikeClaudeRefreshToken(token string) bool {
	token = strings.TrimSpace(token)
	return len(token) > len(ClaudeRefreshTokenPrefix) && strings.HasPrefix(token, ClaudeRefreshTokenPrefix)
}

// ExtractClaudeSetupTokens 从任意粘贴文本抽出全部 sk-ant-oat01- 令牌,保序去重。
func ExtractClaudeSetupTokens(text string) []string {
	return extractClaudeTokensWithPrefix(text, ClaudeSetupTokenPrefix)
}

// ExtractClaudeRefreshTokens 从任意粘贴文本抽出全部 sk-ant-ort01- 刷新令牌,保序去重。
func ExtractClaudeRefreshTokens(text string) []string {
	return extractClaudeTokensWithPrefix(text, ClaudeRefreshTokenPrefix)
}

// extractClaudeTokensWithPrefix 从任意粘贴文本(换行/逗号/空格分隔,甚至夹在句子里)
// 抽出带指定前缀的令牌,保序去重。令牌字符集为 [A-Za-z0-9_-]。
func extractClaudeTokensWithPrefix(text, prefix string) []string {
	out := make([]string, 0, 4)
	seen := make(map[string]struct{})
	rest := text
	for {
		idx := strings.Index(rest, prefix)
		if idx < 0 {
			break
		}
		segment := rest[idx:]
		end := len(segment)
		for i, r := range segment {
			if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_') {
				end = i
				break
			}
		}
		token := segment[:end]
		if len(token) > len(prefix) {
			if _, dup := seen[token]; !dup {
				seen[token] = struct{}{}
				out = append(out, token)
			}
		}
		if end == 0 {
			end = 1
		}
		rest = segment[end:]
	}
	return out
}

// isClaudeSetupTokenLocked 判断账号是否为 Claude Setup Token 凭据。调用方需持有 a.mu。
func (a *Account) isClaudeSetupTokenLocked() bool {
	if !a.isClaudeOAuthLocked() {
		return false
	}
	return InferClaudeAuthKind(a.ClaudeAuthKind, a.AccessToken, a.RefreshToken) == ClaudeAuthKindSetupToken
}

// IsClaudeSetupToken 判断账号是否为 Claude Setup Token(长效、不可刷新)凭据。
func (a *Account) IsClaudeSetupToken() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.isClaudeSetupTokenLocked()
}

func (a *Account) isClaudeAPIKeyLocked() bool {
	return a.isClaudeOAuthLocked() && InferClaudeAuthKind(a.ClaudeAuthKind, a.AccessToken, a.RefreshToken) == ClaudeAuthKindAPIKey
}

// IsClaudeAPIKey 判断账号是否使用独立 API 服务的密钥。
func (a *Account) IsClaudeAPIKey() bool {
	if a == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.isClaudeAPIKeyLocked()
}

// GetClaudeBaseURL 返回 API Key 账号配置的服务地址。
func (a *Account) GetClaudeBaseURL() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.ClaudeBaseURL
}

// EffectiveClaudeAuthKind 返回账号的凭据形态;非 Claude 账号返回空。
func (a *Account) EffectiveClaudeAuthKind() string {
	if a == nil {
		return ""
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	if !a.isClaudeOAuthLocked() {
		return ""
	}
	return InferClaudeAuthKind(a.ClaudeAuthKind, a.AccessToken, a.RefreshToken)
}
