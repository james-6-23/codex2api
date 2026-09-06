package admin

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"

	"github.com/gin-gonic/gin"
)

// Claude / Antigravity 渠道的连通性测试配置：账号页"测试连接"按钮与批量测试
// 默认使用的探测模型和测活内容。全局 test_model / test_content 是 Codex 语义，
// 这两个渠道的模型目录完全不同，需要各自的默认值；留空时模型按账号目录自动选，
// 内容沿用全局测活内容。

const (
	channelTestChannelAntigravity = "antigravity"
	channelTestChannelClaude      = "claude"
)

type channelTestSettingsResponse struct {
	Antigravity database.ChannelTestSettings `json:"antigravity"`
	Claude      database.ChannelTestSettings `json:"claude"`
	// 渠道留空内容时实际生效的全局测活内容，前端用作占位提示。
	DefaultTestContent string `json:"default_test_content"`
	// 渠道并发为 0 时实际生效的全局批量测试并发。
	DefaultTestConcurrency int `json:"default_test_concurrency"`
	// 设置页下拉可选的模型：取自当前号池里该渠道账号目录的并集（Claude 常是带日期的
	// 具体 ID，Antigravity 是对外发布的固定档位），没有账号时回落到默认集。
	ModelChoices map[string][]string `json:"model_choices"`
}

// channelTestModelChoices 汇总各渠道账号目录里可作为测试模型的候选。
func (h *Handler) channelTestModelChoices() map[string][]string {
	antigravity := make([]string, 0, 16)
	claude := make([]string, 0, 16)
	seenAG := map[string]struct{}{}
	seenClaude := map[string]struct{}{}
	appendUnique := func(dst []string, seen map[string]struct{}, models []string) []string {
		for _, model := range models {
			model = strings.TrimSpace(model)
			key := strings.ToLower(model)
			if model == "" {
				continue
			}
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			dst = append(dst, model)
		}
		return dst
	}
	if h != nil && h.store != nil {
		for _, account := range h.store.Accounts() {
			switch {
			case account.IsAntigravityAPI():
				antigravity = appendUnique(antigravity, seenAG, antigravityConnectionTestModels(account))
			case account.IsClaudeOAuth():
				claude = appendUnique(claude, seenClaude, claudeProbeModelIDs(account))
			}
		}
	}
	if len(antigravity) == 0 {
		antigravity = appendUnique(antigravity, seenAG, proxy.AntigravityPublishedModelIDs(auth.AntigravityDefaultModelIDs()))
	}
	if len(claude) == 0 {
		claude = appendUnique(claude, seenClaude, []string{"claude-opus-4-6", "claude-sonnet-4-6", "claude-haiku-4-5"})
	}
	return map[string][]string{
		channelTestChannelAntigravity: antigravity,
		channelTestChannelClaude:      claude,
	}
}

// channelTestConfig 返回当前生效的渠道测试配置。首次访问从数据库加载并缓存，
// 之后由 PUT 同步刷新；无数据库（单测 / 纯内存）时返回零值。
func (h *Handler) channelTestConfig(ctx context.Context) database.ChannelTestConfig {
	if h == nil {
		return database.ChannelTestConfig{}
	}
	if cached := h.channelTestCfg.Load(); cached != nil {
		return *cached
	}
	if h.db == nil {
		return database.ChannelTestConfig{}
	}
	cfg, err := h.db.LoadChannelTestConfig(ctx)
	if err != nil {
		log.Printf("读取渠道连通性测试设置失败，按默认处理: %v", err)
		return database.ChannelTestConfig{}
	}
	h.channelTestCfg.Store(&cfg)
	return cfg
}

// channelTestSettingsForAccount 取账号所属渠道的测试配置；Codex/Grok 等渠道返回零值。
func (h *Handler) channelTestSettingsForAccount(ctx context.Context, account *auth.Account) database.ChannelTestSettings {
	if account == nil {
		return database.ChannelTestSettings{}
	}
	cfg := h.channelTestConfig(ctx)
	switch {
	case account.IsAntigravityAPI():
		return cfg.Antigravity
	case account.IsClaudeOAuth():
		return cfg.Claude
	}
	return database.ChannelTestSettings{}
}

// connectionTestContentForAccount 返回该账号测连应发送的用户输入（已做多行抽取与
// 变量展开）：渠道自定义内容优先，留空沿用全局测活内容。
func (h *Handler) connectionTestContentForAccount(ctx context.Context, account *auth.Account) string {
	content := ""
	if settings := h.channelTestSettingsForAccount(ctx, account); settings.TestContent != "" {
		content = settings.TestContent
	} else {
		content = auth.DefaultTestContent
		if h != nil && h.store != nil {
			content = h.store.GetTestContent()
		}
	}
	return auth.RenderTestContent(content)
}

func (h *Handler) buildChannelTestSettingsResponse(cfg database.ChannelTestConfig) channelTestSettingsResponse {
	defaultContent := auth.DefaultTestContent
	defaultConcurrency := 1
	if h != nil && h.store != nil {
		defaultContent = h.store.GetTestContent()
		if global := h.store.GetTestConcurrency(); global > 0 {
			defaultConcurrency = global
		}
	}
	return channelTestSettingsResponse{
		Antigravity:            cfg.Antigravity,
		Claude:                 cfg.Claude,
		DefaultTestContent:     defaultContent,
		DefaultTestConcurrency: defaultConcurrency,
		ModelChoices:           h.channelTestModelChoices(),
	}
}

// GetChannelTestSettings 返回 Claude / Antigravity 的连通性测试配置。
// GET /api/admin/settings/channel-tests
func (h *Handler) GetChannelTestSettings(c *gin.Context) {
	c.JSON(http.StatusOK, h.buildChannelTestSettingsResponse(h.channelTestConfig(c.Request.Context())))
}

// UpdateChannelTestSettings 保存渠道连通性测试配置。请求体只带要改的渠道即可，
// 未出现的渠道保持原值；模型只做基本校验（不能是生图模型），是否在账号目录里
// 由测连时按账号判定。
// PUT /api/admin/settings/channel-tests  {"antigravity":{"test_model":"...","test_content":"..."}}
func (h *Handler) UpdateChannelTestSettings(c *gin.Context) {
	var req struct {
		Antigravity *database.ChannelTestSettings `json:"antigravity"`
		Claude      *database.ChannelTestSettings `json:"claude"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeError(c, http.StatusBadRequest, "请求体解析失败: "+err.Error())
		return
	}
	if req.Antigravity == nil && req.Claude == nil {
		writeError(c, http.StatusBadRequest, "缺少 antigravity 或 claude 字段")
		return
	}
	if h.db == nil {
		writeError(c, http.StatusServiceUnavailable, "数据库不可用")
		return
	}
	cfg := h.channelTestConfig(c.Request.Context())
	apply := func(channel string, target *database.ChannelTestSettings, incoming *database.ChannelTestSettings) error {
		if incoming == nil {
			return nil
		}
		normalized, err := normalizeChannelTestSettings(channel, *incoming)
		if err != nil {
			return err
		}
		*target = normalized
		return nil
	}
	if err := apply(channelTestChannelAntigravity, &cfg.Antigravity, req.Antigravity); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := apply(channelTestChannelClaude, &cfg.Claude, req.Claude); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	if err := h.db.SaveChannelTestConfig(c.Request.Context(), cfg); err != nil {
		writeError(c, http.StatusInternalServerError, "保存失败: "+err.Error())
		return
	}
	normalized := cfg.Normalized()
	h.channelTestCfg.Store(&normalized)
	log.Printf("设置已更新: channel_test_config antigravity.model=%q claude.model=%q", normalized.Antigravity.TestModel, normalized.Claude.TestModel)
	c.JSON(http.StatusOK, h.buildChannelTestSettingsResponse(normalized))
}

// normalizeChannelTestSettings 校验单渠道配置：模型不能是生图模型，Claude 模型必须
// 以 claude- 开头；内容为空表示沿用全局，否则受全局同一长度上限约束。
func normalizeChannelTestSettings(channel string, settings database.ChannelTestSettings) (database.ChannelTestSettings, error) {
	model := strings.TrimSpace(settings.TestModel)
	if model != "" {
		if !isTextConnectionModel(model) {
			return database.ChannelTestSettings{}, &channelTestSettingsError{channel: channel, message: "测试模型不能是生图模型: " + model}
		}
		if channel == channelTestChannelClaude && !strings.HasPrefix(strings.ToLower(model), "claude-") {
			return database.ChannelTestSettings{}, &channelTestSettingsError{channel: channel, message: "Claude 测试模型必须以 claude- 开头: " + model}
		}
	}
	content := strings.TrimSpace(settings.TestContent)
	if content != "" {
		normalized, err := validateConnectionTestContent(content)
		if err != nil {
			return database.ChannelTestSettings{}, &channelTestSettingsError{channel: channel, message: err.Error()}
		}
		content = normalized
	}
	if settings.TestConcurrency < 0 || settings.TestConcurrency > database.MaxChannelTestConcurrency {
		return database.ChannelTestSettings{}, &channelTestSettingsError{channel: channel, message: fmt.Sprintf("test_concurrency 须在 0~%d 之间（0 = 沿用全局）", database.MaxChannelTestConcurrency)}
	}
	return database.ChannelTestSettings{TestModel: model, TestContent: content, TestConcurrency: settings.TestConcurrency}, nil
}

// batchTestConcurrency 决定一次批量测试的并发：整批账号同属 Claude 或 Antigravity
// 且该渠道配置了并发时用渠道值，否则（混合批次、Codex/Grok、未配置）沿用全局。
func (h *Handler) batchTestConcurrency(ctx context.Context, accounts []*auth.Account) int {
	global := 0
	if h != nil && h.store != nil {
		global = h.store.GetTestConcurrency()
	}
	if global <= 0 {
		global = 1
	}
	if len(accounts) == 0 {
		return global
	}
	channel := ""
	for _, account := range accounts {
		var current string
		switch {
		case account == nil:
			return global
		case account.IsAntigravityAPI():
			current = channelTestChannelAntigravity
		case account.IsClaudeOAuth():
			current = channelTestChannelClaude
		default:
			return global
		}
		if channel == "" {
			channel = current
		} else if channel != current {
			return global
		}
	}
	configured := 0
	cfg := h.channelTestConfig(ctx)
	switch channel {
	case channelTestChannelAntigravity:
		configured = cfg.Antigravity.TestConcurrency
	case channelTestChannelClaude:
		configured = cfg.Claude.TestConcurrency
	}
	if configured > 0 {
		return configured
	}
	return global
}

type channelTestSettingsError struct {
	channel string
	message string
}

func (e *channelTestSettingsError) Error() string {
	return e.channel + ": " + e.message
}
