package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/codex2api/cache"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const promptRiskNamespace = "prompt-filter-risk"
const promptSidecarCacheNamespace = "prompt-filter-sidecar-cache"

func acquirePromptRuntimeLease(ctx context.Context, runtimeCache cache.TokenCache, namespace, key string) (func(), bool) {
	if runtimeCache == nil {
		return func() {}, false
	}
	owner := uuid.NewString()
	deadline := time.NewTimer(500 * time.Millisecond)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		acquired, err := runtimeCache.AcquireLease(ctx, namespace, key, owner, 5*time.Second)
		if err != nil {
			return func() {}, false
		}
		if acquired {
			return func() {
				releaseCtx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				_ = runtimeCache.ReleaseLease(releaseCtx, namespace, key, owner)
			}, true
		}
		select {
		case <-ctx.Done():
			return func() {}, false
		case <-deadline.C:
			return func() {}, false
		case <-ticker.C:
		}
	}
}

type promptRiskRecord struct {
	Score     int       `json:"score"`
	UpdatedAt time.Time `json:"updated_at"`
}

type sidecarRequest struct {
	Direction    string               `json:"direction"`
	Text         string               `json:"text"`
	LocalScore   int                  `json:"local_score"`
	MatchedRules []promptfilter.Match `json:"matched_rules"`
}

type sidecarResponse struct {
	Action     string   `json:"action"`
	Score      int      `json:"score"`
	Confidence float64  `json:"confidence,omitempty"`
	Categories []string `json:"categories"`
	Reason     string   `json:"reason"`
}

func (h *Handler) applyAdvancedPromptProtection(c *gin.Context, text string, verdict promptfilter.Verdict, cfg promptfilter.Config) promptfilter.Verdict {
	verdict = h.applyPromptSemanticProtection(c, text, verdict, cfg)
	if cfg.Advanced.Risk.Enabled {
		verdict = h.applyPromptRisk(c, verdict, cfg)
	}
	return verdict
}

func (h *Handler) applyPromptSemanticProtection(c *gin.Context, text string, verdict promptfilter.Verdict, cfg promptfilter.Config) promptfilter.Verdict {
	if !cfg.Enabled || verdict.TerminalStrictHit {
		return verdict
	}
	cleanRequest := verdict.Action == promptfilter.ActionAllow
	shouldCallSidecar := verdict.Score >= cfg.Advanced.Sidecar.MinScore
	if cfg.Advanced.Risk.Enabled && verdict.Score >= cfg.Advanced.Risk.ReviewThreshold {
		shouldCallSidecar = true
	}
	if cleanRequest && cfg.Advanced.Sidecar.ScanCleanEnabled {
		shouldCallSidecar = deterministicPromptSample(text, cfg.Advanced.Sidecar.SamplePercent)
	}
	if cfg.Advanced.Sidecar.Enabled && shouldCallSidecar {
		verdict = h.applyPromptSidecarWithState(promptGuardRequestContext(c), text, verdict, cfg, cleanRequest)
	}
	return verdict
}

func (h *Handler) applyPromptRisk(c *gin.Context, verdict promptfilter.Verdict, cfg promptfilter.Config) promptfilter.Verdict {
	if h == nil || h.cache == nil || c == nil {
		return verdict
	}
	risk := cfg.Advanced.Risk
	if verdict.Reviewed && !verdict.ReviewFlagged {
		return verdict
	}
	if !shouldAccumulatePromptRisk(verdict) {
		return verdict
	}
	ttl := time.Duration(risk.WindowSeconds) * time.Second
	type weightedRiskKey struct {
		key    string
		weight int
	}
	keys := make([]weightedRiskKey, 0, 3)
	policyContext, verified := h.verifyNewAPIPolicyContext(c, cfg.Advanced.NewAPI, ingressRequestBody(c, nil))
	if verified {
		runtimeScope := newAPIRuntimeScopeForPolicyContext(policyContext)
		keys = append(keys,
			weightedRiskKey{runtimeScope + ":newapi-user:" + hashRiskIdentity(policyContext.Identity.UserID), risk.UserWeightPercent},
			weightedRiskKey{runtimeScope + ":newapi-ip:" + hashRiskIdentity(policyContext.Identity.ClientIP), risk.IPWeightPercent},
		)
		if policyContext.MetaVerified {
			keys = append(keys, weightedRiskKey{runtimeScope + ":newapi-session:" + hashRiskIdentity(policyContext.Meta.SessionFingerprint), risk.SessionWeightPercent})
		}
	} else if cfg.Advanced.NewAPI.Enabled {
		// A shared NewAPI channel key must never combine unrelated users when the
		// signed identity is unavailable. Local filtering still runs; only the
		// cross-request accumulator fails open.
		return verdict
	} else {
		keys = append(keys,
			weightedRiskKey{fmt.Sprintf("user:%d", requestAPIKeyID(c)), risk.UserWeightPercent},
			weightedRiskKey{"ip:" + hashRiskIdentity(c.ClientIP()), risk.IPWeightPercent},
			weightedRiskKey{"session:" + hashRiskIdentity(promptSessionID(c)), risk.SessionWeightPercent},
		)
	}
	effective := verdict.Score
	now := time.Now()
	for _, item := range keys {
		if item.weight <= 0 || strings.HasSuffix(item.key, ":") || strings.HasSuffix(item.key, ":0") {
			continue
		}
		unlock, acquired := acquirePromptRuntimeLease(c.Request.Context(), h.cache, promptRiskNamespace, item.key)
		if !acquired {
			continue
		}
		var record promptRiskRecord
		if raw, ok, _ := h.cache.GetRuntime(c.Request.Context(), promptRiskNamespace, item.key); ok {
			_ = json.Unmarshal(raw, &record)
			age := now.Sub(record.UpdatedAt)
			if age > 0 && age < ttl {
				record.Score = record.Score * int(ttl-age) / int(ttl)
			} else if age >= ttl {
				record.Score = 0
			}
		}
		effective += record.Score * item.weight / 100
		record.Score += verdict.Score
		maxStoredRisk := risk.BlockThreshold * 4
		if maxStoredRisk > 0 && record.Score > maxStoredRisk {
			record.Score = maxStoredRisk
		}
		record.UpdatedAt = now
		if raw, err := json.Marshal(record); err == nil {
			_ = h.cache.SetRuntime(c.Request.Context(), promptRiskNamespace, item.key, raw, ttl)
		}
		unlock()
	}
	if effective > verdict.Score {
		verdict.RiskScore = effective
		verdict.Reason = fmt.Sprintf("%s; cumulative risk=%d", verdict.Reason, effective)
	}
	return verdict
}

// Cumulative risk is an audit and review signal, never an independent block
// source. Only confirmed, high-confidence current-prompt events are persisted;
// generic keyword scores must not make later benign requests fail.
func shouldAccumulatePromptRisk(verdict promptfilter.Verdict) bool {
	if verdict.Reviewed {
		return verdict.ReviewFlagged
	}
	return verdict.SensitiveIntent && (verdict.TerminalStrictHit || verdict.TerminalCategoryHit)
}

func promptSessionID(c *gin.Context) string {
	for _, name := range []string{"X-Session-ID", "OpenAI-Session-ID", "Session-ID"} {
		if value := strings.TrimSpace(c.GetHeader(name)); value != "" {
			return value
		}
	}
	return ""
}

func hashRiskIdentity(value string) string {
	if strings.TrimSpace(value) == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:12])
}

func applyPromptSidecar(ctx context.Context, text string, verdict promptfilter.Verdict, cfg promptfilter.Config) promptfilter.Verdict {
	return (&Handler{}).applyPromptSidecarWithState(ctx, text, verdict, cfg, verdict.Action == promptfilter.ActionAllow)
}

func (h *Handler) applyPromptSidecarWithState(ctx context.Context, text string, verdict promptfilter.Verdict, cfg promptfilter.Config, cleanRequest bool) promptfilter.Verdict {
	sc := promptfilter.NormalizeAdvancedConfig(cfg.Advanced).Sidecar
	baseURL := strings.TrimSpace(sc.BaseURL)
	if baseURL == "" {
		return sidecarFailure(verdict, !cleanRequest && sc.FailClosed, fmt.Errorf("sidecar base URL is empty"))
	}
	text = boundedPromptText(text, sc.MaxTextLength)
	if text == "" {
		return verdict
	}
	payload, _ := json.Marshal(sidecarRequest{Direction: "input", Text: text, LocalScore: verdict.Score, MatchedRules: verdict.Matched})
	cacheDigest := sha256.Sum256(append([]byte(baseURL+"\x00"), payload...))
	cacheKey := hex.EncodeToString(cacheDigest[:])
	if h != nil && h.cache != nil && sc.CacheTTLSeconds > 0 {
		if raw, ok, err := h.cache.GetRuntime(ctx, promptSidecarCacheNamespace, cacheKey); err == nil && ok {
			var cached sidecarResponse
			if json.Unmarshal(raw, &cached) == nil {
				return applyPromptSidecarResponse(verdict, cached, sc.Mode)
			}
		}
	}
	breakerKey := hashRiskIdentity("sidecar\x00" + baseURL)
	if h.promptExtensionBreakerOpen(ctx, breakerKey) {
		return sidecarFailure(verdict, !cleanRequest && sc.FailClosed, fmt.Errorf("sidecar circuit breaker is open"))
	}
	release, acquired := acquirePromptExtensionSlot("sidecar:"+baseURL, sc.MaxConcurrent)
	if !acquired {
		return sidecarFailure(verdict, false, fmt.Errorf("sidecar capacity is exhausted"))
	}
	defer release()
	endpoint := strings.TrimRight(baseURL, "/") + "/v1/guard/check"
	requestCtx, cancel := context.WithTimeout(ctx, time.Duration(sc.TimeoutSeconds)*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return sidecarFailure(verdict, !cleanRequest && sc.FailClosed, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key := strings.TrimSpace(os.Getenv("PROMPT_FILTER_SIDECAR_API_KEY")); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.recordPromptExtensionFailure(ctx, breakerKey, sc.CircuitBreakerFailures, sc.CircuitBreakerSeconds)
		return sidecarFailure(verdict, !cleanRequest && sc.FailClosed, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		h.recordPromptExtensionFailure(ctx, breakerKey, sc.CircuitBreakerFailures, sc.CircuitBreakerSeconds)
		return sidecarFailure(verdict, !cleanRequest && sc.FailClosed, fmt.Errorf("sidecar status %d", resp.StatusCode))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024+1))
	if err != nil {
		h.recordPromptExtensionFailure(ctx, breakerKey, sc.CircuitBreakerFailures, sc.CircuitBreakerSeconds)
		return sidecarFailure(verdict, !cleanRequest && sc.FailClosed, err)
	}
	if len(body) > 64*1024 {
		h.recordPromptExtensionFailure(ctx, breakerKey, sc.CircuitBreakerFailures, sc.CircuitBreakerSeconds)
		return sidecarFailure(verdict, !cleanRequest && sc.FailClosed, fmt.Errorf("sidecar response exceeded 65536 bytes"))
	}
	var result sidecarResponse
	if err := json.Unmarshal(body, &result); err != nil {
		h.recordPromptExtensionFailure(ctx, breakerKey, sc.CircuitBreakerFailures, sc.CircuitBreakerSeconds)
		return sidecarFailure(verdict, !cleanRequest && sc.FailClosed, err)
	}
	result.Score = max(0, min(100, result.Score))
	h.clearPromptExtensionFailure(ctx, breakerKey)
	if h != nil && h.cache != nil && sc.CacheTTLSeconds > 0 {
		if raw, err := json.Marshal(result); err == nil {
			_ = h.cache.SetRuntime(ctx, promptSidecarCacheNamespace, cacheKey, raw, time.Duration(sc.CacheTTLSeconds)*time.Second)
		}
	}
	return applyPromptSidecarResponse(verdict, result, sc.Mode)
}

func applyPromptSidecarResponse(verdict promptfilter.Verdict, result sidecarResponse, mode string) promptfilter.Verdict {
	result.Score = max(0, min(100, result.Score))
	if result.Score > verdict.Score {
		verdict.Score = result.Score
	}
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case promptfilter.GuardModeEnforce:
		if strings.EqualFold(result.Action, promptfilter.ActionBlock) {
			verdict.Action = promptfilter.ActionBlock
		} else if strings.EqualFold(result.Action, promptfilter.ActionWarn) && verdict.Action == promptfilter.ActionAllow {
			verdict.Action = promptfilter.ActionWarn
		}
	case promptfilter.GuardModeWarn:
		if (strings.EqualFold(result.Action, promptfilter.ActionBlock) || strings.EqualFold(result.Action, promptfilter.ActionWarn)) && verdict.Action == promptfilter.ActionAllow {
			verdict.Action = promptfilter.ActionWarn
		}
	}
	if result.Reason != "" {
		verdict.Reason = strings.TrimSpace(verdict.Reason + "; sidecar: " + result.Reason)
	}
	verdict.ReviewModel = "semantic-sidecar"
	return verdict
}

func deterministicPromptSample(text string, percent int) bool {
	if percent <= 0 || strings.TrimSpace(text) == "" {
		return false
	}
	if percent >= 100 {
		return true
	}
	digest := sha256.Sum256([]byte(text))
	value := int(digest[0])<<8 | int(digest[1])
	return value%100 < percent
}

func boundedPromptText(text string, maxRunes int) string {
	text = strings.TrimSpace(text)
	if maxRunes <= 0 || len([]rune(text)) <= maxRunes {
		return text
	}
	runes := []rune(text)
	head := maxRunes / 2
	tail := maxRunes - head
	return string(runes[:head]) + "\n…\n" + string(runes[len(runes)-tail:])
}

func sidecarFailure(verdict promptfilter.Verdict, failClosed bool, err error) promptfilter.Verdict {
	verdict.ReviewError = "sidecar: " + err.Error()
	if failClosed {
		verdict.Action = promptfilter.ActionBlock
	}
	return verdict
}
