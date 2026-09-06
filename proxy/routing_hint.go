package proxy

import (
	"net/http"
	"os"
	"strings"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
)

// codexRoutingHintHeader 是官方 Codex 客户端向上游声明路由意图的请求头，
// 值形如 model=<slug> 或 model=<slug>;tier=priority|flex。
const codexRoutingHintHeader = "x-codex-routing-hint"

// CodexRoutingHintDisabled 报告是否通过环境变量关闭了 routing hint 出站。
// CODEX_DISABLE_ROUTING_HINT=1（或 true）时不再合成该头；入站伪造仍会被剥离。
func CodexRoutingHintDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("CODEX_DISABLE_ROUTING_HINT"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// buildCodexRoutingHint 按最终出站 body（模型映射与 tier 净化之后）合成 hint 值。
// model 缺失或含分隔符、拼出的值含控制字符时返回空（整头不发）。
// tier 放行 priority（fast 归一化）、ultrafast 与 flex；default 是显式的标准路由哨兵，
// 与缺失/auto/scale/未知值一样降为 model-only 形态。
func buildCodexRoutingHint(requestBody []byte) string {
	results := gjson.GetManyBytes(requestBody, "model", "service_tier")
	model := strings.TrimSpace(results[0].String())
	if model == "" || strings.ContainsAny(model, ";=") {
		return ""
	}
	hint := "model=" + model
	tier := strings.ToLower(strings.TrimSpace(results[1].String()))
	if tier == "fast" {
		tier = "priority"
	}
	if tier == "priority" || tier == "flex" || tier == "ultrafast" {
		hint += ";tier=" + tier
	}
	if !validHTTPHeaderValue(hint) {
		return ""
	}
	return hint
}

// ApplyCodexRoutingHint 先剥离任意大小写形态的既有该头（网关所有，下游与
// 账号自定义头传入的一律不认），再仅对官方 Codex 账号按最终出站 body 合成。
// 中转/Grok（relay-style）账号只剥不发。
func ApplyCodexRoutingHint(headers http.Header, account *auth.Account, requestBody []byte) {
	if headers == nil {
		return
	}
	deleteHeaderCaseInsensitive(headers, codexRoutingHintHeader)
	if account == nil || account.IsRelayStyle() || CodexRoutingHintDisabled() {
		return
	}
	if hint := buildCodexRoutingHint(requestBody); hint != "" {
		headers.Set(codexRoutingHintHeader, hint)
	}
}

// deleteHeaderCaseInsensitive 删除任意大小写形态的同名 header
// （http.Header.Del 只删 canonical key，手工构造的 map 可能带小写 key）。
func deleteHeaderCaseInsensitive(headers http.Header, name string) {
	for key := range headers {
		if strings.EqualFold(key, name) {
			delete(headers, key)
		}
	}
}
