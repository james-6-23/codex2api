package proxy

import (
	"testing"

	"github.com/codex2api/database"
)

// 2026-11 官方价格页把已下线模型的名字包进 {"label":[0,"gpt-…"]}，同时保留
// 4 列（旧格式）与 5 列（含缓存写入）两种行；这里按真实页面结构固定回归。
func TestParseOpenAIPricingIslandsReadsWrappedModelLabels(t *testing.T) {
	body := []byte(`
<astro-island component-export="TextTokenPricingTables" props='{"tier":[0,"standard"],"collapsedLatestRowCount":[0,2],"latestSectionLabel":[0,"Latest models"],"rows":[1,[[1,[[0,"gpt-6.2-astra"],[0,8],[0,0.4],[0,10],[0,40]]],[1,[[0,"gpt-5.6-sol"],[0,5],[0,0.5],[0,"-"],[0,30]]],[1,[[0,{"label":[0,"gpt-5.4-nano"],"deprecated":[0,true]}],[0,0.2],[0,0.02],[0,1.25]]],[1,[[0,{"deprecated":[0,true]}],[0,9],[0,9],[0,9]]]]]}'></astro-island>
<astro-island component-export="TextTokenPricingTables" props='{"tier":[0,"fast"],"rows":[1,[[1,[[0,"gpt-6.2-astra"],[0,16],[0,0.8],[0,20],[0,80]]]]]}'></astro-island>
<astro-island component-export="TextTokenPricingTables" props='{"tier":[0,"Ultrafast promotion"],"rows":[1,[[1,[[0,"gpt-6.2-astra"],[0,24],[0,1.2],[0,30],[0,120]]]]]}'></astro-island>
`)

	got := map[string]database.ModelPricingOverride{}
	if err := openAIPricingIslands(body, got); err != nil {
		t.Fatalf("parse pricing islands: %v", err)
	}
	if astra := got["gpt-6.2-astra"]; astra.Input != 8 || astra.CachedInput != 0.4 || astra.Output != 40 {
		t.Fatalf("gpt-6.2-astra standard = %+v", astra)
	}
	// 快照档只覆盖 priority 字段，标准价保持不变。
	if astra := got["gpt-6.2-astra"]; astra.InputPriority != 16 || astra.OutputPriority != 80 {
		t.Fatalf("gpt-6.2-astra fast = %+v", astra)
	}
	// 旧版 4 列行：模型列被包成对象，仍需读出模型名与三档价格。
	if nano := got["gpt-5.4-nano"]; nano.Input != 0.2 || nano.CachedInput != 0.02 || nano.Output != 1.25 {
		t.Fatalf("gpt-5.4-nano = %+v", nano)
	}
	// "-" 表示该档不适用，不能被当成零价或报错。
	if sol := got["gpt-5.6-sol"]; sol.Input != 5 || sol.Output != 30 {
		t.Fatalf("gpt-5.6-sol = %+v", sol)
	}
	// 只有 deprecated、没有 label 的行应当跳过，而不是让整次同步失败。
	if len(got) != 3 {
		t.Fatalf("parsed models = %d, want 3: %+v", len(got), got)
	}
}
