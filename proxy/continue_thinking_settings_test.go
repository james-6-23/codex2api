package proxy

import (
	"testing"

	"github.com/codex2api/database"
)

func TestCodexContinueThinkingSettingsFromRuntime(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })

	// 默认：关闭，轮数为默认 8。
	ApplyRuntimeSettings(DefaultRuntimeSettings())
	if enabled, rounds := codexContinueThinkingSettings(); enabled || rounds != 8 {
		t.Fatalf("默认应关闭且轮数 8, got enabled=%v rounds=%d", enabled, rounds)
	}

	settings := DefaultRuntimeSettings()
	settings.CodexContinueThinking = true
	settings.CodexContinueMaxRounds = 5
	ApplyRuntimeSettings(settings)
	if enabled, rounds := codexContinueThinkingSettings(); !enabled || rounds != 5 {
		t.Fatalf("开启后 got enabled=%v rounds=%d, want true/5", enabled, rounds)
	}
}

func TestCodexContinueMaxRoundsClamp(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })

	cases := []struct {
		in   int
		want int
	}{
		{0, 8},   // 非正 → 默认
		{-3, 8},  // 负 → 默认
		{1, 1},   // 下界
		{8, 8},   // 常规
		{32, 32}, // 上界
		{99, 32}, // 超上界 → 截断
	}
	for _, c := range cases {
		s := DefaultRuntimeSettings()
		s.CodexContinueMaxRounds = c.in
		got := NormalizeRuntimeSettings(s).CodexContinueMaxRounds
		if got != c.want {
			t.Errorf("clamp(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestApplyRuntimeSettingsFromSystemContinueThinking(t *testing.T) {
	previous := CurrentRuntimeSettings()
	t.Cleanup(func() { ApplyRuntimeSettings(previous) })

	next := ApplyRuntimeSettingsFromSystem(&database.SystemSettings{
		CodexContinueThinkingEnabled: true,
		CodexContinueMaxRounds:       12,
		CodexWSSilentMaxRetries:      2,
	})
	if !next.CodexContinueThinking || next.CodexContinueMaxRounds != 12 {
		t.Fatalf("SystemSettings → RuntimeSettings 映射错误: %+v", next)
	}
	// nil settings 回落默认值（关闭 + 8 轮）。
	def := ApplyRuntimeSettingsFromSystem(nil)
	if def.CodexContinueThinking || def.CodexContinueMaxRounds != 8 {
		t.Fatalf("nil settings 应回落默认: %+v", def)
	}
}

func TestNormalizeCodexContinueMaxRoundsDB(t *testing.T) {
	cases := []struct {
		in   int
		want int
	}{
		{0, 8}, {-1, 8}, {1, 1}, {8, 8}, {32, 32}, {33, 32},
	}
	for _, c := range cases {
		if got := database.NormalizeCodexContinueMaxRounds(c.in); got != c.want {
			t.Errorf("NormalizeCodexContinueMaxRounds(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}
