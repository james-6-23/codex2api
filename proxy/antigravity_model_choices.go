package proxy

import (
	"strings"

	"github.com/codex2api/api"
)

// Group only the choices already admitted by account and key permissions.
// Both public list formats use this projection; routing retains fixed aliases.
func antigravityModelChoiceGroups(models []api.Model) (map[string]string, map[string][]string) {
	visible := make(map[string]bool, len(models))
	for _, model := range models {
		visible[strings.ToLower(strings.TrimSpace(model.ID))] = true
	}
	fold := make(map[string]string)
	levels := make(map[string][]string)
	for _, logical := range antigravityLogicalCompatibilityCatalog {
		for _, variant := range logical.variants {
			alias := logical.id + "-" + variant.level
			if visible[alias] {
				fold[alias] = logical.id
				levels[logical.id] = append(levels[logical.id], variant.level)
			}
		}
	}
	return fold, levels
}

func collapseAntigravityModelChoices(models []api.Model) []api.Model {
	fold, levels := antigravityModelChoiceGroups(models)
	result := make([]api.Model, 0, len(models))
	seen := make(map[string]bool, len(models))
	for _, model := range models {
		if base, ok := fold[strings.ToLower(model.ID)]; ok {
			model.ID = base
		}
		key := strings.ToLower(model.ID)
		if seen[key] {
			continue
		}
		seen[key] = true
		if available := levels[key]; len(available) > 0 {
			model.DefaultReasoningLevel = available[0]
			for _, effort := range available {
				model.SupportedReasoningLevels = append(model.SupportedReasoningLevels, api.ModelReasoningLevel{Effort: effort, Description: "Antigravity " + effort + " reasoning"})
			}
		}
		result = append(result, model)
	}
	return result
}
