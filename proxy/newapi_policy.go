package proxy

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

const newAPIPolicyNamespace = "prompt-filter-newapi-policy"
const newAPIReplayNamespace = "prompt-filter-newapi-replay"

type newAPIIdentity struct {
	UserID    string
	ClientIP  string
	RequestID string
}

type newAPIOffenseRecord struct {
	Count     int       `json:"count"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (h *Handler) verifyNewAPIIdentity(c *gin.Context, cfg promptfilter.NewAPIConfig, body []byte) (newAPIIdentity, bool) {
	if c == nil || !cfg.Enabled {
		return newAPIIdentity{}, false
	}
	secret := strings.TrimSpace(os.Getenv("PROMPT_FILTER_NEWAPI_SECRET"))
	if secret == "" {
		secret = strings.TrimSpace(cfg.Secret)
	}
	if secret == "" {
		return newAPIIdentity{}, false
	}
	identity := newAPIIdentity{
		UserID: strings.TrimSpace(c.GetHeader("X-NewAPI-User-ID")), ClientIP: strings.TrimSpace(c.GetHeader("X-NewAPI-Client-IP")),
		RequestID: strings.TrimSpace(c.GetHeader("X-NewAPI-Request-ID")),
	}
	timestampRaw := strings.TrimSpace(c.GetHeader("X-NewAPI-Timestamp"))
	signatureRaw := strings.TrimSpace(c.GetHeader("X-NewAPI-Signature"))
	method := strings.ToUpper(strings.TrimSpace(c.GetHeader("X-NewAPI-Method")))
	path := strings.TrimSpace(c.GetHeader("X-NewAPI-Path"))
	bodyDigest := strings.ToLower(strings.TrimSpace(c.GetHeader("X-NewAPI-Body-SHA256")))
	if identity.UserID == "" || identity.ClientIP == "" || identity.RequestID == "" || timestampRaw == "" || signatureRaw == "" || method == "" || path == "" || bodyDigest == "" {
		return newAPIIdentity{}, false
	}
	timestamp, err := strconv.ParseInt(timestampRaw, 10, 64)
	if err != nil || absInt64(time.Now().Unix()-timestamp) > int64(cfg.MaxClockSkewSeconds) {
		return newAPIIdentity{}, false
	}
	requestPath := c.Request.URL.EscapedPath()
	if requestPath == "" {
		requestPath = c.Request.URL.Path
	}
	digest := sha256.Sum256(body)
	if method != strings.ToUpper(c.Request.Method) || path != requestPath || bodyDigest != hex.EncodeToString(digest[:]) {
		return newAPIIdentity{}, false
	}
	canonical := strings.Join([]string{"v1", timestampRaw, identity.RequestID, identity.UserID, identity.ClientIP, method, path, bodyDigest}, "\n")
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(canonical))
	expected := hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(expected), []byte(strings.ToLower(signatureRaw))) {
		return newAPIIdentity{}, false
	}
	if h == nil || h.cache == nil {
		return newAPIIdentity{}, false
	}
	replayKey := hashRiskIdentity(identity.RequestID)
	unlock, acquired := acquirePromptRuntimeLease(c.Request.Context(), h.cache, newAPIReplayNamespace, replayKey)
	if !acquired {
		return newAPIIdentity{}, false
	}
	defer unlock()
	if _, exists, err := h.cache.GetRuntime(c.Request.Context(), newAPIReplayNamespace, replayKey); err != nil || exists {
		return newAPIIdentity{}, false
	}
	ttl := time.Duration(max(cfg.MaxClockSkewSeconds*2, 60)) * time.Second
	if err := h.cache.SetRuntime(c.Request.Context(), newAPIReplayNamespace, replayKey, []byte("1"), ttl); err != nil {
		return newAPIIdentity{}, false
	}
	return identity, true
}

func absInt64(value int64) int64 {
	if value < 0 {
		return -value
	}
	return value
}

// VerifyNewAPIPolicyHandshake validates the exact signed identity headers used
// by NewAPI without invoking an upstream model or recording an offense.
func (h *Handler) VerifyNewAPIPolicyHandshake(c *gin.Context) {
	cfg := h.store.GetPromptFilterConfig()
	identity, ok := h.verifyNewAPIIdentity(c, cfg.Advanced.NewAPI, nil)
	if !ok {
		c.JSON(http.StatusUnauthorized, gin.H{"success": false, "message": "NewAPI 审计签名校验失败"})
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "NewAPI 审计签名校验成功", "user_id": identity.UserID, "client_ip": identity.ClientIP, "request_id": identity.RequestID, "timestamp": c.GetHeader("X-NewAPI-Timestamp")})
}

func (h *Handler) sendNewAPIPolicyBlock(c *gin.Context, cfg promptfilter.Config, reason string, body []byte) bool {
	identity, verified := h.verifyNewAPIIdentity(c, cfg.Advanced.NewAPI, body)
	if !verified {
		return false
	}
	strike, ban := h.recordNewAPIOffense(c, cfg, identity)
	h.writeNewAPIPolicyHeaders(c, strike, ban)
	api.SendErrorWithStatus(c, api.NewAPIError(api.ErrorCode("request_policy_violation"), "请求违规", api.ErrorTypeInvalidRequest), http.StatusServiceUnavailable)
	return true
}

func (h *Handler) recordNewAPIOffense(c *gin.Context, cfg promptfilter.Config, identity newAPIIdentity) (int, bool) {
	strike := 1
	ban := false
	if h != nil && h.cache != nil {
		key := fmt.Sprintf("user:%s:ip:%s", hashRiskIdentity(identity.UserID), hashRiskIdentity(identity.ClientIP))
		unlock, acquired := acquirePromptRuntimeLease(c.Request.Context(), h.cache, newAPIPolicyNamespace, key)
		if !acquired {
			return strike, ban
		}
		defer unlock()
		var record newAPIOffenseRecord
		if raw, ok, _ := h.cache.GetRuntime(c.Request.Context(), newAPIPolicyNamespace, key); ok {
			_ = json.Unmarshal(raw, &record)
		}
		record.Count++
		record.UpdatedAt = time.Now()
		strike = record.Count
		ban = strike >= cfg.Advanced.NewAPI.BanAfter
		if raw, err := json.Marshal(record); err == nil {
			_ = h.cache.SetRuntime(c.Request.Context(), newAPIPolicyNamespace, key, raw, time.Duration(cfg.Advanced.NewAPI.OffenseWindowSeconds)*time.Second)
		}
	}
	return strike, ban
}

func (h *Handler) writeNewAPIPolicyHeaders(c *gin.Context, strike int, ban bool) {
	c.Header("X-Codex2API-Policy-Violation", "true")
	c.Header("X-Codex2API-Policy-Strike", strconv.Itoa(strike))
	c.Header("X-Codex2API-Policy-Ban", strconv.FormatBool(ban))
	c.Header("X-Codex2API-Policy-Reason", "prompt_policy_violation")
}
