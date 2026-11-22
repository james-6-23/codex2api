package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

const qualityTestPromptLimit = 16000

// A separate document avoids inheriting the admin page's script-src policy (as
// srcdoc/blob documents do). Do not loosen the admin CSP or grant same-origin.
func serveQualityTestPreview(c *gin.Context) {
	c.Header("Content-Security-Policy", "default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; img-src data: blob:; font-src data:; media-src data: blob:; connect-src 'none'; frame-src 'none'; object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'self'; sandbox allow-scripts")
	c.Header("X-Frame-Options", "SAMEORIGIN")
	c.Header("Referrer-Policy", "no-referrer")
	c.Header("Cache-Control", "no-store")
	c.Data(http.StatusOK, "text/html; charset=utf-8", []byte(`<!doctype html><html><head><meta charset="utf-8"></head><body><script>
addEventListener('message', function receive(event) {
  if (event.source !== parent || event.data?.type !== 'quality-test-preview' || typeof event.data.html !== 'string' || event.data.html.length > 1100000) return;
  removeEventListener('message', receive);
  document.open();
  document.write(event.data.html);
  document.close();
});
</script></body></html>`))
}

type qualityTestRequest struct {
	Model           string `json:"model"`
	Prompt          string `json:"prompt"`
	ReasoningEffort string `json:"reasoning_effort"`
	// PromptID references the custom preset the prompt came from; PresetKey names a
	// built-in one. PresetName is the client's display name, kept as a fallback only.
	PromptID   int64  `json:"prompt_id,omitempty"`
	PresetKey  string `json:"preset_key,omitempty"`
	PresetName string `json:"preset_name,omitempty"`
	// TimeoutMinutes is the run's hard limit; 0 selects the default.
	TimeoutMinutes int `json:"timeout_minutes,omitempty"`
}

var builtinPresetKey = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)

// resolveQualityTestPreset snapshots the preset a run came from. Custom presets
// take their name from the database; a vanished preset degrades to "hand-typed".
func (h *Handler) resolveQualityTestPreset(ctx context.Context, req qualityTestRequest) (kind, ref, name string) {
	if req.PromptID > 0 {
		preset, err := h.db.GetQualityTestPrompt(ctx, req.PromptID)
		if err != nil || preset.Prompt != req.Prompt {
			return "", "", ""
		}
		return "custom", strconv.FormatInt(preset.ID, 10), preset.Name
	}
	key := strings.ToLower(strings.TrimSpace(req.PresetKey))
	if key == "" || !builtinPresetKey.MatchString(key) {
		return "", "", ""
	}
	name = strings.TrimSpace(req.PresetName)
	if utf8.RuneCountInString(name) > qualityTestPromptNameLimit {
		name = string([]rune(name)[:qualityTestPromptNameLimit])
	}
	return "builtin", key, name
}

type qualityTestOptions struct {
	Models           []string `json:"models"`
	ReasoningEfforts []string `json:"reasoning_efforts"`
}

// QualityTest runs one administrator-selected account, without scheduler fallback.
// It shares the connection test's transport, cancellation, diagnostics and quota handling.
func (h *Handler) QualityTest(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 128*1024)
	var req qualityTestRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的测试请求"})
		return
	}
	req.Model = strings.TrimSpace(req.Model)
	req.ReasoningEffort = strings.ToLower(strings.TrimSpace(req.ReasoningEffort))
	if req.Model == "" || len(req.Model) > 200 || strings.TrimSpace(req.Prompt) == "" || len(req.Prompt) > qualityTestPromptLimit {
		c.JSON(http.StatusBadRequest, gin.H{"error": "请选择模型，测试提示词不能为空且不能超过 16000 字节"})
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 10*time.Minute)
	defer cancel()
	c.Request = c.Request.WithContext(ctx)
	h.testConnection(c, &req)
}

func (h *Handler) QualityTestOptions(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "无效的账号 ID"})
		return
	}
	account := h.store.FindByID(id)
	if account == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "账号不在运行时池中"})
		return
	}
	c.JSON(http.StatusOK, h.qualityTestOptionsForAccount(c.Request.Context(), account))
}

func (h *Handler) qualityTestOptionsForAccount(ctx context.Context, account *auth.Account) qualityTestOptions {
	options := qualityTestOptions{Models: []string{}, ReasoningEfforts: []string{"", "none", "minimal", "low", "medium", "high", "xhigh", "max", "ultra"}}
	var candidates []string
	switch {
	case account.IsClaudeOAuth():
		candidates = claudeProbeModelIDs(account)
		options.ReasoningEfforts = []string{"", "low", "medium", "high", "max"}
	case account.IsAntigravityAPI():
		candidates = antigravityConnectionTestModels(account)
		// Public Antigravity model IDs already encode a fixed thinking tier.
		options.ReasoningEfforts = []string{""}
	case account.IsRelayStyle():
		candidates = account.OpenAIResponsesModels()
		if account.IsGrokAPI() {
			options.ReasoningEfforts = []string{"", "low", "medium", "high"}
			if len(candidates) == 0 {
				candidates = defaultGrokConnectionTestModels(account)
			}
		}
	default:
		candidates = account.CodexModels()
		if len(candidates) == 0 {
			candidates = proxy.TextTestModelIDs(ctx, h.db)
		}
	}
	for _, model := range candidates {
		model = strings.TrimSpace(model)
		// Effort aliases are proxy routing shortcuts, not upstream model IDs.
		if strings.EqualFold(model, "codex-auto-review") || !isTextConnectionModel(model) || strings.ContainsAny(model, "()") || slices.Contains(options.Models, model) {
			continue
		}
		options.Models = append(options.Models, model)
	}
	return options
}

func (h *Handler) validateQualityTestForAccount(ctx context.Context, account *auth.Account, req qualityTestRequest) error {
	options := h.qualityTestOptionsForAccount(ctx, account)
	if !slices.Contains(options.Models, req.Model) {
		return fmt.Errorf("该账号不支持测试模型: %s", req.Model)
	}
	if !slices.Contains(options.ReasoningEfforts, req.ReasoningEffort) {
		return fmt.Errorf("该渠道不支持所选思考强度")
	}
	return nil
}

func buildQualityTestPayload(account *auth.Account, model string, req qualityTestRequest, securityCfg auth.ClaudeSecurityConfig) ([]byte, error) {
	const instructions = "Return a complete, self-contained HTML document for the user's request. Include all SVG, CSS and JavaScript inline. Do not use external resources. Return only HTML, without Markdown fences or explanations."
	body := map[string]any{"model": model, "stream": true}
	if account.IsClaudeOAuth() {
		maxTokens := int64(32768)
		if securityCfg.MaxOutputTokens > 0 && securityCfg.MaxOutputTokens < maxTokens {
			maxTokens = securityCfg.MaxOutputTokens
		}
		body["max_tokens"] = maxTokens
		body["system"] = instructions
		body["messages"] = []map[string]any{{"role": "user", "content": req.Prompt}}
		if req.ReasoningEffort != "" {
			body["thinking"] = map[string]any{"type": "adaptive"}
			body["output_config"] = map[string]any{"effort": req.ReasoningEffort}
		}
	} else {
		body["store"] = false
		body["instructions"] = instructions
		body["input"] = []map[string]any{{"role": "user", "content": []map[string]any{{"type": "input_text", "text": req.Prompt}}}}
		if req.ReasoningEffort != "" {
			body["reasoning"] = map[string]any{"effort": req.ReasoningEffort}
		}
	}
	return json.Marshal(body)
}
