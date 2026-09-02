import type { ModelPricingOverride } from '../types.ts'

export type PricingPreviewRate = {
  input: number
  cached: number
  output: number
}

export type ModelPricingPreview = {
  mode: 'single' | 'tiered'
  threshold: number
  standard: PricingPreviewRate
  long: PricingPreviewRate | null
  priority: PricingPreviewRate | null
  flexMultiplier: number | null
  expression: string
}

function numberValue(value: unknown): number {
  const parsed = typeof value === 'number' ? value : Number(value)
  return Number.isFinite(parsed) ? parsed : 0
}

function rate(input: unknown, cached: unknown, output: unknown): PricingPreviewRate {
  return {
    input: numberValue(input),
    cached: numberValue(cached),
    output: numberValue(output),
  }
}

function rateExpression(value: PricingPreviewRate): string {
  return `p * ${value.input} + c * ${value.output} + cr * ${value.cached}`
}

function hasRate(value: PricingPreviewRate | null): value is PricingPreviewRate {
  return Boolean(value && (value.input > 0 || value.cached > 0 || value.output > 0))
}

export function buildModelPricingPreview(
  pricing: ModelPricingOverride = {},
): ModelPricingPreview {
  const standard = rate(pricing.input, pricing.cached_input, pricing.output)
  const threshold = Math.max(
    0,
    Math.round(numberValue(pricing.long_context_threshold_tokens)),
  )
  const candidateLong = rate(
    pricing.input_long,
    pricing.cached_input_long,
    pricing.output_long,
  )
  const long = threshold > 0 && hasRate(candidateLong) ? candidateLong : null
  const candidatePriority = rate(
    pricing.input_priority,
    pricing.cached_input_priority,
    pricing.output_priority,
  )
  const priority = hasRate(candidatePriority) ? candidatePriority : null
  const mode = long ? 'tiered' : 'single'
  const baseExpression = long
    ? `len < ${threshold} ? tier("standard", ${rateExpression(standard)}) : tier("long_context", ${rateExpression(long)})`
    : `tier("standard", ${rateExpression(standard)})`
  const withServiceTiers = priority || long
    ? `${baseExpression} * (param("service_tier") == "flex" ? 0.5 : 1) * (param("service_tier") == "priority" ? 2 : 1)`
    : baseExpression

  return {
    mode,
    threshold,
    standard,
    long,
    priority,
    flexMultiplier: priority || long ? 0.5 : null,
    expression: withServiceTiers,
  }
}
