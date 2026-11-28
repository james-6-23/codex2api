import { createLucideIcon, DatabaseZap } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { cn } from '../lib/utils'
import { getUsageTokenBreakdown } from '../lib/usageTokenDisplay'
import { formatUsageNumber as formatTokens } from '../lib/usageFormat'
import { computeOutputTokensPerSec, durationTone, firstTokenTone, formatLatencyMs, formatTokensPerSec } from '../lib/usageTiming'
import UsageTimingBadge from './UsageTimingBadge'

type UsageMetricLog = {
  channel?: string
  status_code: number
  input_tokens: number
  output_tokens: number
  completion_tokens?: number
  cached_tokens: number
  cache_write_5m_tokens?: number
  cache_write_1h_tokens?: number
  duration_ms: number
  first_token_ms: number
}

const usageTableNumClass = 'font-sans text-[12.5px] font-medium tabular-nums tracking-[-0.01em]'

// 缓存写入图标:取 Lucide (ISC) 的 database-plus,与读取用的 DatabaseZap 成对。
// 锁定的 lucide-react 1.0.1 还没收录它,升级前在这里按同一路径注册。
const DatabasePlus = createLucideIcon('database-plus', [
  ['path', { d: 'M19 16v6', key: 'tddt3s' }],
  ['path', { d: 'M21 12.536V5', key: 'zeza6i' }],
  ['path', { d: 'M22 19h-6', key: 'vcuq98' }],
  ['path', { d: 'M3 12A9 3 0 0 0 15.1824 14.8061', key: 'ukc3b1' }],
  ['path', { d: 'M3 5V19A9 3 0 0 0 13.318 21.968', key: '1lyu4j' }],
  ['ellipse', { cx: '12', cy: '5', rx: '9', ry: '3', key: 'msslwz' }],
])

export function UsageInputTokenCount({ log }: { log: UsageMetricLog }) {
  const { t } = useTranslation()
  const tokens = getUsageTokenBreakdown(log)
  const title = tokens.isClaude
    ? t('usage.claudeInputTooltip', {
      input: formatTokens(tokens.inputTokens, true),
      total: formatTokens(tokens.totalInputTokens, true),
      read: formatTokens(tokens.cacheReadTokens, true),
      write: formatTokens(tokens.cacheWriteTokens, true),
    })
    : `${t('usage.inputTokens')}: ${formatTokens(tokens.inputTokens, true)}`

  return (
    <span className="inline-flex items-baseline gap-0.5 text-sky-600 dark:text-sky-400" title={title}>
      <span className="text-[11px] opacity-65">↓</span>
      <span>{formatTokens(tokens.inputTokens, true)}</span>
    </span>
  )
}

export function UsageCacheBadges({ log, align = 'end' }: { log: UsageMetricLog; align?: 'start' | 'end' }) {
  const { t } = useTranslation()
  const tokens = getUsageTokenBreakdown(log)
  if (tokens.cacheReadTokens === 0 && tokens.cacheWriteTokens === 0) {
    return <span className={`${usageTableNumClass} text-muted-foreground/45`}>—</span>
  }
  const readTitle = t('usage.cacheReadTooltip', { tokens: formatTokens(tokens.cacheReadTokens, true) })
  const writeTitle = t('usage.cacheCreateTooltip', {
    tokens: formatTokens(tokens.cacheWriteTokens, true),
    m5: formatTokens(tokens.cacheWrite5mTokens, true),
    h1: formatTokens(tokens.cacheWrite1hTokens, true),
  })

  // 读/写两枚徽章横排(空间不够再换行),不再竖着堆叠把整行撑高
  return (
    <div className={cn('flex flex-wrap items-center gap-1', align === 'start' ? 'justify-start' : 'justify-end')}>
      {tokens.cacheReadTokens > 0 && (
        <span
          title={readTitle}
          aria-label={readTitle}
          className="inline-flex h-[22px] items-center gap-1 rounded-md bg-violet-500/[0.08] px-1.5 font-sans text-[11.5px] font-medium tabular-nums text-violet-700 ring-1 ring-inset ring-violet-500/15 dark:bg-violet-500/15 dark:text-violet-300 dark:ring-violet-400/20"
        >
          <DatabaseZap className="size-3 shrink-0 opacity-80" aria-hidden="true" />
          {formatTokens(tokens.cacheReadTokens, true)}
        </span>
      )}
      {tokens.cacheWriteTokens > 0 && (
        <span
          title={writeTitle}
          aria-label={writeTitle}
          className="inline-flex h-[22px] items-center gap-1 rounded-md bg-amber-500/[0.09] px-1.5 font-sans text-[11.5px] font-medium tabular-nums text-amber-700 ring-1 ring-inset ring-amber-500/20 dark:bg-amber-500/15 dark:text-amber-300 dark:ring-amber-400/20"
        >
          <DatabasePlus className="size-3 shrink-0 opacity-80" aria-hidden="true" />
          {formatTokens(tokens.cacheWriteTokens, true)}
        </span>
      )}
    </div>
  )
}

function tokensPerSecClassName(value: number): string {
  // Rough bands for visual scan; absolute numbers vary by model.
  if (value >= 80) return 'text-emerald-600 dark:text-emerald-400'
  if (value >= 30) return 'text-foreground'
  if (value >= 10) return 'text-amber-600 dark:text-amber-400'
  return 'text-rose-600 dark:text-rose-400'
}

export function TokensPerSecCell({ log }: { log: UsageMetricLog }) {
  const value = computeOutputTokensPerSec(log)
  if (value == null) {
    return <span className={`${usageTableNumClass} text-muted-foreground/45`}>—</span>
  }
  return (
    <span className={`${usageTableNumClass} ${tokensPerSecClassName(value)}`} title={`${value.toFixed(2)} tok/s`}>
      {formatTokensPerSec(value)}
      <span className="ml-0.5 text-[10.5px] font-normal text-muted-foreground/75">tok/s</span>
    </span>
  )
}

/** 首字 + 总耗时聚合列：横向双胶囊，柔和底色且仅异常态着色。 */
export function TimingCell({ log }: { log: UsageMetricLog }) {
  const { t } = useTranslation()
  const firstMs = log.first_token_ms || 0
  const durationMs = log.duration_ms || 0
  if (firstMs <= 0 && durationMs <= 0) {
    return <span className={`${usageTableNumClass} text-muted-foreground/45`}>—</span>
  }

  const firstLabel = firstMs > 0 ? formatLatencyMs(firstMs) : '-'
  const durationLabel = durationMs > 0 ? formatLatencyMs(durationMs) : '-'
  const title = [
    firstMs > 0 ? `${t('usage.timingFirst')}: ${firstLabel}` : null,
    durationMs > 0 ? `${t('usage.timingDuration')}: ${durationLabel}` : null,
  ]
    .filter(Boolean)
    .join(' · ')

  return (
    <div className="inline-flex items-center gap-1" title={title || undefined}>
      <UsageTimingBadge
        kind="first"
        label={t('usage.timingFirst')}
        value={formatLatencyMs(firstMs)}
        tone={firstTokenTone(firstMs)}
      />
      <UsageTimingBadge
        kind="duration"
        label={t('usage.timingDuration')}
        value={formatLatencyMs(durationMs)}
        tone={durationTone(durationMs)}
      />
    </div>
  )
}
