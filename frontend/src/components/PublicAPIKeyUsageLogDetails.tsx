import { Brain } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import type { PublicAPIKeyUsageLog } from '../types'
import { formatUsageNumber } from '../lib/usageFormat'
import CompactionBadges from './CompactionBadges'
import UsageDaybreakBadge from './UsageDaybreakBadge'
import { Badge } from './ui/badge'
import { UsageInputTokenCount } from './UsageLogMetrics'

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
    <div className="whitespace-nowrap font-mono text-[13px] tabular-nums leading-relaxed">
      <UsageInputTokenCount log={log} />
      <span className="mx-1 text-border">|</span>
      <span className="text-emerald-500" title={t('usage.outputTokens')}>↑{formatUsageNumber(log.output_tokens, true)}</span>
      {log.reasoning_tokens > 0 ? (
        <>
          <span className="mx-1 text-border">|</span>
          <span className="inline-flex items-center gap-0.5 text-amber-500" title={t('qualityTest.reasoningTokens')}>
            <Brain className="size-3.5" />{formatUsageNumber(log.reasoning_tokens, true)}
          </span>
        </>
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
