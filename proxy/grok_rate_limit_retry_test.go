package proxy

import (
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

// grok_config 里的 max_rate_limit_retries 应被解析并按账号类型生效:
// Grok 账号且配置 >0 用专属值;配置 0 或非 Grok 账号回落全局。
func TestEffectiveMaxRateLimitRetriesGrok(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency:      2,
		MaxRateLimitRetries: 1,
		GrokConfig:          `{"affinity_mode":"strict","max_rate_limit_retries":4}`,
	})
	if got := store.GrokMaxRateLimitRetries(); got != 4 {
		t.Fatalf("GrokMaxRateLimitRetries = %d, want 4", got)
	}
	h := NewHandler(store, nil, nil, nil)

	grok := &auth.Account{DBID: 1, APIKey: "xai-1", UpstreamType: auth.UpstreamGrok}
	codex := &auth.Account{DBID: 2}

	if got := h.effectiveMaxRateLimitRetries(grok, 1); got != 4 {
		t.Fatalf("grok effective = %d, want 4 (Grok 专属)", got)
	}
	if got := h.effectiveMaxRateLimitRetries(codex, 1); got != 1 {
		t.Fatalf("codex effective = %d, want 1 (全局回落)", got)
	}
	if got := h.effectiveMaxRateLimitRetries(nil, 1); got != 1 {
		t.Fatalf("nil account effective = %d, want 1", got)
	}

	// 配置为 0(未设)时 Grok 账号也回落全局。
	store.SetGrokMaxRateLimitRetries(0)
	if got := h.effectiveMaxRateLimitRetries(grok, 1); got != 1 {
		t.Fatalf("grok with unset(0) = %d, want 1 (跟随全局)", got)
	}

	// 负值钳到 0。
	store.SetGrokMaxRateLimitRetries(-5)
	if got := store.GrokMaxRateLimitRetries(); got != 0 {
		t.Fatalf("negative clamped = %d, want 0", got)
	}
}
