package auth

import (
	"strconv"
	"strings"
	"testing"
)

func TestRenderTestContentSingleLineBackwardCompatible(t *testing.T) {
	if got := RenderTestContent("hi"); got != "hi" {
		t.Fatalf("RenderTestContent(hi) = %q, want hi", got)
	}
	if got := RenderTestContent("  你好，帮我看看  "); got != "你好，帮我看看" {
		t.Fatalf("got %q", got)
	}
	if got := RenderTestContent(""); got != DefaultTestContent {
		t.Fatalf("empty content must fall back to %q, got %q", DefaultTestContent, got)
	}
	if got := RenderTestContent("\n\n  \n"); got != DefaultTestContent {
		t.Fatalf("blank lines must fall back to %q, got %q", DefaultTestContent, got)
	}
}

func TestRenderTestContentPicksEachLine(t *testing.T) {
	raw := "line-a\nline-b\r\nline-c\n\n"
	want := map[string]bool{"line-a": false, "line-b": false, "line-c": false}
	for i := 0; i < 200; i++ {
		got := RenderTestContent(raw)
		if _, ok := want[got]; !ok {
			t.Fatalf("unexpected pick %q", got)
		}
		want[got] = true
	}
	for line, seen := range want {
		if !seen {
			t.Fatalf("line %q never picked in 200 draws", line)
		}
	}
}

func TestTestContentLines(t *testing.T) {
	lines := TestContentLines(" a \n\r\n b\r\nc")
	if len(lines) != 3 || lines[0] != "a" || lines[1] != "b" || lines[2] != "c" {
		t.Fatalf("TestContentLines = %v", lines)
	}
	if lines := TestContentLines(""); lines != nil {
		t.Fatalf("empty input should return nil, got %v", lines)
	}
}

func TestRenderTestContentVariables(t *testing.T) {
	t.Run("time date datetime timestamp", func(t *testing.T) {
		got := RenderTestContent("now {{time}} on {{date}} full {{datetime}} ts {{timestamp}}")
		if strings.Contains(got, "{{") {
			t.Fatalf("all variables should expand, got %q", got)
		}
	})

	t.Run("rand default range", func(t *testing.T) {
		for i := 0; i < 50; i++ {
			got := RenderTestContent("n={{rand}}")
			numStr := strings.TrimPrefix(got, "n=")
			n, err := strconv.Atoi(numStr)
			if err != nil || n < 0 || n > 9999 {
				t.Fatalf("rand out of range: %q", got)
			}
		}
	})

	t.Run("rand with range", func(t *testing.T) {
		for i := 0; i < 100; i++ {
			got := RenderTestContent("{{rand:5-8}}")
			n, err := strconv.Atoi(got)
			if err != nil || n < 5 || n > 8 {
				t.Fatalf("rand:5-8 out of range: %q", got)
			}
		}
	})

	t.Run("rand reversed range swaps", func(t *testing.T) {
		for i := 0; i < 50; i++ {
			got := RenderTestContent("{{rand:9-3}}")
			n, err := strconv.Atoi(got)
			if err != nil || n < 3 || n > 9 {
				t.Fatalf("rand:9-3 should swap to [3,9]: %q", got)
			}
		}
	})

	t.Run("rand single value range", func(t *testing.T) {
		if got := RenderTestContent("{{rand:7-7}}"); got != "7" {
			t.Fatalf("rand:7-7 = %q, want 7", got)
		}
	})

	t.Run("case insensitive with spaces", func(t *testing.T) {
		got := RenderTestContent("{{ RAND:2-2 }} {{Time}}")
		if !strings.HasPrefix(got, "2 ") || strings.Contains(got, "{{") {
			t.Fatalf("got %q", got)
		}
	})
}

func TestRenderTestContentUnknownVariablesPreserved(t *testing.T) {
	for _, raw := range []string{
		"hello {{name}}",       // 未知变量
		"{{rand:abc-def}}",     // 非数字范围
		"{{rand:-5-5}}",        // 负数下界不支持
		"json 示例 {{not closed", // 未闭合
		"literal {{}} braces",  // 空 token
	} {
		got := RenderTestContent(raw)
		if got != raw {
			t.Fatalf("RenderTestContent(%q) = %q, want unchanged", raw, got)
		}
	}
}

func TestRenderTestContentMultipleVariablesOneLine(t *testing.T) {
	got := RenderTestContent("a={{rand:1-1}} b={{rand:2-2}} c={{rand:3-3}}")
	if got != "a=1 b=2 c=3" {
		t.Fatalf("got %q, want a=1 b=2 c=3", got)
	}
}
