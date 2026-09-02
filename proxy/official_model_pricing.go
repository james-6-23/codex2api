package proxy

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/database"
)

const (
	OfficialOpenAIPricingURL = "https://developers.openai.com/api/docs/pricing.md"
	OfficialXAIPricingURL    = "https://docs.x.ai/developers/pricing.md"
	OfficialXAIModelsURL     = "https://docs.x.ai/developers/models"
)

type OfficialPricingSyncOptions struct {
	Models        []string
	IncludeOpenAI bool
	IncludeGrok   bool
	IncludeClaude bool
}

// OfficialAnthropicPricingURL 是 Anthropic 官方价格参考页(仅用于前端展示链接)。
const OfficialAnthropicPricingURL = "https://www.anthropic.com/pricing"

// isClaudeBillingModel 判断某规范计费键是否为 Claude 模型。
func isClaudeBillingModel(model string) bool {
	return strings.Contains(model, "claude") || strings.Contains(model, "opus") ||
		strings.Contains(model, "sonnet") || strings.Contains(model, "haiku")
}

type OfficialPricingSyncResult struct {
	Fetched  int       `json:"fetched"`
	Applied  int       `json:"applied"`
	Skipped  int       `json:"skipped"`
	Missing  []string  `json:"missing,omitempty"`
	Sources  []string  `json:"sources"`
	Warnings []string  `json:"warnings,omitempty"`
	SyncedAt time.Time `json:"synced_at"`
}

// SyncOfficialModelPricing 先在事务外拉取 OpenAI/xAI 官方 Markdown 价目，全部解析
// 完成后才用一次短写入更新覆盖表。管理员 custom 覆盖始终优先，不会被自动同步改写。
func SyncOfficialModelPricing(ctx context.Context, db *database.DB, proxyURL string, options OfficialPricingSyncOptions) (*OfficialPricingSyncResult, error) {
	if db == nil {
		return nil, fmt.Errorf("数据库不可用，无法同步官方模型价格")
	}
	allowed := make(map[string]struct{})
	var grokModels []string
	for _, model := range options.Models {
		key := database.CanonicalBillingModelKey(model)
		if key == "" {
			continue
		}
		allowed[key] = struct{}{}
		if strings.HasPrefix(key, "grok-") {
			grokModels = append(grokModels, key)
		}
	}
	grokModels = uniqueSortedStrings(grokModels)

	result := &OfficialPricingSyncResult{SyncedAt: time.Now().UTC()}
	pricing := make(map[string]database.ModelPricingOverride)
	client := &http.Client{Transport: newCodexStandardTransport(proxyURL), Timeout: 20 * time.Second}

	if options.IncludeOpenAI {
		body, err := fetchOfficialPricingMarkdown(ctx, client, OfficialOpenAIPricingURL)
		if err != nil {
			return result, fmt.Errorf("读取 OpenAI 官方价格失败: %w", err)
		}
		parsed, err := ParseOpenAIOfficialPricingMarkdown(body)
		if err != nil {
			return result, fmt.Errorf("解析 OpenAI 官方价格失败: %w", err)
		}
		for model, override := range parsed {
			if len(allowed) > 0 {
				if _, ok := allowed[model]; !ok {
					continue
				}
			}
			pricing[model] = override
		}
		result.Sources = append(result.Sources, OfficialOpenAIPricingURL)
	}

	if options.IncludeGrok && len(grokModels) > 0 {
		body, err := fetchOfficialPricingMarkdown(ctx, client, OfficialXAIPricingURL)
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("读取 xAI 官方价格失败: %v", err))
		} else {
			xaiPricing, parseErr := ParseXAIOfficialPricingMarkdown(body)
			if parseErr != nil {
				result.Warnings = append(result.Warnings, fmt.Sprintf("解析 xAI 官方价格失败: %v", parseErr))
			} else {
				result.Sources = append(result.Sources, OfficialXAIPricingURL)
				for _, model := range grokModels {
					override, ok := xaiPricing[model]
					if !ok {
						result.Warnings = append(result.Warnings, fmt.Sprintf("%s: xAI 官方总价目未找到该模型，已保留现有价格", model))
						continue
					}
					pricing[model] = override
				}
			}
		}
	}

	// Claude:Anthropic 无可解析的官方价目文档,且账号真实模型是动态发现的(可能含
	// opus-5 / sonnet-5 等新版)。因此对账号当前的每个 claude 模型,用内置家族定价规则
	// (database.GetModelPricing,已含 opus/sonnet/haiku 现代档)算出权威价并落为 synced,
	// 动态覆盖全部模型、不写死具体清单。用户仍可在定价页覆盖。
	if options.IncludeClaude {
		result.Sources = append(result.Sources, OfficialAnthropicPricingURL)
		for model := range allowed {
			if !isClaudeBillingModel(model) {
				continue
			}
			base := database.GetModelPricing(model)
			if base == nil {
				continue
			}
			pricing[model] = database.ModelPricingOverrideFromPricing(base, "")
		}
	}

	result.Fetched = len(pricing)
	if len(pricing) == 0 {
		return result, fmt.Errorf("官方页面未解析到当前模型的价格，已保留现有价格")
	}
	// 未命中判定按 provider 归类:仅对"已启用来源"的模型报缺失。
	for model := range allowed {
		switch {
		case strings.HasPrefix(model, "grok-"):
			if !options.IncludeGrok {
				continue
			}
		case isClaudeBillingModel(model):
			if !options.IncludeClaude {
				continue
			}
		default:
			if !options.IncludeOpenAI {
				continue
			}
		}
		if _, ok := pricing[model]; !ok {
			result.Missing = append(result.Missing, model)
		}
	}
	sort.Strings(result.Missing)

	_, err := db.MutateModelPricingSettings(ctx, nil, func(current map[string]database.ModelPricingOverride) error {
		for model, override := range pricing {
			if existing, ok := current[model]; ok && existing.Source == database.ModelPricingSourceCustom {
				result.Skipped++
				continue
			}
			override.Source = database.ModelPricingSourceSynced
			current[model] = override
			result.Applied++
		}
		return nil
	})
	if err != nil {
		return result, err
	}
	return result, nil
}

func fetchOfficialPricingMarkdown(ctx context.Context, client *http.Client, sourceURL string) ([]byte, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 18*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, sourceURL, nil)
	if err != nil {
		return nil, err
	}
	// xAI's .md endpoint currently returns 404 when a custom Accept header is
	// present, while the same URL without content negotiation returns Markdown.
	// The .md suffix is sufficient for both official providers.
	req.Header.Set("User-Agent", "codex2api-official-pricing-sync")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("官方价格页面返回 %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if len(body) == 0 {
		return nil, fmt.Errorf("官方价格页面为空")
	}
	return body, nil
}

// ParseOpenAIOfficialPricingMarkdown 读取官方 pricing.md 的 Standard 与 Fast 表。
// 表中的 cache-write 价格当前不参与请求成本，因此只投影系统实际记账的 token 字段。
func ParseOpenAIOfficialPricingMarkdown(body []byte) (map[string]database.ModelPricingOverride, error) {
	standard := parseOfficialPricingTable(body, "### Standard pricing data")
	fast := parseOfficialPricingTable(body, "### Fast pricing data")
	if len(standard) == 0 {
		return nil, fmt.Errorf("未找到 Standard pricing data 表")
	}
	out := make(map[string]database.ModelPricingOverride, len(standard))
	for model, cells := range standard {
		if len(cells) < 9 {
			continue
		}
		override := database.ModelPricingOverride{
			Input:           parseOfficialPrice(cells[1]),
			CachedInput:     parseOfficialPrice(cells[2]),
			Output:          parseOfficialPrice(cells[4]),
			InputLong:       parseOfficialPrice(cells[5]),
			CachedInputLong: parseOfficialPrice(cells[6]),
			OutputLong:      parseOfficialPrice(cells[8]),
		}
		if override.InputLong > 0 || override.OutputLong > 0 {
			override.LongContextThresholdTokens = 272000
		}
		out[model] = override
	}
	for model, cells := range fast {
		if len(cells) < 9 {
			continue
		}
		override, ok := out[model]
		if !ok {
			override = database.ModelPricingOverride{}
		}
		override.InputPriority = parseOfficialPrice(cells[1])
		override.CachedInputPriority = parseOfficialPrice(cells[2])
		override.OutputPriority = parseOfficialPrice(cells[4])
		override.InputLongPriority = parseOfficialPrice(cells[5])
		override.CachedInputLongPriority = parseOfficialPrice(cells[6])
		override.OutputLongPriority = parseOfficialPrice(cells[8])
		out[model] = override
	}
	for model, override := range out {
		if override.IsEmpty() {
			delete(out, model)
		}
	}
	return out, nil
}

func parseOfficialPricingTable(body []byte, heading string) map[string][]string {
	rows := make(map[string][]string)
	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	inSection := false
	seenHeader := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !inSection {
			inSection = line == heading
			continue
		}
		if strings.HasPrefix(line, "### ") && line != heading {
			break
		}
		if !strings.HasPrefix(line, "|") {
			if seenHeader && line != "" {
				break
			}
			continue
		}
		cells := splitMarkdownRow(line)
		if len(cells) == 0 {
			continue
		}
		if !seenHeader {
			seenHeader = true
			continue
		}
		if isMarkdownSeparatorRow(cells) {
			continue
		}
		model := normalizeOfficialPricingModel(cells[0])
		if model != "" {
			rows[model] = cells
		}
	}
	return rows
}

// ParseXAIOfficialPricingMarkdown reads xAI's stable central pricing table. It
// intentionally does not guess per-model document URLs: missing rows remain
// untouched and are surfaced as sync warnings.
func ParseXAIOfficialPricingMarkdown(body []byte) (map[string]database.ModelPricingOverride, error) {
	out := make(map[string]database.ModelPricingOverride)
	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	inTable := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !inTable {
			if line == "### Text API Pricing" {
				inTable = true
			}
			continue
		}
		if !strings.HasPrefix(line, "|") {
			if len(out) > 0 && line != "" {
				break
			}
			continue
		}
		cells := splitMarkdownRow(line)
		if len(cells) < 5 || isMarkdownSeparatorRow(cells) || strings.EqualFold(cells[0], "Model") {
			continue
		}
		label := strings.TrimSpace(cells[0])
		model := label
		if index := strings.Index(model, " ("); index >= 0 {
			model = model[:index]
		}
		model = database.CanonicalBillingModelKey(model)
		if model == "" || !strings.HasPrefix(model, "grok-") {
			continue
		}
		override := out[model]
		input := parseOfficialPrice(cells[2])
		cached := parseOfficialPrice(cells[3])
		output := parseOfficialPrice(cells[4])
		isLong := strings.Contains(label, "≥") || strings.Contains(label, ">=")
		if isLong {
			override.InputLong = input
			override.CachedInputLong = cached
			override.OutputLong = output
			override.LongContextThresholdTokens = 200000
		} else {
			override.Input = input
			override.CachedInput = cached
			override.Output = output
		}
		out[model] = override
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	for model, override := range out {
		if override.Input == 0 || override.Output == 0 {
			delete(out, model)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("未找到 xAI Text API Pricing 表")
	}
	return out, nil
}

func splitMarkdownRow(line string) []string {
	line = strings.TrimSpace(strings.Trim(line, "|"))
	if line == "" {
		return nil
	}
	parts := strings.Split(line, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

func isMarkdownSeparatorRow(cells []string) bool {
	if len(cells) == 0 {
		return false
	}
	for _, cell := range cells {
		trimmed := strings.Trim(cell, " :-")
		if trimmed != "" {
			return false
		}
	}
	return true
}

func normalizeOfficialPricingModel(value string) string {
	value = strings.ToLower(strings.Trim(strings.TrimSpace(value), "`"))
	if idx := strings.Index(value, " ("); idx >= 0 {
		value = value[:idx]
	}
	return database.CanonicalBillingModelKey(value)
}

func parseOfficialPrice(value string) float64 {
	value = strings.TrimSpace(value)
	if value == "" || value == "-" {
		return 0
	}
	value = strings.TrimPrefix(value, "$")
	value = strings.ReplaceAll(value, ",", "")
	price, _ := strconv.ParseFloat(strings.TrimSpace(value), 64)
	return price
}

func uniqueSortedStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
