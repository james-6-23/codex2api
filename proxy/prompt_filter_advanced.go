package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
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
	Categories []string `json:"categories"`
	Reason     string   `json:"reason"`
}

func (h *Handler) applyAdvancedPromptProtection(c *gin.Context, text string, verdict promptfilter.Verdict, cfg promptfilter.Config) promptfilter.Verdict {
	if verdict.TerminalStrictHit {
		return verdict
	}
	if cfg.Advanced.Risk.Enabled {
		verdict = h.applyPromptRisk(c, verdict, cfg)
	}
	shouldCallSidecar := verdict.Score >= cfg.Advanced.Sidecar.MinScore
	if cfg.Advanced.Risk.Enabled && verdict.Score >= cfg.Advanced.Risk.ReviewThreshold {
		shouldCallSidecar = true
	}
	if cfg.Advanced.Sidecar.Enabled && shouldCallSidecar {
		verdict = applyPromptSidecar(c.Request.Context(), text, verdict, cfg)
	}
	return verdict
}

func (h *Handler) applyPromptRisk(c *gin.Context, verdict promptfilter.Verdict, cfg promptfilter.Config) promptfilter.Verdict {
	if h == nil || h.cache == nil || c == nil {
		return verdict
	}
	risk := cfg.Advanced.Risk
	ttl := time.Duration(risk.WindowSeconds) * time.Second
	keys := []struct {
		key    string
		weight int
	}{
		{fmt.Sprintf("user:%d", requestAPIKeyID(c)), risk.UserWeightPercent},
		{"ip:" + hashRiskIdentity(c.ClientIP()), risk.IPWeightPercent},
		{"session:" + hashRiskIdentity(promptSessionID(c)), risk.SessionWeightPercent},
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
		record.Score += verdict.RawScore
		record.UpdatedAt = now
		if raw, err := json.Marshal(record); err == nil {
			_ = h.cache.SetRuntime(c.Request.Context(), promptRiskNamespace, item.key, raw, ttl)
		}
		unlock()
	}
	if effective > verdict.Score {
		verdict.Score = effective
		verdict.Reason = fmt.Sprintf("%s; cumulative risk=%d", verdict.Reason, effective)
	}
	if effective >= risk.BlockThreshold {
		verdict.Action = promptfilter.ActionBlock
	}
	return verdict
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
	sc := cfg.Advanced.Sidecar
	baseURL := strings.TrimSpace(sc.BaseURL)
	if baseURL == "" {
		return sidecarFailure(verdict, sc.FailClosed, fmt.Errorf("sidecar base URL is empty"))
	}
	endpoint := strings.TrimRight(baseURL, "/") + "/v1/guard/check"
	payload, _ := json.Marshal(sidecarRequest{Direction: "input", Text: text, LocalScore: verdict.Score, MatchedRules: verdict.Matched})
	requestCtx, cancel := context.WithTimeout(ctx, time.Duration(sc.TimeoutSeconds)*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(requestCtx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return sidecarFailure(verdict, sc.FailClosed, err)
	}
	req.Header.Set("Content-Type", "application/json")
	if key := strings.TrimSpace(os.Getenv("PROMPT_FILTER_SIDECAR_API_KEY")); key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return sidecarFailure(verdict, sc.FailClosed, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return sidecarFailure(verdict, sc.FailClosed, fmt.Errorf("sidecar status %d", resp.StatusCode))
	}
	var result sidecarResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return sidecarFailure(verdict, sc.FailClosed, err)
	}
	if result.Score > verdict.Score {
		verdict.Score = result.Score
	}
	if strings.EqualFold(result.Action, promptfilter.ActionBlock) {
		verdict.Action = promptfilter.ActionBlock
	}
	if strings.EqualFold(result.Action, promptfilter.ActionWarn) && verdict.Action == promptfilter.ActionAllow {
		verdict.Action = promptfilter.ActionWarn
	}
	if result.Reason != "" {
		verdict.Reason = strings.TrimSpace(verdict.Reason + "; sidecar: " + result.Reason)
	}
	return verdict
}

func sidecarFailure(verdict promptfilter.Verdict, failClosed bool, err error) promptfilter.Verdict {
	verdict.ReviewError = "sidecar: " + err.Error()
	if failClosed {
		verdict.Action = promptfilter.ActionBlock
	}
	return verdict
}
