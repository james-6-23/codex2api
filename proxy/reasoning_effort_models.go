package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/codex2api/security"
)

type ReasoningEffortModel struct {
	Model  string `json:"model"`
	Effort string `json:"effort"`
}

func ReasoningEffortModelAlias(model, effort string) string {
	model = strings.TrimSpace(model)
	effort = normalizeConfiguredReasoningEffort(effort)
	if model == "" || effort == "" {
		return ""
	}
	return fmt.Sprintf("%s(%s)", model, effort)
}

func NormalizeReasoningEffortModelsJSON(value string, supportedModels []string) (string, error) {
	entries, err := parseReasoningEffortModelEntries(value, supportedModels, true)
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "[]", nil
	}
	body, err := json.Marshal(entries)
	if err != nil {
		return "", err
	}
	return string(body), nil
}

func parseReasoningEffortModelEntries(value string, supportedModels []string, strict bool) ([]ReasoningEffortModel, error) {
	value = strings.TrimSpace(value)
	if value == "" || value == "[]" {
		return nil, nil
	}

	var raw []ReasoningEffortModel
	dec := json.NewDecoder(strings.NewReader(value))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		if strict {
			return nil, fmt.Errorf("reasoning_effort_models 必须是 JSON 数组: %w", err)
		}
		return nil, nil
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if strict {
			if err == nil {
				return nil, fmt.Errorf("reasoning_effort_models 只能包含一个 JSON 数组")
			}
			return nil, fmt.Errorf("reasoning_effort_models JSON 无效: %w", err)
		}
		return nil, nil
	}

	entries := make([]ReasoningEffortModel, 0, len(raw))
	seen := make(map[string]struct{}, len(raw))
	for i, entry := range raw {
		model := normalizeReasoningEffortBaseModel(entry.Model, supportedModels)
		effort := normalizeConfiguredReasoningEffort(entry.Effort)
		if model == "" || effort == "" {
			if strict {
				return nil, fmt.Errorf("reasoning_effort_models[%d] 需要非空 model 且 effort 必须是 none/minimal/low/medium/high/xhigh/ultra", i)
			}
			continue
		}
		alias := ReasoningEffortModelAlias(model, effort)
		key := strings.ToLower(alias)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		entries = append(entries, ReasoningEffortModel{Model: model, Effort: effort})
	}
	return entries, nil
}

func normalizeReasoningEffortBaseModel(model string, supportedModels []string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		return ""
	}
	canonical := canonicalizeCodexModel(model, supportedModels)
	if err := security.ValidateModelName(canonical); err != nil {
		return ""
	}
	if canonical != "" {
		return canonical
	}
	return strings.ToLower(model)
}

func normalizeConfiguredReasoningEffort(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "none", "minimal", "low", "medium", "high", "xhigh", "ultra":
		return strings.ToLower(strings.TrimSpace(effort))
	case "max":
		return "xhigh"
	default:
		return ""
	}
}

func resolveReasoningEffortModelAlias(model string, settingsJSON string, supportedModels []string) (ReasoningEffortModel, bool) {
	model = strings.TrimSpace(model)
	if model == "" {
		return ReasoningEffortModel{}, false
	}
	entries, _ := parseReasoningEffortModelEntries(settingsJSON, supportedModels, false)
	for _, entry := range entries {
		if strings.EqualFold(model, ReasoningEffortModelAlias(entry.Model, entry.Effort)) {
			return entry, true
		}
	}
	return ReasoningEffortModel{}, false
}
