package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/codex2api/auth"
)

// 订阅到期时间同步：wham/usage 不返回订阅到期字段，JWT 里的
// chatgpt_subscription_active_until 续费后长期停留在旧值，只有网页端
// /backend-api/subscriptions 能拿到续费后的权威到期时间（active_until）。(issue #360)
//
// 该端点在 Cloudflare 后面，普通 TLS 指纹会被拦截（返回 HTML 加载页），
// 必须用 uTLS Chrome 指纹 + 浏览器请求头（Origin/Referer/Chrome UA），
// 且不能带 Codex CLI 的 Originator/UA。

// ChatGPTSubscriptionsURL 是网页端订阅信息端点，按工作区返回当前订阅周期。
const ChatGPTSubscriptionsURL = "https://chatgpt.com/backend-api/subscriptions"

// subscriptionsURLForTest 允许测试替换端点 URL。生产代码不要赋值。
var subscriptionsURLForTest = ""

// SetSubscriptionsURLForTest 供测试替换订阅端点 URL，返回恢复函数。生产代码不要调用。
func SetSubscriptionsURLForTest(url string) (restore func()) {
	old := subscriptionsURLForTest
	subscriptionsURLForTest = url
	return func() { subscriptionsURLForTest = old }
}

// subscriptionsBrowserUserAgent 模拟浏览器访问网页端点；与 uTLS Chrome 指纹配套。
const subscriptionsBrowserUserAgent = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

// subscriptionProbeMinInterval 是同一账号两次订阅到期探针的最小间隔。
// 到期时间只在续费/退订时变化，无需高频访问网页端点。
const subscriptionProbeMinInterval = 6 * time.Hour

// ChatGPTSubscription 是 /backend-api/subscriptions 响应中本服务关心的字段。
type ChatGPTSubscription struct {
	PlanType    string `json:"plan_type"`
	ActiveStart string `json:"active_start"`
	ActiveUntil string `json:"active_until"`
	WillRenew   bool   `json:"will_renew"`
}

// ActiveUntilTime 解析 active_until；缺失或格式非法返回零值。
func (s *ChatGPTSubscription) ActiveUntilTime() time.Time {
	if s == nil {
		return time.Time{}
	}
	raw := strings.TrimSpace(s.ActiveUntil)
	if raw == "" {
		return time.Time{}
	}
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}

// QueryChatGPTSubscription 查询账号当前工作区的订阅信息。
// account_id 必须是工作区 UUID：历史数据污染写入的 user-... 会让上游返回 500，
// 直接跳过不发请求。
func QueryChatGPTSubscription(ctx context.Context, account *auth.Account, proxyURL string) (*ChatGPTSubscription, error) {
	if account == nil {
		return nil, fmt.Errorf("account is nil")
	}
	accessToken := account.GetAccessToken()
	if accessToken == "" {
		return nil, fmt.Errorf("account has no access token")
	}
	accountID := account.EffectiveAccountID()
	if accountID == "" || strings.HasPrefix(accountID, "user-") {
		// 历史污染数据可能把 user-... 写进 account_id 字段，而订阅端点只认
		// 工作区 UUID（user-... 会 500）；回退到 AT JWT 里的 chatgpt_account_id。
		if info := auth.ParseAccessToken(accessToken); info != nil {
			if v := strings.TrimSpace(info.ChatGPTAccountID); v != "" {
				accountID = v
			}
		}
	}
	if accountID == "" {
		return nil, fmt.Errorf("account has no workspace id")
	}
	if strings.HasPrefix(accountID, "user-") {
		return nil, fmt.Errorf("account id %q is not a workspace uuid", accountID)
	}

	endpoint := ChatGPTSubscriptionsURL
	// 网页端点必须用 uTLS Chrome 指纹，与网关的 transport 模式配置无关。
	transport := http.RoundTripper(NewUTLSTransport(proxyURL))
	if subscriptionsURLForTest != "" {
		endpoint = subscriptionsURLForTest
		// 测试用 httptest（明文 HTTP），uTLS 拨号无法使用，回退标准 transport。
		transport = newCodexStandardTransport(proxyURL)
	}
	// Resin 启用时经反代访问（指纹由 Resin 侧承担），与其他账号维护请求一致，
	// 避免全部账号共享本机出口 IP 直连（issue #372）。
	finalURL, resinClient, viaResin := resinMaintenanceTarget(account, endpoint)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, finalURL+"?account_id="+url.QueryEscape(accountID), nil)
	if err != nil {
		return nil, fmt.Errorf("build subscriptions request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Origin", "https://chatgpt.com")
	req.Header.Set("Referer", "https://chatgpt.com/")
	req.Header.Set("User-Agent", subscriptionsBrowserUserAgent)
	if viaResin {
		req.Header.Set("X-Resin-Account", ResinAccountID(account))
	}

	client := resinClient
	if client == nil {
		client = &http.Client{Transport: transport}
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("subscriptions request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<10))
	if err != nil {
		return nil, fmt.Errorf("read subscriptions response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("subscriptions returned status %d: %s", resp.StatusCode, truncateForLog(body, 200))
	}

	var sub ChatGPTSubscription
	if err := json.Unmarshal(body, &sub); err != nil {
		return nil, fmt.Errorf("parse subscriptions response: %w", err)
	}
	return &sub, nil
}

// MaybeSyncSubscriptionExpiry 按需从网页端同步权威订阅到期时间（best-effort）：
// 仅付费套餐且到期时间未知/临近/已过时才发起，带节流；失败只记日志不影响调用方。
// 返回是否更新了到期时间。
func MaybeSyncSubscriptionExpiry(ctx context.Context, store *auth.Store, account *auth.Account, proxyURL string) bool {
	if store == nil || account == nil {
		return false
	}
	now := time.Now()
	if !account.NeedsSubscriptionExpiryProbe(now, subscriptionProbeMinInterval) {
		return false
	}
	// 无论成败都记录尝试时间：上游异常时避免每次探针都重复访问网页端点。
	account.MarkSubscriptionExpiryProbed(now)

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	sub, err := QueryChatGPTSubscription(ctx, account, proxyURL)
	if err != nil {
		log.Printf("[账号 %d] 订阅到期时间同步失败（忽略）: %v", account.DBID, err)
		return false
	}
	activeUntil := sub.ActiveUntilTime()
	if activeUntil.IsZero() {
		return false
	}
	// 已过去的 active_until 与「付费套餐仍在用」矛盾（通常是宽限期/降级中），
	// 写入会立刻被陈旧值清理逻辑清掉，跳过避免展示上反复横跳。
	if !activeUntil.After(now) {
		return false
	}
	changed := store.UpdateAccountSubscriptionExpiresAt(account, activeUntil)
	if changed {
		log.Printf("[账号 %d] 已从上游同步订阅到期时间: %s", account.DBID, activeUntil.Format(time.RFC3339))
	}
	return changed
}
