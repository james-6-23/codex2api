// Fallback only: account-synchronized model discovery takes precedence.
// These are public gateway IDs, never Cloud Code's private backing IDs.
export const ANTIGRAVITY_DEFAULT_MODELS = [
  'gemini-3.8-flash-low', 'gemini-3.8-flash-medium', 'gemini-3.8-flash-high',
  'gemini-3.7-flash-low', 'gemini-3.7-flash-medium', 'gemini-3.7-flash-high',
  'gemini-3.6-flash-low', 'gemini-3.6-flash-medium', 'gemini-3.6-flash-high',
  'gemini-3.5-flash-low', 'gemini-3.5-flash-medium', 'gemini-3.5-flash-high',
  'gemini-3.1-pro-low', 'gemini-3.1-pro-high',
  'claude-opus-4-6-thinking', 'claude-sonnet-4-6', 'gpt-oss-120b-medium',
]

// antigravityModelVersion 取 gemini-<major>.<minor>-… 里的数字版本;解析不出返回 0。
export function antigravityModelVersion(model: string): number {
  const match = /^gemini-(\d+(?:\.\d+)?)/.exec(model.trim().toLowerCase())
  return match ? Number.parseFloat(match[1]) : 0
}

// orderAntigravityTestModels 生成测连弹窗的模型候选:账号行的 models 已是对外发布的
// 固定档位 ID,没同步过目录时回落默认集;跳过生图模型。首选依次为系统设置里该渠道的
// 测试模型、版本最新的 flash 低档——目录会长期残留已下线旧版(gemini-3.5-flash 上游
// 只回一句下线提示就断流),按目录顺序取首个会必失败;与后端自动选模规则一致。
export function orderAntigravityTestModels(models: readonly string[], preferred = ''): string[] {
  const candidates = models
    .map((m) => m.trim())
    .filter((m) => m && !m.toLowerCase().includes('image'))
  const base = candidates.length > 0 ? candidates : [...ANTIGRAVITY_DEFAULT_MODELS]
  const configured = base.find((m) => m.toLowerCase() === preferred.trim().toLowerCase())
  let first = configured
  if (!first) {
    for (const m of base) {
      const lower = m.toLowerCase()
      if (!lower.includes('flash') || !lower.endsWith('-low')) continue
      if (!first || antigravityModelVersion(m) > antigravityModelVersion(first)) first = m
    }
  }
  if (!first) return base
  return [first, ...base.filter((m) => m !== first)]
}
