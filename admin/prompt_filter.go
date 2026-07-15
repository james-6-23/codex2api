package admin

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

type promptFilterSecretRequest struct {
	Secret string `json:"secret"`
}

func maskPromptFilterSecret(secret string) string {
	if secret == "" {
		return ""
	}
	if len(secret) < 12 {
		return "********"
	}
	return secret[:6] + "…" + secret[len(secret)-6:]
}

func (h *Handler) promptFilterSecretStatus(c *gin.Context, reveal string) {
	dbSecret, _ := h.db.GetPromptFilterNewAPISecret(c.Request.Context())
	envSecret := strings.TrimSpace(os.Getenv("PROMPT_FILTER_NEWAPI_SECRET"))
	effective, source := dbSecret, "database"
	if envSecret != "" {
		effective, source = envSecret, "environment"
	}
	if effective == "" {
		source = "none"
	}
	c.JSON(http.StatusOK, gin.H{"configured": effective != "", "source": source, "masked": maskPromptFilterSecret(effective), "secret": reveal})
}

func (h *Handler) GetPromptFilterNewAPISecretStatus(c *gin.Context) {
	h.promptFilterSecretStatus(c, "")
}

func (h *Handler) GeneratePromptFilterNewAPISecret(c *gin.Context) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		writeError(c, http.StatusInternalServerError, "生成随机密钥失败")
		return
	}
	h.savePromptFilterNewAPISecret(c, hex.EncodeToString(buf))
}

func (h *Handler) ReplacePromptFilterNewAPISecret(c *gin.Context) {
	var req promptFilterSecretRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求格式无效")
		return
	}
	h.savePromptFilterNewAPISecret(c, strings.TrimSpace(req.Secret))
}

func (h *Handler) savePromptFilterNewAPISecret(c *gin.Context, secret string) {
	if len(secret) < 32 {
		writeError(c, http.StatusBadRequest, "共享密钥至少需要 32 个字符")
		return
	}
	h.settingsUpdateMu.Lock()
	defer h.settingsUpdateMu.Unlock()
	if err := h.db.SetPromptFilterNewAPISecret(c.Request.Context(), secret); err != nil {
		writeError(c, http.StatusInternalServerError, "保存共享密钥失败")
		return
	}
	cfg := h.store.GetPromptFilterConfig()
	cfg.Advanced.NewAPI.Secret = secret
	h.store.SetPromptFilterConfig(cfg)
	h.promptFilterSecretStatus(c, secret)
}

type promptFilterLogsResponse struct {
	Logs     []*database.PromptFilterLog `json:"logs"`
	Total    int                         `json:"total"`
	Page     int                         `json:"page"`
	PageSize int                         `json:"page_size"`
}

type promptFilterTestRequest struct {
	Text     string `json:"text"`
	Endpoint string `json:"endpoint"`
	Model    string `json:"model"`
}

type promptFilterTestResponse struct {
	Verdict promptfilter.Verdict `json:"verdict"`
}

type promptFilterRulePatternTestRequest struct {
	Pattern string `json:"pattern"`
	Text    string `json:"text"`
}

type promptFilterRulePatternTestResponse struct {
	Matched bool   `json:"matched"`
	Error   string `json:"error,omitempty"`
}

type promptFilterRuleItem struct {
	Name     string `json:"name"`
	Pattern  string `json:"pattern"`
	Weight   int    `json:"weight"`
	Category string `json:"category,omitempty"`
	Strict   bool   `json:"strict,omitempty"`
	Enabled  bool   `json:"enabled"`
	Builtin  bool   `json:"builtin"`
}

type promptFilterRulesResponse struct {
	BuiltinPatterns  []promptFilterRuleItem       `json:"builtin_patterns"`
	CustomPatterns   []promptfilter.PatternConfig `json:"custom_patterns"`
	DisabledPatterns []string                     `json:"disabled_patterns"`
}

func (h *Handler) inspectImageStudioPromptFilter(c *gin.Context, text string, model string, keyID int64, keyName string, keyMasked string) bool {
	return h.inspectImagePromptFilter(c, text, model, keyID, keyName, keyMasked, "/api/admin/images/jobs", nil, false)
}

func (h *Handler) inspectImagePromptFilter(c *gin.Context, text string, model string, keyID int64, keyName string, keyMasked string, endpoint string, writeBlock func(*gin.Context), redactPreview bool) bool {
	if h == nil || h.store == nil {
		return false
	}
	cfg := h.store.GetPromptFilterConfig()
	verdict := promptfilter.InspectText(text, cfg)
	if shouldReviewPromptFilterVerdict(verdict, cfg) {
		verdict = reviewPromptFilterVerdict(c.Request.Context(), text, verdict, cfg)
	}
	if verdict.Action == promptfilter.ActionWarn {
		c.Header("X-Prompt-Filter-Warning", verdict.Reason)
		return false
	}
	if verdict.Action != promptfilter.ActionBlock {
		return false
	}
	textPreview := promptfilter.RedactedPreview(verdict.TextPreview, 500)
	if redactPreview {
		textPreview = "[redacted]"
	}
	h.recordPromptFilterLog(c, &database.PromptFilterLogInput{
		Source:          "local_filter",
		Endpoint:        endpoint,
		Model:           model,
		Action:          verdict.Action,
		Mode:            verdict.Mode,
		Score:           verdict.Score,
		Threshold:       verdict.Threshold,
		MatchedPatterns: promptfilter.MatchesJSON(verdict.Matched),
		TextPreview:     textPreview,
		APIKeyID:        keyID,
		APIKeyName:      keyName,
		APIKeyMasked:    keyMasked,
		ClientIP:        c.ClientIP(),
		ReviewModel:     verdict.ReviewModel,
		ReviewFlagged:   verdict.ReviewFlagged,
		ReviewError:     verdict.ReviewError,
	})
	if writeBlock != nil {
		writeBlock(c)
	} else {
		writeError(c, http.StatusBadRequest, "Prompt 被检查规则拦截")
	}
	return true
}

func (h *Handler) recordPromptFilterLog(c *gin.Context, input *database.PromptFilterLogInput) {
	if h == nil || h.db == nil || input == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_ = h.db.InsertPromptFilterLog(ctx, input)
}

func (h *Handler) ListPromptFilterLogs(c *gin.Context) {
	page := positiveQueryInt(c, "page", 1)
	pageSize := positiveQueryInt(c, "page_size", positiveQueryInt(c, "limit", 100))
	apiKeyID := int64(0)
	if raw := strings.TrimSpace(c.Query("api_key_id")); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil && parsed > 0 {
			apiKeyID = parsed
		}
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	logs, total, err := h.db.ListPromptFilterLogsPage(ctx, database.PromptFilterLogQuery{
		Page:     page,
		PageSize: pageSize,
		Source:   c.Query("source"),
		Action:   c.Query("action"),
		Endpoint: c.Query("endpoint"),
		Model:    c.Query("model"),
		APIKeyID: apiKeyID,
		Query:    c.Query("q"),
	})
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if logs == nil {
		logs = []*database.PromptFilterLog{}
	}
	c.JSON(http.StatusOK, promptFilterLogsResponse{Logs: logs, Total: total, Page: page, PageSize: pageSize})
}

func (h *Handler) ClearPromptFilterLogs(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	if err := h.db.ClearPromptFilterLogs(ctx); err != nil {
		writeInternalError(c, err)
		return
	}
	writeMessage(c, http.StatusOK, "Prompt 检查日志已清空")
}

// MatchPromptFilterLog 按时间/端点/APIKey 找到与某次请求最接近的一条提示词过滤日志，
// 用于「使用统计」里点击 cyber_policy 报错时查看触发的完整请求内容。
// GET /api/prompt-filter/logs/match?at=<RFC3339>&endpoint=&api_key_id=&source=
func (h *Handler) MatchPromptFilterLog(c *gin.Context) {
	atRaw := strings.TrimSpace(c.Query("at"))
	if atRaw == "" {
		writeError(c, http.StatusBadRequest, "缺少 at 参数")
		return
	}
	at, err := time.Parse(time.RFC3339, atRaw)
	if err != nil {
		writeError(c, http.StatusBadRequest, "at 参数格式无效（需 RFC3339）")
		return
	}
	source := strings.TrimSpace(c.Query("source"))
	if source == "" {
		source = "upstream_cyber_policy"
	}
	apiKeyID := int64(0)
	if raw := strings.TrimSpace(c.Query("api_key_id")); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil && parsed > 0 {
			apiKeyID = parsed
		}
	}
	windowSeconds := positiveQueryInt(c, "window", 15)

	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	log, err := h.db.FindNearestPromptFilterLog(ctx, at, source, strings.TrimSpace(c.Query("endpoint")), apiKeyID, windowSeconds)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"found": log != nil, "log": log})
}

func (h *Handler) TestPromptFilter(c *gin.Context) {
	var req promptFilterTestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求体无效")
		return
	}
	req.Text = strings.TrimSpace(req.Text)
	if req.Text == "" {
		writeError(c, http.StatusBadRequest, "text 不能为空")
		return
	}
	if len([]rune(req.Text)) > 20000 {
		writeError(c, http.StatusBadRequest, "text 不能超过 20000 个字符")
		return
	}
	cfg := h.store.GetPromptFilterConfig()
	cfg.Enabled = true
	verdict := promptfilter.InspectText(req.Text, cfg)
	if shouldReviewPromptFilterVerdict(verdict, cfg) {
		verdict = reviewPromptFilterVerdict(c.Request.Context(), req.Text, verdict, cfg)
	}
	c.JSON(http.StatusOK, promptFilterTestResponse{Verdict: verdict})
}

func (h *Handler) TestPromptFilterRulePattern(c *gin.Context) {
	var req promptFilterRulePatternTestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求体无效")
		return
	}
	trimmedPattern := strings.TrimSpace(req.Pattern)
	if trimmedPattern == "" {
		writeError(c, http.StatusBadRequest, "pattern 不能为空")
		return
	}
	if req.Text == "" {
		writeError(c, http.StatusBadRequest, "text 不能为空")
		return
	}
	if len([]rune(req.Pattern)) > 5000 {
		writeError(c, http.StatusBadRequest, "pattern 不能超过 5000 个字符")
		return
	}
	if len([]rune(req.Text)) > 20000 {
		writeError(c, http.StatusBadRequest, "text 不能超过 20000 个字符")
		return
	}
	re, err := regexp.Compile(req.Pattern)
	if err != nil {
		c.JSON(http.StatusOK, promptFilterRulePatternTestResponse{Matched: false, Error: err.Error()})
		return
	}
	c.JSON(http.StatusOK, promptFilterRulePatternTestResponse{Matched: re.MatchString(req.Text)})
}

func (h *Handler) GetPromptFilterRules(c *gin.Context) {
	cfg := h.store.GetPromptFilterConfig()
	disabled := map[string]bool{}
	for _, name := range cfg.DisabledPatterns {
		disabled[strings.ToLower(strings.TrimSpace(name))] = true
	}
	builtin := promptfilter.BuiltinPatternConfigs()
	items := make([]promptFilterRuleItem, 0, len(builtin))
	for _, pattern := range builtin {
		items = append(items, promptFilterRuleItem{
			Name:     pattern.Name,
			Pattern:  pattern.Pattern,
			Weight:   pattern.Weight,
			Category: pattern.Category,
			Strict:   pattern.Strict,
			Enabled:  !disabled[strings.ToLower(strings.TrimSpace(pattern.Name))],
			Builtin:  true,
		})
	}
	c.JSON(http.StatusOK, promptFilterRulesResponse{
		BuiltinPatterns:  items,
		CustomPatterns:   cfg.CustomPatterns,
		DisabledPatterns: cfg.DisabledPatterns,
	})
}

func positiveQueryInt(c *gin.Context, key string, fallback int) int {
	raw := strings.TrimSpace(c.Query(key))
	if raw == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}

func shouldReviewPromptFilterVerdict(verdict promptfilter.Verdict, cfg promptfilter.Config) bool {
	if verdict.TerminalStrictHit {
		return false
	}
	if verdict.Action != promptfilter.ActionWarn && verdict.Action != promptfilter.ActionBlock {
		return false
	}
	return promptfilter.NormalizeReviewConfig(cfg.Review).Ready()
}

func reviewPromptFilterVerdict(ctx context.Context, text string, verdict promptfilter.Verdict, cfg promptfilter.Config) promptfilter.Verdict {
	flagged, model, err := promptfilter.DefaultReviewClient.ReviewText(ctx, text, cfg.Review)
	return promptfilter.ApplyReviewResult(verdict, flagged, model, err, cfg.Review)
}
