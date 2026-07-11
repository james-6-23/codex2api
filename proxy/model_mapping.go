package proxy

import (
	"encoding/json"
	"strings"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type modelMappingRule struct {
	From       string
	To         string
	Index      int
	Wildcard   bool
	LiteralLen int
	StarCount  int
}

func parseModelMappingRules(mappingJSON string) []modelMappingRule {
	mappingJSON = strings.TrimSpace(mappingJSON)
	if mappingJSON == "" || mappingJSON == "{}" {
		return nil
	}

	dec := json.NewDecoder(strings.NewReader(mappingJSON))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return nil
	}
	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return nil
	}

	rules := make([]modelMappingRule, 0)
	index := 0
	for dec.More() {
		keyTok, err := dec.Token()
		if err != nil {
			return nil
		}
		key, ok := keyTok.(string)
		if !ok {
			return nil
		}
		var rawValue any
		if err := dec.Decode(&rawValue); err != nil {
			return nil
		}
		value, ok := rawValue.(string)
		if !ok {
			continue
		}

		from := strings.TrimSpace(key)
		to := strings.TrimSpace(value)
		if from == "" || to == "" {
			continue
		}
		starCount := strings.Count(from, "*")
		rules = append(rules, modelMappingRule{
			From:       from,
			To:         to,
			Index:      index,
			Wildcard:   starCount > 0,
			LiteralLen: len(strings.ReplaceAll(from, "*", "")),
			StarCount:  starCount,
		})
		index++
	}
	return rules
}

func resolveConfiguredModelMapping(model string, mappingJSON string, supportedModels []string) (string, bool) {
	return resolveModelMappingFromRules(model, parseModelMappingRules(mappingJSON), supportedModels)
}

func resolveModelMappingFromRules(model string, rules []modelMappingRule, supportedModels []string) (string, bool) {
	model = strings.TrimSpace(model)
	if model == "" {
		return "", false
	}

	if len(rules) == 0 {
		return model, false
	}

	for _, rule := range rules {
		if !rule.Wildcard && strings.EqualFold(rule.From, model) {
			return canonicalizeCodexModel(rule.To, supportedModels), true
		}
	}

	var best *modelMappingRule
	for i := range rules {
		rule := &rules[i]
		if !rule.Wildcard || !wildcardModelPatternMatch(rule.From, model) {
			continue
		}
		if best == nil || isMoreSpecificModelMapping(rule, best) {
			best = rule
		}
	}
	if best != nil {
		return canonicalizeCodexModel(best.To, supportedModels), true
	}
	return model, false
}

func isMoreSpecificModelMapping(candidate, current *modelMappingRule) bool {
	if candidate.LiteralLen != current.LiteralLen {
		return candidate.LiteralLen > current.LiteralLen
	}
	if candidate.StarCount != current.StarCount {
		return candidate.StarCount < current.StarCount
	}
	return candidate.Index < current.Index
}

func wildcardModelPatternMatch(pattern string, model string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	model = strings.ToLower(strings.TrimSpace(model))
	if pattern == "" {
		return model == ""
	}
	if !strings.Contains(pattern, "*") {
		return pattern == model
	}

	parts := strings.Split(pattern, "*")
	position := 0
	if first := parts[0]; first != "" {
		if !strings.HasPrefix(model, first) {
			return false
		}
		position = len(first)
	}

	for i := 1; i < len(parts); i++ {
		part := parts[i]
		if part == "" {
			continue
		}
		idx := strings.Index(model[position:], part)
		if idx < 0 {
			return false
		}
		position += idx + len(part)
	}

	if last := parts[len(parts)-1]; last != "" {
		return strings.HasSuffix(model, last)
	}
	return true
}

func applyReasoningEffortModelToBody(rawBody []byte, entry ReasoningEffortModel) ([]byte, error) {
	updatedBody, err := sjson.SetBytes(rawBody, "model", entry.Model)
	if err != nil {
		return rawBody, err
	}
	updatedBody, err = sjson.SetBytes(updatedBody, "reasoning_effort", entry.Effort)
	if err != nil {
		return rawBody, err
	}
	updatedBody, err = sjson.SetBytes(updatedBody, "reasoning.effort", entry.Effort)
	if err != nil {
		return rawBody, err
	}
	return updatedBody, nil
}

func (h *Handler) applyConfiguredModelMappingToBody(rawBody []byte, supportedModels []string) ([]byte, string, string, bool) {
	originalModel := strings.TrimSpace(gjson.GetBytes(rawBody, "model").String())
	effectiveModel := originalModel
	if originalModel == "" || !gjson.ValidBytes(rawBody) || h == nil || h.store == nil {
		return rawBody, originalModel, effectiveModel, false
	}

	updatedBody := rawBody
	modelForMapping := originalModel
	mappingApplied := false
	if entry, ok := resolveReasoningEffortModelAlias(originalModel, h.store.GetReasoningEffortModels(), supportedModels); ok {
		var err error
		updatedBody, err = applyReasoningEffortModelToBody(updatedBody, entry)
		if err != nil {
			return rawBody, originalModel, effectiveModel, false
		}
		modelForMapping = entry.Model
		effectiveModel = entry.Model
		mappingApplied = !strings.EqualFold(originalModel, entry.Model)
	}

	mappedModel, ok := resolveConfiguredModelMapping(modelForMapping, h.store.GetCodexModelMapping(), supportedModels)
	if ok && mappedModel != "" && !strings.EqualFold(mappedModel, modelForMapping) {
		var err error
		updatedBody, err = sjson.SetBytes(updatedBody, "model", mappedModel)
		if err != nil {
			return rawBody, originalModel, effectiveModel, mappingApplied
		}
		effectiveModel = mappedModel
		mappingApplied = true
	}
	return updatedBody, originalModel, effectiveModel, mappingApplied
}

func resolveAccountModelMapping(account *auth.Account, model string) (string, bool) {
	model = strings.TrimSpace(model)
	if account == nil || model == "" {
		return model, false
	}
	accountModels := account.OpenAIResponsesModels()
	if len(accountModels) == 0 {
		return model, false
	}
	mappedModel, ok := resolveConfiguredModelMapping(model, account.OpenAIResponsesModelMapping(), accountModels)
	if !ok || mappedModel == "" {
		return model, false
	}
	return mappedModel, true
}

func ResolveAccountModelMapping(account *auth.Account, model string) (string, bool) {
	return resolveAccountModelMapping(account, model)
}

func resolveAccountModelMappingForCandidates(account *auth.Account, models ...string) (string, bool) {
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		key := strings.ToLower(model)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		if mappedModel, ok := resolveAccountModelMapping(account, model); ok && mappedModel != "" {
			return mappedModel, true
		}
	}
	return "", false
}

func accountModelMappingAliases(account *auth.Account) []string {
	if account == nil {
		return nil
	}
	accountModels := account.OpenAIResponsesModels()
	if len(accountModels) == 0 {
		return nil
	}
	rules := parseModelMappingRules(account.OpenAIResponsesModelMapping())
	if len(rules) == 0 {
		return nil
	}
	aliases := make([]string, 0, len(rules))
	for _, rule := range rules {
		if rule.Wildcard {
			continue
		}
		mappedModel, ok := resolveConfiguredModelMapping(rule.From, account.OpenAIResponsesModelMapping(), accountModels)
		if !ok || mappedModel == "" || !account.SupportsOpenAIResponsesModel(mappedModel) {
			continue
		}
		aliases = append(aliases, rule.From)
	}
	return aliases
}

func (h *Handler) applyAccountModelMappingToBody(rawBody []byte, account *auth.Account) ([]byte, string, bool) {
	return h.applyAccountModelMappingToBodyForModels(rawBody, account)
}

func (h *Handler) applyAccountModelMappingToBodyForModels(rawBody []byte, account *auth.Account, modelCandidates ...string) ([]byte, string, bool) {
	model := strings.TrimSpace(gjson.GetBytes(rawBody, "model").String())
	if model == "" || !gjson.ValidBytes(rawBody) || account == nil || !account.IsOpenAIResponsesAPI() {
		return rawBody, model, false
	}
	modelCandidates = append(modelCandidates, model)
	mappedModel, ok := resolveAccountModelMappingForCandidates(account, modelCandidates...)
	if !ok || mappedModel == "" || strings.EqualFold(mappedModel, model) {
		return rawBody, model, false
	}
	updatedBody, err := sjson.SetBytes(rawBody, "model", mappedModel)
	if err != nil {
		return rawBody, model, false
	}
	return updatedBody, mappedModel, true
}

func (h *Handler) resolveConfiguredRequestModel(model string, supportedModels []string) (string, bool) {
	model = strings.TrimSpace(model)
	if model == "" || h == nil || h.store == nil {
		return model, false
	}
	resolved := false
	if entry, ok := resolveReasoningEffortModelAlias(model, h.store.GetReasoningEffortModels(), supportedModels); ok {
		model = entry.Model
		resolved = true
	}
	mappedModel, ok := resolveConfiguredModelMapping(model, h.store.GetCodexModelMapping(), supportedModels)
	if !ok || mappedModel == "" || mappedModel == model {
		return model, resolved
	}
	return mappedModel, true
}

func usageEffectiveModelForMapping(originalModel string, effectiveModel string, mapped bool) string {
	if !mapped {
		return ""
	}
	originalModel = strings.TrimSpace(originalModel)
	effectiveModel = strings.TrimSpace(effectiveModel)
	if originalModel == "" || effectiveModel == "" || strings.EqualFold(originalModel, effectiveModel) {
		return ""
	}
	return effectiveModel
}

// compactOpenAIModelSuffix 是 newapi 等聚合网关为 /responses/compact 请求追加的模型名
// 后缀。例如 newapi 把 gpt-5.4 路由到 compact 端点时会发送 gpt-5.4-openai-compact，
// 而真实 Codex 上游只认识基础模型名 gpt-5.4。
const compactOpenAIModelSuffix = "-openai-compact"

// stripCompactModelSuffix 去除 compact 模型名上的 -openai-compact 后缀，返回去除后的
// 模型名与是否发生改写。仅供 /v1/responses/compact 使用：让 newapi 侧渠道仍能以
// gpt-5.4-openai-compact 命名，而 codex2api 内部按 gpt-5.4 校验与转发上游。
func stripCompactModelSuffix(model string) (string, bool) {
	trimmed := strings.TrimSpace(model)
	if trimmed == "" {
		return model, false
	}
	if !strings.HasSuffix(strings.ToLower(trimmed), compactOpenAIModelSuffix) {
		return model, false
	}
	stripped := strings.TrimSpace(trimmed[:len(trimmed)-len(compactOpenAIModelSuffix)])
	if stripped == "" {
		return model, false
	}
	return stripped, true
}

// stripCompactModelSuffixFromBody 若请求体的 model 带 -openai-compact 后缀则去除，
// 返回改写后的 body、去后缀的模型名与是否改写。JSON 无效或无 model 字段时原样返回。
func stripCompactModelSuffixFromBody(rawBody []byte) ([]byte, string, bool) {
	model := strings.TrimSpace(gjson.GetBytes(rawBody, "model").String())
	stripped, ok := stripCompactModelSuffix(model)
	if !ok {
		return rawBody, model, false
	}
	updated, err := sjson.SetBytes(rawBody, "model", stripped)
	if err != nil {
		return rawBody, model, false
	}
	return updated, stripped, true
}

// compactMappingCandidate 是 compact 请求做模型映射时的一个尝试名。synthetic 标记
// 该名字是内部拼出的 <model>-openai-compact 别名（客户端并未真的发它）：这类别名
// 只允许命中显式以 -openai-compact 结尾的规则，避免 "gpt-*"、"*" 之类通用规则被
// 合成别名劫持、压过基础名本应命中的规则。
type compactMappingCandidate struct {
	model     string
	synthetic bool
}

// compactMappingCandidates returns endpoint-qualified aliases before their
// base names so compact-specific rules override general model rules.
func compactMappingCandidates(models ...string) []compactMappingCandidate {
	candidates := make([]compactMappingCandidate, 0, len(models)*2)
	seen := make(map[string]struct{}, len(models)*2)
	add := func(model string, synthetic bool) {
		model = strings.TrimSpace(model)
		if model == "" {
			return
		}
		key := strings.ToLower(model)
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		candidates = append(candidates, compactMappingCandidate{model: model, synthetic: synthetic})
	}

	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if baseModel, stripped := stripCompactModelSuffix(model); stripped {
			add(model, false)
			add(baseModel, false)
			continue
		}
		add(model+compactOpenAIModelSuffix, true)
		add(model, false)
	}
	return candidates
}

// compactAliasScopedRules 过滤出显式针对 -openai-compact 别名的规则（From 以该
// 后缀结尾，含 "*-openai-compact" 这类通配）。合成别名只在这些规则里匹配。
func compactAliasScopedRules(rules []modelMappingRule) []modelMappingRule {
	scoped := make([]modelMappingRule, 0, len(rules))
	for _, rule := range rules {
		if strings.HasSuffix(strings.ToLower(rule.From), compactOpenAIModelSuffix) {
			scoped = append(scoped, rule)
		}
	}
	return scoped
}

// normalizeCompactMappingTarget 剥离映射目标上的 -openai-compact 后缀。codex2api
// 发往上游的始终是 /v1/responses/compact 端点，该后缀只是入站别名约定；把它原样
// 转发会让上游按字面模型名查找而失败（中转模型列表里的 xxx-openai-compact 通常
// 只是路由标记，不是可直接请求的模型）。
func normalizeCompactMappingTarget(model string) string {
	if base, stripped := stripCompactModelSuffix(model); stripped {
		return base
	}
	return model
}

// resolveAccountCompactModelMappingForCandidates 按候选顺序解析账号级映射：
// 合成别名只匹配 compact 专用规则，真实名字走全部规则；命中目标统一去后缀。
func resolveAccountCompactModelMappingForCandidates(account *auth.Account, candidates []compactMappingCandidate) (string, bool) {
	if account == nil {
		return "", false
	}
	accountModels := account.OpenAIResponsesModels()
	if len(accountModels) == 0 {
		return "", false
	}
	rules := parseModelMappingRules(account.OpenAIResponsesModelMapping())
	if len(rules) == 0 {
		return "", false
	}
	scopedRules := compactAliasScopedRules(rules)
	for _, candidate := range candidates {
		candidateRules := rules
		if candidate.synthetic {
			candidateRules = scopedRules
		}
		mappedModel, ok := resolveModelMappingFromRules(candidate.model, candidateRules, accountModels)
		if !ok || mappedModel == "" {
			continue
		}
		return normalizeCompactMappingTarget(mappedModel), true
	}
	return "", false
}

// applyAccountCompactModelMappingToBody 是 compact 链路的账号级映射改写：
// 与 applyAccountModelMappingToBodyForModels 相同的语义，但候选带 synthetic
// 标记且映射目标不允许携带 -openai-compact 后缀进入上游请求体。
func (h *Handler) applyAccountCompactModelMappingToBody(rawBody []byte, account *auth.Account, modelCandidates ...string) ([]byte, string, bool) {
	model := strings.TrimSpace(gjson.GetBytes(rawBody, "model").String())
	if model == "" || !gjson.ValidBytes(rawBody) || account == nil || !account.IsOpenAIResponsesAPI() {
		return rawBody, model, false
	}
	candidates := compactMappingCandidates(append(modelCandidates, model)...)
	mappedModel, ok := resolveAccountCompactModelMappingForCandidates(account, candidates)
	if !ok || mappedModel == "" || strings.EqualFold(mappedModel, model) {
		return rawBody, model, false
	}
	updatedBody, err := sjson.SetBytes(rawBody, "model", mappedModel)
	if err != nil {
		return rawBody, model, false
	}
	return updatedBody, mappedModel, true
}

// applyConfiguredCompactModelMappingToBody resolves the endpoint-qualified
// compact alias before the base model. This lets the same rule work whether a
// client sends "model" or "model-openai-compact", while preserving the full
// requested name for per-account routing and usage logs.
func (h *Handler) applyConfiguredCompactModelMappingToBody(rawBody []byte, supportedModels []string) ([]byte, string, string, bool) {
	originalModel := strings.TrimSpace(gjson.GetBytes(rawBody, "model").String())
	if originalModel == "" || !gjson.ValidBytes(rawBody) || h == nil || h.store == nil {
		return rawBody, originalModel, originalModel, false
	}

	if candidates := compactMappingCandidates(originalModel); len(candidates) > 0 {
		compactAlias := candidates[0]
		rules := parseModelMappingRules(h.store.GetCodexModelMapping())
		if compactAlias.synthetic {
			rules = compactAliasScopedRules(rules)
		}
		if mappedModel, ok := resolveModelMappingFromRules(compactAlias.model, rules, supportedModels); ok && mappedModel != "" && !strings.EqualFold(mappedModel, compactAlias.model) {
			mappedModel = normalizeCompactMappingTarget(mappedModel)
			effectiveModel := mappedModel
			updatedBody := rawBody
			var err error
			if entry, ok := resolveReasoningEffortModelAlias(mappedModel, h.store.GetReasoningEffortModels(), supportedModels); ok {
				updatedBody, err = applyReasoningEffortModelToBody(updatedBody, entry)
				effectiveModel = entry.Model
			} else {
				updatedBody, err = sjson.SetBytes(updatedBody, "model", mappedModel)
			}
			if err != nil {
				return rawBody, originalModel, originalModel, false
			}
			return updatedBody, originalModel, effectiveModel, true
		}
	}

	baseBody, baseModel, stripped := stripCompactModelSuffixFromBody(rawBody)
	if !stripped {
		baseBody = rawBody
		baseModel = originalModel
	}
	mappedBody, _, effectiveModel, mappingApplied := h.applyConfiguredModelMappingToBody(baseBody, supportedModels)
	if effectiveModel == "" {
		effectiveModel = baseModel
	}
	return mappedBody, originalModel, effectiveModel, mappingApplied || stripped
}
