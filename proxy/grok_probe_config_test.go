package proxy

import (
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
)

// grok_config 里的定期探测开关/间隔应被 store 正确解析并暴露,间隔钳到下限。
func TestGrokProbeConfigRoundTrip(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{
		MaxConcurrency: 2,
		GrokConfig:     `{"affinity_mode":"strict","probe_enabled":true,"probe_interval_minutes":45}`,
	})
	if !store.GrokProbeEnabled() {
		t.Fatal("probe_enabled=true should be parsed as enabled")
	}
	if got := store.GrokProbeIntervalMinutes(); got != 45 {
		t.Fatalf("interval = %d, want 45", got)
	}

	// 缺省配置 → 关闭 + 默认间隔。
	store2 := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2, GrokConfig: `{"affinity_mode":"strict"}`})
	if store2.GrokProbeEnabled() {
		t.Fatal("missing probe_enabled should default to disabled")
	}
	if got := store2.GrokProbeIntervalMinutes(); got != auth.GrokProbeDefaultIntervalMinutes {
		t.Fatalf("default interval = %d, want %d", got, auth.GrokProbeDefaultIntervalMinutes)
	}

	// 低于下限的间隔应被钳到下限。
	store.SetGrokProbeConfig(true, 1)
	if got := store.GrokProbeIntervalMinutes(); got != auth.GrokProbeMinIntervalMinutes {
		t.Fatalf("clamped interval = %d, want %d", got, auth.GrokProbeMinIntervalMinutes)
	}
}

// EnabledGrokAccounts 只返回未停用的 Grok 账号,过滤 Codex 账号。
func TestEnabledGrokAccountsFilter(t *testing.T) {
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 2})
	grok := &auth.Account{DBID: 1, APIKey: "xai-1", UpstreamType: auth.UpstreamGrok, PlanType: "free"}
	codex := &auth.Account{DBID: 2}
	store.AddAccount(grok)
	store.AddAccount(codex)

	got := store.EnabledGrokAccounts()
	if len(got) != 1 || got[0].DBID != 1 {
		t.Fatalf("EnabledGrokAccounts = %+v, want only the Grok account", got)
	}
}
