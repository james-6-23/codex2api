const IMAGE_BASE_MODELS = ['gpt-image-2.5-flare', 'gpt-image-2.5-sunburst', 'gpt-image-2']

export const IMAGE_MODELS = IMAGE_BASE_MODELS.flatMap(model =>
  ['', '-2k', '-4k'].map(suffix => ({ label: model + suffix, value: model + suffix })),
)

export function isImage25Model(model: string): boolean {
  return /^gpt-image-2\.5-(flare|sunburst)(-\d{4}-\d{2}-\d{2})?$/.test(
    model.trim().toLowerCase().replace(/-(2k|4k)$/, ''),
  )
}

export function imageQualityOptions(model: string) {
  const values = ['auto', 'low', 'medium', 'high']
  if (isImage25Model(model)) values.push('xhigh', 'max')
  return values.map(value => ({ label: value === 'auto' ? 'Auto' : value, value }))
}

// 仅调整工作台的模型切换；公共 API 保留调用方显式传入的质量。
export function normalizeImageQualityForModel(quality: string, model: string): string {
  return (quality === 'xhigh' || quality === 'max') && !isImage25Model(model) ? 'high' : quality
}
