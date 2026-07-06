package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/codex2api/auth"
)

// CodexInviteURL 是 ChatGPT 推荐邀请端点。
const CodexInviteURL = "https://chatgpt.com/backend-api/wham/referrals/invite"

// DefaultReferralKey 是默认的 referral key（持久邀请链接）。
const DefaultReferralKey = "codex_referral_persistent_invite"

// 邀请上游使用浏览器型 header（与 codex_cli_rs 探针不同），更贴近官方网页端调用。
const (
	inviteOriginator = "Codex Desktop"
	inviteLanguage   = "zh-CN"
	inviteUserAgent  = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/149.0.0.0 Safari/537.36"
)

// codexInviteURLForTest 允许测试替换默认 URL。生产代码不要赋值。
var codexInviteURLForTest = ""

// CodexInviteResult 是一次邀请调用的结构化结果。
type CodexInviteResult struct {
	OK          bool              `json:"ok"`
	StatusCode  int               `json:"status_code"`
	RequestID   string            `json:"request_id,omitempty"`
	ReferralKey string            `json:"referral_key"`
	Emails      []string          `json:"emails"`
	Invites     []CodexInviteItem `json:"invites,omitempty"`
	Upstream    json.RawMessage   `json:"upstream,omitempty"`
	UpstreamRaw string            `json:"upstream_raw,omitempty"`
}

// CodexInviteItem 是上游 invites[] 里的单条邀请记录。
type CodexInviteItem struct {
	ReferralID string `json:"referral_id,omitempty"`
	Email      string `json:"email,omitempty"`
	InviteURL  string `json:"invite_url,omitempty"`
}

// SendCodexInvite 通过账号凭证向 ChatGPT 推荐邀请端点发送邀请邮件。
// referralKey 为空时使用 DefaultReferralKey。返回结构化结果；非 2xx 时
// OK=false 且 Upstream/UpstreamRaw 保留上游响应，由调用方据 StatusCode 处理。
func SendCodexInvite(ctx context.Context, account *auth.Account, proxyURL, referralKey string, emails []string) (*CodexInviteResult, error) {
	if account == nil {
		return nil, fmt.Errorf("account is nil")
	}
	accessToken := account.GetAccessToken()
	if accessToken == "" {
		return nil, fmt.Errorf("account has no access token")
	}
	if len(emails) == 0 {
		return nil, fmt.Errorf("no emails to invite")
	}
	if strings.TrimSpace(referralKey) == "" {
		referralKey = DefaultReferralKey
	}

	url := CodexInviteURL
	if codexInviteURLForTest != "" {
		url = codexInviteURLForTest
	}

	payload, _ := json.Marshal(map[string]any{
		"referral_key": referralKey,
		"emails":       emails,
	})

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build invite request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Oai-Language", inviteLanguage)
	req.Header.Set("Originator", inviteOriginator)
	req.Header.Set("User-Agent", inviteUserAgent)
	// 邀请操作作用于自定义头覆盖后的空间,与实际流量指向一致。
	if accountID := account.EffectiveAccountID(); accountID != "" {
		req.Header.Set("Chatgpt-Account-Id", accountID)
	}

	client := &http.Client{Transport: newCodexStandardTransport(proxyURL)}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("invite request: %w", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()

	result := &CodexInviteResult{
		OK:          resp.StatusCode >= 200 && resp.StatusCode < 300,
		StatusCode:  resp.StatusCode,
		RequestID:   resp.Header.Get("x-oai-request-id"),
		ReferralKey: referralKey,
		Emails:      emails,
	}

	// 上游响应：能解析为 JSON 就放进 Upstream 并尝试抽取 invites[]，否则留原始串。
	if json.Valid(body) {
		result.Upstream = json.RawMessage(body)
		var parsed struct {
			Invites []CodexInviteItem `json:"invites"`
		}
		if err := json.Unmarshal(body, &parsed); err == nil {
			result.Invites = parsed.Invites
		}
	} else if len(body) > 0 {
		result.UpstreamRaw = string(body)
	}

	return result, nil
}
