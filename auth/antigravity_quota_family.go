package auth

import (
	"strings"
)

// antigravityQuotaFamily 是 Antigravity 配额桶的「模型族」：上游配额汇总只按分组下发
// 桶（gemini-weekly / gemini-5h / 3p-weekly / 3p-5h …），不说明哪个模型扣哪个桶，
// 因此这里按桶 ID 与模型 ID 的稳定前缀把两者归到同一族，供门禁、压力分与临期优先
// 只看「本次请求真正会扣的那几个桶」。
//
// 归族失败（未知前缀的桶 / 无法识别的模型）一律走保守口径：未知桶对所有模型都生效，
// 无法识别的模型看全部桶——与引入分族之前的整号口径一致，绝不因为猜错族而放行。
type antigravityQuotaFamily string

const (
	antigravityQuotaFamilyUnknown    antigravityQuotaFamily = ""
	antigravityQuotaFamilyGemini     antigravityQuotaFamily = "gemini"
	antigravityQuotaFamilyThirdParty antigravityQuotaFamily = "3p"
	// antigravityQuotaFamilyArgon 是旗舰 Gemini 的独立周配额（Ultra 计划）。快照里没有
	// 该族的桶时，Argon 模型回退到 Gemini 族，避免在桶缺席时整族不设防。
	antigravityQuotaFamilyArgon antigravityQuotaFamily = "argon"
)

// antigravityBucketQuotaFamily 按桶 ID 归族。Argon 先于 gemini 前缀判定：Argon 桶的
// ID 同样以 gemini 开头，顺序颠倒会把旗舰周配额算进普通 Gemini 族。
func antigravityBucketQuotaFamily(bucketID string) antigravityQuotaFamily {
	id := strings.ToLower(strings.TrimSpace(bucketID))
	switch {
	case id == "":
		return antigravityQuotaFamilyUnknown
	case strings.Contains(id, "argon"):
		return antigravityQuotaFamilyArgon
	case strings.HasPrefix(id, "3p"):
		return antigravityQuotaFamilyThirdParty
	case strings.HasPrefix(id, "gemini"):
		return antigravityQuotaFamilyGemini
	default:
		return antigravityQuotaFamilyUnknown
	}
}

// antigravityModelQuotaFamily 按 public / wire 模型 ID 归族。Claude 与 GPT-OSS 共用
// 「Claude and GPT models」分组（3p-* 桶），Gemini 家族共用 gemini-* 桶。
func antigravityModelQuotaFamily(model string) antigravityQuotaFamily {
	m := strings.ToLower(strings.TrimSpace(model))
	m = strings.TrimPrefix(m, "models/")
	switch {
	case m == "":
		return antigravityQuotaFamilyUnknown
	case strings.Contains(m, "argon"):
		return antigravityQuotaFamilyArgon
	case strings.HasPrefix(m, "gemini"):
		return antigravityQuotaFamilyGemini
	case strings.HasPrefix(m, "claude"), strings.HasPrefix(m, "gpt"):
		return antigravityQuotaFamilyThirdParty
	default:
		return antigravityQuotaFamilyUnknown
	}
}

// antigravityQuotaBucketScope 判定一个桶是否落在某次请求的配额作用域内。
type antigravityQuotaBucketScope func(bucket AntigravityQuotaBucket) bool

// antigravityQuotaScopeAll 是整号作用域：所有桶都算（无模型上下文时的口径）。
func antigravityQuotaScopeAll(AntigravityQuotaBucket) bool { return true }

// antigravityQuotaScopeForModel 返回 model 在该快照上的桶作用域。
//
//   - 模型无法归族：整号作用域（保守，等同分族前的行为）；
//   - Argon 模型：快照有 Argon 桶就看 Argon 族，否则回退 Gemini 族；
//   - 其余：只看同族桶。
//
// 未知族的桶对任何模型都算在作用域内：不知道它限制谁，就当它限制所有人。
// includeUnknown=false 时排除未知族桶，供只做「加分」的临期打分使用——加分不能建立在
// 猜测上，而门禁/压力的保守口径需要它们。
func antigravityQuotaScopeForModel(snapshot *AntigravityQuotaSnapshot, model string, includeUnknown bool) (antigravityQuotaBucketScope, bool) {
	family := antigravityModelQuotaFamily(model)
	if family == antigravityQuotaFamilyUnknown {
		return antigravityQuotaScopeAll, false
	}
	if family == antigravityQuotaFamilyArgon && !antigravitySnapshotHasQuotaFamily(snapshot, antigravityQuotaFamilyArgon) {
		family = antigravityQuotaFamilyGemini
	}
	return func(bucket AntigravityQuotaBucket) bool {
		bucketFamily := antigravityBucketQuotaFamily(bucket.BucketID)
		if bucketFamily == antigravityQuotaFamilyUnknown {
			return includeUnknown
		}
		return bucketFamily == family
	}, true
}

// antigravitySnapshotHasQuotaFamily 报告快照里是否存在属于 family 的白名单窗口桶。
func antigravitySnapshotHasQuotaFamily(snapshot *AntigravityQuotaSnapshot, family antigravityQuotaFamily) bool {
	if snapshot == nil {
		return false
	}
	for _, g := range snapshot.Groups {
		for _, b := range g.Buckets {
			if _, ok := NormalizeAntigravityWindow(b.Window); !ok {
				continue
			}
			if antigravityBucketQuotaFamily(b.BucketID) == family {
				return true
			}
		}
	}
	return false
}
