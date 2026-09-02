package proxy

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/database"
	"golang.org/x/net/html"
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

// OfficialAnthropicPricingURL 是包含模型 API 价格表的 Anthropic 官方文档。
const OfficialAnthropicPricingURL = "https://platform.claude.com/docs/en/about-claude/pricing"

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

// SyncOfficialModelPricing 先在事务外拉取 OpenAI/xAI Markdown 与 Anthropic HTML 价目，全部解析
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

	// Claude 使用 Anthropic 官方 HTML 模型价格表，不能把代码内置回退价伪装成
	// "官方同步"。官方页面不可用时明确失败，管理员可稍后重试；自定义覆盖仍优先。
	// Anthropic 页面不可达/解析失败时与 xAI 一致降级为 warning:OpenAI/xAI 已解析
	// 的结果照常落库,不能因为一个来源拖垮整轮同步(定时同步默认三家都开)。
	if options.IncludeClaude {
		body, err := fetchOfficialPricingMarkdown(ctx, client, OfficialAnthropicPricingURL)
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("读取 Anthropic 官方价格失败: %v", err))
		} else if parsed, parseErr := ParseAnthropicOfficialPricingHTML(body); parseErr != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("解析 Anthropic 官方价格失败: %v", parseErr))
		} else {
			result.Sources = append(result.Sources, OfficialAnthropicPricingURL)
			for model, override := range projectClaudeOfficialPricing(allowed, parsed) {
				pricing[model] = override
			}
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

// projectClaudeOfficialPricing maps account-advertised Claude models onto the
// official pricing rows. Official rows use stable family IDs while accounts
// often advertise dated variants (e.g. claude-sonnet-4-5-20250929). An exact
// match wins; otherwise the longest official row that prefixes the candidate
// wins, so claude-opus-4-5 resolves to claude-opus-4-5 and never to the
// legacy claude-opus-4 row regardless of map iteration order. With no allowed
// set every official row is imported as-is.
func projectClaudeOfficialPricing(allowed map[string]struct{}, parsed map[string]database.ModelPricingOverride) map[string]database.ModelPricingOverride {
	out := make(map[string]database.ModelPricingOverride)
	if len(allowed) == 0 {
		for model, override := range parsed {
			out[model] = override
		}
		return out
	}
	for model := range allowed {
		if !isClaudeBillingModel(model) {
			continue
		}
		candidate := strings.ReplaceAll(model, ".", "-")
		if override, ok := parsed[candidate]; ok {
			out[model] = override
			continue
		}
		bestKey := ""
		for officialModel := range parsed {
			if strings.HasPrefix(candidate, officialModel+"-") && len(officialModel) > len(bestKey) {
				bestKey = officialModel
			}
		}
		if bestKey != "" {
			out[model] = parsed[bestKey]
		}
	}
	return out
}

// ParseAnthropicOfficialPricingHTML parses the model pricing table published by
// Anthropic. The page is server-rendered HTML and has changed CSS classes several
// times, so parsing is intentionally based on table headers rather than styling.
// Returned cached_input is cache-read (hit) pricing; cache creation prices are
// kept separately in cache_write_5m/cache_write_1h.
func ParseAnthropicOfficialPricingHTML(body []byte) (map[string]database.ModelPricingOverride, error) {
	doc, err := html.Parse(bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	out := make(map[string]database.ModelPricingOverride)
	var walk func(*html.Node)
	walk = func(node *html.Node) {
		if node.Type == html.ElementNode && node.Data == "table" {
			parseAnthropicPricingTable(node, out)
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	if len(out) == 0 {
		return nil, fmt.Errorf("未找到 Anthropic model pricing 表")
	}
	return out, nil
}

func parseAnthropicPricingTable(table *html.Node, out map[string]database.ModelPricingOverride) {
	var rows [][]string
	var collect func(*html.Node)
	collect = func(node *html.Node) {
		if node != table && node.Type == html.ElementNode && node.Data == "tr" {
			cells := make([]string, 0, 8)
			for child := node.FirstChild; child != nil; child = child.NextSibling {
				if child.Type == html.ElementNode && (child.Data == "th" || child.Data == "td") {
					cells = append(cells, strings.TrimSpace(htmlNodeText(child)))
				}
			}
			if len(cells) > 0 {
				rows = append(rows, cells)
			}
			return
		}
		for child := node.FirstChild; child != nil; child = child.NextSibling {
			collect(child)
		}
	}
	collect(table)
	if len(rows) < 2 {
		return
	}
	header := make([]string, len(rows[0]))
	for i, cell := range rows[0] {
		header[i] = strings.ToLower(strings.Join(strings.Fields(cell), " "))
	}
	find := func(needles ...string) int {
		for i, cell := range header {
			matched := true
			for _, needle := range needles {
				if !strings.Contains(cell, needle) {
					matched = false
					break
				}
			}
			if matched {
				return i
			}
		}
		return -1
	}
	modelIdx := find("model")
	inputIdx := find("input")
	write5Idx := find("5m", "cache", "write")
	write1Idx := find("1h", "cache", "write")
	readIdx := find("cache", "hit")
	if readIdx < 0 {
		readIdx = find("cache", "refresh")
	}
	if readIdx < 0 {
		readIdx = find("cache", "read")
	}
	outputIdx := find("output")
	if modelIdx < 0 || inputIdx < 0 || readIdx < 0 || outputIdx < 0 {
		return
	}
	for _, row := range rows[1:] {
		if modelIdx >= len(row) || inputIdx >= len(row) || outputIdx >= len(row) || readIdx >= len(row) {
			continue
		}
		model := normalizeAnthropicPricingModel(row[modelIdx])
		if model == "" || !isClaudeBillingModel(model) {
			continue
		}
		override := database.ModelPricingOverride{
			Input:       parseOfficialPrice(row[inputIdx]),
			CachedInput: parseOfficialPrice(row[readIdx]),
			Output:      parseOfficialPrice(row[outputIdx]),
		}
		if write5Idx >= 0 && write5Idx < len(row) {
			override.CacheWrite5m = parseOfficialPrice(row[write5Idx])
		}
		if write1Idx >= 0 && write1Idx < len(row) {
			override.CacheWrite1h = parseOfficialPrice(row[write1Idx])
		}
		if override.Input > 0 && override.Output > 0 {
			out[model] = override
		}
	}
}

func htmlNodeText(node *html.Node) string {
	if node == nil {
		return ""
	}
	if node.Type == html.TextNode {
		return node.Data
	}
	var b strings.Builder
	for child := node.FirstChild; child != nil; child = child.NextSibling {
		b.WriteString(htmlNodeText(child))
	}
	return b.String()
}

func normalizeAnthropicPricingModel(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	if idx := strings.Index(value, " ("); idx >= 0 {
		value = value[:idx]
	}
	value = strings.NewReplacer(" ", "-", ".", "-").Replace(value)
	return database.CanonicalBillingModelKey(value)
}

func parseOfficialPrice(value string) float64 {
	value = strings.TrimSpace(value)
	if value == "" || value == "-" {
		return 0
	}
	value = strings.TrimPrefix(value, "$")
	value = strings.ReplaceAll(value, ",", "")
	if fields := strings.Fields(value); len(fields) > 0 {
		value = strings.TrimPrefix(fields[0], "$")
	}
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
