package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"testing"
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

// subscriptionHeaderProfile 是访问订阅端点时的身份形态。
//
// 实测（2026-09-12，出口 IP 被网页端 JS 人机验证拦下的机器）：带 Origin/Referer/Chrome UA
// 的浏览器伪装会被挑战（403 HTML），而同一 IP、同一令牌换成 Codex CLI 的 UA + Originator
// 直接 200 拿到 active_until——挑战是被浏览器伪装触发的，不是 IP 本身。因此默认先以
// Codex 身份访问（与 wham 探针一致），被挑战/拒绝再回退浏览器伪装。
type subscriptionHeaderProfile int

const (
	subscriptionHeadersCodex subscriptionHeaderProfile = iota
	subscriptionHeadersBrowser
)

func (p subscriptionHeaderProfile) String() string {
	if p == subscriptionHeadersBrowser {
		return "browser"
	}
	return "codex"
}

// applySubscriptionHeaders 写入订阅端点的身份头。浏览器形态是专为绕过网页端挑战的
// 回退，保持独立的浏览器协议；Codex 形态与其他维护请求同源（issue #774）。
func applySubscriptionHeaders(req *http.Request, account *auth.Account, profile subscriptionHeaderProfile) error {
	req.Header.Set("Accept", "application/json")
	switch profile {
	case subscriptionHeadersBrowser:
		req.Header.Set("Origin", "https://chatgpt.com")
		req.Header.Set("Referer", "https://chatgpt.com/")
		req.Header.Set("User-Agent", subscriptionsBrowserUserAgent)
	default:
		return applyCodexMaintenanceHeaders(req, account)
	}
	return nil
}

// shouldRetrySubscriptionWithBrowser 判断某种身份被拒后是否值得换另一种身份再试：
// 只对 403（挑战页/WAF 拒绝）换身份，401/404/5xx 换头也没用。
func shouldRetrySubscriptionWithBrowser(err error) bool {
	var httpErr *SubscriptionHTTPError
	return errors.As(err, &httpErr) && httpErr.Status == http.StatusForbidden
}

// subscriptionProbeMinInterval 是同一账号两次订阅到期探针的最小间隔。
// 到期时间只在续费/退订时变化，无需高频访问网页端点。
const subscriptionProbeMinInterval = 6 * time.Hour

// ChatGPTSubscription 是 /backend-api/subscriptions 响应中本服务关心的字段。
type ChatGPTSubscription struct {
	PlanType      string `json:"plan_type"`
	ActiveStart   string `json:"active_start"`
	ActiveUntil   string `json:"active_until"`
	BillingPeriod string `json:"billing_period"`
	WillRenew     bool   `json:"will_renew"`
	IsDelinquent  bool   `json:"is_delinquent"`
	// GracePeriodEnd 上游字段形态未固定（可能是 RFC3339 或 unix 秒），原样保留后解析。
	GracePeriodEnd json.RawMessage `json:"grace_period_end_timestamp"`
}

// GracePeriodEndTime 解析宽限期结束时间；缺失/null/格式非法返回零值。
func (s *ChatGPTSubscription) GracePeriodEndTime() time.Time {
	if s == nil || len(s.GracePeriodEnd) == 0 {
		return time.Time{}
	}
	raw := strings.TrimSpace(string(s.GracePeriodEnd))
	if raw == "" || raw == "null" {
		return time.Time{}
	}
	var asString string
	if err := json.Unmarshal(s.GracePeriodEnd, &asString); err == nil {
		if t, err := time.Parse(time.RFC3339, strings.TrimSpace(asString)); err == nil {
			return t
		}
		return time.Time{}
	}
	var asNumber float64
	if err := json.Unmarshal(s.GracePeriodEnd, &asNumber); err == nil && asNumber > 0 {
		secs := int64(asNumber)
		if secs > 1e12 { // 毫秒
			secs /= 1000
		}
		return time.Unix(secs, 0).UTC()
	}
	return time.Time{}
}

// SubscriptionHTTPError 是订阅端点返回非 200 时的错误，保留状态码供调用方分类。
type SubscriptionHTTPError struct {
	Status int
	Body   string
}

func (e *SubscriptionHTTPError) Error() string {
	return fmt.Sprintf("subscriptions returned status %d: %s", e.Status, e.Body)
}

// NoSubscription 判断上游是否明确表示该工作区没有订阅记录（k12/edu 等计划实测 404）。
func (e *SubscriptionHTTPError) NoSubscription() bool {
	return e != nil && e.Status == http.StatusNotFound
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

// QueryChatGPTSubscription 查询账号当前工作区的订阅信息：先以 Codex CLI 身份访问，
// 403 时回退浏览器伪装（两种身份在不同出口 IP 上各有被拒的情况）。
// account_id 必须是工作区 UUID：历史数据污染写入的 user-... 会让上游返回 500，
// 直接跳过不发请求。
func QueryChatGPTSubscription(ctx context.Context, account *auth.Account, proxyURL string) (*ChatGPTSubscription, error) {
	sub, err := queryChatGPTSubscriptionWith(ctx, account, proxyURL, subscriptionHeadersCodex)
	if err == nil || !shouldRetrySubscriptionWithBrowser(err) || ctx.Err() != nil {
		return sub, err
	}
	firstErr := err
	sub, err = queryChatGPTSubscriptionWith(ctx, account, proxyURL, subscriptionHeadersBrowser)
	if err == nil {
		if account != nil {
			log.Printf("[账号 %d] 订阅端点以 Codex 身份被拒（%s），浏览器身份成功", account.DBID, subscriptionErrorForStorage(firstErr))
		}
		return sub, nil
	}
	return nil, err
}

func queryChatGPTSubscriptionWith(ctx context.Context, account *auth.Account, proxyURL string, profile subscriptionHeaderProfile) (*ChatGPTSubscription, error) {
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
	useTestTransport := false
	if subscriptionsURLForTest != "" {
		endpoint = subscriptionsURLForTest
		useTestTransport = true
	}
	// Resin 启用时经反代访问（指纹由 Resin 侧承担），与其他账号维护请求一致，
	// 避免全部账号共享本机出口 IP 直连（issue #372）。
	finalURL, resinClient, viaResin := resinMaintenanceTarget(account, endpoint)

	// 网页端点用 uTLS Chrome 指纹（两种身份形态都走它），与网关的 transport 模式配置无关。
	// 池化持有（按账号+代理隔离）：一次性 uTLS transport 用完即弃，其名下的
	// HTTP/2 连接没有任何持有者去关闭，会持续泄漏（issue #446）。
	var client *http.Client
	switch {
	case resinClient != nil:
		client = resinClient
	case useTestTransport:
		// 测试用 httptest（明文 HTTP），uTLS 拨号无法使用，回退标准 transport。
		client = &http.Client{Transport: newCodexStandardTransport(proxyURL)}
	default:
		client = getMaintenanceClient(account, proxyURL, maintenancePurposeSubscription, true)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, finalURL+"?account_id="+url.QueryEscape(accountID), nil)
	if err != nil {
		return nil, fmt.Errorf("build subscriptions request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	if err := applySubscriptionHeaders(req, account, profile); err != nil {
		return nil, err
	}
	if viaResin {
		req.Header.Set("X-Resin-Account", ResinAccountID(account))
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
		return nil, &SubscriptionHTTPError{Status: resp.StatusCode, Body: truncateForLog(body, 200)}
	}

	var sub ChatGPTSubscription
	if err := json.Unmarshal(body, &sub); err != nil {
		return nil, fmt.Errorf("parse subscriptions response: %w", err)
	}
	return &sub, nil
}

// SubscriptionSyncOutcome 是一次订阅同步的结果分类。
type SubscriptionSyncOutcome string

const (
	// SubscriptionSyncUpdated 拿到了新的权威到期时间并已写入。
	SubscriptionSyncUpdated SubscriptionSyncOutcome = "updated"
	// SubscriptionSyncUnchanged 查询成功，到期时间与已知值一致。
	SubscriptionSyncUnchanged SubscriptionSyncOutcome = "unchanged"
	// SubscriptionSyncNoSubscription 订阅提供方对该工作区没有订阅记录（不算失败）。
	SubscriptionSyncNoSubscription SubscriptionSyncOutcome = "no_subscription"
	// SubscriptionSyncUnsupported 非付费套餐，不发起查询。
	SubscriptionSyncUnsupported SubscriptionSyncOutcome = "unsupported"
	// SubscriptionSyncFailed 查询失败（网络/鉴权/被拦截/响应缺字段）。
	SubscriptionSyncFailed SubscriptionSyncOutcome = "failed"
)

// SubscriptionSyncResult 是一次订阅同步的结构化结果。
type SubscriptionSyncResult struct {
	Outcome      SubscriptionSyncOutcome
	ExpiresAt    time.Time
	Subscription *ChatGPTSubscription
	Err          error
}

// Updated 报告是否写入了新的到期时间。
func (r SubscriptionSyncResult) Updated() bool { return r.Outcome == SubscriptionSyncUpdated }

// subscriptionErrorForStorage 把错误压成可落库/可展示的一句话：
// 非 200 只留状态码，HTML 响应体（Cloudflare 拦截页）标注疑似被拦截，不把整页塞进库。
// SubscriptionErrorAntiBotChallenge 是出口 IP 被网页端要求人机验证时的稳定错误文案，
// 前端据此给出「绑定代理后重试」的提示。
const SubscriptionErrorAntiBotChallenge = "anti-bot challenge for this exit IP; bind a proxy to the account and retry"

// SubscriptionErrorSummary 是 subscriptionErrorForStorage 的导出别名，供管理端响应复用。
func SubscriptionErrorSummary(err error) string { return subscriptionErrorForStorage(err) }

func subscriptionErrorForStorage(err error) string {
	if err == nil {
		return ""
	}
	var httpErr *SubscriptionHTTPError
	if errors.As(err, &httpErr) {
		body := strings.TrimSpace(httpErr.Body)
		lower := strings.ToLower(body)
		if strings.HasPrefix(lower, "<html") || strings.HasPrefix(lower, "<!doctype") || strings.Contains(lower, "<head>") {
			// 两种身份都被网页端人机验证拦下（"Enable JavaScript and cookies to continue"），
			// 网关过不了 JS 挑战；剩下的出路是给账号绑定干净代理，文案里直接说。
			return fmt.Sprintf("HTTP %d: %s", httpErr.Status, SubscriptionErrorAntiBotChallenge)
		}
		if len(body) > 120 {
			body = body[:120] + "…"
		}
		if body == "" {
			return fmt.Sprintf("HTTP %d", httpErr.Status)
		}
		return fmt.Sprintf("HTTP %d: %s", httpErr.Status, body)
	}
	msg := strings.TrimSpace(err.Error())
	if len(msg) > 200 {
		msg = msg[:200] + "…"
	}
	return msg
}

// SyncSubscriptionExpiry 无条件向订阅提供方查询一次并把结果落到账号订阅元数据：
// 成功时写权威到期时间 + 自动续期 + 宽限期并标 confirmed；404 标 unsupported；
// 其余失败标 failed 并保留最后一次已知业务状态。调用方负责节流。
func SyncSubscriptionExpiry(ctx context.Context, store *auth.Store, account *auth.Account, proxyURL string) SubscriptionSyncResult {
	if store == nil || account == nil {
		return SubscriptionSyncResult{Outcome: SubscriptionSyncFailed, Err: fmt.Errorf("store or account is nil")}
	}
	now := time.Now()
	planType := account.GetPlanType()
	previousExpiry := account.GetSubscriptionExpiresAt()
	if !auth.SubscriptionPlanTracked(planType, previousExpiry) || strings.EqualFold(strings.TrimSpace(planType), "free") {
		return SubscriptionSyncResult{Outcome: SubscriptionSyncUnsupported}
	}
	// 无论成败都记录尝试时间：上游异常时避免每次探针都重复访问网页端点。
	account.MarkSubscriptionExpiryProbed(now)

	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	sub, err := QueryChatGPTSubscription(ctx, account, proxyURL)
	if err != nil {
		var httpErr *SubscriptionHTTPError
		if errors.As(err, &httpErr) && httpErr.NoSubscription() {
			store.UpdateSubscriptionMeta(account, func(m *auth.SubscriptionMeta) {
				m.CheckedAt = now
				m.SyncState = auth.SubscriptionSyncUnsupported
				m.WillRenew = auth.SubscriptionAutoRenewUnsupported
				m.Error = ""
			})
			return SubscriptionSyncResult{Outcome: SubscriptionSyncNoSubscription, ExpiresAt: previousExpiry}
		}
		log.Printf("[账号 %d] 订阅到期时间同步失败: %v", account.DBID, err)
		store.UpdateSubscriptionMeta(account, func(m *auth.SubscriptionMeta) {
			m.CheckedAt = now
			m.Error = subscriptionErrorForStorage(err)
			if m.LastKnownStatus == "" && previousExpiry.IsZero() {
				m.SyncState = auth.SubscriptionSyncUnknown
			} else {
				m.SyncState = auth.SubscriptionSyncFailed
			}
		})
		return SubscriptionSyncResult{Outcome: SubscriptionSyncFailed, ExpiresAt: previousExpiry, Err: err}
	}

	activeUntil := sub.ActiveUntilTime()
	if activeUntil.IsZero() {
		err := fmt.Errorf("subscriptions response has no active_until")
		store.UpdateSubscriptionMeta(account, func(m *auth.SubscriptionMeta) {
			m.CheckedAt = now
			m.Error = err.Error()
			if m.LastKnownStatus == "" && previousExpiry.IsZero() {
				m.SyncState = auth.SubscriptionSyncUnknown
			} else {
				m.SyncState = auth.SubscriptionSyncFailed
			}
		})
		return SubscriptionSyncResult{Outcome: SubscriptionSyncFailed, ExpiresAt: previousExpiry, Subscription: sub, Err: err}
	}

	graceUntil := sub.GracePeriodEndTime()
	willRenew := auth.SubscriptionAutoRenewDisabled
	if sub.WillRenew {
		willRenew = auth.SubscriptionAutoRenewEnabled
	}

	// 已过去的 active_until 与「付费套餐仍在用」矛盾：有宽限期结束时间时按宽限期
	// 落库（陈旧值清理会放过宽限期内的到期时间）；没有则不写入，避免刚写就被清理，
	// 展示上反复横跳。
	applyExpiry := activeUntil.After(now) || (!graceUntil.IsZero() && graceUntil.After(now))
	changed := false
	if applyExpiry {
		changed = store.UpdateAccountSubscriptionExpiresAt(account, activeUntil)
	}
	effectiveExpiry := previousExpiry
	if applyExpiry {
		effectiveExpiry = activeUntil
	}
	status, _, _ := auth.ComputeSubscriptionBusinessStatus(effectiveExpiry, graceUntil, now, time.Local)
	store.UpdateSubscriptionMeta(account, func(m *auth.SubscriptionMeta) {
		m.CheckedAt = now
		m.Error = ""
		m.WillRenew = willRenew
		if applyExpiry {
			m.SyncState = auth.SubscriptionSyncConfirmed
			m.Source = auth.SubscriptionSourceProviderAPI
			m.LastKnownStatus = status
			m.GraceUntil = graceUntil
		} else if m.SyncState != auth.SubscriptionSyncPending {
			// 提供方给的是已过去的到期时间又没有宽限期，与付费套餐仍在用矛盾：
			// 「已续费 · 待确认」继续保持待确认，其余情况按查询成功记录。
			m.SyncState = auth.SubscriptionSyncConfirmed
		}
		// 有效期真的向后延了才算「检测到续费」；首次拿到到期时间不算。
		if changed && !previousExpiry.IsZero() && activeUntil.After(previousExpiry) {
			m.RenewalDetectedAt = now
		}
	})
	if changed {
		log.Printf("[账号 %d] 已从上游同步订阅到期时间: %s", account.DBID, activeUntil.Format(time.RFC3339))
		return SubscriptionSyncResult{Outcome: SubscriptionSyncUpdated, ExpiresAt: activeUntil, Subscription: sub}
	}
	return SubscriptionSyncResult{Outcome: SubscriptionSyncUnchanged, ExpiresAt: effectiveExpiry, Subscription: sub}
}

// MaybeSyncSubscriptionExpiry 按需从网页端同步权威订阅到期时间（best-effort）：
// 仅付费套餐且到期时间未知/临近/已过时才发起，带节流；失败只记日志不影响调用方。
// 返回是否更新了到期时间。
func MaybeSyncSubscriptionExpiry(ctx context.Context, store *auth.Store, account *auth.Account, proxyURL string) bool {
	if store == nil || account == nil {
		return false
	}
	if !account.NeedsSubscriptionExpiryProbe(time.Now(), subscriptionProbeMinInterval) {
		return false
	}
	return SyncSubscriptionExpiry(ctx, store, account, proxyURL).Updated()
}

// 陈旧到期时间被清理（推断已续费）后立即发起一次权威同步，把 pending 尽快落成
// confirmed。异步执行；同一账号在途时不重复发起。
func init() {
	auth.OnStaleSubscriptionCleared = func(store *auth.Store, account *auth.Account) {
		if store == nil || account == nil {
			return
		}
		// 测试进程里没有替换订阅端点时不要真去打 chatgpt.com。
		if testing.Testing() && subscriptionsURLForTest == "" {
			return
		}
		if !account.BeginSubscriptionSync() {
			return
		}
		go func() {
			defer account.EndSubscriptionSync()
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			SyncSubscriptionExpiry(ctx, store, account, store.ResolveProxyForAccount(account))
		}()
	}
}
