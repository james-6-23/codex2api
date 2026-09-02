package admin

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

const promptFilterAuditContextMaxRunes = 2000

type promptFilterLogsResponse struct {
	Logs     []*database.PromptFilterLog `json:"logs"`
	Total    int                         `json:"total"`
	Page     int                         `json:"page"`
	PageSize int                         `json:"page_size"`
}

type promptPolicyIncidentsResponse struct {
	Incidents []*database.PromptPolicyIncident `json:"incidents"`
	Total     int                              `json:"total"`
	Page      int                              `json:"page"`
	PageSize  int                              `json:"page_size"`
}

type promptPolicyIncidentDetailResponse struct {
	Incident  *database.PromptPolicyIncident        `json:"incident"`
	Matches   json.RawMessage                       `json:"matches"`
	Candidate *database.PromptRuleCandidate         `json:"candidate,omitempty"`
	Evidence  *database.PromptRuleCandidateEvidence `json:"evidence,omitempty"`
}

type promptPolicyAuditQueueHealth struct {
	Enqueued      uint64 `json:"enqueued"`
	Completed     uint64 `json:"completed"`
	DroppedHigh   uint64 `json:"dropped_high"`
	DroppedLow    uint64 `json:"dropped_low"`
	Failed        uint64 `json:"failed"`
	PendingHigh   int    `json:"pending_high"`
	PendingLow    int    `json:"pending_low"`
	RetainedBytes int64  `json:"retained_bytes"`
}

type promptPolicyAuditHealthResponse struct {
	OK                      bool                             `json:"ok"`
	Status                  string                           `json:"status"`
	StorageReady            bool                             `json:"storage_ready"`
	PromptFilterEnabled     bool                             `json:"prompt_filter_enabled"`
	ReviewEnabled           bool                             `json:"review_enabled"`
	ReviewFailClosed        bool                             `json:"review_fail_closed"`
	ReviewPool              promptfilter.ReviewKeyPoolHealth `json:"review_pool"`
	ConversationLockEnabled bool                             `json:"conversation_lock_enabled"`
	IncidentCount           int                              `json:"incident_count"`
	LatestIncidentID        string                           `json:"latest_incident_id,omitempty"`
	LatestIncidentAt        *time.Time                       `json:"latest_incident_at,omitempty"`
	Queue                   promptPolicyAuditQueueHealth     `json:"queue"`
}

type promptFilterTestRequest struct {
	Text     string `json:"text"`
	Endpoint string `json:"endpoint"`
	Model    string `json:"model"`
}

type promptFilterTestResponse struct {
	Verdict  promptfilter.Verdict     `json:"verdict"`
	Decision promptfilter.Decision    `json:"decision"`
	Protocol promptfilter.Protocol    `json:"protocol"`
	Provider promptfilter.ModelFamily `json:"provider"`
	Endpoint string                   `json:"endpoint"`
	Model    string                   `json:"model"`
}

type promptReviewTestRequest struct {
	Text                 string             `json:"text"`
	APIKey               string             `json:"api_key"`
	BaseURL              string             `json:"base_url"`
	Model                string             `json:"model"`
	RequestMode          string             `json:"request_mode"`
	SystemPrompt         string             `json:"system_prompt"`
	UserPromptTemplate   string             `json:"user_prompt_template"`
	PayloadTemplate      string             `json:"payload_template"`
	ConfidenceThreshold  float64            `json:"confidence_threshold"`
	ModerationThresholds map[string]float64 `json:"moderation_thresholds"`
	TimeoutSeconds       int                `json:"timeout_seconds"`
	MaxConcurrent        int                `json:"max_concurrent"`
	MaxTextLength        int                `json:"max_text_length"`
	TestAllKeys          bool               `json:"test_all_keys"`
}

type promptReviewKeyTestResult struct {
	KeyIndex             int                `json:"key_index"`
	KeyID                string             `json:"key_id,omitempty"`
	KeyMasked            string             `json:"key_masked,omitempty"`
	OK                   bool               `json:"ok"`
	Endpoint             string             `json:"endpoint,omitempty"`
	Model                string             `json:"model,omitempty"`
	Flagged              bool               `json:"flagged"`
	Confidence           float64            `json:"confidence"`
	Reason               string             `json:"reason,omitempty"`
	HighestCategory      string             `json:"highest_category,omitempty"`
	DecisionCategory     string             `json:"decision_category,omitempty"`
	DecisionScore        float64            `json:"decision_score"`
	DecisionThreshold    float64            `json:"decision_threshold"`
	CategoryScores       map[string]float64 `json:"category_scores,omitempty"`
	ModerationThresholds map[string]float64 `json:"moderation_thresholds,omitempty"`
	LatencyMS            int64              `json:"latency_ms"`
	Error                string             `json:"error,omitempty"`
}

type promptReviewAPIKeyDescriptor struct {
	ID     string `json:"id"`
	Index  int    `json:"index"`
	Masked string `json:"masked"`
}

type promptReviewAPIKeysResponse struct {
	Items []promptReviewAPIKeyDescriptor `json:"items"`
	Count int                            `json:"count"`
}

type promptReviewProfileResponse struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	BaseURL       string    `json:"base_url"`
	Model         string    `json:"model"`
	RequestMode   string    `json:"request_mode"`
	TimeoutSecond int       `json:"timeout_seconds"`
	KeyCount      int       `json:"key_count"`
	Active        bool      `json:"active"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type promptReviewProfilesResponse struct {
	Profiles []promptReviewProfileResponse `json:"profiles"`
}

type promptReviewProfileRequest struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	BaseURL       string `json:"base_url"`
	Model         string `json:"model"`
	RequestMode   string `json:"request_mode"`
	APIKey        string `json:"api_key"`
	AdapterJSON   string `json:"adapter_json"`
	TimeoutSecond int    `json:"timeout_seconds"`
}

func toPromptReviewProfileResponse(profile database.PromptReviewProfile) promptReviewProfileResponse {
	return promptReviewProfileResponse{
		ID: profile.ID, Name: profile.Name, BaseURL: profile.BaseURL, Model: profile.Model,
		RequestMode: profile.RequestMode, TimeoutSecond: profile.TimeoutSecond,
		KeyCount: len((promptfilter.ReviewConfig{APIKey: profile.APIKeys}).APIKeyList()),
		Active:   profile.Active, CreatedAt: profile.CreatedAt, UpdatedAt: profile.UpdatedAt,
	}
}

func promptReviewProfileAdapterJSON(raw string, fallback promptfilter.ReviewAdapterConfig) (string, promptfilter.ReviewAdapterConfig, error) {
	adapter := fallback
	if strings.TrimSpace(raw) != "" {
		if err := json.Unmarshal([]byte(raw), &adapter); err != nil {
			return "", adapter, errors.New("adapter_json 无效")
		}
	}
	adapter = promptfilter.NormalizeReviewAdapterConfig(adapter)
	encoded, err := json.Marshal(adapter)
	if err != nil {
		return "", adapter, err
	}
	return string(encoded), adapter, nil
}

func (h *Handler) ListPromptReviewProfiles(c *gin.Context) {
	if h == nil || h.db == nil {
		writeError(c, http.StatusServiceUnavailable, "Prompt 审核配置不可用")
		return
	}
	profiles, err := h.db.ListPromptReviewProfiles(c.Request.Context())
	if err != nil {
		writeInternalError(c, err)
		return
	}
	items := make([]promptReviewProfileResponse, 0, len(profiles))
	for _, profile := range profiles {
		items = append(items, toPromptReviewProfileResponse(profile))
	}
	c.JSON(http.StatusOK, promptReviewProfilesResponse{Profiles: items})
}

func (h *Handler) SavePromptReviewProfile(c *gin.Context) {
	if h == nil || h.db == nil || h.store == nil {
		writeError(c, http.StatusServiceUnavailable, "Prompt 审核配置不可用")
		return
	}
	var req promptReviewProfileRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求体无效")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" || len([]rune(req.Name)) > 120 {
		writeError(c, http.StatusBadRequest, "档案名称不能为空且不能超过 120 个字符")
		return
	}
	current := h.store.GetPromptFilterConfig().Review
	profileID := strings.TrimSpace(req.ID)
	var existing *database.PromptReviewProfile
	if profileID != "" {
		var err error
		existing, err = h.db.GetPromptReviewProfile(c.Request.Context(), profileID)
		if err != nil {
			writeInternalError(c, err)
			return
		}
	}
	if profileID == "" {
		profileID = uuid.NewString()
	}
	apiKeys := strings.TrimSpace(req.APIKey)
	if apiKeys == "" {
		if existing != nil {
			apiKeys = existing.APIKeys
		} else {
			// 留空表示保存当前已生效的 Key，避免用户为了保存新档案而
			// 必须再次粘贴现有 DeepSeek Key。
			apiKeys = current.APIKey
		}
	}
	baseURL := strings.TrimSpace(req.BaseURL)
	if baseURL == "" && existing != nil {
		baseURL = existing.BaseURL
	}
	model := strings.TrimSpace(req.Model)
	if model == "" && existing != nil {
		model = existing.Model
	}
	requestMode := strings.TrimSpace(req.RequestMode)
	if requestMode == "" && existing != nil {
		requestMode = existing.RequestMode
	}
	if req.TimeoutSecond <= 0 && existing != nil {
		req.TimeoutSecond = existing.TimeoutSecond
	}
	if req.TimeoutSecond <= 0 {
		req.TimeoutSecond = current.TimeoutSeconds
	}
	adapterRaw := req.AdapterJSON
	if strings.TrimSpace(adapterRaw) == "" && existing != nil {
		adapterRaw = existing.AdapterJSON
	}
	if strings.TrimSpace(adapterRaw) == "" {
		adapterRaw, _, _ = promptReviewProfileAdapterJSON("", current.Adapter)
	}
	canonicalAdapter, adapter, err := promptReviewProfileAdapterJSON(adapterRaw, current.Adapter)
	if err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	reviewCfg := current
	reviewCfg.APIKey, reviewCfg.BaseURL, reviewCfg.Model = apiKeys, baseURL, model
	reviewCfg.TimeoutSeconds, reviewCfg.Adapter = req.TimeoutSecond, adapter
	reviewCfg = promptfilter.NormalizeReviewConfig(reviewCfg)
	if err := promptfilter.ValidateReviewConfig(reviewCfg); err != nil {
		writeError(c, http.StatusBadRequest, "Prompt 审核配置无效: "+err.Error())
		return
	}
	profile := database.PromptReviewProfile{ID: profileID, Name: req.Name, BaseURL: reviewCfg.BaseURL, Model: reviewCfg.Model,
		RequestMode: reviewCfg.Adapter.RequestMode, AdapterJSON: canonicalAdapter, APIKeys: reviewCfg.APIKey,
		TimeoutSecond: reviewCfg.TimeoutSeconds, Active: existing != nil && existing.Active}
	if err := h.db.UpsertPromptReviewProfile(c.Request.Context(), profile); err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, toPromptReviewProfileResponse(profile))
}

func (h *Handler) ActivatePromptReviewProfile(c *gin.Context) {
	if h == nil || h.db == nil || h.store == nil {
		writeError(c, http.StatusServiceUnavailable, "Prompt 审核配置不可用")
		return
	}
	profile, err := h.db.GetPromptReviewProfile(c.Request.Context(), strings.TrimSpace(c.Param("profile_id")))
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if profile == nil {
		writeError(c, http.StatusNotFound, "审核配置档案不存在")
		return
	}
	var adapter promptfilter.ReviewAdapterConfig
	if err := json.Unmarshal([]byte(profile.AdapterJSON), &adapter); err != nil {
		writeError(c, http.StatusConflict, "审核配置档案中的适配器配置无效")
		return
	}
	adapter = promptfilter.NormalizeReviewAdapterConfig(adapter)
	h.settingsUpdateMu.Lock()
	defer h.settingsUpdateMu.Unlock()
	settings, err := h.db.GetSystemSettings(c.Request.Context())
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if settings == nil {
		writeError(c, http.StatusNotFound, "系统设置不存在")
		return
	}
	patchRaw, _ := json.Marshal(map[string]any{"review_adapter": adapter})
	document, err := promptfilter.MergeAdvancedConfigDocument(settings.PromptFilterAdvancedConfig, string(patchRaw))
	if err != nil {
		writeError(c, http.StatusConflict, "审核配置档案无法应用: "+err.Error())
		return
	}
	settings.PromptFilterReviewAPIKey = profile.APIKeys
	settings.PromptFilterReviewBaseURL = profile.BaseURL
	settings.PromptFilterReviewModel = profile.Model
	settings.PromptFilterReviewTimeoutSeconds = profile.TimeoutSecond
	settings.PromptFilterAdvancedConfig = document.Raw
	settings.PreservePromptFilterReviewAPIKey = false
	if err := h.db.UpdateSystemSettings(c.Request.Context(), settings); err != nil {
		writeInternalError(c, err)
		return
	}
	runtimeCfg := h.store.GetPromptFilterConfig()
	runtimeCfg.Review.APIKey, runtimeCfg.Review.BaseURL, runtimeCfg.Review.Model = profile.APIKeys, profile.BaseURL, profile.Model
	runtimeCfg.Review.TimeoutSeconds, runtimeCfg.Review.Adapter = profile.TimeoutSecond, adapter
	if err := h.store.SetPromptFilterConfigWithAdvancedRaw(runtimeCfg, document.Raw); err != nil {
		writeInternalError(c, err)
		return
	}
	if err := h.db.SetPromptReviewProfileActive(c.Request.Context(), profile.ID); err != nil {
		writeInternalError(c, err)
		return
	}
	profile.Active = true
	c.JSON(http.StatusOK, toPromptReviewProfileResponse(*profile))
}

func (h *Handler) DeletePromptReviewProfile(c *gin.Context) {
	if h == nil || h.db == nil {
		writeError(c, http.StatusServiceUnavailable, "Prompt 审核配置不可用")
		return
	}
	if err := h.db.DeletePromptReviewProfile(c.Request.Context(), strings.TrimSpace(c.Param("profile_id"))); err != nil {
		writeInternalError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"ok": true})
}

func promptReviewAPIKeyID(key string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(key)))
	return hex.EncodeToString(sum[:])
}

// maskPromptReviewAPIKey 按 rune 切片(字节切会截断多字节字符),且只在
// 前后缀不可能重叠、多数字符仍被遮蔽时(≥12 字符)才展示明文片段。
func maskPromptReviewAPIKey(key string) string {
	runes := []rune(strings.TrimSpace(key))
	if len(runes) < 12 {
		return "••••"
	}
	return string(runes[:3]) + "••••" + string(runes[len(runes)-4:])
}

func promptReviewAPIKeyDescriptors(keys []string) []promptReviewAPIKeyDescriptor {
	items := make([]promptReviewAPIKeyDescriptor, 0, len(keys))
	for index, key := range keys {
		items = append(items, promptReviewAPIKeyDescriptor{
			ID: promptReviewAPIKeyID(key), Index: index + 1, Masked: maskPromptReviewAPIKey(key),
		})
	}
	return items
}

type promptReviewTestResponse struct {
	OK                   bool                        `json:"ok"`
	Endpoint             string                      `json:"endpoint"`
	Model                string                      `json:"model"`
	Flagged              bool                        `json:"flagged"`
	Confidence           float64                     `json:"confidence"`
	ConfidenceThreshold  float64                     `json:"confidence_threshold"`
	Reason               string                      `json:"reason,omitempty"`
	HighestCategory      string                      `json:"highest_category,omitempty"`
	DecisionCategory     string                      `json:"decision_category,omitempty"`
	DecisionScore        float64                     `json:"decision_score"`
	DecisionThreshold    float64                     `json:"decision_threshold"`
	CategoryScores       map[string]float64          `json:"category_scores,omitempty"`
	ModerationThresholds map[string]float64          `json:"moderation_thresholds,omitempty"`
	LatencyMS            int64                       `json:"latency_ms"`
	KeyCount             int                         `json:"key_count,omitempty"`
	Results              []promptReviewKeyTestResult `json:"results,omitempty"`
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
		verdict = promptfilter.ApplyReviewMode(verdict, cfg.Mode)
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
		MatchContext:    promptfilter.RedactedPreview(verdict.MatchContext, promptFilterAuditContextMaxRunes),
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
	priority := database.PromptFilterLogPriorityLow
	if input.Action == promptfilter.ActionWarn || input.Action == promptfilter.ActionBlock {
		priority = database.PromptFilterLogPriorityHigh
	}
	_ = h.db.EnqueuePromptFilterLog(input, priority)
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
		Page:                page,
		PageSize:            pageSize,
		Source:              c.Query("source"),
		Action:              c.Query("action"),
		Endpoint:            c.Query("endpoint"),
		Model:               c.Query("model"),
		APIKeyID:            apiKeyID,
		Query:               c.Query("q"),
		ReviewState:         c.Query("reviewed"),
		ReviewResult:        c.Query("review_result"),
		ExcludeIntelligence: true,
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
	var err error
	message := "Prompt 检查日志已清空；风险画像和上游 CY 事件已保留"
	source := strings.ToLower(strings.TrimSpace(c.Query("source")))
	if source != "" {
		if source != "local_filter" {
			writeError(c, http.StatusBadRequest, "source 仅支持 local_filter")
			return
		}
		err = h.db.ClearPromptFilterLogsBySource(ctx, source)
		message = "本地过滤与异步审计日志已清空；风险画像已保留"
	} else {
		switch strings.ToLower(strings.TrimSpace(c.Query("reviewed"))) {
		case "":
			err = h.db.ClearPromptFilterLogs(ctx)
		case "true", "reviewed":
			err = h.db.ClearPromptFilterLogsByReviewStatus(ctx, true)
			message = "外部模型复核历史已清空；风险画像已保留"
		case "false", "not_reviewed":
			err = h.db.ClearPromptFilterLogsByReviewStatus(ctx, false)
			message = "本地过滤与异步审计日志已清空；风险画像已保留"
		default:
			writeError(c, http.StatusBadRequest, "reviewed 必须为 true 或 false")
			return
		}
	}
	if err != nil {
		writeInternalError(c, err)
		return
	}
	writeMessage(c, http.StatusOK, message)
}

func (h *Handler) ClearPromptPolicyIncidents(c *gin.Context) {
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	if err := h.db.ClearPromptPolicyIncidents(ctx); err != nil {
		writeInternalError(c, err)
		return
	}
	writeMessage(c, http.StatusOK, "上游 CY 事件已清空；风险画像已保留")
}

func (h *Handler) DeletePromptPolicyIncident(c *gin.Context) {
	incidentID := strings.TrimSpace(c.Param("incident_id"))
	if incidentID == "" {
		writeError(c, http.StatusBadRequest, "incident_id 不能为空")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Second)
	defer cancel()
	if err := h.db.DeletePromptPolicyIncident(ctx, incidentID); errors.Is(err, sql.ErrNoRows) {
		writeError(c, http.StatusNotFound, "CY 事件不存在或已删除")
		return
	} else if err != nil {
		writeInternalError(c, err)
		return
	}
	writeMessage(c, http.StatusOK, "CY 事件已删除；风险画像和学习证据已保留")
}

func (h *Handler) GetPromptPolicyAuditHealth(c *gin.Context) {
	if h == nil || h.db == nil {
		writeError(c, http.StatusServiceUnavailable, "CY 审计存储不可用")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	incidents, total, err := h.db.ListPromptPolicyIncidentsPage(ctx, database.PromptPolicyIncidentQuery{Page: 1, PageSize: 1})
	if err != nil {
		writeInternalError(c, err)
		return
	}
	stats := h.db.PromptFilterAuditStats()
	cfg := promptfilter.DefaultConfig()
	if h.store != nil {
		cfg = h.store.GetPromptFilterConfig()
	}
	response := promptPolicyAuditHealthResponse{
		OK: true, Status: "healthy", StorageReady: true,
		PromptFilterEnabled: cfg.Enabled, ReviewEnabled: cfg.Review.Enabled,
		ReviewFailClosed: cfg.Review.FailClosed, ReviewPool: promptfilter.ReviewKeyPoolStatus(cfg.Review),
		ConversationLockEnabled: cfg.Advanced.Enforcement.ConversationLockEnabled,
		IncidentCount:           total,
		Queue: promptPolicyAuditQueueHealth{
			Enqueued: stats.Enqueued, Completed: stats.Completed, DroppedHigh: stats.DroppedHigh,
			DroppedLow: stats.DroppedLow, Failed: stats.Failed, PendingHigh: stats.PendingHigh,
			PendingLow: stats.PendingLow, RetainedBytes: stats.RetainedBytes,
		},
	}
	// 主动关闭的功能不算故障（面板上已有独立的开关状态展示）；只有真实异常才降级：
	// 审计队列丢高优/写入失败，或审查已启用但 key 池空配置/全冷却。
	reviewPoolFault := response.ReviewEnabled && (response.ReviewPool.Configured == 0 || response.ReviewPool.Available == 0)
	if reviewPoolFault || stats.DroppedHigh > 0 || stats.Failed > 0 {
		response.OK = false
		response.Status = "degraded"
	}
	if len(incidents) > 0 && incidents[0] != nil {
		response.LatestIncidentID = incidents[0].IncidentID
		latest := incidents[0].CreatedAt
		response.LatestIncidentAt = &latest
	}
	c.JSON(http.StatusOK, response)
}

func (h *Handler) ListPromptPolicyIncidents(c *gin.Context) {
	page := positiveQueryInt(c, "page", 1)
	pageSize := positiveQueryInt(c, "page_size", positiveQueryInt(c, "limit", 20))
	apiKeyID := int64(0)
	if raw := strings.TrimSpace(c.Query("api_key_id")); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil && parsed > 0 {
			apiKeyID = parsed
		}
	}
	accountID := int64(0)
	if raw := strings.TrimSpace(c.Query("account_id")); raw != "" {
		if parsed, err := strconv.ParseInt(raw, 10, 64); err == nil && parsed > 0 {
			accountID = parsed
		}
	}
	var localMiss *bool
	if raw := strings.TrimSpace(c.Query("local_miss")); raw != "" {
		if parsed, err := strconv.ParseBool(raw); err == nil {
			localMiss = &parsed
		}
	}
	evaluationState := strings.TrimSpace(c.Query("evaluation_state"))
	if evaluationState == "" {
		evaluationState = strings.TrimSpace(c.Query("status"))
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	incidents, total, err := h.db.ListPromptPolicyIncidentsPage(ctx, database.PromptPolicyIncidentQuery{
		Page: page, PageSize: pageSize, Endpoint: c.Query("endpoint"), Model: c.Query("model"), APIKeyID: apiKeyID, AccountID: accountID,
		EvaluationState: evaluationState, Outcome: c.Query("outcome"), LocalComparison: c.Query("local_comparison"), LocalMiss: localMiss, Query: c.Query("q"),
	})
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if incidents == nil {
		incidents = []*database.PromptPolicyIncident{}
	}
	for _, incident := range incidents {
		h.enrichPromptPolicyIncidentRouting(incident)
	}
	c.JSON(http.StatusOK, promptPolicyIncidentsResponse{Incidents: incidents, Total: total, Page: page, PageSize: pageSize})
}

func (h *Handler) GetPromptPolicyIncident(c *gin.Context) {
	incidentID := strings.TrimSpace(c.Param("incident_id"))
	if incidentID == "" {
		writeError(c, http.StatusBadRequest, "缺少 incident_id")
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	incident, err := h.db.GetPromptPolicyIncident(ctx, incidentID)
	if errors.Is(err, sql.ErrNoRows) {
		writeError(c, http.StatusNotFound, "CY 事件不存在")
		return
	}
	if err != nil {
		writeInternalError(c, err)
		return
	}
	h.enrichPromptPolicyIncidentRouting(incident)
	matches := json.RawMessage(incident.LocalMatchedPatterns)
	if !json.Valid(matches) {
		matches = json.RawMessage("[]")
	}
	response := promptPolicyIncidentDetailResponse{Incident: incident, Matches: matches}
	if incident.CandidateID > 0 {
		if candidate, candidateErr := h.db.GetPromptRuleCandidate(ctx, incident.CandidateID); candidateErr == nil {
			response.Candidate = candidate
		}
		if incident.CandidateEvidenceID > 0 {
			if item, evidenceErr := h.db.GetPromptRuleCandidateEvidence(ctx, incident.CandidateEvidenceID); evidenceErr == nil && item.CandidateID == incident.CandidateID {
				response.Evidence = item
			}
		}
	}
	c.JSON(http.StatusOK, response)
}

func (h *Handler) enrichPromptPolicyIncidentRouting(incident *database.PromptPolicyIncident) {
	if incident == nil {
		return
	}
	hasEventSnapshot := strings.TrimSpace(incident.AccountName) != "" ||
		strings.TrimSpace(incident.AccountPlatform) != "" ||
		len(incident.AccountGroupIDs) > 0 || len(incident.AccountGroupNames) > 0 ||
		len(incident.APIKeyAllowedGroupIDs) > 0 || len(incident.APIKeyAllowedGroupNames) > 0
	if hasEventSnapshot {
		incident.RoutingSnapshotState = "event_snapshot"
		return
	}
	if h == nil || h.store == nil {
		incident.RoutingSnapshotState = "unavailable"
		return
	}

	inferred := false
	if incident.AccountID > 0 {
		if account := h.store.FindByID(incident.AccountID); account != nil {
			account.Mu().RLock()
			incident.AccountName = strings.TrimSpace(account.Email)
			incident.AccountPlatform = strings.TrimSpace(account.UpstreamType)
			account.Mu().RUnlock()
			if incident.AccountPlatform == "" {
				incident.AccountPlatform = database.UpstreamChannelCodex
			}
			incident.AccountGroupIDs = account.GroupIDSnapshot()
			incident.AccountGroupNames = h.store.ResolveGroupNames(incident.AccountGroupIDs)
			inferred = true
		}
	}
	if incident.APIKeyID > 0 {
		incident.APIKeyAllowedGroupIDs = h.store.GetAPIKeyAllowedGroups(incident.APIKeyID)
		incident.APIKeyAllowedGroupNames = h.store.ResolveGroupNames(incident.APIKeyAllowedGroupIDs)
		if len(incident.APIKeyAllowedGroupIDs) > 0 {
			inferred = true
		}
	}
	if inferred {
		incident.RoutingSnapshotState = "current_inferred"
		return
	}
	incident.RoutingSnapshotState = "unavailable"
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
	c.JSON(http.StatusOK, gin.H{"found": log != nil, "log": log, "legacy_inferred": log != nil})
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
	evaluator := h.imageProxy
	if evaluator == nil {
		evaluator = proxy.NewHandler(nil, nil, nil, nil)
	}
	result := evaluator.EvaluatePromptGuardTextForTest(c, cfg, req.Text, req.Endpoint, req.Model)
	c.JSON(http.StatusOK, promptFilterTestResponse{
		Verdict:  result.Verdict,
		Decision: result.Decision,
		Protocol: result.Protocol,
		Provider: result.Provider,
		Endpoint: result.Endpoint,
		Model:    result.Model,
	})
}

type promptReviewModelsRequest struct {
	BaseURL        string `json:"base_url"`
	APIKey         string `json:"api_key"`
	TimeoutSeconds int    `json:"timeout_seconds"`
}

// ListPromptReviewModels 从审查供应商拉取 OpenAI 兼容的模型列表,供前端选择审查模型。
// base_url/api_key 允许携带表单草稿值覆盖已存配置,便于保存前先拉列表。
func (h *Handler) ListPromptReviewModels(c *gin.Context) {
	if h == nil || h.store == nil {
		writeError(c, http.StatusServiceUnavailable, "Prompt 审核配置不可用")
		return
	}
	var req promptReviewModelsRequest
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(c, http.StatusBadRequest, "请求体无效")
		return
	}
	reviewCfg := h.store.GetPromptFilterConfig().Review
	if key := strings.TrimSpace(req.APIKey); key != "" {
		reviewCfg.APIKey = key
	}
	if baseURL := strings.TrimSpace(req.BaseURL); baseURL != "" {
		reviewCfg.BaseURL = baseURL
	}
	if req.TimeoutSeconds > 0 {
		reviewCfg.TimeoutSeconds = req.TimeoutSeconds
	}
	if len(promptfilter.NormalizeReviewConfig(reviewCfg).APIKeyList()) == 0 {
		writeError(c, http.StatusBadRequest, "请先配置审查 API Key")
		return
	}
	models, endpoint, err := promptfilter.DefaultReviewClient.ListReviewModels(c.Request.Context(), reviewCfg)
	if err != nil {
		writeError(c, http.StatusBadGateway, "拉取模型列表失败: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"endpoint": endpoint, "models": models})
}

func (h *Handler) TestPromptReviewConnection(c *gin.Context) {
	var req promptReviewTestRequest
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
	if h == nil || h.store == nil {
		writeError(c, http.StatusServiceUnavailable, "Prompt 审核配置不可用")
		return
	}
	reviewCfg := h.store.GetPromptFilterConfig().Review
	reviewCfg.Enabled = true
	if key := strings.TrimSpace(req.APIKey); key != "" {
		reviewCfg.APIKey = key
	}
	if baseURL := strings.TrimSpace(req.BaseURL); baseURL != "" {
		reviewCfg.BaseURL = baseURL
	}
	if model := strings.TrimSpace(req.Model); model != "" {
		reviewCfg.Model = model
	}
	if req.TimeoutSeconds > 0 {
		reviewCfg.TimeoutSeconds = req.TimeoutSeconds
	}
	reviewCfg.Adapter = promptfilter.ReviewAdapterConfig{
		RequestMode:          req.RequestMode,
		SystemPrompt:         req.SystemPrompt,
		UserPromptTemplate:   req.UserPromptTemplate,
		PayloadTemplate:      req.PayloadTemplate,
		ConfidenceThreshold:  req.ConfidenceThreshold,
		ModerationThresholds: req.ModerationThresholds,
		MaxConcurrent:        req.MaxConcurrent,
		MaxTextLength:        req.MaxTextLength,
	}
	reviewCfg = promptfilter.NormalizeReviewConfig(reviewCfg)
	if err := promptfilter.ValidateReviewConfig(reviewCfg); err != nil {
		writeError(c, http.StatusBadRequest, "Prompt 审核配置无效: "+err.Error())
		return
	}
	keys := reviewCfg.APIKeyList()
	if req.TestAllKeys && len(keys) > 1 {
		type indexedResult struct {
			index   int
			outcome promptfilter.ReviewOutcome
			latency int64
			err     error
		}
		resultCh := make(chan indexedResult, len(keys))
		started := time.Now()
		limit := reviewCfg.Adapter.MaxConcurrent
		if limit <= 0 || limit > len(keys) {
			limit = len(keys)
		}
		slots := make(chan struct{}, limit)
		for index, key := range keys {
			go func(index int, key string) {
				slots <- struct{}{}
				defer func() { <-slots }()
				keyCfg := reviewCfg
				keyCfg.APIKey = key
				keyStarted := time.Now()
				outcome, testErr := promptfilter.DefaultReviewClient.ReviewTextDetailed(c.Request.Context(), req.Text, keyCfg)
				resultCh <- indexedResult{index: index, outcome: outcome, latency: time.Since(keyStarted).Milliseconds(), err: testErr}
			}(index, key)
		}
		results := make([]promptReviewKeyTestResult, len(keys))
		descriptors := promptReviewAPIKeyDescriptors(keys)
		allOK := true
		var first promptfilter.ReviewOutcome
		for range keys {
			item := <-resultCh
			if item.index == 0 {
				first = item.outcome
			}
			result := promptReviewKeyTestResult{
				KeyIndex: item.index + 1, KeyID: descriptors[item.index].ID, KeyMasked: descriptors[item.index].Masked,
				OK: item.err == nil, Flagged: item.outcome.Flagged,
				Endpoint: item.outcome.Endpoint, Model: item.outcome.Model, Confidence: item.outcome.Confidence,
				Reason: item.outcome.Reason, HighestCategory: item.outcome.HighestCategory,
				DecisionCategory: item.outcome.DecisionCategory, DecisionScore: item.outcome.DecisionScore,
				DecisionThreshold: item.outcome.DecisionThreshold,
				CategoryScores:    item.outcome.CategoryScores, ModerationThresholds: item.outcome.ModerationThresholds,
				LatencyMS: item.latency,
			}
			if item.err != nil {
				allOK = false
				result.Error = item.err.Error()
			}
			results[item.index] = result
		}
		c.JSON(http.StatusOK, promptReviewTestResponse{
			OK: allOK, Endpoint: first.Endpoint, Model: reviewCfg.Model, Flagged: first.Flagged,
			Confidence: first.Confidence, ConfidenceThreshold: reviewCfg.Adapter.ConfidenceThreshold,
			Reason: first.Reason, HighestCategory: first.HighestCategory,
			DecisionCategory: first.DecisionCategory, DecisionScore: first.DecisionScore,
			DecisionThreshold: first.DecisionThreshold,
			CategoryScores:    first.CategoryScores, ModerationThresholds: first.ModerationThresholds,
			LatencyMS: time.Since(started).Milliseconds(), KeyCount: len(keys), Results: results,
		})
		return
	}
	started := time.Now()
	outcome, err := promptfilter.DefaultReviewClient.ReviewTextDetailed(c.Request.Context(), req.Text, reviewCfg)
	if err != nil {
		writeError(c, http.StatusBadGateway, "Prompt 审核连接测试失败: "+err.Error())
		return
	}
	c.JSON(http.StatusOK, promptReviewTestResponse{
		OK:                   true,
		Endpoint:             outcome.Endpoint,
		Model:                outcome.Model,
		Flagged:              outcome.Flagged,
		Confidence:           outcome.Confidence,
		ConfidenceThreshold:  reviewCfg.Adapter.ConfidenceThreshold,
		Reason:               outcome.Reason,
		HighestCategory:      outcome.HighestCategory,
		DecisionCategory:     outcome.DecisionCategory,
		DecisionScore:        outcome.DecisionScore,
		DecisionThreshold:    outcome.DecisionThreshold,
		CategoryScores:       outcome.CategoryScores,
		ModerationThresholds: outcome.ModerationThresholds,
		LatencyMS:            time.Since(started).Milliseconds(),
		KeyCount:             len(keys),
	})
}

func (h *Handler) ListPromptReviewAPIKeys(c *gin.Context) {
	if h == nil || h.store == nil {
		writeError(c, http.StatusServiceUnavailable, "Prompt 审核配置不可用")
		return
	}
	items := promptReviewAPIKeyDescriptors(h.store.GetPromptFilterConfig().Review.APIKeyList())
	c.JSON(http.StatusOK, promptReviewAPIKeysResponse{Items: items, Count: len(items)})
}

func (h *Handler) DeletePromptReviewAPIKey(c *gin.Context) {
	if h == nil || h.store == nil || h.db == nil {
		writeError(c, http.StatusServiceUnavailable, "Prompt 审核配置不可用")
		return
	}
	keyID := strings.ToLower(strings.TrimSpace(c.Param("key_id")))
	if len(keyID) != sha256.Size*2 {
		writeError(c, http.StatusBadRequest, "审查 Key 标识无效")
		return
	}
	if _, err := hex.DecodeString(keyID); err != nil {
		writeError(c, http.StatusBadRequest, "审查 Key 标识无效")
		return
	}

	h.settingsUpdateMu.Lock()
	defer h.settingsUpdateMu.Unlock()
	settings, err := h.db.GetSystemSettings(c.Request.Context())
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if settings == nil {
		writeError(c, http.StatusNotFound, "审查 Key 不存在")
		return
	}
	// storedRaw 保持数据库原值不做 trim:CAS 按原值精确比较,存量值若带
	// 换行/制表符,trim 后将永远匹配不上而恒 409。
	storedRaw := settings.PromptFilterReviewAPIKey
	keys := (promptfilter.ReviewConfig{APIKey: storedRaw}).APIKeyList()
	remaining := make([]string, 0, len(keys))
	found := false
	deletedMasked := ""
	for _, key := range keys {
		if promptReviewAPIKeyID(key) == keyID {
			found = true
			deletedMasked = maskPromptReviewAPIKey(key)
			continue
		}
		remaining = append(remaining, key)
	}
	if !found {
		writeError(c, http.StatusNotFound, "审查 Key 不存在或已被删除")
		return
	}
	if settings.PromptFilterReviewEnabled && len(remaining) == 0 {
		writeError(c, http.StatusConflict, "模型复核启用时不能删除最后一个审查 Key，请先关闭模型复核或添加替代 Key")
		return
	}
	replacement := strings.Join(remaining, "\n")
	runtimeCfg := h.store.GetPromptFilterConfig()
	runtimeCfg.Review.APIKey = replacement
	runtimeCfg = promptfilter.NormalizeConfig(runtimeCfg)
	if err := promptfilter.ValidateReviewConfig(runtimeCfg.Review); err != nil {
		writeError(c, http.StatusConflict, "删除后审查配置无效: "+err.Error())
		return
	}
	swapped, err := h.db.CompareAndSwapPromptFilterReviewAPIKeys(c.Request.Context(), storedRaw, replacement)
	if err != nil {
		writeInternalError(c, err)
		return
	}
	if !swapped {
		writeError(c, http.StatusConflict, "审查 Key 列表已被其他操作修改，请刷新后重试")
		return
	}
	if err := h.store.SetPromptFilterConfigWithAdvancedRaw(runtimeCfg, h.store.GetPromptFilterAdvancedConfig()); err != nil {
		writeError(c, http.StatusInternalServerError, "审查 Key 已保存，但运行时配置更新失败")
		return
	}
	log.Printf("审查 Key 已删除: %s (来自 %s)，剩余 %d 个", deletedMasked, c.ClientIP(), len(remaining))
	items := promptReviewAPIKeyDescriptors(remaining)
	c.JSON(http.StatusOK, promptReviewAPIKeysResponse{Items: items, Count: len(items)})
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
	return cfg.Enabled && promptfilter.ShouldReviewVerdict(verdict, cfg.Review)
}

func reviewPromptFilterVerdict(ctx context.Context, text string, verdict promptfilter.Verdict, cfg promptfilter.Config) promptfilter.Verdict {
	if strings.TrimSpace(text) == "" {
		return verdict
	}
	outcome, err := promptfilter.DefaultReviewClient.ReviewTextDetailed(ctx, text, cfg.Review)
	return promptfilter.ApplyReviewOutcome(verdict, outcome, err, cfg.Review)
}
