import { Brain, DatabaseZap } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import type { PublicAPIKeyUsageLog } from '../types'
import { formatUsageNumber } from '../lib/usageFormat'
import { computeOutputTokensPerSec, durationTone, firstTokenTone, formatLatencyMs, formatTokensPerSec, tokensPerSecTone } from '../lib/usageTiming'
import CompactionBadges from './CompactionBadges'
import UsageDaybreakBadge from './UsageDaybreakBadge'
import { Badge } from './ui/badge'
import UsageTimingBadge from './UsageTimingBadge'

export function PublicUsageRequestType({ log }: { log: PublicAPIKeyUsageLog }) {
  return (
    <div className="flex flex-wrap items-center gap-1.5">
      <Badge
        variant="outline"
        className={`border-transparent text-[11px] font-semibold ${log.stream
          ? 'bg-indigo-500/12 text-indigo-600 dark:bg-indigo-500/20 dark:text-indigo-400'
          : 'bg-amber-500/12 text-amber-700 dark:bg-amber-500/20 dark:text-amber-300'}`}
      >
        {log.stream ? 'stream' : 'sync'}
      </Badge>
      <UsageDaybreakBadge program={log.daybreak_program} />
      <CompactionBadges compact={log.compact} hasCompactionHistory={log.has_compaction_history} />
    </div>
  )
}

export function PublicUsageTokens({ log }: { log: PublicAPIKeyUsageLog }) {
  const { t } = useTranslation()
  if (log.status_code >= 400 || !(log.input_tokens > 0 || log.output_tokens > 0 || log.reasoning_tokens > 0)) {
    return <span className="font-mono text-[12px] text-muted-foreground">-</span>
  }
  return (
    <div className="flex flex-wrap items-center gap-x-1 gap-y-0.5 font-mono text-[12px] tabular-nums lg:flex-nowrap lg:justify-end lg:whitespace-nowrap lg:text-[13px]">
      <span className="text-blue-500" title={t('usage.inputTokens')}>↓{formatUsageNumber(log.input_tokens, true)}</span>
      <span className="text-border">|</span>
      <span className="text-emerald-500" title={t('usage.outputTokens')}>↑{formatUsageNumber(log.output_tokens, true)}</span>
      {log.reasoning_tokens > 0 ? (
        <>
          <span className="text-border">|</span>
          <span className="inline-flex items-center gap-0.5 text-amber-500" title={t('qualityTest.reasoningTokens')}>
            <Brain className="size-3.5" />{formatUsageNumber(log.reasoning_tokens, true)}
          </span>
        </>
      ) : null}
      {log.cached_tokens > 0 ? (
        <span className="inline-flex items-center gap-0.5 text-indigo-500" title={t('usage.cacheReadCost')}>
          <DatabaseZap className="size-3" />{formatUsageNumber(log.cached_tokens, true)}
        </span>
      ) : null}
    </div>
  )
}

export function PublicUsageUserAgent({ value }: { value: string }) {
  return (
    <span className="block break-all font-mono text-[12px] text-muted-foreground lg:max-w-[240px] lg:truncate" title={value || '-'}>
      {value || '-'}
    </span>
  )
}

export function PublicUsagePerformance({ log }: { log: PublicAPIKeyUsageLog }) {
  const { t } = useTranslation()
  const speed = computeOutputTokensPerSec(log)
  return (
    <div className="flex flex-col items-end gap-1">
      <UsageTimingBadge kind="first" label="TTFT" value={formatLatencyMs(log.first_token_ms)} tone={firstTokenTone(log.first_token_ms)} title={t('usage.tableFirstTokenHint')} />
      <UsageTimingBadge kind="duration" label={t('usage.timingDuration')} value={formatLatencyMs(log.duration_ms)} tone={durationTone(log.duration_ms)} title={t('usage.tableDuration')} />
      <UsageTimingBadge kind="speed" label="tok/s" value={speed == null ? '-' : formatTokensPerSec(speed)} tone={tokensPerSecTone(speed)} title={t('usage.tableTokensPerSecHint')} />
    </div>
  )
}
