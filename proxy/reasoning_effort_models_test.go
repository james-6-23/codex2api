package proxy

import "testing"

// 设置里的思考强度别名同样按模型放行 max:5.6+ 保留,旧模型钳到 xhigh。
func TestParseReasoningEffortModelEntries_MaxGatedByModel(t *testing.T) {
	supported := []string{"gpt-5.6-sol", "gpt-5.4"}
	entries, err := parseReasoningEffortModelEntries(
		`[{"model":"gpt-5.6-sol","effort":"max"},{"model":"gpt-5.4","effort":"max"}]`,
		supported, true)
	if err != nil {
		t.Fatalf("parse error: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2: %+v", len(entries), entries)
	}
	if entries[0].Effort != "max" {
		t.Fatalf("gpt-5.6-sol effort = %q, want max", entries[0].Effort)
	}
	if entries[1].Effort != "xhigh" {
		t.Fatalf("gpt-5.4 effort = %q, want xhigh", entries[1].Effort)
	}
}
