import type { ReactNode } from 'react'
import { useTranslation } from 'react-i18next'
import { BarChart3, Clock, Gauge, Zap } from 'lucide-react'
import type { UsageStats } from '../types'
import { formatUsageNumber } from '../lib/usageFormat'
import { Card, CardContent } from '@/components/ui/card'

interface UsageStatsSummaryProps {
  stats: UsageStats
  className?: string
  showFullUsageNumbers?: boolean
}

export default function UsageStatsSummary({
  stats,
  className = '',
  showFullUsageNumbers = false,
}: UsageStatsSummaryProps) {
  const { t, i18n } = useTranslation()
  const locale = i18n.language

  return (
    <Card className={`py-0 ${className}`}>
      <CardContent className="p-4">
        <h3 className="mb-3 text-base font-semibold text-foreground">{t('dashboard.usageStats')}</h3>
        <div className="grid gap-3 md:grid-cols-2 xl:grid-cols-4">
          <MetricGroup
            icon={<BarChart3 className="size-5" />}
            iconBg="bg-blue-500/10 text-blue-500"
            title={t('dashboard.trafficGroup')}
            primaryLabel={t('dashboard.todayRequests')}
            primaryValue={formatInteger(stats.today_requests, locale)}
          >
            <MetricLine label={t('dashboard.totalRequests')} value={formatInteger(stats.total_requests, locale)} />
            <MetricLine
              label={t('dashboard.rpmTpm')}
              value={`${formatInteger(stats.rpm, locale)} / ${formatUsageNumber(stats.tpm, showFullUsageNumbers, locale)}`}
            />
          </MetricGroup>

          <MetricGroup
            icon={<Zap className="size-5" />}
            iconBg="bg-purple-500/10 text-purple-500"
            title={t('dashboard.tokenGroup')}
            primaryLabel={t('dashboard.todayTokens')}
            primaryValue={formatUsageNumber(stats.today_tokens, showFullUsageNumbers, locale)}
          >
            <MetricLine label={t('dashboard.totalTokens')} value={formatUsageNumber(stats.total_tokens, showFullUsageNumbers, locale)} />
            <MetricLine label={t('dashboard.billing')} value={`${t('usage.todayCost')}: ${formatMoney(stats.today_user_billed)} / ${t('dashboard.totalCostShort')}: ${formatMoney(stats.total_user_billed)}`} />
          </MetricGroup>

          <MetricGroup
            icon={<Gauge className="size-5" />}
            iconBg="bg-teal-500/10 text-teal-500"
            title={t('dashboard.cacheGroup')}
            primaryLabel={t('dashboard.todayCacheHitRate')}
            primaryValue={formatPercent(stats.today_cache_rate ?? 0)}
          >
            <MetricLine
              label={t('dashboard.todayCachedTokens')}
              value={formatUsageNumber(stats.today_cached_tokens ?? 0, showFullUsageNumbers, locale)}
            />
            <MetricLine label={t('dashboard.totalCacheHitRate')} value={formatPercent(stats.total_cache_rate ?? 0)} />
          </MetricGroup>

          <MetricGroup
            icon={<Clock className="size-5" />}
            iconBg="bg-cyan-500/10 text-cyan-500"
            title={t('dashboard.healthGroup')}
            primaryLabel={t('dashboard.avgFirstTokenLatency')}
            primaryValue={formatLatency(stats.avg_first_token_ms)}
          >
            <MetricLine label={t('dashboard.avgCompletionLatency')} value={formatLatency(stats.avg_duration_ms)} />
            <MetricLine label={t('dashboard.todayErrorRate')} value={formatPercent(stats.error_rate)} tone={stats.error_rate > 1 ? 'danger' : 'default'} />
          </MetricGroup>
        </div>
      </CardContent>
    </Card>
  )
}

function MetricGroup({
  icon,
  iconBg,
  title,
  primaryLabel,
  primaryValue,
  children,
}: {
  icon: ReactNode
  iconBg: string
  title: string
  primaryLabel: string
  primaryValue: string
  children: ReactNode
}) {
  return (
    <section className="min-w-0 rounded-lg border border-border/70 bg-muted/25 p-3.5">
      <div className="mb-2.5 flex items-center gap-3">
        <div className={`flex size-8 shrink-0 items-center justify-center rounded-md ${iconBg}`} aria-hidden="true">
          {icon}
        </div>
        <div className="min-w-0">
          <div className="truncate text-sm font-semibold text-foreground" title={title}>{title}</div>
          <div className="truncate text-xs text-muted-foreground" title={primaryLabel}>{primaryLabel}</div>
        </div>
      </div>
      <div className="truncate text-[26px] font-bold leading-none tabular-nums text-foreground" title={primaryValue}>
        {primaryValue}
      </div>
      <div className="mt-2.5 space-y-1.5 border-t border-border/60 pt-2">
        {children}
      </div>
    </section>
  )
}

function MetricLine({ label, value, tone = 'default' }: { label: string; value: string; tone?: 'default' | 'danger' }) {
  return (
    <div className="flex min-w-0 items-center justify-between gap-3 text-sm">
      <span className="truncate text-muted-foreground" title={label}>{label}</span>
      <span className={`shrink-0 font-semibold tabular-nums ${tone === 'danger' ? 'text-destructive' : 'text-foreground'}`} title={value}>
        {value}
      </span>
    </div>
  )
}

function formatInteger(value: number, locale: string): string {
  return Math.round(value).toLocaleString(locale)
}

function formatPercent(value: number): string {
  return `${value.toFixed(value >= 10 ? 1 : 2)}%`
}

function formatLatency(value?: number): string {
  const ms = value ?? 0
  if (ms <= 0) return '-'
  if (ms >= 1000) return `${(ms / 1000).toFixed(1)}s`
  return `${Math.round(ms)}ms`
}

function formatMoney(value: number): string {
  if (value >= 100) return `$${value.toLocaleString(undefined, { maximumFractionDigits: 1 })}`
  if (value >= 1) return `$${value.toFixed(2)}`
  return `$${value.toFixed(4)}`
}
