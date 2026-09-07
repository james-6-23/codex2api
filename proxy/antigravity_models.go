package proxy

import (
	"fmt"
	"sort"
	"strings"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func (h *Handler) antigravityAcceptedModels() []string {
	ids := antigravityAcceptedModelIDs()
	if h != nil && h.store != nil {
		for _, account := range h.store.Accounts() {
			if account.IsAntigravityAPI() && account.AntigravityDispatchEnabled() {
				ids = append(ids, antigravityPublicModelsForAccount(account)...)
			}
		}
	}
	return ids
}

type antigravityReasoningVariant struct {
	level          string
	wireModel      string
	thinkingBudget int
}

type antigravityPublicModelDefinition struct {
	id                    string
	wireModel             string
	reasoningLevels       []string
	defaultReasoningLevel string
	variants              []antigravityReasoningVariant
}

// antigravityPublicModelCatalog is the exact fixed-tier surface advertised to
// downstream clients. The native Codex manifest folds known variants into one
// base model with separate reasoning controls; these IDs remain callable.
var antigravityPublicModelCatalog = []antigravityPublicModelDefinition{
	{
		id: "gemini-3.5-flash-low", wireModel: "gemini-3.5-flash-extra-low",
		variants: []antigravityReasoningVariant{{level: "low", wireModel: "gemini-3.5-flash-extra-low", thinkingBudget: 1000}},
	},
	{
		id: "gemini-3.5-flash-medium", wireModel: "gemini-3.5-flash-low",
		variants: []antigravityReasoningVariant{{level: "medium", wireModel: "gemini-3.5-flash-low", thinkingBudget: 4000}},
	},
	{
		id: "gemini-3.5-flash-high", wireModel: "gemini-3-flash-agent",
		variants: []antigravityReasoningVariant{{level: "high", wireModel: "gemini-3-flash-agent", thinkingBudget: 10000}},
	},
	{
		id: "gemini-3.6-flash-low", wireModel: "gemini-3.6-flash-low",
		variants: []antigravityReasoningVariant{{level: "low", wireModel: "gemini-3.6-flash-low", thinkingBudget: 4096}},
	},
	{id: "gemini-3.6-flash-medium", wireModel: "gemini-3.6-flash-medium", variants: []antigravityReasoningVariant{{level: "medium", wireModel: "gemini-3.6-flash-medium", thinkingBudget: 8192}}},
	{id: "gemini-3.6-flash-high", wireModel: "gemini-3.6-flash-high", variants: []antigravityReasoningVariant{{level: "high", wireModel: "gemini-3.6-flash-high", thinkingBudget: 24576}}},
	{id: "gemini-3.7-flash-low", wireModel: "gemini-3.7-flash-tiered", variants: []antigravityReasoningVariant{{level: "low", wireModel: "gemini-3.7-flash-tiered", thinkingBudget: 4096}}},
	{id: "gemini-3.7-flash-medium", wireModel: "gemini-3.7-flash-tiered", variants: []antigravityReasoningVariant{{level: "medium", wireModel: "gemini-3.7-flash-tiered", thinkingBudget: 8192}}},
	{id: "gemini-3.7-flash-high", wireModel: "gemini-3.7-flash-tiered", variants: []antigravityReasoningVariant{{level: "high", wireModel: "gemini-3.7-flash-tiered", thinkingBudget: 24576}}},
	{id: "gemini-3.8-flash-low", wireModel: "gemini-3.8-flash-tiered", variants: []antigravityReasoningVariant{{level: "low", wireModel: "gemini-3.8-flash-tiered"}}},
	{id: "gemini-3.8-flash-medium", wireModel: "gemini-3.8-flash-tiered", variants: []antigravityReasoningVariant{{level: "medium", wireModel: "gemini-3.8-flash-tiered"}}},
	{id: "gemini-3.8-flash-high", wireModel: "gemini-3.8-flash-tiered", variants: []antigravityReasoningVariant{{level: "high", wireModel: "gemini-3.8-flash-tiered"}}},
	{id: "gemini-3.1-pro-low", wireModel: "gemini-3.1-pro-low", variants: []antigravityReasoningVariant{{level: "low", wireModel: "gemini-3.1-pro-low", thinkingBudget: 1001}}},
	{id: "gemini-3.1-pro-high", wireModel: "gemini-pro-agent", variants: []antigravityReasoningVariant{{level: "high", wireModel: "gemini-pro-agent", thinkingBudget: 10001}}},
	{id: "claude-opus-4-6-thinking", wireModel: "claude-opus-4-6-thinking"},
	{id: "claude-sonnet-4-6", wireModel: "claude-sonnet-4-6"},
	{id: "gpt-oss-120b-medium", wireModel: "gpt-oss-120b-medium"},
}

// antigravityLogicalCompatibilityCatalog keeps the former logical model names
// callable for existing clients and supplies the native Codex base model names.
var antigravityLogicalCompatibilityCatalog = []antigravityPublicModelDefinition{
	{id: "gemini-3.5-flash", defaultReasoningLevel: "medium", variants: []antigravityReasoningVariant{
		{level: "low", wireModel: "gemini-3.5-flash-extra-low", thinkingBudget: 1000},
		{level: "medium", wireModel: "gemini-3.5-flash-low", thinkingBudget: 4000},
		{level: "high", wireModel: "gemini-3-flash-agent", thinkingBudget: 10000},
	}},
	{id: "gemini-3.6-flash", defaultReasoningLevel: "medium", variants: []antigravityReasoningVariant{
		{level: "low", wireModel: "gemini-3.6-flash-low", thinkingBudget: 4096},
		{level: "medium", wireModel: "gemini-3.6-flash-medium", thinkingBudget: 8192},
		{level: "high", wireModel: "gemini-3.6-flash-high", thinkingBudget: 24576},
	}},
	{id: "gemini-3.7-flash", defaultReasoningLevel: "medium", variants: []antigravityReasoningVariant{
		{level: "low", wireModel: "gemini-3.7-flash-tiered", thinkingBudget: 4096},
		{level: "medium", wireModel: "gemini-3.7-flash-tiered", thinkingBudget: 8192},
		{level: "high", wireModel: "gemini-3.7-flash-tiered", thinkingBudget: 24576},
	}},
	{id: "gemini-3.8-flash", defaultReasoningLevel: "low", variants: []antigravityReasoningVariant{
		{level: "low", wireModel: "gemini-3.8-flash-tiered"},
		{level: "medium", wireModel: "gemini-3.8-flash-tiered"},
		{level: "high", wireModel: "gemini-3.8-flash-tiered"},
	}},
	{id: "gemini-3.1-pro", defaultReasoningLevel: "high", variants: []antigravityReasoningVariant{
		{level: "low", wireModel: "gemini-3.1-pro-low", thinkingBudget: 1001},
		{level: "high", wireModel: "gemini-pro-agent", thinkingBudget: 10001},
	}},
}

func antigravityPublicModel(model string) (antigravityPublicModelDefinition, bool) {
	name := strings.ToLower(strings.TrimSpace(model))
	if name == "" {
		return antigravityPublicModelDefinition{}, false
	}
	for _, definition := range antigravityPublicModelCatalog {
		if definition.id == name {
			return definition, true
		}
	}
	return antigravityPublicModelDefinition{}, false
}

func antigravityLogicalCompatibilityModel(model string) (antigravityPublicModelDefinition, bool) {
	name := strings.ToLower(strings.TrimSpace(model))
	for _, definition := range antigravityLogicalCompatibilityCatalog {
		if definition.id == name {
			return definition, true
		}
	}
	return antigravityPublicModelDefinition{}, false
}

// antigravityCompatibilityAlias is retained for internal/test compatibility
// with older callers that treated fixed-tier IDs as aliases. They are now
// first-class public IDs, but still resolve to the same fixed variant.
func antigravityCompatibilityAlias(model string) (antigravityReasoningVariant, bool) {
	definition, ok := antigravityPublicModel(model)
	if !ok || len(definition.variants) != 1 {
		return antigravityReasoningVariant{}, false
	}
	return definition.variants[0], true
}

func antigravityPublicModelIDs() []string {
	models := make([]string, 0, len(antigravityPublicModelCatalog))
	for _, definition := range antigravityPublicModelCatalog {
		models = append(models, definition.id)
	}
	return models
}

func antigravityAcceptedModelIDs() []string {
	models := antigravityPublicModelIDs()
	for _, definition := range antigravityLogicalCompatibilityCatalog {
		models = append(models, definition.id)
	}
	return models
}

func antigravityPublicModelWireID(model string) (string, bool) {
	definition, ok := antigravityPublicModel(model)
	if !ok {
		if logical, logicalOK := antigravityLogicalCompatibilityModel(model); logicalOK {
			variant, variantOK := antigravityVariantForDefinition(logical, nil)
			return variant.wireModel, variantOK
		}
		return "", false
	}
	if definition.wireModel != "" {
		return definition.wireModel, true
	}
	variant, ok := antigravityVariantForDefinition(definition, nil)
	return variant.wireModel, ok
}

// AntigravityWireModelID returns the default backing for a logical model and
// the fixed backing for a compatibility alias.
func AntigravityWireModelID(model string) (string, bool) {
	return antigravityPublicModelWireID(model)
}

// AntigravityWireModelIDs returns every physical backing required to serve all
// advertised reasoning levels of a logical model. Admin persistence uses this
// to avoid creating an account that advertises tiers it cannot route.
func AntigravityWireModelIDs(model string) []string {
	definition, ok := antigravityPublicModel(model)
	if !ok {
		definition, ok = antigravityLogicalCompatibilityModel(model)
		if !ok {
			return nil
		}
	}
	if definition.wireModel != "" {
		return []string{definition.wireModel}
	}
	seen := make(map[string]struct{}, len(definition.variants))
	wires := make([]string, 0, len(definition.variants))
	for _, variant := range definition.variants {
		if _, exists := seen[variant.wireModel]; exists {
			continue
		}
		seen[variant.wireModel] = struct{}{}
		wires = append(wires, variant.wireModel)
	}
	return wires
}

// AntigravityPublishedModelIDs projects a synchronized raw/wire catalog into
// fixed-tier downstream model IDs. Multiple public tiers may intentionally use
// one wire model with different thinking budgets (Gemini 3.7).
func AntigravityPublishedModelIDs(rawModels []string) []string {
	available := make(map[string]struct{}, len(rawModels))
	for _, model := range rawModels {
		name := strings.ToLower(strings.TrimSpace(model))
		if name != "" {
			available[name] = struct{}{}
		}
	}
	models := make([]string, 0, len(antigravityPublicModelCatalog))
	for _, definition := range antigravityPublicModelCatalog {
		wires := AntigravityWireModelIDs(definition.id)
		if len(wires) == 0 {
			continue
		}
		complete := true
		for _, wire := range wires {
			if _, ok := available[wire]; !ok {
				complete = false
				break
			}
		}
		if complete {
			models = append(models, definition.id)
		}
	}
	// Unrecognized models keep their actual upstream ID. Do not infer thinking
	// tiers, aliases, or modality support from a future version number.
	var discovered []string
	seenDiscovered := make(map[string]bool)
	for _, rawModel := range rawModels {
		model := strings.TrimSpace(rawModel)
		name := strings.ToLower(model)
		// Provider-internal chat placeholders, IDE tab completions, and an
		// alternate physical backing of a known base are not extra chat models.
		if name == "chat_20706" || name == "chat_23310" || strings.HasPrefix(name, "tab_") {
			continue
		}
		if strings.HasSuffix(name, "-tiered") {
			if _, known := antigravityLogicalCompatibilityModel(strings.TrimSuffix(name, "-tiered")); known {
				continue
			}
		}
		// These old IDs currently identify Gemini 3.1 Flash Lite in the upstream
		// catalog. Hide redundant menu entries when its actual public ID exists;
		// this does not rewrite callers' existing model requests.
		if _, hasLite := available["gemini-3.1-flash-lite"]; hasLite && (name == "gemini-2.5-flash" || name == "gemini-2.5-flash-lite" || name == "gemini-2.5-flash-thinking") {
			continue
		}
		if !antigravityKnownWireModel(model) && antigravityResponsesTextModel(model) {
			if _, known := antigravityPublicModel(model); !known && !seenDiscovered[model] {
				discovered = append(discovered, model)
				seenDiscovered[model] = true
			}
		}
	}
	sort.Strings(discovered)
	models = append(models, discovered...)
	return models
}

func antigravityKnownWireModel(model string) bool {
	for _, definition := range antigravityPublicModelCatalog {
		for _, wire := range AntigravityWireModelIDs(definition.id) {
			if strings.EqualFold(model, wire) {
				return true
			}
		}
	}
	return false
}

func antigravityPublicModelsForAccount(account *auth.Account) []string {
	if account == nil {
		return nil
	}
	return AntigravityPublishedModelIDs(account.AntigravityModels())
}

func antigravityAccountSupportsPublicModel(account *auth.Account, model string) bool {
	_, ok := antigravityResolvePublicModelForAccount(account, model)
	return ok
}

// antigravityResolvePublicModelForAccount accepts both logical models and old
// aliases, while requiring the physical backing(s) needed by that contract.
func antigravityResolvePublicModelForAccount(account *auth.Account, model string) (string, bool) {
	if account == nil {
		return "", false
	}
	wires := AntigravityWireModelIDs(model)
	if len(wires) == 0 {
		if !antigravityKnownWireModel(model) && antigravityResponsesTextModel(model) {
			for _, actual := range account.AntigravityModels() {
				if strings.EqualFold(strings.TrimSpace(actual), strings.TrimSpace(model)) {
					return strings.TrimSpace(actual), true
				}
			}
		}
		return "", false
	}
	available := make(map[string]struct{}, len(account.AntigravityModels()))
	for _, item := range account.AntigravityModels() {
		available[strings.ToLower(strings.TrimSpace(item))] = struct{}{}
	}
	for _, wire := range wires {
		if _, ok := available[wire]; !ok {
			return "", false
		}
	}
	defaultWire, ok := antigravityPublicModelWireID(model)
	return defaultWire, ok
}

// antigravityResponsesTextModel is a transport compatibility guard, not an
// advertising source. Raw backing IDs remain callable internally.
func antigravityResponsesTextModel(model string) bool {
	name := strings.ToLower(strings.TrimSpace(model))
	if name == "" {
		return false
	}
	return !strings.HasPrefix(name, "imagen") &&
		!strings.Contains(name, "-image") &&
		!strings.Contains(name, "image-")
}

func antigravityCodexReasoningLevels(model string) []string {
	definition, ok := antigravityPublicModel(model)
	if !ok || len(definition.reasoningLevels) == 0 {
		return nil
	}
	return append([]string(nil), definition.reasoningLevels...)
}

func antigravityVariantForDefinition(definition antigravityPublicModelDefinition, reasoning map[string]any) (antigravityReasoningVariant, bool) {
	if len(definition.variants) == 0 {
		return antigravityReasoningVariant{}, false
	}
	if len(definition.variants) == 1 {
		return definition.variants[0], true
	}
	level := definition.defaultReasoningLevel
	if len(reasoning) > 0 {
		level = antigravityGeminiReasoningTier(reasoning)
	}
	for _, variant := range definition.variants {
		if variant.level == level {
			return variant, true
		}
	}
	// Pro exposes only low/high; medium and unknown values use its documented
	// default rather than inventing a third tier.
	for _, variant := range definition.variants {
		if variant.level == definition.defaultReasoningLevel {
			return variant, true
		}
	}
	return antigravityReasoningVariant{}, false
}

func antigravityResolvedVariant(model string, reasoning map[string]any) (antigravityReasoningVariant, bool) {
	// A configured redirect turns a bare logical model into a fixed tier before
	// the usual effort-based selection; fixed tiers are never redirected.
	if target, ok := antigravityRedirectedModel(model, reasoning); ok {
		model = target
	}
	definition, ok := antigravityPublicModel(model)
	if !ok {
		definition, ok = antigravityLogicalCompatibilityModel(model)
		if !ok {
			return antigravityReasoningVariant{}, false
		}
	}
	return antigravityVariantForDefinition(definition, reasoning)
}

// AntigravityRedirectChoice describes one bare logical model and the fixed
// public tiers an operator may redirect it to.
type AntigravityRedirectChoice struct {
	Model        string   `json:"model"`
	DefaultLevel string   `json:"default_level"`
	Tiers        []string `json:"tiers"`
}

// AntigravityRedirectChoices lists every logical model that can carry a
// redirect, in catalog order, with its tier IDs (`<model>-<level>`).
func AntigravityRedirectChoices() []AntigravityRedirectChoice {
	choices := make([]AntigravityRedirectChoice, 0, len(antigravityLogicalCompatibilityCatalog))
	for _, logical := range antigravityLogicalCompatibilityCatalog {
		tiers := make([]string, 0, len(logical.variants))
		for _, variant := range logical.variants {
			tier := logical.id + "-" + variant.level
			if _, ok := antigravityPublicModel(tier); ok {
				tiers = append(tiers, tier)
			}
		}
		choices = append(choices, AntigravityRedirectChoice{Model: logical.id, DefaultLevel: logical.defaultReasoningLevel, Tiers: tiers})
	}
	return choices
}

// ValidateAntigravityModelRedirects checks that every redirect maps a bare
// logical model to one of its own fixed tiers. Anything else (a tier as the
// key, a tier of a different model, a raw wire ID) is rejected so the
// settings page cannot persist a rule the adapter would silently ignore.
func ValidateAntigravityModelRedirects(redirects map[string]string) error {
	for model, target := range redirects {
		logical, ok := antigravityLogicalCompatibilityModel(model)
		if !ok {
			return fmt.Errorf("model %q is not a redirectable logical model", model)
		}
		valid := false
		for _, variant := range logical.variants {
			if strings.EqualFold(strings.TrimSpace(target), logical.id+"-"+variant.level) {
				valid = true
				break
			}
		}
		if !valid {
			return fmt.Errorf("model %q cannot be redirected to %q", model, target)
		}
	}
	return nil
}

// antigravityRedirectedModel resolves the configured fixed tier for a bare
// logical model. By default the redirect only fills in a missing
// reasoning.effort; with the override flag it also replaces an explicit one.
func antigravityRedirectedModel(model string, reasoning map[string]any) (string, bool) {
	logical, ok := antigravityLogicalCompatibilityModel(model)
	if !ok {
		return "", false
	}
	target := auth.AntigravityModelRedirect(logical.id)
	if target == "" {
		return "", false
	}
	if _, known := antigravityPublicModel(target); !known {
		return "", false
	}
	if lowerStringField(reasoning, "effort") != "" && !auth.AntigravityRedirectOverridesEffort() {
		return "", false
	}
	return target, true
}

// antigravityFoldLogicalModel rewrites a bare logical model in a Responses
// body into the fixed tier it will actually run as: the configured redirect
// first, otherwise the request's reasoning.effort (bare requests default to
// low). It returns the rewritten body and the tier ID, or an API error for an
// effort the model does not offer.
func antigravityFoldLogicalModel(rawBody []byte, requestModel string) ([]byte, string, *api.APIError) {
	logical, known := antigravityLogicalCompatibilityModel(requestModel)
	if !known {
		return rawBody, requestModel, nil
	}
	effort := strings.ToLower(strings.TrimSpace(gjson.GetBytes(rawBody, "reasoning.effort").String()))
	reasoning := map[string]any{}
	if effort != "" {
		reasoning["effort"] = effort
	}
	if target, ok := antigravityRedirectedModel(logical.id, reasoning); ok {
		rawBody, _ = sjson.SetBytes(rawBody, "model", target)
		return rawBody, target, nil
	}
	if effort == "" {
		effort = "low"
	}
	allowedEfforts := make([]string, 0, len(logical.variants))
	validEffort := false
	for _, option := range logical.variants {
		allowedEfforts = append(allowedEfforts, option.level)
		validEffort = validEffort || effort == option.level
	}
	if !validEffort {
		return rawBody, requestModel, api.NewAPIError(api.ErrCodeInvalidParameter, "Model "+logical.id+" supports reasoning.effort: "+strings.Join(allowedEfforts, ", "), api.ErrorTypeInvalidRequest)
	}
	variant, ok := antigravityVariantForDefinition(logical, map[string]any{"effort": effort})
	if !ok {
		return rawBody, requestModel, nil
	}
	mapped := logical.id + "-" + variant.level
	rawBody, _ = sjson.SetBytes(rawBody, "model", mapped)
	return rawBody, mapped, nil
}
