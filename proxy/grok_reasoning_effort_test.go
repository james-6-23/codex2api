package proxy

import (
	"testing"

	"github.com/tidwall/gjson"
)

// Grok build 只支持 low/medium/high：xhigh/max 降到 high、minimal 降到 low，其余原样。
func TestClampGrokReasoningEffort(t *testing.T) {
	cases := []struct {
		name string
		in   string
		path string
		want string
	}{
		{"responses xhigh→high", `{"reasoning":{"effort":"xhigh"}}`, "reasoning.effort", "high"},
		{"responses max→high", `{"reasoning":{"effort":"max"}}`, "reasoning.effort", "high"},
		{"responses minimal→low", `{"reasoning":{"effort":"minimal"}}`, "reasoning.effort", "low"},
		{"responses high stays", `{"reasoning":{"effort":"high"}}`, "reasoning.effort", "high"},
		{"responses medium stays", `{"reasoning":{"effort":"medium"}}`, "reasoning.effort", "medium"},
		{"chat reasoning_effort xhigh→high", `{"reasoning_effort":"xhigh"}`, "reasoning_effort", "high"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			out := clampGrokReasoningEffort([]byte(c.in))
			if got := gjson.GetBytes(out, c.path).String(); got != c.want {
				t.Fatalf("%s = %q, want %q (out=%s)", c.path, got, c.want, out)
			}
		})
	}

	// 无 effort 字段不崩、不误增。
	out := clampGrokReasoningEffort([]byte(`{"model":"grok-4.5"}`))
	if gjson.GetBytes(out, "reasoning.effort").Exists() || gjson.GetBytes(out, "reasoning_effort").Exists() {
		t.Fatalf("不应凭空注入 effort 字段: %s", out)
	}

	// sanitize 主入口也应带上降级。
	sane := sanitizeGrokRequestBody([]byte(`{"reasoning":{"effort":"xhigh"},"service_tier":"priority"}`))
	if got := gjson.GetBytes(sane, "reasoning.effort").String(); got != "high" {
		t.Fatalf("sanitize 后 effort = %q, want high", got)
	}
	if gjson.GetBytes(sane, "service_tier").Exists() {
		t.Fatalf("sanitize 应仍剥离 service_tier")
	}
}
