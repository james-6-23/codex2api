package proxy

import (
	"net/http"
	"testing"

	"github.com/codex2api/auth"
)

func codexAccount() *auth.Account {
	// 空 UpstreamType 的账号即官方 Codex 账号（非中转、非 Grok）。
	return &auth.Account{AccountID: "acc-1"}
}

func relayAccount() *auth.Account {
	return &auth.Account{
		UpstreamType: auth.UpstreamOpenAIResponses,
		BaseURL:      "https://relay.example.com/v1",
		APIKey:       "sk-relay",
	}
}

func TestBuildCodexRoutingHint(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"fast normalizes to priority", `{"model":"gpt-5.6-codex","service_tier":"fast"}`, "model=gpt-5.6-codex;tier=priority"},
		{"priority kept", `{"model":"gpt-5.6-codex","service_tier":"priority"}`, "model=gpt-5.6-codex;tier=priority"},
		{"ultrafast kept", `{"model":"gpt-5.6-sol","service_tier":"ultrafast"}`, "model=gpt-5.6-sol;tier=ultrafast"},
		{"flex kept", `{"model":"gpt-5.6-codex","service_tier":"flex"}`, "model=gpt-5.6-codex;tier=flex"},
		{"default drops to model-only", `{"model":"gpt-5.6","service_tier":"default"}`, "model=gpt-5.6"},
		{"auto drops to model-only", `{"model":"gpt-5.6","service_tier":"auto"}`, "model=gpt-5.6"},
		{"scale drops to model-only", `{"model":"gpt-5.6","service_tier":"scale"}`, "model=gpt-5.6"},
		{"unknown tier drops to model-only", `{"model":"gpt-5.6","service_tier":"turbo"}`, "model=gpt-5.6"},
		{"missing tier is model-only", `{"model":"gpt-5.6"}`, "model=gpt-5.6"},
		{"uppercase tier normalized", `{"model":"gpt-5.6","service_tier":"PRIORITY"}`, "model=gpt-5.6;tier=priority"},
		{"missing model yields empty", `{"service_tier":"priority"}`, ""},
		{"model with semicolon rejected", `{"model":"a;b","service_tier":"flex"}`, ""},
		{"model with equals rejected", `{"model":"a=b"}`, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := buildCodexRoutingHint([]byte(tc.body)); got != tc.want {
				t.Fatalf("buildCodexRoutingHint(%s) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

func TestApplyCodexRoutingHintOnlyForOfficialCodex(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-codex","service_tier":"fast"}`)

	// 官方 Codex 账号:合成 hint。
	h := http.Header{}
	ApplyCodexRoutingHint(h, codexAccount(), body)
	if got := h.Get("X-Codex-Routing-Hint"); got != "model=gpt-5.6-codex;tier=priority" {
		t.Fatalf("codex account hint = %q, want priority hint", got)
	}

	// 中转账号:只剥不发。
	h = http.Header{}
	ApplyCodexRoutingHint(h, relayAccount(), body)
	if got := h.Get("X-Codex-Routing-Hint"); got != "" {
		t.Fatalf("relay account hint = %q, want empty", got)
	}

	// nil 账号:不发。
	h = http.Header{}
	ApplyCodexRoutingHint(h, nil, body)
	if got := h.Get("X-Codex-Routing-Hint"); got != "" {
		t.Fatalf("nil account hint = %q, want empty", got)
	}
}

func TestApplyCodexRoutingHintStripsSpoofedHeaders(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6","service_tier":"default"}`)

	// 下游用任意大小写伪造该头,应被无条件剥离后按网关值重建。
	h := http.Header{
		"x-codex-routing-hint": []string{"model=evil;tier=priority"},
		"X-Codex-Routing-Hint": []string{"model=evil2"},
	}
	ApplyCodexRoutingHint(h, codexAccount(), body)
	values := h.Values("X-Codex-Routing-Hint")
	if len(values) != 1 || values[0] != "model=gpt-5.6" {
		t.Fatalf("hint values = %v, want single gateway value [model=gpt-5.6]", values)
	}

	// 中转账号伪造:同样被剥离且不重建。
	h = http.Header{"x-codex-routing-hint": []string{"model=evil"}}
	ApplyCodexRoutingHint(h, relayAccount(), body)
	for key := range h {
		if len(h[key]) > 0 && h.Get("X-Codex-Routing-Hint") != "" {
			t.Fatalf("relay account left spoofed hint: %v", h)
		}
	}
}

// hint 必须取"最终出站 body"：上游只接受 priority，flex/auto/default/scale 会被
// sanitizeServiceTierForUpstream 从出站体剥离，此时 hint 应随之降为 model-only，
// 不能声明一个实际没发出去的 tier。本用例串起真实管道的两步以钉死该语义。
func TestRoutingHintFollowsSanitizedOutboundTier(t *testing.T) {
	cases := []struct {
		clientTier string
		want       string
	}{
		{"priority", "model=gpt-5.6-sol;tier=priority"},
		{"fast", "model=gpt-5.6-sol;tier=priority"},
		{"flex", "model=gpt-5.6-sol"},
		{"default", "model=gpt-5.6-sol"},
		{"auto", "model=gpt-5.6-sol"},
		{"scale", "model=gpt-5.6-sol"},
	}
	for _, tc := range cases {
		t.Run(tc.clientTier, func(t *testing.T) {
			body := []byte(`{"model":"gpt-5.6-sol","service_tier":"` + tc.clientTier + `"}`)
			outbound := sanitizeServiceTierForUpstream(body)
			if got := buildCodexRoutingHint(outbound); got != tc.want {
				t.Fatalf("client tier %q -> hint %q, want %q (outbound body: %s)", tc.clientTier, got, tc.want, outbound)
			}
		})
	}
}

func TestApplyCodexRoutingHintDisabledByEnv(t *testing.T) {
	t.Setenv("CODEX_DISABLE_ROUTING_HINT", "1")
	body := []byte(`{"model":"gpt-5.6-codex","service_tier":"fast"}`)

	// 关闭后不合成,但入站伪造仍被剥离。
	h := http.Header{"x-codex-routing-hint": []string{"model=evil"}}
	ApplyCodexRoutingHint(h, codexAccount(), body)
	if got := h.Get("X-Codex-Routing-Hint"); got != "" {
		t.Fatalf("hint = %q, want empty when disabled", got)
	}
}
