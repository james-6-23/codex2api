package proxy

import (
	"testing"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

func TestResolveConfiguredModelMappingExactAndWildcard(t *testing.T) {
	mapping := `{
		"gpt-*": "gpt-5.5",
		"gpt-5.4": "gpt-5.2",
		"*-mini": "gpt-5.4-mini",
		"*codex*": "gpt-5.3-codex"
	}`
	supported := []string{"gpt-5.5", "gpt-5.4-mini", "gpt-5.3-codex", "gpt-5.2"}

	tests := []struct {
		name  string
		model string
		want  string
	}{
		{name: "exact beats wildcard", model: "gpt-5.4", want: "gpt-5.2"},
		{name: "prefix wildcard", model: "gpt-5.1", want: "gpt-5.5"},
		{name: "suffix wildcard", model: "custom-mini", want: "gpt-5.4-mini"},
		{name: "substring wildcard", model: "my-codex-alias", want: "gpt-5.3-codex"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := resolveConfiguredModelMapping(tt.model, mapping, supported)
			if !ok {
				t.Fatalf("expected mapping for %q", tt.model)
			}
			if got != tt.want {
				t.Fatalf("resolveConfiguredModelMapping(%q) = %q, want %q", tt.model, got, tt.want)
			}
		})
	}
}

func TestResolveConfiguredModelMappingSpecificWildcardWins(t *testing.T) {
	mapping := `{
		"gpt-*": "gpt-5.5",
		"gpt-5.4-*": "gpt-5.4-mini"
	}`
	got, ok := resolveConfiguredModelMapping("gpt-5.4-preview", mapping, []string{"gpt-5.5", "gpt-5.4-mini"})
	if !ok {
		t.Fatal("expected wildcard mapping")
	}
	if got != "gpt-5.4-mini" {
		t.Fatalf("mapped model = %q, want gpt-5.4-mini", got)
	}
}

func TestResolveConfiguredModelMappingCanonicalizesTargetAlias(t *testing.T) {
	got, ok := resolveConfiguredModelMapping("legacy-model", `{"legacy-model":"gpt5.5"}`, []string{"gpt-5.5"})
	if !ok {
		t.Fatal("expected exact mapping")
	}
	if got != "gpt-5.5" {
		t.Fatalf("mapped model = %q, want gpt-5.5", got)
	}

	got, ok = resolveConfiguredModelMapping("legacy-model", `{"legacy-model":"GPT-5.5"}`, []string{"gpt-5.5"})
	if !ok {
		t.Fatal("expected exact mapping")
	}
	if got != "gpt-5.5" {
		t.Fatalf("case-normalized mapped model = %q, want gpt-5.5", got)
	}
}

func TestResolveConfiguredModelMappingIgnoresInvalidJSON(t *testing.T) {
	got, ok := resolveConfiguredModelMapping("gpt-5.4", `{bad json`, []string{"gpt-5.5"})
	if ok {
		t.Fatal("invalid JSON should not map")
	}
	if got != "gpt-5.4" {
		t.Fatalf("model = %q, want original", got)
	}
}

func TestResolveAnthropicModelUsesWildcardBeforeDefaultFallback(t *testing.T) {
	got := resolveAnthropicModel("claude-opus-4-7", `{"claude-opus-*":"gpt-5.5"}`, []string{"gpt-5.5", "gpt-5.4"})
	if got != "gpt-5.5" {
		t.Fatalf("resolveAnthropicModel wildcard = %q, want gpt-5.5", got)
	}

	got = resolveAnthropicModel("claude-haiku-4-5", `{}`, []string{"gpt-5.4-mini"})
	if got != "gpt-5.4-mini" {
		t.Fatalf("resolveAnthropicModel default = %q, want gpt-5.4-mini", got)
	}
}

func TestApplyConfiguredModelMappingToBodyRewritesBeforeValidation(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	store.SetCodexModelMapping(`{"gpt-legacy-*":"gpt-5.5"}`)
	handler := NewHandler(store, nil, nil, nil)

	body, original, effective, mapped := handler.applyConfiguredModelMappingToBody(
		[]byte(`{"model":"gpt-legacy-1","input":"hello"}`),
		[]string{"gpt-5.5"},
	)
	if !mapped {
		t.Fatal("expected body model to be mapped")
	}
	if original != "gpt-legacy-1" || effective != "gpt-5.5" {
		t.Fatalf("original/effective = %q/%q, want gpt-legacy-1/gpt-5.5", original, effective)
	}
	if got := gjson.GetBytes(body, "model").String(); got != "gpt-5.5" {
		t.Fatalf("body model = %q, want gpt-5.5; body=%s", got, body)
	}
}

func TestApplyReasoningEffortModelAliasToBody(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	store.SetReasoningEffortModels(`[{"model":"gpt-5.5","effort":"xhigh"}]`)
	handler := NewHandler(store, nil, nil, nil)

	body, original, effective, mapped := handler.applyConfiguredModelMappingToBody(
		[]byte(`{"model":"gpt-5.5(xhigh)","input":"hello"}`),
		[]string{"gpt-5.5", "gpt-5.5(xhigh)"},
	)
	if !mapped {
		t.Fatal("expected alias to be resolved")
	}
	if original != "gpt-5.5(xhigh)" || effective != "gpt-5.5" {
		t.Fatalf("original/effective = %q/%q, want gpt-5.5(xhigh)/gpt-5.5", original, effective)
	}
	if got := gjson.GetBytes(body, "model").String(); got != "gpt-5.5" {
		t.Fatalf("body model = %q, want gpt-5.5; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "reasoning_effort").String(); got != "xhigh" {
		t.Fatalf("reasoning_effort = %q, want xhigh; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "reasoning.effort").String(); got != "xhigh" {
		t.Fatalf("reasoning.effort = %q, want xhigh; body=%s", got, body)
	}
}

// ultra 是预埋的思考强度档位（未来新模型可能支持），必须在 alias 配置与
// 请求级 effort 归一化中原样透传，而不是被钳位回 high。
func TestApplyReasoningEffortModelAliasSupportsUltra(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	store.SetReasoningEffortModels(`[{"model":"gpt-5.5","effort":"ultra"}]`)
	handler := NewHandler(store, nil, nil, nil)

	body, original, effective, mapped := handler.applyConfiguredModelMappingToBody(
		[]byte(`{"model":"gpt-5.5(ultra)","input":"hello"}`),
		[]string{"gpt-5.5", "gpt-5.5(ultra)"},
	)
	if !mapped {
		t.Fatal("expected ultra alias to be resolved")
	}
	if original != "gpt-5.5(ultra)" || effective != "gpt-5.5" {
		t.Fatalf("original/effective = %q/%q, want gpt-5.5(ultra)/gpt-5.5", original, effective)
	}
	if got := gjson.GetBytes(body, "reasoning.effort").String(); got != "ultra" {
		t.Fatalf("reasoning.effort = %q, want ultra; body=%s", got, body)
	}
}

func TestNormalizeReasoningEffortLevels(t *testing.T) {
	cases := map[string]string{
		"none":    "none",
		"minimal": "minimal",
		"low":     "low",
		"medium":  "medium",
		"high":    "high",
		"xhigh":   "xhigh",
		"ultra":   "ultra",
		"ULTRA":   "ultra",
		"max":     "xhigh",
		"unknown": "high",
		"":        "",
	}
	for input, want := range cases {
		if got := normalizeReasoningEffort(input); got != want {
			t.Errorf("normalizeReasoningEffort(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestApplyReasoningEffortModelAliasBeforeCodexMapping(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	store.SetReasoningEffortModels(`[{"model":"gpt-5.5","effort":"xhigh"}]`)
	store.SetCodexModelMapping(`{"gpt-5.5":"gpt-5.4"}`)
	handler := NewHandler(store, nil, nil, nil)

	body, original, effective, mapped := handler.applyConfiguredModelMappingToBody(
		[]byte(`{"model":"gpt-5.5(xhigh)","input":"hello"}`),
		[]string{"gpt-5.5", "gpt-5.4", "gpt-5.5(xhigh)"},
	)
	if !mapped {
		t.Fatal("expected alias and mapping to be applied")
	}
	if original != "gpt-5.5(xhigh)" || effective != "gpt-5.4" {
		t.Fatalf("original/effective = %q/%q, want gpt-5.5(xhigh)/gpt-5.4", original, effective)
	}
	if got := gjson.GetBytes(body, "model").String(); got != "gpt-5.4" {
		t.Fatalf("body model = %q, want gpt-5.4; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "reasoning.effort").String(); got != "xhigh" {
		t.Fatalf("reasoning.effort = %q, want xhigh; body=%s", got, body)
	}
}

func TestApplyConfiguredModelMappingToBodyIgnoresClaudeMappingSetting(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	store.SetModelMapping(`{"gpt-5.2":"gpt-5.5"}`)
	handler := NewHandler(store, nil, nil, nil)

	body, original, effective, mapped := handler.applyConfiguredModelMappingToBody(
		[]byte(`{"model":"gpt-5.2","input":"hello"}`),
		[]string{"gpt-5.5"},
	)
	if mapped {
		t.Fatal("Claude model_mapping should not rewrite Codex/OpenAI requests")
	}
	if original != "gpt-5.2" || effective != "gpt-5.2" {
		t.Fatalf("original/effective = %q/%q, want gpt-5.2/gpt-5.2", original, effective)
	}
	if got := gjson.GetBytes(body, "model").String(); got != "gpt-5.2" {
		t.Fatalf("body model = %q, want gpt-5.2; body=%s", got, body)
	}
}

func TestApplyConfiguredCompactModelMappingPreservesRequestedAlias(t *testing.T) {
	tests := []struct {
		name    string
		mapping string
		want    string
	}{
		{
			name:    "full compact alias maps first",
			mapping: `{"gpt-5.6-sol-openai-compact":"gpt-5.5"}`,
			want:    "gpt-5.5",
		},
		{
			name:    "base model mapping follows suffix fallback",
			mapping: `{"gpt-5.6-sol":"gpt-5.5"}`,
			want:    "gpt-5.5",
		},
		{
			name:    "suffix fallback remains effective without a rule",
			mapping: `{}`,
			want:    "gpt-5.6-sol",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := auth.NewStore(nil, nil, nil)
			store.SetCodexModelMapping(tt.mapping)
			handler := NewHandler(store, nil, nil, nil)

			body, original, effective, mapped := handler.applyConfiguredCompactModelMappingToBody(
				[]byte(`{"model":"gpt-5.6-sol-openai-compact","input":"hello"}`),
				[]string{"gpt-5.6-sol", "gpt-5.5"},
			)

			if !mapped {
				t.Fatal("compact alias should be normalized or mapped")
			}
			if original != "gpt-5.6-sol-openai-compact" {
				t.Fatalf("original model = %q, want full client alias", original)
			}
			if effective != tt.want {
				t.Fatalf("effective model = %q, want %q", effective, tt.want)
			}
			if got := gjson.GetBytes(body, "model").String(); got != tt.want {
				t.Fatalf("body model = %q, want %q; body=%s", got, tt.want, body)
			}
		})
	}
}

func TestApplyConfiguredCompactModelMappingMatchesSyntheticCompactAlias(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	store.SetCodexModelMapping(`{"gpt-5.6-sol-openai-compact":"gpt-5.5"}`)
	handler := NewHandler(store, nil, nil, nil)

	body, original, effective, mapped := handler.applyConfiguredCompactModelMappingToBody(
		[]byte(`{"model":"gpt-5.6-sol","input":"hello"}`),
		[]string{"gpt-5.6-sol", "gpt-5.5"},
	)

	if !mapped {
		t.Fatal("endpoint-qualified compact alias should map a base-model request")
	}
	if original != "gpt-5.6-sol" || effective != "gpt-5.5" {
		t.Fatalf("original/effective = %q/%q, want gpt-5.6-sol/gpt-5.5", original, effective)
	}
	if got := gjson.GetBytes(body, "model").String(); got != "gpt-5.5" {
		t.Fatalf("body model = %q, want gpt-5.5; body=%s", got, body)
	}
}

func TestApplyConfiguredCompactModelMappingResolvesReasoningEffortTarget(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	store.SetReasoningEffortModels(`[{"model":"gpt-5.5","effort":"xhigh"}]`)
	store.SetCodexModelMapping(`{"gpt-5.6-sol-openai-compact":"gpt-5.5(xhigh)"}`)
	handler := NewHandler(store, nil, nil, nil)

	body, original, effective, mapped := handler.applyConfiguredCompactModelMappingToBody(
		[]byte(`{"model":"gpt-5.6-sol","input":"hello"}`),
		[]string{"gpt-5.6-sol", "gpt-5.5", "gpt-5.5(xhigh)"},
	)

	if !mapped {
		t.Fatal("compact alias should map to the reasoning-effort target")
	}
	if original != "gpt-5.6-sol" || effective != "gpt-5.5" {
		t.Fatalf("original/effective = %q/%q, want gpt-5.6-sol/gpt-5.5", original, effective)
	}
	if got := gjson.GetBytes(body, "model").String(); got != "gpt-5.5" {
		t.Fatalf("body model = %q, want gpt-5.5; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "reasoning_effort").String(); got != "xhigh" {
		t.Fatalf("reasoning_effort = %q, want xhigh; body=%s", got, body)
	}
	if got := gjson.GetBytes(body, "reasoning.effort").String(); got != "xhigh" {
		t.Fatalf("reasoning.effort = %q, want xhigh; body=%s", got, body)
	}
}

func TestStripCompactModelSuffix(t *testing.T) {
	tests := []struct {
		name      string
		model     string
		want      string
		wantMatch bool
	}{
		{name: "gpt-5.4 compact", model: "gpt-5.4-openai-compact", want: "gpt-5.4", wantMatch: true},
		{name: "gpt-5.5 compact", model: "gpt-5.5-openai-compact", want: "gpt-5.5", wantMatch: true},
		{name: "case insensitive suffix", model: "gpt-5.4-OpenAI-Compact", want: "gpt-5.4", wantMatch: true},
		{name: "trims whitespace", model: "  gpt-5.4-openai-compact  ", want: "gpt-5.4", wantMatch: true},
		{name: "no suffix untouched", model: "gpt-5.4", want: "gpt-5.4", wantMatch: false},
		{name: "codex base untouched", model: "gpt-5.3-codex", want: "gpt-5.3-codex", wantMatch: false},
		{name: "suffix only stays", model: "-openai-compact", want: "-openai-compact", wantMatch: false},
		{name: "empty stays", model: "", want: "", wantMatch: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, matched := stripCompactModelSuffix(tt.model)
			if matched != tt.wantMatch {
				t.Fatalf("stripCompactModelSuffix(%q) matched = %v, want %v", tt.model, matched, tt.wantMatch)
			}
			if got != tt.want {
				t.Fatalf("stripCompactModelSuffix(%q) = %q, want %q", tt.model, got, tt.want)
			}
		})
	}
}

func TestStripCompactModelSuffixFromBody(t *testing.T) {
	body, model, matched := stripCompactModelSuffixFromBody([]byte(`{"model":"gpt-5.4-openai-compact","input":"hi"}`))
	if !matched {
		t.Fatalf("expected compact suffix to be stripped")
	}
	if model != "gpt-5.4" {
		t.Fatalf("stripped model = %q, want gpt-5.4", model)
	}
	if got := gjson.GetBytes(body, "model").String(); got != "gpt-5.4" {
		t.Fatalf("body model = %q, want gpt-5.4; body=%s", got, body)
	}
	// input 等其它字段保持不变
	if got := gjson.GetBytes(body, "input").String(); got != "hi" {
		t.Fatalf("input should be preserved, got %q", got)
	}

	// 无后缀请求原样返回，不改写。
	orig := []byte(`{"model":"gpt-5.4","input":"hi"}`)
	body2, model2, matched2 := stripCompactModelSuffixFromBody(orig)
	if matched2 {
		t.Fatalf("plain model should not be rewritten")
	}
	if model2 != "gpt-5.4" {
		t.Fatalf("model = %q, want gpt-5.4", model2)
	}
	if string(body2) != string(orig) {
		t.Fatalf("body should be untouched, got %s", body2)
	}
}

// 合成的 -openai-compact 别名不得命中 "gpt-*"、"*" 之类通用规则，
// 否则会压过基础名本应命中的精确规则（issue: PR #350 回归）。
func TestCompactSyntheticAliasDoesNotMatchGenericWildcardRules(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	store.SetCodexModelMapping(`{"gpt-5.5":"model-a","gpt-*":"model-b"}`)
	handler := NewHandler(store, nil, nil, nil)

	body, _, effective, mapped := handler.applyConfiguredCompactModelMappingToBody(
		[]byte(`{"model":"gpt-5.5","input":"hello"}`),
		[]string{"gpt-5.5", "model-a", "model-b"},
	)

	if !mapped || effective != "model-a" {
		t.Fatalf("effective = %q (mapped=%v), want exact base rule model-a", effective, mapped)
	}
	if got := gjson.GetBytes(body, "model").String(); got != "model-a" {
		t.Fatalf("body model = %q, want model-a", got)
	}
}

// 显式针对 compact 别名的通配规则（*-openai-compact）仍可命中合成别名。
func TestCompactSyntheticAliasMatchesCompactScopedWildcardRule(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	store.SetCodexModelMapping(`{"*-openai-compact":"gpt-5.5"}`)
	handler := NewHandler(store, nil, nil, nil)

	_, _, effective, mapped := handler.applyConfiguredCompactModelMappingToBody(
		[]byte(`{"model":"gpt-5.6-sol","input":"hello"}`),
		[]string{"gpt-5.6-sol", "gpt-5.5"},
	)

	if !mapped || effective != "gpt-5.5" {
		t.Fatalf("effective = %q (mapped=%v), want gpt-5.5 via *-openai-compact rule", effective, mapped)
	}
}

// 映射目标携带 -openai-compact 后缀时须剥离后再写入请求体：
// 上游 compact 端点只认真实模型名（issue: PR #350 回归，导致上游收到
// gpt-5.6-sol-openai-compact 而压缩失败）。
func TestCompactMappingTargetSuffixIsStripped(t *testing.T) {
	store := auth.NewStore(nil, nil, nil)
	store.SetCodexModelMapping(`{"gpt-5.6-sol-openai-compact":"gpt-5.4-openai-compact"}`)
	handler := NewHandler(store, nil, nil, nil)

	body, _, effective, mapped := handler.applyConfiguredCompactModelMappingToBody(
		[]byte(`{"model":"gpt-5.6-sol-openai-compact","input":"hello"}`),
		[]string{"gpt-5.6-sol", "gpt-5.4"},
	)

	if !mapped || effective != "gpt-5.4" {
		t.Fatalf("effective = %q (mapped=%v), want suffix-stripped gpt-5.4", effective, mapped)
	}
	if got := gjson.GetBytes(body, "model").String(); got != "gpt-5.4" {
		t.Fatalf("body model = %q, want gpt-5.4", got)
	}
}

// 账号级映射：合成别名命中恒等规则（suffixed→suffixed）时不得把带后缀
// 名字发往上游；候选回退到基础名后应保持 PR #350 之前的行为。
func TestResolveAccountCompactModelMappingNormalizesIdentitySuffixRule(t *testing.T) {
	account := &auth.Account{
		DBID:         1,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      "http://example.invalid",
		APIKey:       "sk-x",
		Models:       []string{"gpt-5.6-sol", "gpt-5.6-sol-openai-compact"},
		ModelMapping: `{"gpt-5.6-sol-openai-compact":"gpt-5.6-sol-openai-compact"}`,
		PlanType:     "api",
	}

	mapped, ok := resolveAccountCompactModelMappingForCandidates(account, compactMappingCandidates("gpt-5.6-sol"))
	if !ok || mapped != "gpt-5.6-sol" {
		t.Fatalf("mapped = %q (ok=%v), want base gpt-5.6-sol", mapped, ok)
	}
}

// 账号级映射：合成别名不吃通用通配规则，基础名的精确规则优先。
func TestResolveAccountCompactModelMappingKeepsExactBaseRulePrecedence(t *testing.T) {
	account := &auth.Account{
		DBID:         2,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      "http://example.invalid",
		APIKey:       "sk-x",
		Models:       []string{"model-a", "model-b"},
		ModelMapping: `{"gpt-5.5":"model-a","gpt-*":"model-b"}`,
		PlanType:     "api",
	}

	mapped, ok := resolveAccountCompactModelMappingForCandidates(account, compactMappingCandidates("gpt-5.5"))
	if !ok || mapped != "model-a" {
		t.Fatalf("mapped = %q (ok=%v), want exact base rule model-a", mapped, ok)
	}
}

// 恒等 suffixed→suffixed 规则不得把仅列出带后缀名字的账号从 compact 池中
// 踢除（PR #350 之前该规则是死配置，账号按基础名参与过滤）。
func TestCompactAccountFilterParityWithStaleSuffixRule(t *testing.T) {
	rejected := &auth.Account{
		DBID:         3,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      "http://example.invalid",
		APIKey:       "sk-x",
		Models:       []string{"gpt-5.6-sol-openai-compact"},
		ModelMapping: `{"gpt-5.6-sol-openai-compact":"gpt-5.6-sol-openai-compact"}`,
		PlanType:     "api",
	}
	accepted := &auth.Account{
		DBID:         4,
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      "http://example.invalid",
		APIKey:       "sk-x",
		Models:       []string{"gpt-5.6-sol", "gpt-5.6-sol-openai-compact"},
		ModelMapping: `{"gpt-5.6-sol-openai-compact":"gpt-5.6-sol-openai-compact"}`,
		PlanType:     "api",
	}

	filter := accountFilterForCompactResponsesModelWithOriginal("gpt-5.6-sol", "gpt-5.6-sol", false)
	if filter(rejected) {
		t.Error("account without the base model should stay rejected (pre-#350 parity)")
	}
	if !filter(accepted) {
		t.Error("account listing the base model should be accepted")
	}
}
