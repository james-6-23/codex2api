package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/codex2api/security"
)

// maxImportedProxies 限制单次导入能注册进代理池的代理条数。导入文件是外部输入，
// 没有上限的话一份畸形文件就能把 proxies 表冲爆。
const maxImportedProxies = 500

// proxyOverwritePolicy 决定 upsert 命中已有账号时如何处理它已经绑好的代理。
type proxyOverwritePolicy bool

const (
	// overwriteAccountProxy 用传入的代理覆盖已有绑定。
	overwriteAccountProxy proxyOverwritePolicy = false
	// preserveAccountProxy 保留已有绑定，只在账号还没绑代理时填入。
	preserveAccountProxy proxyOverwritePolicy = true
)

// importProxyBinding 是一条待注册的「账号 → 代理」绑定。各渠道的导入条目结构互不
// 相同（Codex 的 importToken、Grok 的原始文件内容、Antigravity 的 document），统一
// 投影成这个形态后共用同一套注册逻辑。
type importProxyBinding struct {
	// url 由 registerImportedProxyBindings 原地规范化：非法值被清空，调用方据此
	// 回落到表单填写的代理。
	url string
	// enabled 是源端导出的启用状态，nil 表示文件没写（旧文件）。只用于告警统计。
	enabled *bool
}

// importProxyOutcome 汇总一次导入的代理注册结果，用于 SSE 计数与告警。
type importProxyOutcome struct {
	inserted int
	skipped  int
	// unusable 是注册后仍不在启用集里的代理。可能是源端就标了禁用，也可能是
	// 本机早就存在同 URL 且被禁用 / 测试失败——ON CONFLICT DO NOTHING 不会
	// 复活它。代理池开启时，绑定这些代理的账号一律不可调度。
	unusable int
	warnings []string
}

// warning 把告警合成一条 SSE 文案；无告警时返回空串。
func (o importProxyOutcome) warning() string {
	return strings.Join(o.warnings, "；")
}

// importedProxyLabel 给本批代理打上时间戳标签，便于事后在代理管理页按批筛选清理。
func importedProxyLabel() string {
	return "imported-" + time.Now().UTC().Format("20060102-1504")
}

// registerImportedProxies 是 Codex 导入条目的入口，把 importToken 上的代理三件套
// 投影成通用绑定后交给 registerImportedProxyBindings，再把规范化结果写回条目。
func (h *Handler) registerImportedProxies(ctx context.Context, tokens []importToken) (importProxyOutcome, error) {
	bindings := make([]importProxyBinding, len(tokens))
	for i := range tokens {
		bindings[i] = importProxyBinding{url: tokens[i].proxyURL, enabled: tokens[i].proxyEnabled}
	}
	outcome, err := h.registerImportedProxyBindings(ctx, bindings)
	for i := range tokens {
		tokens[i].proxyURL = bindings[i].url
	}
	return outcome, err
}

// registerImportedProxyBindings 把导入文件里携带的代理写进代理表并同步到内存代理池，
// 同时原地规范化 bindings 上的代理 URL（非法条目清空，对应账号退回表单代理）。
//
// 三步顺序不能调换：先写代理表 → 再同步代理池 → 最后才允许写账号。账号一旦绑上
// 一个「已在 managedProxySet、却不在 proxyPoolSet」的 URL，就会被判定为无可用
// 出口而整批不可调度；先写账号再同步代理池正好会开出这样一个窗口。同步失败按
// 错误返回，由调用方中止整次导入——继续导入只会写出一批不可调度的账号。
func (h *Handler) registerImportedProxyBindings(ctx context.Context, bindings []importProxyBinding) (importProxyOutcome, error) {
	var outcome importProxyOutcome

	// 收集 + 校验 + 去重。非法条目跳过而不是整批 400：一条烂代理不该毁掉整个
	// 号池的导入（这与 AddProxies 的 fail-fast 有意不同，导入场景更需要容错）。
	resolved := make(map[string]string, len(bindings))
	seen := make(map[string]bool, len(bindings))
	ordered := make([]string, 0, len(bindings))
	for i := range bindings {
		raw := strings.TrimSpace(bindings[i].url)
		if raw == "" {
			continue
		}
		normalized, known := resolved[raw]
		if !known {
			var err error
			normalized, err = normalizeManagedProxyURL(raw)
			if err != nil {
				normalized = ""
				outcome.skipped++
				log.Printf("导入代理: 跳过无效代理 %s: %v", security.MaskURLCredentials(raw), err)
			}
			resolved[raw] = normalized
			if normalized != "" && !seen[normalized] {
				seen[normalized] = true
				ordered = append(ordered, normalized)
			}
		}
		bindings[i].url = normalized
	}
	if outcome.skipped > 0 {
		outcome.warnings = append(outcome.warnings,
			fmt.Sprintf("%d 个代理格式无效已跳过，对应账号改用表单填写的代理", outcome.skipped))
	}
	if len(ordered) == 0 {
		return outcome, nil
	}

	if len(ordered) > maxImportedProxies {
		// 超限时一条都不注册。注册一半会让另一半账号绑上未入池的 URL，
		// 那种半生效状态比"整体退回表单代理"难解释得多。
		for i := range bindings {
			bindings[i].url = ""
		}
		outcome.skipped += len(ordered)
		outcome.warnings = append(outcome.warnings,
			fmt.Sprintf("文件内代理 %d 条，超过单次 %d 条上限，已全部跳过", len(ordered), maxImportedProxies))
		return outcome, nil
	}

	inserted, err := h.db.InsertProxies(ctx, ordered, importedProxyLabel())
	if err != nil {
		return outcome, fmt.Errorf("写入代理表失败: %w", err)
	}
	outcome.inserted = inserted

	if h.store != nil {
		if err := h.store.ReloadProxyPool(); err != nil {
			return outcome, fmt.Errorf("刷新代理池失败: %w", err)
		}
		if unusable := h.store.UnusableManagedProxies(ordered); len(unusable) > 0 {
			outcome.unusable = len(unusable)
			outcome.warnings = append(outcome.warnings,
				fmt.Sprintf("%d 个代理在本机处于禁用或测试失败状态，绑定它们的账号暂不会被调度", len(unusable)))
		}
	}
	if disabled := countDisabledAtSource(bindings); disabled > 0 {
		outcome.warnings = append(outcome.warnings,
			fmt.Sprintf("%d 个代理在源端是禁用状态，已按启用导入，如需保持禁用请到代理管理页关闭", disabled))
	}

	log.Printf("导入代理: 文件内 %d 条唯一代理，新增 %d 条，跳过 %d 条，不可用 %d 条",
		len(ordered), outcome.inserted, outcome.skipped, outcome.unusable)
	return outcome, nil
}

// accountHasProxyBinding 判断账号当前是否已经绑了代理。查询失败时返回 true——
// 宁可保留未知的原绑定，也不要凭一次读失败去覆盖操作员配好的代理。
//
// 必须带上回收站账号：导入命中既有身份时，对方十有八九正躺在回收站（删过的号重新
// 导入走的就是这条合并/复活路径），用不含已删除行的查询会一律读不到，
// 于是既有绑定判不出来、空绑定也永远填不上文件里的代理。
func (h *Handler) accountHasProxyBinding(ctx context.Context, accountID int64) bool {
	row, err := h.db.GetAccountByIDIncludingDeleted(ctx, accountID)
	if err != nil {
		log.Printf("导入代理: 读取账号 %d 现有代理失败，按已绑定处理: %v", accountID, err)
		return true
	}
	return row != nil && strings.TrimSpace(row.ProxyURL) != ""
}

// grokFileProxyBinding 从一份 Grok 凭据文件里取出代理三件套。
//
// Grok 的导入走 ParseGrokAuthJSON，那条链路只认凭据字段、不认代理，所以这里单独
// 扫一遍原始 JSON。代理写在顶层（本仓库导出的形态），tokens 包装形态则下探一层。
// 解析失败一律当作"没带代理"：文件本身是否合法由 ParseGrokAuthJSON 判定并报错，
// 这里再报一次只会把错误信息搅乱。
func grokFileProxyBinding(content string) importProxyBinding {
	var root map[string]json.RawMessage
	if err := json.Unmarshal([]byte(content), &root); err != nil || root == nil {
		return importProxyBinding{}
	}
	if binding := grokProxyBindingFromNode(root); binding.url != "" {
		return binding
	}
	var nested map[string]json.RawMessage
	if raw, ok := root["tokens"]; ok && json.Unmarshal(raw, &nested) == nil {
		return grokProxyBindingFromNode(nested)
	}
	return importProxyBinding{}
}

func grokProxyBindingFromNode(node map[string]json.RawMessage) importProxyBinding {
	var binding importProxyBinding
	if raw, ok := node["proxy_url"]; ok {
		var url string
		if json.Unmarshal(raw, &url) == nil {
			binding.url = strings.TrimSpace(url)
		}
	}
	if binding.url == "" {
		return importProxyBinding{}
	}
	if raw, ok := node["proxy_enabled"]; ok {
		var enabled bool
		if json.Unmarshal(raw, &enabled) == nil {
			binding.enabled = &enabled
		}
	}
	return binding
}

// countDisabledAtSource 统计源端标记为禁用的代理条数。这些代理一律以启用态
// 导入：照搬禁用状态会让绑定它们的账号立刻 fail-closed，用户看到的是"导入成功
// 但账号全废"。以启用态导入 + 明确告知，信息没丢，要禁用由操作员自己去点。
func countDisabledAtSource(bindings []importProxyBinding) int {
	disabled := make(map[string]bool)
	for _, b := range bindings {
		proxyURL := strings.TrimSpace(b.url)
		if proxyURL == "" || b.enabled == nil || *b.enabled {
			continue
		}
		disabled[proxyURL] = true
	}
	return len(disabled)
}
