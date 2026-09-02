package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/codex2api/auth"
)

// 推荐邀请端点。上游已把 wham/ 前缀下的旧路径下线（GET 返回 404 Not Found），
// 现在的三个端点都挂在 /backend-api/referrals/invite 下：
//   - POST  ""            发送邀请，必填 program_id / entrypoint / emails
//   - GET   /eligibility  资格门控 + 剩余配额 + 官方文案
//   - GET   /tracking     已发邀请列表与兑换状态
const (
	codexInviteBaseURL = "https://chatgpt.com/backend-api/referrals/invite"

	// CodexInviteURL 是发送邀请的端点。
	CodexInviteURL = codexInviteBaseURL
	// CodexInviteEligibilityURL 是邀请资格与配额查询端点。
	CodexInviteEligibilityURL = codexInviteBaseURL + "/eligibility"
	// CodexInviteTrackingURL 是已发邀请跟踪端点。
	CodexInviteTrackingURL = codexInviteBaseURL + "/tracking"
)

// 邀请计划标识。旧版单字段 referral_key 已被上游拆成 program_id + entrypoint 两个字段。
const (
	// DefaultProgramID 是 Codex 消费者版推荐计划。
	DefaultProgramID = "codex_referral_consumer"
	// DefaultEntrypoint 是常驻邀请入口（对应旧的 persistent invite）。
	DefaultEntrypoint = "persistent"
)

// 跟踪列表默认查询窗口与条数，与官方客户端一致。
const (
	defaultTrackingPeriod = "past_90_days"
	defaultTrackingLimit  = 100
)

// 邀请上游使用浏览器型 header（与 codex_cli_rs 探针不同），更贴近官方网页端调用。
// 实测无需携带任何 Cookie（含 _devicecheck）即可通过上游校验。
const (
	inviteOriginator = "Codex Desktop"
	inviteLanguage   = "zh-CN"
	inviteUserAgent  = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/150.0.0.0 Safari/537.36"
)

// 测试替换用的端点覆盖。生产代码不要赋值。
var (
	codexInviteURLForTest            = ""
	codexInviteEligibilityURLForTest = ""
	codexInviteTrackingURLForTest    = ""
)

// CodexInviteResult 是一次邀请调用的结构化结果。
type CodexInviteResult struct {
	OK         bool              `json:"ok"`
	StatusCode int               `json:"status_code"`
	RequestID  string            `json:"request_id,omitempty"`
	ProgramID  string            `json:"program_id"`
	Entrypoint string            `json:"entrypoint"`
	Emails     []string          `json:"emails"`
	Invites    []CodexInviteItem `json:"invites,omitempty"`
	// UpstreamMessage 是上游 detail 里的人类可读原因，FailedEmails 是被拒的收件人。
	// 例如「此人已收到推荐邀请」——这与账号资格无关，只是该收件人不合规，
	// 不能笼统按 403 报成「账号无邀请资格」。
	UpstreamMessage string   `json:"upstream_message,omitempty"`
	FailedEmails    []string `json:"failed_emails,omitempty"`
	// Challenged 为真表示被 Cloudflare 挑战拦下，不是上游的业务结论——调用方应提示重试，
	// 不能按 StatusCode 解读成「无资格」。
	Challenged  bool            `json:"challenged,omitempty"`
	Upstream    json.RawMessage `json:"upstream,omitempty"`
	UpstreamRaw string          `json:"upstream_raw,omitempty"`
}

// CodexInviteItem 是上游返回的单条邀请记录。
type CodexInviteItem struct {
	ReferralID string `json:"referral_id,omitempty"`
	Email      string `json:"email,omitempty"`
	InviteURL  string `json:"invite_url,omitempty"`
}

// CodexInviteGrant 是一条奖励条目。同一次邀请通常给邀请人和受邀人各一条。
// grant_type 目前实测为 personal_credits；rate_limit_* 字段在限流重置类计划下才有值。
type CodexInviteGrant struct {
	Recipient string  `json:"recipient,omitempty"`
	GrantType string  `json:"grant_type,omitempty"`
	Amount    float64 `json:"amount,omitempty"`
	RewardID  string  `json:"reward_id,omitempty"`
}

// CodexInviteTimeFrameRule 是一条配额规则。capacity_type 区分两种独立上限：
// send 是发送次数上限，reward 是能拿到奖励的次数上限（后者通常远小于前者）。
type CodexInviteTimeFrameRule struct {
	InvitesSent  int    `json:"invites_sent"`
	InvitesTotal int    `json:"invites_total"`
	TimeFrame    string `json:"time_frame,omitempty"`
	Type         string `json:"type,omitempty"`
	CapacityType string `json:"capacity_type,omitempty"`
}

// CodexInviteEligibility 是邀请资格与配额查询结果。
// RemainingSendCapacity / RemainingRewardCapacity 用指针区分「上游明确说 0」与
// 「上游没给这个字段」——前者要拦住发送，后者只能放行。
type CodexInviteEligibility struct {
	OK         bool   `json:"ok"`
	StatusCode int    `json:"status_code"`
	RequestID  string `json:"request_id,omitempty"`

	ShouldShow           bool   `json:"should_show"`
	IneligibleReason     string `json:"ineligible_reason,omitempty"`
	IneligibleReasonCode string `json:"ineligible_reason_code,omitempty"`
	ProgramID            string `json:"program_id,omitempty"`
	Entrypoint           string `json:"entrypoint,omitempty"`
	OfferID              string `json:"offer_id,omitempty"`

	Grants                  []CodexInviteGrant `json:"grants,omitempty"`
	RemainingSendCapacity   *int               `json:"remaining_send_capacity,omitempty"`
	RemainingRewardCapacity *int               `json:"remaining_reward_capacity,omitempty"`

	Title          string                     `json:"title,omitempty"`
	Description    string                     `json:"description,omitempty"`
	Rules          []string                   `json:"rules,omitempty"`
	TimeFrameRules []CodexInviteTimeFrameRule `json:"time_frame_rules,omitempty"`

	RequiresExplicitConfirmation bool `json:"requires_explicit_confirmation,omitempty"`

	// UpstreamMessage 见 CodexInviteResult.UpstreamMessage。
	UpstreamMessage string `json:"upstream_message,omitempty"`
	// Challenged 见 CodexInviteResult.Challenged。
	Challenged  bool            `json:"challenged,omitempty"`
	Upstream    json.RawMessage `json:"upstream,omitempty"`
	UpstreamRaw string          `json:"upstream_raw,omitempty"`
}

// CodexInviteTrackingItem 是一条已发邀请记录。
type CodexInviteTrackingItem struct {
	ReferralID        string             `json:"referral_id,omitempty"`
	Email             string             `json:"email,omitempty"`
	Status            string             `json:"status,omitempty"`
	CanResend         bool               `json:"can_resend"`
	InviteURL         string             `json:"invite_url,omitempty"`
	ResendAvailableAt string             `json:"resend_available_at,omitempty"`
	Grants            []CodexInviteGrant `json:"grants,omitempty"`
	CreatedAt         string             `json:"created_at,omitempty"`
	ExpiresAt         string             `json:"expires_at,omitempty"`
}

// CodexInviteTracking 是已发邀请列表查询结果。
type CodexInviteTracking struct {
	OK         bool   `json:"ok"`
	StatusCode int    `json:"status_code"`
	RequestID  string `json:"request_id,omitempty"`

	Items  []CodexInviteTrackingItem `json:"items"`
	Cursor string                    `json:"cursor,omitempty"`

	// UpstreamMessage 见 CodexInviteResult.UpstreamMessage。
	UpstreamMessage string `json:"upstream_message,omitempty"`
	// Challenged 见 CodexInviteResult.Challenged。
	Challenged  bool            `json:"challenged,omitempty"`
	Upstream    json.RawMessage `json:"upstream,omitempty"`
	UpstreamRaw string          `json:"upstream_raw,omitempty"`
}

// parseInviteFailureDetail 解析上游失败响应里的 detail 字段。
//
// 这个字段是多态的，两种形态都实测到过：
//   - 字符串：{"detail":"Referral invites are not available for your plan"}  账号级无资格
//   - 对象：  {"detail":{"message":"此人已收到推荐邀请","failed_emails":["x@y"]}}  收件人级被拒
//
// 后者与账号资格无关，必须把原因和失败邮箱透出来，否则调用方只看到一个 403，
// 会把「这个收件人不合规」误报成「这个账号不能发邀请」。
func parseInviteFailureDetail(body []byte) (string, []string) {
	var envelope struct {
		Detail json.RawMessage `json:"detail"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || len(envelope.Detail) == 0 {
		return "", nil
	}
	var asString string
	if err := json.Unmarshal(envelope.Detail, &asString); err == nil {
		return asString, nil
	}
	var asObject struct {
		Message      string   `json:"message"`
		FailedEmails []string `json:"failed_emails"`
	}
	if err := json.Unmarshal(envelope.Detail, &asObject); err == nil {
		return asObject.Message, asObject.FailedEmails
	}
	return "", nil
}

// normalizeInviteProgram 补齐空的计划标识为默认值。
func normalizeInviteProgram(programID, entrypoint string) (string, string) {
	if strings.TrimSpace(programID) == "" {
		programID = DefaultProgramID
	}
	if strings.TrimSpace(entrypoint) == "" {
		entrypoint = DefaultEntrypoint
	}
	return programID, entrypoint
}

// NormalizeInviteProgram 是 normalizeInviteProgram 的导出版本，供调用方在发起
// 请求前先算出实际生效的计划标识——缓存键必须按归一化后的值拼，否则「空参数」
// 与「显式传默认值」会各存一份内容相同的条目。
func NormalizeInviteProgram(programID, entrypoint string) (string, string) {
	return normalizeInviteProgram(programID, entrypoint)
}

// normalizeInviteTracking 补齐跟踪查询的窗口与条数为官方客户端默认值。
func normalizeInviteTracking(period string, limit int) (string, int) {
	if strings.TrimSpace(period) == "" {
		period = defaultTrackingPeriod
	}
	if limit <= 0 || limit > defaultTrackingLimit {
		limit = defaultTrackingLimit
	}
	return period, limit
}

// NormalizeInviteTracking 是 normalizeInviteTracking 的导出版本。理由同
// NormalizeInviteProgram：limit=0 与 limit=100 取回的是同一份数据，缓存键要一致。
func NormalizeInviteTracking(period string, limit int) (string, int) {
	return normalizeInviteTracking(period, limit)
}

// newInviteRequest 构造带邀请专用 header 的请求。body 为 nil 时发 GET。
func newInviteRequest(ctx context.Context, account *auth.Account, method, endpoint string, body []byte) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return nil, fmt.Errorf("build invite request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+account.GetAccessToken())
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Oai-Language", inviteLanguage)
	req.Header.Set("Originator", inviteOriginator)
	req.Header.Set("User-Agent", inviteUserAgent)
	// 邀请操作作用于自定义头覆盖后的空间,与实际流量指向一致。
	if accountID := account.EffectiveAccountID(); accountID != "" {
		req.Header.Set("Chatgpt-Account-Id", accountID)
	}
	return req, nil
}

// inviteCookieJars 按账号缓存 cookie jar。这些端点走 Cloudflare 的 bot 管理，
// 不带 cookie 的请求每次都是冷启动，实测约半数会被丢进 managed challenge（403 + 挑战页）。
// 复用上一次成功响应里的 __cf_bm / _cfuvid 能显著降低被挑战概率。按账号隔离而非全局共用，
// 避免把某个账号的会话 cookie（oai-sc）串到别的账号上。
var inviteCookieJars sync.Map

// inviteCookieJarFor 取出账号对应的 cookie jar，没有则新建。
func inviteCookieJarFor(account *auth.Account) http.CookieJar {
	key := fmt.Sprintf("%d|%s", account.DBID, account.EffectiveAccountID())
	if cached, ok := inviteCookieJars.Load(key); ok {
		return cached.(http.CookieJar)
	}
	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil
	}
	actual, _ := inviteCookieJars.LoadOrStore(key, http.CookieJar(jar))
	return actual.(http.CookieJar)
}

// cloudflareChallengeMarker 替换挑战页正文，避免把十几 KB 的 HTML 回给前端。
const cloudflareChallengeMarker = "cloudflare managed challenge (html challenge page omitted)"

// isCloudflareChallenge 判定响应是否为 Cloudflare 的挑战页而非上游业务响应。
// 挑战页会借用 403/429/503 状态码，直接按状态码解读会把「被拦下」误判成「无资格」。
func isCloudflareChallenge(status int, body []byte) bool {
	switch status {
	case http.StatusForbidden, http.StatusTooManyRequests, http.StatusServiceUnavailable:
	default:
		return false
	}
	// 先按「是不是 HTML」排除业务 JSON，避免对正常的 {"detail":...} 做全文扫描。
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '<' {
		return false
	}
	// 特征标记必须全文匹配：挑战页把内联 SVG 图标放在脚本之前，实测 cf_chl_opt
	// 出现在 7KB 之后，只扫开头会漏判（正文已由 LimitReader 限到 1MB）。
	for _, marker := range [][]byte{
		[]byte("cf_chl_opt"),
		[]byte("challenge-platform"),
		[]byte("Enable JavaScript and cookies to continue"),
	} {
		if bytes.Contains(body, marker) {
			return true
		}
	}
	return false
}

// inviteHTTPClient 返回邀请请求用的 Client。
//
// 这些端点是网页端 /backend-api 路径，挂在 Cloudflare bot 管理后面：用 Go 默认 TLS 指纹
// 直连会被随机丢进 managed challenge（同一账号连续请求在 200 与 403 挑战页之间反复跳），
// 因此和订阅同步一样强制 uTLS Chrome 指纹。transport 取自池子（连接由池持有，不会像
// 一次性 uTLS 实例那样泄漏 h2 连接），jar 按账号另挂，让 __cf_bm 能在多次请求间复用。
// useTestTransport 供 httptest（明文 HTTP）用，uTLS 无法对明文端口拨号。
func inviteHTTPClient(account *auth.Account, proxyURL string, useTestTransport bool) *http.Client {
	jar := inviteCookieJarFor(account)
	if useTestTransport {
		return &http.Client{Transport: newCodexStandardTransport(proxyURL), Jar: jar}
	}
	pooled := getMaintenanceClient(account, proxyURL, maintenancePurposeInvite, true)
	return &http.Client{Transport: pooled.Transport, Jar: jar}
}

// doInviteRequest 执行请求并读回状态码、request id、响应体与是否被 Cloudflare 挑战。
func doInviteRequest(req *http.Request, account *auth.Account, proxyURL string, useTestTransport bool) (int, string, []byte, bool, error) {
	client := inviteHTTPClient(account, proxyURL, useTestTransport)
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", nil, false, fmt.Errorf("invite request: %w", err)
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	_ = resp.Body.Close()
	return resp.StatusCode, resp.Header.Get("x-oai-request-id"), body, isCloudflareChallenge(resp.StatusCode, body), nil
}

// inviteAccountReady 校验账号具备发起邀请所需的凭证。
func inviteAccountReady(account *auth.Account) error {
	if account == nil {
		return fmt.Errorf("account is nil")
	}
	if account.GetAccessToken() == "" {
		return fmt.Errorf("account has no access token")
	}
	return nil
}

// SendCodexInvite 通过账号凭证向 ChatGPT 推荐邀请端点发送邀请邮件。
// programID / entrypoint 为空时使用默认的消费者版常驻入口。返回结构化结果；
// 非 2xx 时 OK=false 且 Upstream/UpstreamRaw 保留上游响应，由调用方据 StatusCode 处理。
func SendCodexInvite(ctx context.Context, account *auth.Account, proxyURL, programID, entrypoint string, emails []string) (*CodexInviteResult, error) {
	if err := inviteAccountReady(account); err != nil {
		return nil, err
	}
	if len(emails) == 0 {
		return nil, fmt.Errorf("no emails to invite")
	}
	programID, entrypoint = normalizeInviteProgram(programID, entrypoint)

	endpoint := CodexInviteURL
	useTestTransport := codexInviteURLForTest != ""
	if useTestTransport {
		endpoint = codexInviteURLForTest
	}

	payload, _ := json.Marshal(map[string]any{
		"program_id": programID,
		"entrypoint": entrypoint,
		"emails":     emails,
	})

	req, err := newInviteRequest(ctx, account, http.MethodPost, endpoint, payload)
	if err != nil {
		return nil, err
	}
	status, requestID, body, challenged, err := doInviteRequest(req, account, proxyURL, useTestTransport)
	if err != nil {
		return nil, err
	}

	result := &CodexInviteResult{
		OK:         status >= 200 && status < 300,
		StatusCode: status,
		RequestID:  requestID,
		ProgramID:  programID,
		Entrypoint: entrypoint,
		Emails:     emails,
		Challenged: challenged,
	}

	// 被 Cloudflare 挑战时正文是挑战页 HTML，没有任何上游语义，丢掉只留标记。
	if challenged {
		result.UpstreamRaw = cloudflareChallengeMarker
		return result, nil
	}

	// 上游响应：能解析为 JSON 就放进 Upstream 并抽取邀请明细，否则留原始串。
	// 明细字段名在两个端点间不一致（发送用 invites[]、跟踪用 items[]），两种都收。
	if json.Valid(body) {
		result.Upstream = json.RawMessage(body)
		if !result.OK {
			result.UpstreamMessage, result.FailedEmails = parseInviteFailureDetail(body)
		}
		var parsed struct {
			Invites []CodexInviteItem `json:"invites"`
			Items   []CodexInviteItem `json:"items"`
		}
		if err := json.Unmarshal(body, &parsed); err == nil {
			if len(parsed.Invites) > 0 {
				result.Invites = parsed.Invites
			} else {
				result.Invites = parsed.Items
			}
		}
	} else if len(body) > 0 {
		result.UpstreamRaw = string(body)
	}

	return result, nil
}

// QueryCodexInviteEligibility 查询账号在指定推荐计划下的资格与剩余配额。
func QueryCodexInviteEligibility(ctx context.Context, account *auth.Account, proxyURL, programID, entrypoint string) (*CodexInviteEligibility, error) {
	if err := inviteAccountReady(account); err != nil {
		return nil, err
	}
	programID, entrypoint = normalizeInviteProgram(programID, entrypoint)

	endpoint := CodexInviteEligibilityURL
	useTestTransport := codexInviteEligibilityURLForTest != ""
	if useTestTransport {
		endpoint = codexInviteEligibilityURLForTest
	}
	query := url.Values{}
	query.Set("program_id", programID)
	query.Set("entrypoint", entrypoint)

	req, err := newInviteRequest(ctx, account, http.MethodGet, endpoint+"?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	status, requestID, body, challenged, err := doInviteRequest(req, account, proxyURL, useTestTransport)
	if err != nil {
		return nil, err
	}

	result := &CodexInviteEligibility{
		OK:         status >= 200 && status < 300,
		StatusCode: status,
		RequestID:  requestID,
		ProgramID:  programID,
		Entrypoint: entrypoint,
		Challenged: challenged,
	}
	if challenged {
		result.UpstreamRaw = cloudflareChallengeMarker
		return result, nil
	}
	if json.Valid(body) {
		result.Upstream = json.RawMessage(body)
		if !result.OK {
			result.UpstreamMessage, _ = parseInviteFailureDetail(body)
		}
		// 解析进临时结构再拷贝，避免上游的 program_id/entrypoint 缺失时把请求参数覆盖成空。
		var parsed struct {
			ShouldShow           bool                       `json:"should_show"`
			IneligibleReason     string                     `json:"ineligible_reason"`
			IneligibleReasonCode string                     `json:"ineligible_reason_code"`
			ProgramID            string                     `json:"program_id"`
			Entrypoint           string                     `json:"entrypoint"`
			OfferID              string                     `json:"offer_id"`
			Grants               []CodexInviteGrant         `json:"grants"`
			RemainingSend        *int                       `json:"remaining_send_capacity"`
			RemainingReward      *int                       `json:"remaining_reward_capacity"`
			Title                string                     `json:"title"`
			Description          string                     `json:"description"`
			Rules                []string                   `json:"rules"`
			TimeFrameRules       []CodexInviteTimeFrameRule `json:"time_frame_rules"`
			RequiresConfirm      bool                       `json:"requires_explicit_confirmation"`
		}
		if err := json.Unmarshal(body, &parsed); err == nil {
			result.ShouldShow = parsed.ShouldShow
			result.IneligibleReason = parsed.IneligibleReason
			result.IneligibleReasonCode = parsed.IneligibleReasonCode
			if parsed.ProgramID != "" {
				result.ProgramID = parsed.ProgramID
			}
			if parsed.Entrypoint != "" {
				result.Entrypoint = parsed.Entrypoint
			}
			result.OfferID = parsed.OfferID
			result.Grants = parsed.Grants
			result.RemainingSendCapacity = parsed.RemainingSend
			result.RemainingRewardCapacity = parsed.RemainingReward
			result.Title = parsed.Title
			result.Description = parsed.Description
			result.Rules = parsed.Rules
			result.TimeFrameRules = parsed.TimeFrameRules
			result.RequiresExplicitConfirmation = parsed.RequiresConfirm
		}
	} else if len(body) > 0 {
		result.UpstreamRaw = string(body)
	}

	return result, nil
}

// QueryCodexInviteTracking 查询账号已发出的邀请及其兑换状态。
// period / limit 为空或非正时使用官方客户端默认值（past_90_days / 100）。
func QueryCodexInviteTracking(ctx context.Context, account *auth.Account, proxyURL, programID, period string, limit int) (*CodexInviteTracking, error) {
	if err := inviteAccountReady(account); err != nil {
		return nil, err
	}
	programID, _ = normalizeInviteProgram(programID, "")
	period, limit = normalizeInviteTracking(period, limit)

	endpoint := CodexInviteTrackingURL
	useTestTransport := codexInviteTrackingURLForTest != ""
	if useTestTransport {
		endpoint = codexInviteTrackingURLForTest
	}
	query := url.Values{}
	query.Set("limit", strconv.Itoa(limit))
	query.Set("period", period)
	query.Set("program_id", programID)

	req, err := newInviteRequest(ctx, account, http.MethodGet, endpoint+"?"+query.Encode(), nil)
	if err != nil {
		return nil, err
	}
	status, requestID, body, challenged, err := doInviteRequest(req, account, proxyURL, useTestTransport)
	if err != nil {
		return nil, err
	}

	result := &CodexInviteTracking{
		OK:         status >= 200 && status < 300,
		StatusCode: status,
		RequestID:  requestID,
		Items:      []CodexInviteTrackingItem{},
		Challenged: challenged,
	}
	if challenged {
		result.UpstreamRaw = cloudflareChallengeMarker
		return result, nil
	}
	if json.Valid(body) {
		result.Upstream = json.RawMessage(body)
		if !result.OK {
			result.UpstreamMessage, _ = parseInviteFailureDetail(body)
		}
		var parsed struct {
			Items   []CodexInviteTrackingItem `json:"items"`
			Invites []CodexInviteTrackingItem `json:"invites"`
			Cursor  string                    `json:"cursor"`
		}
		if err := json.Unmarshal(body, &parsed); err == nil {
			if len(parsed.Items) > 0 {
				result.Items = parsed.Items
			} else if len(parsed.Invites) > 0 {
				result.Items = parsed.Invites
			}
			result.Cursor = parsed.Cursor
		}
	} else if len(body) > 0 {
		result.UpstreamRaw = string(body)
	}

	return result, nil
}
