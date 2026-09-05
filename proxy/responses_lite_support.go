package proxy

import (
	"log"
	"strings"
	"sync"

	"github.com/tidwall/gjson"
)

// 上游按模型 gate Responses Lite:模型清单的 use_responses_lite 字段声明哪些
// 模型走 lite 通道(实测 gpt-5.6 全系 + codex-auto-review 为 true,gpt-5.5/5.4
// 系为 false),不支持的模型带 lite 信号上游必 400 unsupported_value(param=model,
// "This model is not supported when using X-OpenAI-Internal-Codex-Responses-Lite")。
// 下游客户端按"它自己请求的模型"决定发不发信号,而网关的模型映射/payload 规则
// 可能把模型改写成另一个——这里维护 slug→能力表,发出前剥离必死组合。
//
// builtin 种子来自 2026-08 上游清单实测;清单每次经网关透传/同步时由
// RecordResponsesLiteSupportFromManifest 学习真值覆盖,种子过期可自愈。
var builtinResponsesLiteSupport = map[string]bool{
	"gpt-5.6-sol":       true,
	"gpt-5.6-terra":     true,
	"gpt-5.6-luna":      true,
	"codex-auto-review": true,
	"gpt-5.5":           false,
	"gpt-5.4":           false,
	"gpt-5.4-mini":      false,
}

var learnedResponsesLiteSupport = struct {
	sync.RWMutex
	bySlug map[string]bool
}{bySlug: make(map[string]bool)}

// RecordResponsesLiteSupportFromManifest 从上游模型清单学习每个模型的
// use_responses_lite 能力。清单是上游真值,覆盖内置种子;字段缺失或非布尔的
// 条目跳过(保持"未知",不干预)。
func RecordResponsesLiteSupportFromManifest(manifest []byte) {
	if len(manifest) == 0 {
		return
	}
	items := gjson.GetBytes(manifest, "models")
	if !items.IsArray() {
		return
	}
	type liteEntry struct {
		slug string
		lite bool
	}
	entries := make([]liteEntry, 0, 8)
	items.ForEach(func(_, item gjson.Result) bool {
		slug := strings.ToLower(strings.TrimSpace(item.Get("slug").String()))
		if slug == "" {
			return true
		}
		flag := item.Get("use_responses_lite")
		if flag.Type != gjson.True && flag.Type != gjson.False {
			return true
		}
		entries = append(entries, liteEntry{slug: slug, lite: flag.Bool()})
		return true
	})
	if len(entries) == 0 {
		return
	}
	learnedResponsesLiteSupport.Lock()
	for _, entry := range entries {
		learnedResponsesLiteSupport.bySlug[entry.slug] = entry.lite
	}
	learnedResponsesLiteSupport.Unlock()
}

// responsesLiteSupportForModel 返回模型是否支持 Responses Lite。
// known=false 表示两张表都没有该模型(新模型/别名),调用方应保持透传不干预。
func responsesLiteSupportForModel(model string) (supports bool, known bool) {
	model = strings.ToLower(strings.TrimSpace(model))
	if model == "" {
		return false, false
	}
	learnedResponsesLiteSupport.RLock()
	supports, known = learnedResponsesLiteSupport.bySlug[model]
	learnedResponsesLiteSupport.RUnlock()
	if known {
		return supports, true
	}
	supports, known = builtinResponsesLiteSupport[model]
	return supports, known
}

// gateResponsesLiteForModel 在模型定稿后收敛下游 lite 信号:已知不支持 lite 的
// 模型剥离信号(必 400 → 可运行),支持或未知的模型保持原样透传。
func gateResponsesLiteForModel(requested bool, requestBody []byte) bool {
	if !requested {
		return false
	}
	model := strings.TrimSpace(gjson.GetBytes(requestBody, "model").String())
	if supports, known := responsesLiteSupportForModel(model); known && !supports {
		log.Printf("[Lite] 模型 %s 不支持 Responses Lite,已剥离下游 lite 信号(模型映射/规则改写或下游误发)", model)
		return false
	}
	return true
}
