type UsageTokenSource = {
  channel?: string
  input_tokens: number
  output_tokens: number
  cached_tokens: number
  cache_write_5m_tokens?: number
  cache_write_1h_tokens?: number
}

function normalizeTokenCount(value: unknown): number {
  return typeof value === 'number' && Number.isFinite(value) && value > 0
    ? value
    : 0
}

// Stored Claude input includes cache reads and writes. Show its uncached input
// separately without changing the totals used by statistics and billing.
export function getUsageTokenBreakdown(log: UsageTokenSource) {
  const isClaude =
    typeof log.channel === 'string' && log.channel.trim().toLowerCase() === 'claude'
  const totalInputTokens = normalizeTokenCount(log.input_tokens)
  const outputTokens = normalizeTokenCount(log.output_tokens)
  const cacheReadTokens = normalizeTokenCount(log.cached_tokens)
  const cacheWrite5mTokens = normalizeTokenCount(log.cache_write_5m_tokens)
  const cacheWrite1hTokens = normalizeTokenCount(log.cache_write_1h_tokens)
  const cacheWriteTokens = cacheWrite5mTokens + cacheWrite1hTokens
  const inputTokens = isClaude
    ? Math.max(0, totalInputTokens - cacheReadTokens - cacheWriteTokens)
    : totalInputTokens

  return {
    isClaude,
    totalInputTokens,
    inputTokens,
    outputTokens,
    cacheReadTokens,
    cacheWriteTokens,
    cacheWrite5mTokens,
    cacheWrite1hTokens,
  }
}
