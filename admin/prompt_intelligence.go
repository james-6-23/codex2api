package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

var defaultIntelligenceQueries = []string{
	"LLM jailbreak prompt injection",
	"大模型 破限 提示词",
	"GPT 破甲 提示词",
	"AI 越狱 提示词",
	"中文 prompt injection 绕过",
	"ChatGPT jailbreak prompt",
	"Codex prompt injection jailbreak",
}

var githubPromptSearchBaseURL = "https://api.github.com/search/repositories"

type promptIntelligenceSource struct {
	Provider    string `json:"provider"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	Description string `json:"description"`
	UpdatedAt   string `json:"updated_at"`
}

type promptIntelligenceCandidate struct {
	Name      string `json:"name"`
	Pattern   string `json:"pattern"`
	Weight    int    `json:"weight"`
	Category  string `json:"category"`
	Strict    bool   `json:"strict"`
	Rationale string `json:"rationale,omitempty"`
	SourceURL string `json:"source_url,omitempty"`
	Status    string `json:"status,omitempty"`
}

type promptIntelligenceRun struct {
	StartedAt  time.Time                     `json:"started_at"`
	FinishedAt time.Time                     `json:"finished_at"`
	Queries    []string                      `json:"queries"`
	Sources    []promptIntelligenceSource    `json:"sources"`
	Candidates []promptIntelligenceCandidate `json:"candidates"`
	ModelCalls int                           `json:"model_calls"`
	Added      int                           `json:"added"`
	Errors     []string                      `json:"errors"`
}

type promptIntelligenceHistoryResponse struct {
	Runs  []*promptIntelligenceRun `json:"runs"`
	Total int                      `json:"total"`
}

var promptIntelligenceRunMu sync.Mutex

func (h *Handler) StartPromptIntelligence(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(time.Hour)
		defer ticker.Stop()
		var lastRun time.Time
		for {
			cfg := h.store.GetPromptFilterConfig().Advanced.Intelligence
			if cfg.Enabled && (lastRun.IsZero() || time.Since(lastRun) >= time.Duration(cfg.IntervalHours)*time.Hour) {
				lastRun = time.Now()
				_, _ = h.runPromptIntelligence(ctx, cfg)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

func (h *Handler) RunPromptIntelligence(c *gin.Context) {
	cfg := h.store.GetPromptFilterConfig().Advanced.Intelligence
	run, err := h.runPromptIntelligence(c.Request.Context(), cfg)
	if err != nil {
		writeError(c, http.StatusConflict, err.Error())
		return
	}
	c.JSON(http.StatusOK, run)
}

func (h *Handler) ListPromptIntelligenceHistory(c *gin.Context) {
	page := positiveQueryInt(c, "page", 1)
	pageSize := positiveQueryInt(c, "page_size", 20)
	logs, total, err := h.db.ListPromptFilterLogsPage(c.Request.Context(), database.PromptFilterLogQuery{Page: page, PageSize: pageSize, Source: "intel_run"})
	if err != nil {
		writeInternalError(c, err)
		return
	}
	runs := make([]*promptIntelligenceRun, 0, len(logs))
	for _, item := range logs {
		var run promptIntelligenceRun
		if json.Unmarshal([]byte(item.FullText), &run) == nil {
			runs = append(runs, &run)
		}
	}
	c.JSON(http.StatusOK, promptIntelligenceHistoryResponse{Runs: runs, Total: total})
}

func (h *Handler) AddPromptIntelligenceCandidate(c *gin.Context) {
	var candidate promptIntelligenceCandidate
	if err := c.ShouldBindJSON(&candidate); err != nil {
		writeError(c, http.StatusBadRequest, "候选规则格式无效")
		return
	}
	if err := validateIntelligenceCandidate(candidate); err != nil {
		writeError(c, http.StatusBadRequest, err.Error())
		return
	}
	added, updated, err := h.addPromptIntelligenceCandidates(c.Request.Context(), []promptIntelligenceCandidate{candidate})
	if err != nil {
		writeError(c, http.StatusInternalServerError, err.Error())
		return
	}
	c.JSON(http.StatusOK, gin.H{"added": added, "updated": updated})
}

func (h *Handler) runPromptIntelligence(ctx context.Context, cfg promptfilter.IntelligenceConfig) (*promptIntelligenceRun, error) {
	if !promptIntelligenceRunMu.TryLock() {
		return nil, fmt.Errorf("规则情报更新任务正在运行")
	}
	defer promptIntelligenceRunMu.Unlock()
	cfg = promptfilter.NormalizeAdvancedConfig(promptfilter.AdvancedConfig{Intelligence: cfg}).Intelligence
	queries := mergeIntelligenceQueries(defaultIntelligenceQueries, cfg.Queries)
	run := &promptIntelligenceRun{StartedAt: time.Now(), Queries: queries, Sources: []promptIntelligenceSource{}, Candidates: []promptIntelligenceCandidate{}, Errors: []string{}}
	perQuery := cfg.MaxSearchResults / len(queries)
	if perQuery < 1 {
		perQuery = 1
	}
	seenSources := map[string]bool{}
	for _, query := range queries {
		remaining := cfg.MaxSearchResults - len(run.Sources)
		if remaining <= 0 {
			break
		}
		limit := perQuery
		if limit > remaining {
			limit = remaining
		}
		items, err := searchGitHubPromptIntelligence(ctx, query, limit)
		if err != nil {
			run.Errors = append(run.Errors, err.Error())
			continue
		}
		for _, item := range items {
			if !seenSources[item.URL] {
				seenSources[item.URL] = true
				run.Sources = append(run.Sources, item)
			}
		}
	}
	h.insertIntelligenceLog(ctx, "intel_search", "searched", "", run.Sources, run.Errors)
	if cfg.ModelEnabled && cfg.MaxModelCalls > 0 && len(run.Sources) > 0 {
		candidates, err := h.analyzePromptIntelligenceWithPool(ctx, cfg.Model, run.Sources)
		run.ModelCalls = 1
		if err != nil {
			run.Errors = append(run.Errors, err.Error())
			h.insertIntelligenceLog(ctx, "intel_model", "error", cfg.Model, nil, []string{err.Error()})
		} else {
			run.Candidates = h.comparePromptIntelligenceCandidates(candidates)
			h.insertIntelligenceLog(ctx, "intel_model", "analyzed", cfg.Model, candidates, nil)
		}
	}
	if cfg.AutoAdd && len(run.Candidates) > 0 {
		added, updated, err := h.addPromptIntelligenceCandidates(ctx, run.Candidates)
		if err != nil {
			run.Errors = append(run.Errors, err.Error())
		} else {
			run.Added = added + updated
		}
	}
	run.FinishedAt = time.Now()
	h.insertIntelligenceLog(ctx, "intel_run", "completed", cfg.Model, run, run.Errors)
	return run, nil
}

func mergeIntelligenceQueries(groups ...[]string) []string {
	seen := map[string]bool{}
	result := make([]string, 0)
	for _, group := range groups {
		for _, query := range group {
			query = strings.TrimSpace(query)
			key := strings.ToLower(query)
			if query != "" && !seen[key] {
				seen[key] = true
				result = append(result, query)
			}
		}
	}
	return result
}

func (h *Handler) comparePromptIntelligenceCandidates(candidates []promptIntelligenceCandidate) []promptIntelligenceCandidate {
	cfg := h.store.GetPromptFilterConfig()
	exactPatterns := map[string]bool{}
	builtinNames := map[string]bool{}
	customByName := map[string]promptfilter.PatternConfig{}
	for _, item := range promptfilter.BuiltinPatternConfigs() {
		exactPatterns[item.Pattern] = true
		builtinNames[strings.ToLower(strings.TrimSpace(item.Name))] = true
	}
	for _, item := range cfg.CustomPatterns {
		exactPatterns[item.Pattern] = true
		customByName[strings.ToLower(strings.TrimSpace(item.Name))] = item
	}
	result := make([]promptIntelligenceCandidate, 0, len(candidates))
	for _, candidate := range candidates {
		name := strings.ToLower(strings.TrimSpace(candidate.Name))
		if exactPatterns[candidate.Pattern] || builtinNames[name] {
			continue
		}
		if _, exists := customByName[name]; exists {
			candidate.Status = "update"
		} else {
			candidate.Status = "new"
		}
		result = append(result, candidate)
	}
	return result
}

func searchGitHubPromptIntelligence(ctx context.Context, query string, limit int) ([]promptIntelligenceSource, error) {
	if limit > 30 {
		limit = 30
	}
	u := githubPromptSearchBaseURL + "?q=" + url.QueryEscape(query) + "&sort=updated&order=desc&per_page=" + fmt.Sprint(limit)
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "Codex2API-Prompt-Intelligence")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GitHub 搜索失败: %w", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub 搜索返回 HTTP %d", resp.StatusCode)
	}
	var raw struct {
		Items []map[string]any `json:"items"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	items := make([]promptIntelligenceSource, 0, len(raw.Items))
	for _, item := range raw.Items {
		items = append(items, promptIntelligenceSource{Provider: "github", Title: stringValue(item["full_name"]), URL: stringValue(item["html_url"]), Description: promptfilter.RedactedPreview(stringValue(item["description"]), 500), UpdatedAt: stringValue(item["updated_at"])})
	}
	return items, nil
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	return fmt.Sprint(value)
}

func (h *Handler) analyzePromptIntelligenceWithPool(ctx context.Context, model string, sources []promptIntelligenceSource) ([]promptIntelligenceCandidate, error) {
	sourceJSON, _ := json.Marshal(sources)
	prompt := `你是防御性提示词注入规则分析器。根据公开项目的标题、描述和链接，只提取新的越狱/提示词注入语言特征。不要复述攻击教程，不要生成可执行攻击内容。返回严格 JSON 数组，每项字段为 name、pattern(RE2兼容正则)、weight(1-100)、category、strict、rationale、source_url。最多10项；没有可靠候选就返回[]。公开来源：` + string(sourceJSON)
	body, _ := json.Marshal(map[string]any{"model": model, "input": prompt, "stream": false})
	status, response := h.imageProxy.ExecuteInternalResponse(ctx, body)
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("号池模型分析失败: HTTP %d", status)
	}
	text := extractResponseOutputText(response)
	start, end := strings.Index(text, "["), strings.LastIndex(text, "]")
	if start < 0 || end < start {
		return nil, fmt.Errorf("号池模型未返回 JSON 候选规则")
	}
	var candidates []promptIntelligenceCandidate
	if err := json.Unmarshal([]byte(text[start:end+1]), &candidates); err != nil {
		return nil, fmt.Errorf("候选规则 JSON 无效: %w", err)
	}
	valid := candidates[:0]
	for _, candidate := range candidates {
		if validateIntelligenceCandidate(candidate) == nil {
			valid = append(valid, candidate)
		}
		if len(valid) >= 10 {
			break
		}
	}
	return valid, nil
}

func extractResponseOutputText(data []byte) string {
	var value any
	if json.Unmarshal(data, &value) != nil {
		return ""
	}
	var texts []string
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if x["type"] == "output_text" {
				if s, ok := x["text"].(string); ok {
					texts = append(texts, s)
				}
			}
			for _, child := range x {
				walk(child)
			}
		case []any:
			for _, child := range x {
				walk(child)
			}
		}
	}
	walk(value)
	return strings.Join(texts, "\n")
}

func validateIntelligenceCandidate(candidate promptIntelligenceCandidate) error {
	if strings.TrimSpace(candidate.Name) == "" || len(candidate.Name) > 100 {
		return fmt.Errorf("规则名称无效")
	}
	if len(candidate.Pattern) == 0 || len(candidate.Pattern) > 5000 {
		return fmt.Errorf("规则 pattern 无效")
	}
	if _, err := regexp.Compile(candidate.Pattern); err != nil {
		return fmt.Errorf("规则正则无效: %v", err)
	}
	if candidate.Weight < 1 || candidate.Weight > 100 {
		return fmt.Errorf("规则权重必须为 1-100")
	}
	return nil
}

func (h *Handler) addPromptIntelligenceCandidates(ctx context.Context, candidates []promptIntelligenceCandidate) (int, int, error) {
	h.settingsUpdateMu.Lock()
	defer h.settingsUpdateMu.Unlock()
	settings, err := h.db.GetSystemSettings(ctx)
	if err != nil {
		return 0, 0, err
	}
	cfg := h.store.GetPromptFilterConfig()
	existing := map[string]int{}
	for index, item := range cfg.CustomPatterns {
		existing[strings.ToLower(strings.TrimSpace(item.Name))] = index
	}
	added, updated := 0, 0
	for _, candidate := range candidates {
		if validateIntelligenceCandidate(candidate) != nil {
			continue
		}
		item := promptfilter.PatternConfig{Name: candidate.Name, Pattern: candidate.Pattern, Weight: candidate.Weight, Category: candidate.Category, Strict: candidate.Strict}
		name := strings.ToLower(strings.TrimSpace(candidate.Name))
		if index, exists := existing[name]; exists {
			if cfg.CustomPatterns[index].Pattern == candidate.Pattern && cfg.CustomPatterns[index].Weight == candidate.Weight && cfg.CustomPatterns[index].Category == candidate.Category && cfg.CustomPatterns[index].Strict == candidate.Strict {
				continue
			}
			cfg.CustomPatterns[index] = item
			updated++
		} else {
			cfg.CustomPatterns = append(cfg.CustomPatterns, item)
			existing[name] = len(cfg.CustomPatterns) - 1
			added++
		}
	}
	if added == 0 && updated == 0 {
		return 0, 0, nil
	}
	settings.PromptFilterCustomPatterns = promptfilter.MarshalCustomPatterns(cfg.CustomPatterns)
	if err := h.db.UpdateSystemSettings(ctx, settings); err != nil {
		return 0, 0, err
	}
	h.store.SetPromptFilterConfig(cfg)
	h.insertIntelligenceLog(ctx, "intel_rule_add", "added_or_updated", "", candidates, nil)
	return added, updated, nil
}

func (h *Handler) insertIntelligenceLog(ctx context.Context, source, action, model string, value any, errors []string) {
	data, _ := json.Marshal(value)
	errorText := strings.Join(errors, "; ")
	_ = h.db.InsertPromptFilterLog(ctx, &database.PromptFilterLogInput{Source: source, Endpoint: "prompt_intelligence", Model: model, Action: action, Mode: "audit", MatchedPatterns: "[]", TextPreview: promptfilter.RedactedPreview(string(data), 500), FullText: string(data), ErrorCode: errorText})
}
