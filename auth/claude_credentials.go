package auth

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ==================== Claude Code 凭据文件解析 ====================
//
// 除了管理台里的交互式授权，Claude 账号还能从 Claude Code 自己的凭据文件导入
// （~/.claude/.credentials.json 及各类导出工具的产物）。形态在野外有好几种：
//
//	{"claudeAiOauth":{"accessToken":"…","refreshToken":"…","expiresAt":1786548694331,…},"email":"…"}
//	{"accessToken":"…","refreshToken":"…","expiresAt":"2026-08-12T11:49:59Z"}       // 扁平形态
//	[ {…}, {…} ]                                                                    // 数组批量
//	{…}\n{…}                                                                        // 每行一个
//
// 三者都要认：用户手上的文件形态不由我们决定。expiresAt 既可能是毫秒时间戳、秒
// 时间戳，也可能是 RFC3339 串；scopes 既可能是数组也可能是空格分隔的单串。

// ClaudeCredential 是从凭据文件里解析出的单个账号。
type ClaudeCredential struct {
	AccessToken      string
	RefreshToken     string
	ExpiresAt        time.Time
	Scope            string
	SubscriptionType string
	Email            string
	AccountUUID      string
	OrganizationUUID string
}

type claudeOAuthBlob struct {
	AccessToken       string          `json:"accessToken"`
	AccessTokenSnake  string          `json:"access_token"`
	RefreshToken      string          `json:"refreshToken"`
	RefreshSnake      string          `json:"refresh_token"`
	ExpiresAt         json.RawMessage `json:"expiresAt"`
	ExpiresAtSnake    json.RawMessage `json:"expires_at"`
	Scopes            json.RawMessage `json:"scopes"`
	Scope             json.RawMessage `json:"scope"`
	SubscriptionType  string          `json:"subscriptionType"`
	SubscriptionSnake string          `json:"subscription_type"`
	Email             string          `json:"email"`
	AccountUUID       string          `json:"account_uuid"`
	AccountUUIDCamel  string          `json:"accountUuid"`
	OrganizationUUID  string          `json:"organization_uuid"`
	OrgUUIDCamel      string          `json:"organizationUuid"`
}

type claudeCredentialFile struct {
	ClaudeAIOauth *claudeOAuthBlob `json:"claudeAiOauth"`
	// 外层同名字段：包裹形态把身份信息放在 claudeAiOauth 之外。
	Email            string `json:"email"`
	AccountUUID      string `json:"account_uuid"`
	OrganizationUUID string `json:"organization_uuid"`
}

// ParseClaudeCredentialsJSON 解析一份凭据内容，返回其中全部账号（顺序保留）。
// 单对象、数组、按行拼接三种形态都接受；整份内容无法解析时返回错误。
func ParseClaudeCredentialsJSON(raw []byte) ([]ClaudeCredential, error) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return nil, fmt.Errorf("凭据内容为空")
	}

	if strings.HasPrefix(trimmed, "[") {
		var items []json.RawMessage
		if err := json.Unmarshal([]byte(trimmed), &items); err != nil {
			return nil, fmt.Errorf("解析凭据数组失败: %w", err)
		}
		creds := make([]ClaudeCredential, 0, len(items))
		for i, item := range items {
			cred, err := parseClaudeCredentialObject(item)
			if err != nil {
				return nil, fmt.Errorf("第 %d 条凭据无效: %w", i+1, err)
			}
			creds = append(creds, cred)
		}
		if len(creds) == 0 {
			return nil, fmt.Errorf("凭据数组为空")
		}
		return creds, nil
	}

	if cred, err := parseClaudeCredentialObject([]byte(trimmed)); err == nil {
		return []ClaudeCredential{cred}, nil
	}

	// 逐行形态：任一行解析失败就整份报错，避免静默丢号。
	creds := make([]ClaudeCredential, 0)
	for i, line := range strings.Split(trimmed, "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(line), ","))
		if line == "" {
			continue
		}
		cred, err := parseClaudeCredentialObject([]byte(line))
		if err != nil {
			return nil, fmt.Errorf("第 %d 行凭据无效: %w", i+1, err)
		}
		creds = append(creds, cred)
	}
	if len(creds) == 0 {
		return nil, fmt.Errorf("未解析出任何 Claude 凭据")
	}
	return creds, nil
}

func parseClaudeCredentialObject(raw []byte) (ClaudeCredential, error) {
	var wrapper claudeCredentialFile
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return ClaudeCredential{}, fmt.Errorf("JSON 解析失败: %w", err)
	}
	var flat claudeOAuthBlob
	if err := json.Unmarshal(raw, &flat); err != nil {
		return ClaudeCredential{}, fmt.Errorf("JSON 解析失败: %w", err)
	}

	blob := flat
	if wrapper.ClaudeAIOauth != nil {
		blob = *wrapper.ClaudeAIOauth
	}

	cred := ClaudeCredential{
		AccessToken:      firstNonEmptyTrimmed(blob.AccessToken, blob.AccessTokenSnake),
		RefreshToken:     firstNonEmptyTrimmed(blob.RefreshToken, blob.RefreshSnake),
		SubscriptionType: firstNonEmptyTrimmed(blob.SubscriptionType, blob.SubscriptionSnake),
		Email:            firstNonEmptyTrimmed(blob.Email, wrapper.Email),
		AccountUUID:      firstNonEmptyTrimmed(blob.AccountUUID, blob.AccountUUIDCamel, wrapper.AccountUUID),
		OrganizationUUID: firstNonEmptyTrimmed(blob.OrganizationUUID, blob.OrgUUIDCamel, wrapper.OrganizationUUID),
		Scope:            claudeScopeFromJSON(blob.Scopes, blob.Scope),
	}
	// RT 是账号活下去的唯一凭据：AT 只有一小时，导入一个没有 RT 的号等于导入一行
	// 注定变成 error 状态的账号。宁可让这一条报错，让用户回去重新导出。
	if cred.RefreshToken == "" {
		return ClaudeCredential{}, fmt.Errorf("缺少 refresh_token")
	}
	cred.ExpiresAt = claudeExpiryFromJSON(blob.ExpiresAt, blob.ExpiresAtSnake)
	return cred, nil
}

// claudeScopeFromJSON 把 scopes（数组）或 scope（空格分隔串）归一成单个空格分隔串。
func claudeScopeFromJSON(values ...json.RawMessage) string {
	for _, value := range values {
		if len(value) == 0 {
			continue
		}
		var list []string
		if err := json.Unmarshal(value, &list); err == nil {
			parts := make([]string, 0, len(list))
			for _, item := range list {
				if trimmed := strings.TrimSpace(item); trimmed != "" {
					parts = append(parts, trimmed)
				}
			}
			if len(parts) > 0 {
				return strings.Join(parts, " ")
			}
			continue
		}
		var single string
		if err := json.Unmarshal(value, &single); err == nil {
			if trimmed := strings.TrimSpace(single); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

// claudeExpiryFromJSON 解析过期时刻：毫秒 / 秒时间戳与 RFC3339 串都接受。
// 无法识别时返回零值，调用方按「已过期」处理（导入时会立刻刷一次）。
func claudeExpiryFromJSON(values ...json.RawMessage) time.Time {
	for _, value := range values {
		if len(value) == 0 {
			continue
		}
		var number json.Number
		if err := json.Unmarshal(value, &number); err == nil {
			if raw, convErr := strconv.ParseInt(number.String(), 10, 64); convErr == nil && raw > 0 {
				return claudeEpochToTime(raw)
			}
			continue
		}
		var text string
		if err := json.Unmarshal(value, &text); err == nil {
			trimmed := strings.TrimSpace(text)
			if trimmed == "" {
				continue
			}
			if parsed, parseErr := time.Parse(time.RFC3339, trimmed); parseErr == nil {
				return parsed
			}
			if raw, convErr := strconv.ParseInt(trimmed, 10, 64); convErr == nil && raw > 0 {
				return claudeEpochToTime(raw)
			}
		}
	}
	return time.Time{}
}

// claudeEpochToTime 区分秒与毫秒时间戳：1e11 秒 ≈ 公元 5138 年，
// 超过它的整数只可能是毫秒。
func claudeEpochToTime(raw int64) time.Time {
	if raw > 1e11 {
		return time.UnixMilli(raw)
	}
	return time.Unix(raw, 0)
}

// ClaudeSubscriptionScopeSufficient 判断凭据的 scope 是否覆盖 Claude Code 推理。
// 官方授权链路会拿到 user:sessions:claude_code；某些导出文件只有 user:chat /
// user:inference，上游对这类 token 走 /v1/messages 可能直接 403。导入不因此中断，
// 但要能把这条信息如实报给用户。
func ClaudeSubscriptionScopeSufficient(scope string) bool {
	value := strings.ToLower(strings.TrimSpace(scope))
	if value == "" {
		// 文件没写 scope 不等于没有权限，交给真实请求去判定。
		return true
	}
	return strings.Contains(value, "user:inference")
}

// ClaudePlanFromSubscriptionType 把凭据文件里的 subscriptionType 映射成套餐键。
// 上游串形如 default_claude_ai / claude_max_20x；default_claude_ai 是订阅版
// Claude.ai，不含档位关键字，只有 bootstrap 的 rate limit tier 能细分，
// 所以无法确定时返回空，由 bootstrap 决定。
func ClaudePlanFromSubscriptionType(subscriptionType string) string {
	return ClaudePlanFromRateLimitTier(subscriptionType)
}
