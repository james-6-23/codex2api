package proxy

import (
	"fmt"

	"github.com/tidwall/gjson"
)

func selectAnthropicCodexTools(req anthropicRequest) ([]anthropicTool, error) {
	if len(req.Tools) <= maxTools {
		return req.Tools, nil
	}
	needed := make(map[string]bool)
	if gjson.GetBytes(req.ToolChoice, "type").String() == "tool" {
		needed[gjson.GetBytes(req.ToolChoice, "name").String()] = true
	}
	for _, message := range req.Messages {
		for _, block := range parseAnthropicContent(message.Content) {
			if block.Type == "tool_use" {
				needed[block.Name] = true
			}
			if block.Type == "tool_result" {
				for _, result := range parseAnthropicContent(block.Content) {
					if result.Type == "tool_reference" {
						needed[result.ToolName] = true
					}
				}
			}
		}
	}
	keep := make([]bool, len(req.Tools))
	count := 0
	for i, tool := range req.Tools {
		if !tool.DeferLoading || needed[tool.Name] {
			keep[i] = true
			count++
		}
	}
	if count > maxTools {
		return nil, fmt.Errorf("Codex mapping requires %d active tools but allows at most %d; use defer_loading or reduce discovered tool history", count, maxTools)
	}
	// Fill unused slots without displacing discovered tools or changing declaration order.
	for i := range req.Tools {
		if count == maxTools {
			break
		}
		if !keep[i] {
			keep[i] = true
			count++
		}
	}
	selected := make([]anthropicTool, 0, count)
	for i, tool := range req.Tools {
		if keep[i] {
			selected = append(selected, tool)
		}
	}
	return selected, nil
}
