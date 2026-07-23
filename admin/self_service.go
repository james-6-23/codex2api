package admin

import (
	"context"
	"log"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
)

// ==================== 账号自助添加公开门户 ====================
//
// 面向外部/游客的公开页：填联系邮箱 → 生成 ChatGPT 授权链接 → 登录后回填授权码 →
// 账号以「待审核」状态入库（enabled=false + tag self-service），管理员批准后才进调度池。
// 本期不做人机验证，仅 IP 限流 + 邮箱格式校验。凭证与 token 不回显给提交者。

const (
	selfServiceTag       = "self-service"
	selfServiceSource    = "self-service"
	selfServiceRateLimit = 10           // 单 IP 窗口内最多请求数
	selfServiceRateWin   = time.Hour    // 限流窗口
	selfServiceEmailMax  = 254          // 邮箱长度上限
)

var selfServiceEmailRe = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)

// selfServiceRateLimiter 简单的按 IP 滑动计数限流（内存版，够用即可）。
type selfServiceRateLimiter struct {
	mu   sync.Mutex
	hits map[string][]time.Time
}

var globalSelfServiceLimiter = &selfServiceRateLimiter{hits: make(map[string][]time.Time)}

// allow 报告某 IP 是否在窗口内仍有配额；有则记一次。now 由调用方传入以便测试。
func (l *selfServiceRateLimiter) allow(ip string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cutoff := now.Add(-selfServiceRateWin)
	kept := l.hits[ip][:0]
	for _, t := range l.hits[ip] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= selfServiceRateLimit {
		l.hits[ip] = kept
		return false
	}
	l.hits[ip] = append(kept, now)
	return true
}

// PublicAccountPortalPageEnabled 报告账号自助门户是否开启（默认关）。
func (h *Handler) PublicAccountPortalPageEnabled(ctx context.Context) (bool, error) {
	if h == nil || h.db == nil {
		return false, nil
	}
	settings, err := h.db.GetSystemSettings(ctx)
	if err != nil {
		return false, err
	}
	if settings == nil {
		return false, nil
	}
	return settings.PublicAccountPortalPageEnabled, nil
}

// accountPortalMiddleware 门控公开门户 API：开关关闭一律 404；开启则按 IP 限流。
func (h *Handler) accountPortalMiddleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if h == nil || h.db == nil {
			writeError(c, http.StatusServiceUnavailable, "服务未就绪")
			c.Abort()
			return
		}
		ctx, cancel := context.WithTimeout(c.Request.Context(), 3*time.Second)
		ok, err := h.PublicAccountPortalPageEnabled(ctx)
		cancel()
		if err != nil {
			writeInternalError(c, err)
			c.Abort()
			return
		}
		if !ok {
			writeError(c, http.StatusNotFound, "账号自助门户未启用")
			c.Abort()
			return
		}
		if !globalSelfServiceLimiter.allow(c.ClientIP(), time.Now()) {
			security.SecurityAuditLog("ACCOUNT_PORTAL_RATE_LIMITED", "ip="+security.SanitizeLog(c.ClientIP()))
			writeError(c, http.StatusTooManyRequests, "请求过于频繁，请稍后再试")
			c.Abort()
			return
		}
		c.Next()
	}
}

func normalizeContactEmail(raw string) (string, bool) {
	email := strings.TrimSpace(security.SanitizeInput(raw))
	if email == "" || len(email) > selfServiceEmailMax || !selfServiceEmailRe.MatchString(email) {
		return "", false
	}
	return email, true
}

// GenerateAccountPortalAuthURL 生成 ChatGPT 授权链接（公开）。
// POST /api/account-portal/generate-auth-url  body: { contact_email }
func (h *Handler) GenerateAccountPortalAuthURL(c *gin.Context) {
	var req struct {
		ContactEmail string `json:"contact_email"`
	}
	_ = c.ShouldBindJSON(&req)

	email, ok := normalizeContactEmail(req.ContactEmail)
	if !ok {
		writeError(c, http.StatusBadRequest, "请填写有效的联系邮箱")
		return
	}

	state, err := oauthRandomHex(32)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "生成 state 失败")
		return
	}
	codeVerifier, err := oauthRandomHex(64)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "生成 code_verifier 失败")
		return
	}
	sessionID, err := oauthRandomHex(16)
	if err != nil {
		writeError(c, http.StatusInternalServerError, "生成 session_id 失败")
		return
	}

	globalOAuthStore.set(sessionID, &oauthSession{
		State:        state,
		CodeVerifier: codeVerifier,
		RedirectURI:  oauthDefaultRedirectURI,
		ContactEmail: email,
		SelfService:  true,
		CreatedAt:    time.Now(),
	})

	authURL := buildOAuthAuthorizeURL(oauthDefaultRedirectURI, state, codeVerifier)
	c.JSON(http.StatusOK, gin.H{
		"auth_url":   authURL,
		"session_id": sessionID,
	})
}

// SubmitAccountPortalCode 用回填的授权码兑换并以「待审核」入库（公开）。
// POST /api/account-portal/submit-code  body: { session_id, code, state }
func (h *Handler) SubmitAccountPortalCode(c *gin.Context) {
	var req struct {
		SessionID string `json:"session_id"`
		Code      string `json:"code"`
		State     string `json:"state"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式错误")
		return
	}
	req.SessionID = strings.TrimSpace(req.SessionID)
	req.Code = strings.TrimSpace(req.Code)
	req.State = strings.TrimSpace(req.State)
	if req.SessionID == "" || req.Code == "" || req.State == "" {
		writeError(c, http.StatusBadRequest, "session_id、code 和 state 均为必填")
		return
	}

	sess, ok := globalOAuthStore.get(req.SessionID)
	if !ok || !sess.SelfService {
		writeError(c, http.StatusBadRequest, "会话不存在或已过期（有效期 30 分钟），请重新发起授权")
		return
	}
	if req.State != sess.State {
		writeError(c, http.StatusBadRequest, "state 不匹配，请重新发起授权")
		return
	}

	proxyURL := strings.TrimSpace(sess.ProxyURL)
	if proxyURL == "" {
		proxyURL = h.store.GetProxyURL()
	}

	tokenResp, accountInfo, err := doOAuthCodeExchange(c.Request.Context(), req.Code, sess.CodeVerifier, sess.RedirectURI, proxyURL, "oauth-"+req.SessionID)
	if err != nil {
		writeError(c, http.StatusBadGateway, "授权码兑换失败，请确认复制的授权码完整无误")
		return
	}
	globalOAuthStore.delete(req.SessionID)

	if tokenResp.RefreshToken == "" {
		writeError(c, http.StatusBadGateway, "授权未返回 refresh_token，请重新发起授权")
		return
	}
	seed := normalizeTokenCredentialSeed(tokenCredentialSeed{
		refreshToken: tokenResp.RefreshToken,
		accessToken:  tokenResp.AccessToken,
		idToken:      tokenResp.IDToken,
		expiresIn:    tokenResp.ExpiresIn,
	})

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	name := seed.email
	if name == "" {
		name = "self-service-account"
	}

	if _, err := h.upsertSelfServiceAccount(ctx, name, proxyURL, seed, sess.ContactEmail); err != nil {
		if err == errDuplicateOAuthIdentity {
			// 已在池中（或已提交待审核）：对提交者返回中性的成功语义，避免探测账号是否存在。
			c.JSON(http.StatusOK, gin.H{"message": "该账号已在系统中，无需重复提交"})
			return
		}
		writeError(c, http.StatusInternalServerError, "提交失败，请稍后重试")
		return
	}

	email := seed.email
	if accountInfo != nil && accountInfo.Email != "" {
		email = accountInfo.Email
	}
	security.SecurityAuditLog("ACCOUNT_PORTAL_SUBMITTED", "ip="+security.SanitizeLog(c.ClientIP())+" contact="+security.SanitizeLog(sess.ContactEmail)+" account="+security.SanitizeLog(email))

	// 不回显 token / refresh_token；仅告知已进入待审核。
	c.JSON(http.StatusOK, gin.H{
		"message": "已提交，等待管理员审核后生效。感谢你的贡献！",
	})
}

// upsertSelfServiceAccount 以「待审核」状态入库：enabled=false + note(联系人) + tag self-service，
// 不加入运行时调度池（管理员批准后再启用）。重复账号返回 errDuplicateOAuthIdentity。
func (h *Handler) upsertSelfServiceAccount(ctx context.Context, name, proxyURL string, seed tokenCredentialSeed, contactEmail string) (int64, error) {
	seed = normalizeTokenCredentialSeed(seed)
	if seed.email != "" && seed.workspaceID != "" {
		h.mergeDuplicateMu.Lock()
		defer h.mergeDuplicateMu.Unlock()
		if duplicateID, err := h.findOAuthIdentityDuplicate(ctx, seed, 0); err != nil {
			return 0, err
		} else if duplicateID > 0 {
			return 0, errDuplicateOAuthIdentity
		}
	}

	id, err := h.db.InsertAccountWithCredentials(ctx, name, tokenCredentialMap(seed), proxyURL)
	if err != nil {
		return 0, err
	}
	// 待审核：禁用 + 备注 + 打标；不调用 Store.AddAccount，故不进调度池。
	if err := h.db.SetAccountEnabled(ctx, id, false); err != nil {
		return 0, err
	}
	note := "自助提交联系人: " + contactEmail
	if err := h.db.UpdateAccountNote(ctx, id, note); err != nil {
		return 0, err
	}
	if err := h.db.SetAccountTags(ctx, id, []string{selfServiceTag}); err != nil {
		return 0, err
	}
	h.db.InsertAccountEventAsync(id, "added", selfServiceSource)
	log.Printf("自助门户新增待审核账号 %d (%s, 联系人 %s)", id, name, contactEmail)
	return id, nil
}
