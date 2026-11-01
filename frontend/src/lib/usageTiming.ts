type UsageTimingLog = {
  status_code: number
  output_tokens: number
  completion_tokens?: number
  duration_ms: number
  first_token_ms: number
}

const MIN_GENERATION_WINDOW_MS = 20
const MS_PER_SECOND = 1_000

export type TimingTone = 'good' | 'warn' | 'bad' | 'muted'

export function computeOutputTokensPerSec(log: UsageTimingLog): number | null {
  if (log.status_code >= 400) return null
  const outputTokens = Math.max(0, log.output_tokens || log.completion_tokens || 0)
  if (outputTokens <= 0 || log.duration_ms <= 0) return null
  let generationMs = log.duration_ms
  if (log.first_token_ms > 0 && log.first_token_ms < log.duration_ms) {
    generationMs = log.duration_ms - log.first_token_ms
  }
  // 首字过于接近结束时改用总耗时，避免极短窗口夸大吞吐。
  if (generationMs < MIN_GENERATION_WINDOW_MS) generationMs = log.duration_ms
  if (generationMs <= 0) return null
  return outputTokens / (generationMs / MS_PER_SECOND)
}

export function formatTokensPerSec(value: number): string {
  if (!Number.isFinite(value) || value <= 0) return '-'
  if (value >= 1000) return `${(value / 1000).toFixed(1)}k`
  if (value >= 100) return value.toFixed(0)
  if (value >= 10) return value.toFixed(1)
  return value.toFixed(2)
}

export function formatLatencyMs(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return '-'
  if (ms >= MS_PER_SECOND) return `${(ms / MS_PER_SECOND).toFixed(1)}s`
  return `${Math.round(ms)}ms`
}

export function firstTokenTone(ms: number): TimingTone {
  if (ms <= 0) return 'muted'
  if (ms > 5000) return 'bad'
  if (ms > 2000) return 'warn'
  return 'good'
}

export function durationTone(ms: number): TimingTone {
  if (ms <= 0) return 'muted'
  if (ms > 30000) return 'bad'
  if (ms > 10000) return 'warn'
  return 'good'
}
