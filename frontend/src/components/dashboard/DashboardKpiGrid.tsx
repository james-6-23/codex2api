import type { ReactNode } from 'react'
import { useId, useMemo } from 'react'
import { useTranslation } from 'react-i18next'
import { Activity, Coins, Gauge, Users } from 'lucide-react'
import type { ChartAggregation, UsageStats } from '../../types'
import { formatUsageNumber } from '../../lib/usageFormat'
import { cn } from '@/lib/utils'

interface DashboardKpiGridProps {
  total: number
  available: number
  rateLimited: number
  errorCount: number
  todayRequests: number
  usageStats: UsageStats | null
  chartData: ChartAggregation | null
  rangeLabel: string
  showFullUsageNumbers: boolean
}

type KpiTone = 'primary' | 'emerald' | 'violet' | 'amber' | 'rose'

const TONE_STYLES: Record<KpiTone, { icon: string; stroke: string; fill: string; wash: string }> = {
  primary: {
    icon: 'bg-primary/10 text-primary ring-1 ring-primary/15',
    stroke: 'var(--color-primary)',
    fill: 'var(--color-primary)',
    wash: 'bg-[radial-gradient(ellipse_at_top_right,color-mix(in_oklab,var(--color-primary)_9%,transparent),transparent_68%)]',
  },
  emerald: {
    icon: 'bg-emerald-500/12 text-emerald-600 dark:text-emerald-400 ring-1 ring-emerald-500/20',
    stroke: '#10b981',
    fill: '#10b981',
    wash: 'bg-[radial-gradient(ellipse_at_top_right,rgba(16,185,129,0.09),transparent_68%)]',
  },
  violet: {
    icon: 'bg-violet-500/12 text-violet-600 dark:text-violet-400 ring-1 ring-violet-500/20',
    stroke: '#8b5cf6',
    fill: '#8b5cf6',
    wash: 'bg-[radial-gradient(ellipse_at_top_right,rgba(139,92,246,0.09),transparent_68%)]',
  },
  amber: {
    icon: 'bg-amber-500/12 text-amber-600 dark:text-amber-400 ring-1 ring-amber-500/20',
    stroke: '#f59e0b',
    fill: '#f59e0b',
    wash: 'bg-[radial-gradient(ellipse_at_top_right,rgba(245,158,11,0.10),transparent_68%)]',
  },
  rose: {
    icon: 'bg-rose-500/12 text-rose-600 dark:text-rose-400 ring-1 ring-rose-500/20',
    stroke: '#ef4444',
    fill: '#ef4444',
    wash: 'bg-[radial-gradient(ellipse_at_top_right,rgba(239,68,68,0.10),transparent_68%)]',
  },
}

function buildSmoothPath(coords: Array<{ x: number; y: number }>): string {
  if (coords.length === 0) return ''
  if (coords.length === 1) return `M${coords[0].x.toFixed(1)},${coords[0].y.toFixed(1)}`
  let d = `M${coords[0].x.toFixed(1)},${coords[0].y.toFixed(1)}`
  for (let i = 0; i < coords.length - 1; i += 1) {
    const p0 = coords[Math.max(0, i - 1)]
    const p1 = coords[i]
    const p2 = coords[i + 1]
    const p3 = coords[Math.min(coords.length - 1, i + 2)]
    const cp1x = p1.x + (p2.x - p0.x) / 6
    const cp1y = p1.y + (p2.y - p0.y) / 6
    const cp2x = p2.x - (p3.x - p1.x) / 6
    const cp2y = p2.y - (p3.y - p1.y) / 6
    d += ` C${cp1x.toFixed(1)},${cp1y.toFixed(1)} ${cp2x.toFixed(1)},${cp2y.toFixed(1)} ${p2.x.toFixed(1)},${p2.y.toFixed(1)}`
  }
  return d
}

function SmoothSparkline({ values, tone }: { values: number[]; tone: KpiTone }) {
  const gradId = useId()
  const geometry = useMemo(() => {
    if (values.length < 2) return null
    const slice = values.slice(-18)
    const max = Math.max(...slice)
    const min = Math.min(...slice)
    const span = max - min
    const width = 104
    const height = 44
    const padY = 6
    const padX = 3
    const usableW = width - padX * 2
    const usableH = height - padY * 2
    const coords = slice.map((val, idx) => {
      const x = padX + (idx / (slice.length - 1)) * usableW
      const norm = span > 0 ? (val - min) / span : max > 0 ? 0.55 : 0.14
      const y = height - padY - norm * usableH
      return { x, y }
    })
    const linePath = buildSmoothPath(coords)
    const last = coords[coords.length - 1]
    const first = coords[0]
    const areaPath = `${linePath} L${last.x.toFixed(1)},${height} L${first.x.toFixed(1)},${height} Z`
    return { width, height, linePath, areaPath, last }
  }, [values])

  if (!geometry) return null
  const palette = TONE_STYLES[tone]

  return (
    <svg
      viewBox={`0 0 ${geometry.width} ${geometry.height}`}
      className="h-11 w-[104px] shrink-0 overflow-visible"
      aria-hidden="true"
    >
      <defs>
        <linearGradient id={gradId} x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stopColor={palette.fill} stopOpacity="0.22" />
          <stop offset="85%" stopColor={palette.fill} stopOpacity="0.02" />
          <stop offset="100%" stopColor={palette.fill} stopOpacity="0" />
        </linearGradient>
      </defs>
      <path d={geometry.areaPath} fill={`url(#${gradId})`} />
      <path
        d={geometry.linePath}
        fill="none"
        stroke={palette.stroke}
        strokeWidth="2"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
      <circle cx={geometry.last.x} cy={geometry.last.y} r="5.5" fill={palette.stroke} opacity="0.16" />
      <circle cx={geometry.last.x} cy={geometry.last.y} r="2.5" fill={palette.stroke} />
    </svg>
  )
}

function AvailabilityRing({ percent, hasAccounts }: { percent: number; hasAccounts: boolean }) {
  const size = 46
  const stroke = 4.2
  const radius = (size - stroke) / 2
  const circumference = 2 * Math.PI * radius
  const clamped = hasAccounts ? Math.max(0, Math.min(100, percent)) : 0
  const dashOffset = circumference - (clamped / 100) * circumference
  const strokeColor = !hasAccounts
    ? 'var(--color-muted-foreground)'
    : clamped >= 80
      ? '#10b981'
      : clamped >= 50
        ? '#f59e0b'
        : '#ef4444'

  return (
    <div className="relative flex size-[46px] shrink-0 items-center justify-center">
      <svg width={size} height={size} viewBox={`0 0 ${size} ${size}`} className="-rotate-90" aria-hidden="true">
        <circle
          cx={size / 2}
          cy={size / 2}
          r={radius}
          fill="none"
          stroke="currentColor"
          strokeWidth={stroke}
          className="text-muted/80"
        />
        <circle
          cx={size / 2}
          cy={size / 2}
          r={radius}
          fill="none"
          stroke={strokeColor}
          strokeWidth={stroke}
          strokeLinecap="round"
          strokeDasharray={circumference}
          strokeDashoffset={dashOffset}
          className="transition-[stroke-dashoffset] duration-500 ease-out"
        />
      </svg>
      <span className="absolute font-sans text-[10.5px] font-semibold tracking-tight text-foreground tabular-nums">
        {hasAccounts ? `${clamped}%` : '--'}
      </span>
    </div>
  )
}

function KpiCard({
  icon,
  tone,
  label,
  value,
  suffix,
  visual,
  leftDetail,
  rightDetail,
}: {
  icon: ReactNode
  tone: KpiTone
  label: string
  value: string
  suffix?: string
  visual?: ReactNode
  leftDetail: { label: string; value: string; tone?: 'default' | 'danger' | 'warn' | 'success' }
  rightDetail: { label: string; value: string; tone?: 'default' | 'danger' | 'warn' | 'success' }
}) {
  const styles = TONE_STYLES[tone]

  return (
    <article className="group relative flex min-w-0 flex-col justify-between overflow-hidden rounded-2xl border border-border/75 bg-card p-5 shadow-2xs transition-all duration-200 hover:border-border hover:shadow-xs">
      <div aria-hidden="true" className={cn('pointer-events-none absolute inset-0 opacity-90', styles.wash)} />

      <div className="relative z-10 flex items-center justify-between gap-2">
        <span className="truncate text-xs font-medium text-muted-foreground" title={label}>
          {label}
        </span>
        <span className={cn('flex size-8 shrink-0 items-center justify-center rounded-lg', styles.icon)} aria-hidden="true">
          {icon}
        </span>
      </div>

      <div className="relative z-10 mt-3 flex items-end justify-between gap-3">
        <div className="min-w-0">
          <div className="flex items-baseline gap-1">
            <strong
              className="truncate font-sans text-[28px] leading-none font-semibold tracking-[-0.03em] text-foreground tabular-nums sm:text-[31px]"
              title={value}
            >
              {value}
            </strong>
            {suffix ? (
              <span className="font-sans text-[15px] font-normal text-muted-foreground/70 tabular-nums">
                {suffix}
              </span>
            ) : null}
          </div>
        </div>
        {visual}
      </div>

      <div className="relative z-10 mt-4 grid grid-cols-2 gap-3 border-t border-border/55 pt-3">
        <div className="min-w-0">
          <span className="block truncate text-[11px] text-muted-foreground">{leftDetail.label}</span>
          <span
            className={cn(
              'mt-0.5 block truncate font-sans text-[13px] font-medium tracking-[-0.01em] tabular-nums',
              leftDetail.tone === 'danger'
                ? 'text-destructive'
                : leftDetail.tone === 'warn'
                  ? 'text-amber-600 dark:text-amber-400'
                  : leftDetail.tone === 'success'
                    ? 'text-emerald-600 dark:text-emerald-400'
                    : 'text-foreground',
            )}
            title={leftDetail.value}
          >
            {leftDetail.value}
          </span>
        </div>
        <div className="min-w-0 border-l border-border/55 pl-3">
          <span className="block truncate text-[11px] text-muted-foreground">{rightDetail.label}</span>
          <span
            className={cn(
              'mt-0.5 block truncate font-sans text-[13px] font-medium tracking-[-0.01em] tabular-nums',
              rightDetail.tone === 'danger'
                ? 'text-destructive'
                : rightDetail.tone === 'warn'
                  ? 'text-amber-600 dark:text-amber-400'
                  : rightDetail.tone === 'success'
                    ? 'text-emerald-600 dark:text-emerald-400'
                    : 'text-foreground',
            )}
            title={rightDetail.value}
          >
            {rightDetail.value}
          </span>
        </div>
      </div>
    </article>
  )
}

function formatMoney(value: number): string {
  if (!Number.isFinite(value) || value <= 0) return '$0.00'
  if (value >= 100) return `$${value.toLocaleString(undefined, { maximumFractionDigits: 1 })}`
  if (value >= 1) return `$${value.toFixed(2)}`
  return `$${value.toFixed(4)}`
}

function formatLatency(ms?: number): string {
  if (!ms || ms <= 0) return '-'
  if (ms >= 1000) return `${(ms / 1000).toFixed(1)}s`
  return `${Math.round(ms)}ms`
}

function formatPercent(value: number): string {
  if (!Number.isFinite(value)) return '0%'
  return `${value.toFixed(value >= 10 ? 1 : 2)}%`
}

export default function DashboardKpiGrid({
  total,
  available,
  rateLimited,
  errorCount,
  todayRequests,
  usageStats,
  chartData,
  rangeLabel,
  showFullUsageNumbers,
}: DashboardKpiGridProps) {
  const { t, i18n } = useTranslation()
  const locale = i18n.language

  const availabilityRate = total > 0 ? Math.round((available / total) * 100) : 0
  const timeline = chartData?.timeline ?? []
  const requestSparkline = useMemo(() => timeline.map((p) => p.requests), [timeline])
  const tokenSparkline = useMemo(() => timeline.map((p) => p.input_tokens + p.output_tokens), [timeline])
  const cacheSparkline = useMemo(
    () =>
      timeline.map((p) => {
        const denom = p.input_tokens + p.cached_tokens
        return denom > 0 ? (p.cached_tokens / denom) * 100 : 0
      }),
    [timeline],
  )

  const rangeRequests = usageStats?.today_requests ?? 0
  const rangeTokens = usageStats?.today_tokens ?? 0
  const rangeBilled = usageStats?.today_user_billed ?? 0
  const totalTokens = usageStats?.total_tokens ?? 0
  const cacheRate = usageStats?.today_cache_rate ?? 0
  const cachedTokens = usageStats?.today_cached_tokens ?? 0
  const errorRate = usageStats?.error_rate ?? 0
  const rpm = usageStats?.rpm ?? 0
  const avgTtft = formatLatency(usageStats?.avg_first_token_ms)

  return (
    <section
      aria-label={t('dashboard.kpiGridLabel')}
      className="grid grid-cols-1 gap-3.5 sm:grid-cols-2 xl:grid-cols-4"
    >
      {/* 1. Account Pool */}
      <KpiCard
        icon={<Users className="size-4" />}
        tone={errorCount > 0 ? 'amber' : 'emerald'}
        label={t('dashboard.kpiAccounts')}
        value={String(available)}
        suffix={`/${total}`}
        visual={<AvailabilityRing percent={availabilityRate} hasAccounts={total > 0} />}
        leftDetail={{
          label: t('dashboard.rateLimited'),
          value: String(rateLimited),
          tone: rateLimited > 0 ? 'warn' : 'default',
        }}
        rightDetail={{
          label: t('dashboard.error'),
          value: String(errorCount),
          tone: errorCount > 0 ? 'danger' : 'default',
        }}
      />

      {/* 2. Request Traffic */}
      <KpiCard
        icon={<Activity className="size-4" />}
        tone="primary"
        label={t('dashboard.rangeRequests', { range: rangeLabel })}
        value={Math.round(rangeRequests).toLocaleString(locale)}
        visual={<SmoothSparkline values={requestSparkline} tone="primary" />}
        leftDetail={{
          label: t('dashboard.todayRequests'),
          value: Math.round(todayRequests).toLocaleString(locale),
        }}
        rightDetail={{
          label: 'RPM / TTFT',
          value: `${Math.round(rpm).toLocaleString(locale)} · ${avgTtft}`,
        }}
      />

      {/* 3. Tokens & Billing */}
      <KpiCard
        icon={<Coins className="size-4" />}
        tone="violet"
        label={t('dashboard.rangeTokens', { range: rangeLabel })}
        value={formatUsageNumber(rangeTokens, showFullUsageNumbers, locale)}
        visual={<SmoothSparkline values={tokenSparkline} tone="violet" />}
        leftDetail={{
          label: t('usage.tableCost'),
          value: formatMoney(rangeBilled),
          tone: rangeBilled > 0 ? 'success' : 'default',
        }}
        rightDetail={{
          label: t('dashboard.totalTokens'),
          value: formatUsageNumber(totalTokens, false, locale),
        }}
      />

      {/* 4. Cache & Reliability */}
      <KpiCard
        icon={<Gauge className="size-4" />}
        tone={errorRate > 5 ? 'rose' : 'emerald'}
        label={t('dashboard.rangeCacheHitRate', { range: rangeLabel })}
        value={formatPercent(cacheRate)}
        visual={<SmoothSparkline values={cacheSparkline} tone={errorRate > 5 ? 'rose' : 'emerald'} />}
        leftDetail={{
          label: t('dashboard.kpiCached'),
          value: formatUsageNumber(cachedTokens, false, locale),
        }}
        rightDetail={{
          label: t('dashboard.kpiErrorRate'),
          value: formatPercent(errorRate),
          tone: errorRate > 2 ? 'danger' : errorRate > 0 ? 'warn' : 'success',
        }}
      />
    </section>
  )
}
