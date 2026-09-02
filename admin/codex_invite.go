package admin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const (
	inviteDefaultMaxEmails        = 10
	inviteUpperMaxEmails          = 50
	inviteRecipientCheckMaxEmails = 200
)

const (
	// v1:响应结构变更时升版，让 Redis 里旧部署写入的条目自然失效。
	inviteEligibilityCacheNamespace = "admin:codex-invite-eligibility:v1"
	inviteTrackingCacheNamespace    = "admin:codex-invite-tracking:v1"

	// 两级 TTL 分工不同：运行态缓存只挡秒级重复（切号来回点、多个管理员同时
	// 开着页面），快照 TTL 才决定多久真正回一次上游。上游这两个端点挂在
	// Cloudflare bot 管理后面，请求越密越容易被丢进 managed challenge，
	// 少打一次就是少一次被挑战的机会。
	inviteRuntimeCacheTTL = 60 * time.Second
	// 资格是月度累计配额，只会被「本网关发邀请」和「用户自己在官网发」改动，
	// 前者发生时会主动失效，所以窗口可以给得长一些。
	inviteEligibilitySnapshotTTL = 15 * time.Minute
	// 已发记录的兑换状态由受邀人行为驱动，网关完全不可知，只能靠短窗口兜。
	inviteTrackingSnapshotTTL = 5 * time.Minute
)

// inviteCachedPayload 是运行态缓存里的条目。ObservedAt/ExpiresAt 跟着数据一起存，
// 让 Redis 条目和数据库快照携带同一组元信息——否则从两条路径读出的「更新于」
// 会不一致。ExpiresAt 是数据的有效期（分钟级），与 Redis 键自身的 TTL（60s）无关。
type inviteCachedPayload struct {
	HTTPStatus int             `json:"http_status"`
	ObservedAt time.Time       `json:"observed_at"`
	ExpiresAt  time.Time       `json:"expires_at"`
	Payload    json.RawMessage `json:"payload"`
}

// inviteCacheMeta 随响应下发，让前端能显示数据是何时取回的、是不是缓存。
type inviteCacheMeta struct {
	Source     string     `json:"source"` // upstream / runtime / snapshot
	ObservedAt *time.Time `json:"observed_at,omitempty"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
}

func inviteUpstreamMeta() *inviteCacheMeta {
	now := time.Now()
	return &inviteCacheMeta{Source: "upstream", ObservedAt: &now}
}

// inviteForceRefresh 判断是否绕过缓存直连上游。前端的手动刷新按钮与发送后的
// 重新拉取都会带上这个参数。
func inviteForceRefresh(c *gin.Context) bool {
	switch strings.ToLower(strings.TrimSpace(c.Query("refresh"))) {
	case "1", "true", "yes":
		return true
	}
	return false
}

// 缓存作用域：同一份结果的完整标识。归一化后的参数才进 scope，否则「不传 limit」
// 与「传 100」会各存一份内容完全相同的条目。
func inviteEligibilityScope(programID, entrypoint string) string {
	return programID + "|" + entrypoint
}

func inviteTrackingScope(programID, period string, limit int) string {
	return fmt.Sprintf("%s|%s|%d", programID, period, limit)
}

// readInviteCache 依次尝试运行态缓存与数据库快照，命中则把结果解进 dest。
// 未命中返回 nil。任何一层出错都只记日志并继续回上游——缓存不该让功能不可用。
func (h *Handler) readInviteCache(ctx context.Context, namespace, kind string, accountID, generation int64, scope string, dest any) *inviteCacheMeta {
	now := time.Now()

	key := inviteCacheKey(accountID, generation, scope)
	var cached inviteCachedPayload
	if h.getRuntimeJSON(ctx, namespace, key, &cached) &&
		len(cached.Payload) > 0 && now.Before(cached.ExpiresAt) {
		if err := json.Unmarshal(cached.Payload, dest); err == nil {
			return &inviteCacheMeta{Source: "runtime", ObservedAt: &cached.ObservedAt, ExpiresAt: &cached.ExpiresAt}
		}
	}

	if h.db == nil {
		return nil
	}
	snap, err := h.db.GetCodexInviteSnapshot(ctx, accountID, kind, scope)
	if err != nil {
		log.Printf("读取邀请快照失败: account=%d kind=%s err=%v", accountID, kind, err)
		return nil
	}
	// 凭据代数不同说明账号重新授权过，旧快照描述的是另一份身份的资格。
	if snap == nil || snap.Expired(now) || snap.CredentialGeneration != generation {
		return nil
	}
	if err := json.Unmarshal(snap.Payload, dest); err != nil {
		return nil
	}
	// 回填运行态缓存，让同一份快照在接下来的 60 秒里不再回库。
	h.setRuntimeJSON(ctx, namespace, key, inviteCachedPayload{
		HTTPStatus: snap.HTTPStatus,
		ObservedAt: snap.ObservedAt,
		ExpiresAt:  snap.ExpiresAt,
		Payload:    snap.Payload,
	}, inviteRuntimeCacheTTL)
	return &inviteCacheMeta{Source: "snapshot", ObservedAt: &snap.ObservedAt, ExpiresAt: &snap.ExpiresAt}
}

// writeInviteCache 落一次成功观测。只在上游给出业务结论时调用——失败绝不入缓存，
// 见 database.CodexInviteSnapshot 的说明。
func (h *Handler) writeInviteCache(ctx context.Context, namespace, kind string, accountID, generation int64, scope string, httpStatus int, ttl time.Duration, value any) {
	payload, err := json.Marshal(value)
	if err != nil {
		log.Printf("编码邀请缓存失败: account=%d kind=%s err=%v", accountID, kind, err)
		return
	}
	now := time.Now()
	expiresAt := now.Add(ttl)

	h.setRuntimeJSON(ctx, namespace, inviteCacheKey(accountID, generation, scope), inviteCachedPayload{
		HTTPStatus: httpStatus,
		ObservedAt: now,
		ExpiresAt:  expiresAt,
		Payload:    payload,
	}, inviteRuntimeCacheTTL)

	if h.db == nil {
		return
	}
	if err := h.db.UpsertCodexInviteSnapshot(ctx, &database.CodexInviteSnapshot{
		AccountID:            accountID,
		Kind:                 kind,
		Scope:                scope,
		CredentialGeneration: generation,
		HTTPStatus:           httpStatus,
		Payload:              payload,
		ObservedAt:           now,
		ExpiresAt:            expiresAt,
	}); err != nil {
		log.Printf("写入邀请快照失败: account=%d kind=%s err=%v", accountID, kind, err)
	}
}

func inviteCacheKey(accountID, generation int64, scope string) string {
	return fmt.Sprintf("%d|%d|%s", accountID, generation, scope)
}

// invalidateInviteCache 在配额确实被改动后清缓存（目前只有「发送成功」一种情形）。
//
// 快照按账号整体删除，这是权威的那一层。运行态缓存的键含参数组合，无法枚举，
// 只删默认组合——非默认参数只可能由手工调 API 产生，最多陈旧一个 runtime TTL；
// 前端在发送后会带 refresh=1 重新拉取，走不到缓存。
func (h *Handler) invalidateInviteCache(ctx context.Context, accountID, generation int64, programID, entrypoint string) {
	if h.cache != nil {
		period, limit := proxy.NormalizeInviteTracking("", 0)
		targets := []struct {
			namespace string
			scope     string
		}{
			{inviteEligibilityCacheNamespace, inviteEligibilityScope(programID, entrypoint)},
			{inviteTrackingCacheNamespace, inviteTrackingScope(programID, period, limit)},
		}
		for _, target := range targets {
			if err := h.cache.DeleteRuntime(ctx, target.namespace, inviteCacheKey(accountID, generation, target.scope)); err != nil {
				log.Printf("清理邀请运行态缓存失败: account=%d namespace=%s err=%v", accountID, target.namespace, err)
			}
		}
	}
	if h.db == nil {
		return
	}
	if err := h.db.DeleteCodexInviteSnapshots(ctx, accountID); err != nil {
		log.Printf("清理邀请快照失败: account=%d err=%v", accountID, err)
	}
}

var inviteEmailPattern = regexp.MustCompile(`^[^\s@]+@[^\s@]+\.[^\s@]+$`)
var inviteEmailSplitter = regexp.MustCompile(`[,;\r\n\t ]+`)

type inviteRequest struct {
	Emails     []string `json:"emails"`
	EmailsText string   `json:"emails_text"`
	// 上游已把旧的单字段 referral_key 拆成 program_id + entrypoint，留空走默认计划。
	ProgramID  string `json:"program_id"`
	Entrypoint string `json:"entrypoint"`
	ProxyURL   string `json:"proxy_url"`
	MaxEmails  int    `json:"max_emails"`
}

type inviteRecipientCheckRequest struct {
	Emails []string `json:"emails"`
}

// CheckInviteRecipients 批量查询收件邮箱是否已经被本网关预占或邀请过。
// POST /api/accounts/invite/recipients/check
func (h *Handler) CheckInviteRecipients(c *gin.Context) {
	if h.db == nil {
		writeError(c, http.StatusServiceUnavailable, "邀请邮箱账本不可用")
		return
	}

	var req inviteRecipientCheckRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	emails, err := collectInviteEmails(req.Emails, "", inviteRecipientCheckMaxEmails)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	recipients, err := h.db.ListCodexInviteRecipientsByEmails(c.Request.Context(), emails)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "查询邀请邮箱状态失败: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"recipients": recipients})
}

// SendInvite 通过指定账号向 ChatGPT 推荐邀请端点发送邀请邮件。
// POST /api/accounts/:id/invite
func (h *Handler) SendInvite(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return
	}

	var req inviteRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}

	maxEmails := inviteDefaultMaxEmails
	if req.MaxEmails > 0 && req.MaxEmails < maxEmails {
		maxEmails = req.MaxEmails
	}
	if maxEmails > inviteUpperMaxEmails {
		maxEmails = inviteUpperMaxEmails
	}

	emails, err := collectInviteEmails(req.Emails, req.EmailsText, maxEmails)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}

	account := h.findAccountByID(id)
	if account == nil {
		writeError(c, http.StatusNotFound, "账号不存在")
		return
	}
	if account.GetAccessToken() == "" {
		writeError(c, http.StatusBadRequest, "账号没有可用的 access token，请先刷新账号")
		return
	}
	if h.db == nil {
		// 不能在没有持久账本的情况下先发邮件：上游成功后将无法阻止再次邀请。
		writeError(c, http.StatusServiceUnavailable, "邀请邮箱账本不可用")
		return
	}

	proxyURL := strings.TrimSpace(req.ProxyURL)
	if proxyURL == "" {
		proxyURL = h.store.ResolveProxyForAccount(account)
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 45*time.Second)
	defer cancel()

	programID, entrypoint := proxy.NormalizeInviteProgram(req.ProgramID, req.Entrypoint)
	reservationID := uuid.NewString()
	if _, err := h.db.ReserveCodexInviteRecipients(ctx, reservationID, account.DBID, programID, entrypoint, emails); err != nil {
		var conflict *database.CodexInviteRecipientConflictError
		if errors.As(err, &conflict) {
			writeError(c, http.StatusConflict, "以下邮箱已经邀请过或正在发送，不能重复邀请: "+strings.Join(conflict.Emails, ", "))
			return
		}
		writeError(c, http.StatusInternalServerError, "预占邀请邮箱失败: "+err.Error())
		return
	}

	sendInvite := h.sendCodexInvite
	if sendInvite == nil {
		sendInvite = proxy.SendCodexInvite
	}
	result, err := sendInvite(ctx, account, proxyURL, programID, entrypoint, emails)
	if err != nil {
		// 网络错误不能证明上游没有受理。为保证同一邮箱至多发送一次，保守标记为
		// unknown，等待 tracking 或管理员核对，而不是自动释放后允许立即重试。
		if _, markErr := h.db.MarkCodexInviteRecipientsUnknown(c.Request.Context(), reservationID, "", 0); markErr != nil {
			log.Printf("标记邀请邮箱结果未知失败: reservation=%s err=%v", reservationID, markErr)
		}
		writeError(c, http.StatusBadGateway, "邀请请求失败: "+err.Error())
		return
	}

	if result.Challenged {
		// 挑战页明确没有进入邀请业务端点，可以安全释放，保留输入供用户稍后重试。
		if _, releaseErr := h.db.ReleaseCodexInviteRecipients(c.Request.Context(), reservationID); releaseErr != nil {
			log.Printf("释放被挑战的邀请邮箱失败: reservation=%s err=%v", reservationID, releaseErr)
		}
	} else if !result.OK {
		// 上游明确给出业务拒绝时本次没有成功发送。若原因明确说明收件人已收到
		// 邀请，则把对应邮箱记为 known_invited；其余预占释放，允许修正后重试。
		observedAt := time.Now()
		evidence := inviteRecipientEvidenceFromInviteItems(result.Invites, observedAt)
		if inviteFailureIndicatesAlreadyInvited(result.UpstreamMessage) && len(result.FailedEmails) > 0 {
			evidence = append(evidence, inviteRecipientEvidenceFromEmails(result.FailedEmails, observedAt)...)
		}
		canRelease := true
		if len(evidence) > 0 {
			if _, markErr := h.db.MarkCodexInviteRecipientsKnownInvited(c.Request.Context(), reservationID,
				result.RequestID, result.StatusCode, evidence, observedAt); markErr != nil {
				// 上游已经给出收件人级证据，但本地没能精确固化时不能解锁重试；
				// 把本批剩余预占转 unknown，宁可人工核对也不冒发第二封的风险。
				canRelease = false
				log.Printf("记录上游已邀请邮箱失败: reservation=%s err=%v", reservationID, markErr)
				if _, unknownErr := h.db.MarkCodexInviteRecipientsUnknown(c.Request.Context(), reservationID,
					result.RequestID, result.StatusCode); unknownErr != nil {
					log.Printf("保守锁定邀请邮箱失败: reservation=%s err=%v", reservationID, unknownErr)
				}
			}
		}
		if canRelease {
			if _, releaseErr := h.db.ReleaseCodexInviteRecipients(c.Request.Context(), reservationID); releaseErr != nil {
				log.Printf("释放失败邀请邮箱预占失败: reservation=%s err=%v", reservationID, releaseErr)
			}
		}
	}

	// 只要请求真到过上游，配额就可能已经动过——收件人级被拒也可能已扣掉发送次数。
	// 唯一确定没动的是被 Cloudflare 挡在门外的那次。宁可多回一次上游，也不能
	// 让页面继续显示发送前的剩余次数。
	if !result.Challenged {
		h.invalidateInviteCache(c.Request.Context(), account.DBID, account.GetCredentialGeneration(), programID, entrypoint)
	}

	if !result.OK {
		// 常见：无 referral 资格的账号返回 403。透传上游响应供前端展示。
		c.JSON(http.StatusOK, gin.H{
			"ok":     false,
			"result": result,
		})
		return
	}

	invitedAt := time.Now()
	evidence := inviteRecipientEvidenceFromInviteItems(result.Invites, invitedAt)
	if _, finalizeErr := h.db.FinalizeCodexInviteRecipients(c.Request.Context(), reservationID,
		result.RequestID, result.StatusCode, evidence, invitedAt); finalizeErr != nil {
		// 邮件已经发出，绝不能把成功响应改成可重试的失败。预占行仍会阻止重复发送。
		log.Printf("固化邀请邮箱成功状态失败: reservation=%s err=%v", reservationID, finalizeErr)
	}

	c.JSON(http.StatusOK, gin.H{
		"ok":              true,
		"result":          result,
		"recorded_emails": emails,
	})
}

func inviteFailureIndicatesAlreadyInvited(message string) bool {
	normalized := strings.ToLower(strings.TrimSpace(message))
	if normalized == "" {
		return false
	}
	if strings.Contains(normalized, "已") &&
		(strings.Contains(normalized, "邀请") || strings.Contains(normalized, "邀請")) {
		return true
	}
	for _, marker := range []string{
		"already invited",
		"already received an invite",
		"already received a referral",
		"has already been invited",
		"previously invited",
	} {
		if strings.Contains(normalized, marker) {
			return true
		}
	}
	return false
}

func inviteRecipientEvidenceFromEmails(emails []string, observedAt time.Time) []database.CodexInviteRecipientEvidence {
	evidence := make([]database.CodexInviteRecipientEvidence, 0, len(emails))
	for _, email := range emails {
		if strings.TrimSpace(email) == "" {
			continue
		}
		evidence = append(evidence, database.CodexInviteRecipientEvidence{
			Email:     email,
			InvitedAt: observedAt,
		})
	}
	return evidence
}

func inviteRecipientEvidenceFromInviteItems(items []proxy.CodexInviteItem, observedAt time.Time) []database.CodexInviteRecipientEvidence {
	evidence := make([]database.CodexInviteRecipientEvidence, 0, len(items))
	for _, item := range items {
		if strings.TrimSpace(item.Email) == "" {
			continue
		}
		evidence = append(evidence, database.CodexInviteRecipientEvidence{
			Email:      item.Email,
			ReferralID: item.ReferralID,
			InviteURL:  item.InviteURL,
			InvitedAt:  observedAt,
		})
	}
	return evidence
}

// rememberInviteTrackingRecipients 把上游 tracking 证明存在的历史邀请并入全局账本。
// 无论邀请已兑换、过期还是可重发，产品规则都是「一个邮箱只邀请一次」，因此都保留。
func (h *Handler) rememberInviteTrackingRecipients(ctx context.Context, accountID int64, programID, entrypoint string, result *proxy.CodexInviteTracking) {
	if h.db == nil || result == nil || !result.OK || result.Challenged || len(result.Items) == 0 {
		return
	}
	evidence := make([]database.CodexInviteRecipientEvidence, 0, len(result.Items))
	for _, item := range result.Items {
		if strings.TrimSpace(item.Email) == "" {
			continue
		}
		invitedAt := time.Time{}
		if item.CreatedAt != "" {
			invitedAt, _ = time.Parse(time.RFC3339Nano, item.CreatedAt)
		}
		evidence = append(evidence, database.CodexInviteRecipientEvidence{
			Email:                   item.Email,
			ReferralID:              item.ReferralID,
			InviteURL:               item.InviteURL,
			UpstreamRecipientStatus: item.Status,
			InvitedAt:               invitedAt,
		})
	}
	if len(evidence) == 0 {
		return
	}
	_, err := h.db.UpsertCodexInviteRecipientsFromTracking(ctx, accountID, programID, entrypoint, result.StatusCode, evidence)
	if err != nil {
		log.Printf("回填邀请邮箱账本失败: account=%d err=%v", accountID, err)
	}
}

// GetInviteEligibility 查询指定账号的推荐邀请资格与剩余配额。
// GET /api/accounts/:id/invite/eligibility
func (h *Handler) GetInviteEligibility(c *gin.Context) {
	account, proxyURL, ok := h.resolveInviteAccount(c)
	if !ok {
		return
	}

	programID, entrypoint := proxy.NormalizeInviteProgram(
		strings.TrimSpace(c.Query("program_id")), strings.TrimSpace(c.Query("entrypoint")))
	scope := inviteEligibilityScope(programID, entrypoint)
	generation := account.GetCredentialGeneration()
	reqCtx := c.Request.Context()

	if !inviteForceRefresh(c) {
		var cached proxy.CodexInviteEligibility
		if meta := h.readInviteCache(reqCtx, inviteEligibilityCacheNamespace,
			database.CodexInviteSnapshotEligibility, account.DBID, generation, scope, &cached); meta != nil {
			c.JSON(http.StatusOK, gin.H{"ok": cached.OK, "result": cached, "cache": meta})
			return
		}
	}

	ctx, cancel := context.WithTimeout(reqCtx, 30*time.Second)
	defer cancel()

	result, err := proxy.QueryCodexInviteEligibility(ctx, account, proxyURL, programID, entrypoint)
	if err != nil {
		writeError(c, http.StatusBadGateway, "资格查询失败: "+err.Error())
		return
	}

	// 只有上游给出业务结论时才入缓存。挑战页借用 403、与「无资格」同码，
	// 缓存它等于把一次 Cloudflare 拦截固化成十五分钟的「这个号不能发邀请」。
	// 200 + should_show=false 是真结论，照常缓存。
	if result.OK && !result.Challenged {
		h.writeInviteCache(reqCtx, inviteEligibilityCacheNamespace, database.CodexInviteSnapshotEligibility,
			account.DBID, generation, scope, result.StatusCode, inviteEligibilitySnapshotTTL, result)
	}

	// 上游非 2xx 不算网关错误，透传供前端展示（如无资格账号的 403）。
	c.JSON(http.StatusOK, gin.H{"ok": result.OK, "result": result, "cache": inviteUpstreamMeta()})
}

// GetInviteTracking 查询指定账号已发出的邀请及兑换状态。
// GET /api/accounts/:id/invite/tracking
func (h *Handler) GetInviteTracking(c *gin.Context) {
	account, proxyURL, ok := h.resolveInviteAccount(c)
	if !ok {
		return
	}

	rawLimit, _ := strconv.Atoi(strings.TrimSpace(c.Query("limit")))
	programID, entrypoint := proxy.NormalizeInviteProgram(strings.TrimSpace(c.Query("program_id")), "")
	period, limit := proxy.NormalizeInviteTracking(strings.TrimSpace(c.Query("period")), rawLimit)
	scope := inviteTrackingScope(programID, period, limit)
	generation := account.GetCredentialGeneration()
	reqCtx := c.Request.Context()

	if !inviteForceRefresh(c) {
		var cached proxy.CodexInviteTracking
		if meta := h.readInviteCache(reqCtx, inviteTrackingCacheNamespace,
			database.CodexInviteSnapshotTracking, account.DBID, generation, scope, &cached); meta != nil {
			h.rememberInviteTrackingRecipients(reqCtx, account.DBID, programID, entrypoint, &cached)
			c.JSON(http.StatusOK, gin.H{"ok": cached.OK, "result": cached, "cache": meta})
			return
		}
	}

	ctx, cancel := context.WithTimeout(reqCtx, 30*time.Second)
	defer cancel()

	result, err := proxy.QueryCodexInviteTracking(ctx, account, proxyURL, programID, period, limit)
	if err != nil {
		writeError(c, http.StatusBadGateway, "邀请记录查询失败: "+err.Error())
		return
	}

	if result.OK && !result.Challenged {
		h.rememberInviteTrackingRecipients(reqCtx, account.DBID, programID, entrypoint, result)
		h.writeInviteCache(reqCtx, inviteTrackingCacheNamespace, database.CodexInviteSnapshotTracking,
			account.DBID, generation, scope, result.StatusCode, inviteTrackingSnapshotTTL, result)
	}

	c.JSON(http.StatusOK, gin.H{"ok": result.OK, "result": result, "cache": inviteUpstreamMeta()})
}

// resolveInviteAccount 解析路径上的账号 ID、校验凭证并解出代理。
// 校验失败时已写入错误响应，返回 ok=false。
func (h *Handler) resolveInviteAccount(c *gin.Context) (*auth.Account, string, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		writeError(c, http.StatusBadRequest, "无效的账号 ID")
		return nil, "", false
	}
	account := h.findAccountByID(id)
	if account == nil {
		writeError(c, http.StatusNotFound, "账号不存在")
		return nil, "", false
	}
	if account.GetAccessToken() == "" {
		writeError(c, http.StatusBadRequest, "账号没有可用的 access token，请先刷新账号")
		return nil, "", false
	}

	proxyURL := strings.TrimSpace(c.Query("proxy_url"))
	if proxyURL == "" {
		proxyURL = h.store.ResolveProxyForAccount(account)
	}
	return account, proxyURL, true
}

// collectInviteEmails 合并 emails[] 与文本来源，去重（忽略大小写）、校验格式、夹紧上限。
func collectInviteEmails(list []string, text string, maxEmails int) ([]string, error) {
	raw := make([]string, 0, len(list))
	raw = append(raw, list...)
	if strings.TrimSpace(text) != "" {
		raw = append(raw, inviteEmailSplitter.Split(text, -1)...)
	}

	seen := make(map[string]struct{})
	emails := make([]string, 0, len(raw))
	for _, e := range raw {
		e = strings.TrimSpace(e)
		if e == "" {
			continue
		}
		key := strings.ToLower(e)
		if _, ok := seen[key]; ok {
			continue
		}
		if !inviteEmailPattern.MatchString(e) {
			return nil, fmt.Errorf("邮箱格式不正确: %s", e)
		}
		seen[key] = struct{}{}
		emails = append(emails, e)
	}

	if len(emails) == 0 {
		return nil, fmt.Errorf("请至少提供一个邮箱")
	}
	if len(emails) > maxEmails {
		return nil, fmt.Errorf("邮箱数量超过上限 %d", maxEmails)
	}
	return emails, nil
}
