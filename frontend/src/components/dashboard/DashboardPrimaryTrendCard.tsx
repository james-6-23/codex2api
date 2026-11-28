import { useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import {
  Area,
  AreaChart,
  Bar,
  BarChart,
  CartesianGrid,
  ComposedChart,
  Line,
  LineChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts'
import { Activity, Coins, Layers, Timer } from 'lucide-react'
import { Card, CardContent } from '@/components/ui/card'
import StateShell from '../StateShell'
import type { ChartAggregation, UsageStats } from '../../types'
import { getBucketConfig, type TimeRangeKey } from '../../lib/timeRange'
import { getTimezone } from '../../utils/time'
import { cn } from '@/lib/utils'

export type DashboardTrendTab = 'traffic' | 'tokens' | 'latency' | 'billing'

interface DashboardPrimaryTrendCardProps {
  chartData: ChartAggregation | null
  usageStats: UsageStats | null
  timeRange: TimeRangeKey
  refreshedAt: number | null
  refreshIntervalMs: number
  loading?: boolean
}

const chartMargin = { top: 10, right: 12, left: 0, bottom: 0 }
const gridColor = 'var(--color-border)'
const axisColor = 'var(--color-muted-foreground)'

const compactFormatter = new Intl.NumberFormat(undefined, {
  notation: 'compact',
  maximumFractionDigits: 1,
})

function formatCompact(value: number): string {
  if (!Number.isFinite(value)) return '0'
  if (Math.abs(value) < 1000) return String(Math.round(value))
  return compactFormatter.format(value)
}

function formatDurationMs(ms: number | null | undefined): string {
  if (!ms || !Number.isFinite(ms) || ms <= 0) return '-'
  if (ms >= 1000) return `${(ms / 1000).toFixed(1)}s`
  return `${Math.round(ms)}ms`
}

function formatUsd(value: number): string {
  if (!Number.isFinite(value) || value <= 0) return '$0.00'
  if (value >= 100) return `$${value.toLocaleString(undefined, { maximumFractionDigits: 1 })}`
  if (value >= 1) return `$${value.toFixed(2)}`
  return `$${value.toFixed(4)}`
}

function formatClock(ts: number | null): string {
  if (!ts) return '--:--:--'
  return new Date(ts).toLocaleTimeString(undefined, {
    hour: '2-digit',
    minute: '2-digit',
    second: '2-digit',
    hour12: false,
    timeZone: getTimezone(),
  })
}

function formatBucketAxis(date: Date, bucketMinutes: number): string {
  const tz = getTimezone()
  if (bucketMinutes >= 1440) {
    return date.toLocaleDateString(undefined, { month: '2-digit', day: '2-digit', timeZone: tz })
  }
  if (bucketMinutes >= 360) {
    return date.toLocaleString(undefined, { month: '2-digit', day: '2-digit', hour: '2-digit', hour12: false, timeZone: tz })
  }
  return date.toLocaleTimeString(undefined, { hour: '2-digit', minute: '2-digit', hour12: false, timeZone: tz })
}

function formatBucketFull(date: Date, bucketMinutes: number): string {
  const tz = getTimezone()
  if (bucketMinutes >= 1440) {
    return date.toLocaleDateString(undefined, { year: 'numeric', month: '2-digit', day: '2-digit', timeZone: tz })
  }
  return date.toLocaleString(undefined, { month: '2-digit', day: '2-digit', hour: '2-digit', minute: '2-digit', hour12: false, timeZone: tz })
}

function StudioChartTooltip({
  active,
  payload,
  formatter,
}: {
  active?: boolean
  payload?: ReadonlyArray<{ name?: string | number; value?: unknown; color?: string; payload?: { fullLabel?: string } }>
  formatter: (val: number) => string
}) {
  if (!active || !payload || payload.length === 0) return null
  const header = payload[0]?.payload?.fullLabel ?? ''
  return (
    <div className="min-w-[152px] rounded-xl border border-border/80 bg-popover/95 px-3 py-2.5 text-xs shadow-lg backdrop-blur-md">
      {header ? (
        <div className="mb-1.5 border-b border-border/60 pb-1 text-[11px] font-medium text-muted-foreground tabular-nums">
          {header}
        </div>
      ) : null}
      <div className="space-y-1">
        {payload.map((entry, idx) => (
          <div key={`${entry.name}-${idx}`} className="flex items-center justify-between gap-4">
            <span className="inline-flex items-center gap-1.5 text-muted-foreground">
              <span className="size-2 rounded-full" style={{ backgroundColor: entry.color || 'var(--color-primary)' }} />
              {entry.name}
            </span>
            <span className="font-sans font-medium text-foreground tabular-nums">
              {formatter(Number(entry.value ?? 0))}
            </span>
          </div>
        ))}
      </div>
    </div>
  )
}

export default function DashboardPrimaryTrendCard({
  chartData,
  usageStats,
  timeRange,
  refreshedAt,
  refreshIntervalMs,
  loading = false,
}: DashboardPrimaryTrendCardProps) {
  const { t } = useTranslation()
  const [activeTab, setActiveTab] = useState<DashboardTrendTab>('traffic')
  const [pinnedSeries, setPinnedSeries] = useState<string | null>(null)
  const { bucketMinutes } = getBucketConfig(timeRange)
  const isLive = timeRange === '1h'

  const seriesData = useMemo(() => {
    const timeline = chartData?.timeline ?? []
    return timeline.map((pt) => {
      const date = new Date(pt.bucket)
      return {
        label: formatBucketAxis(date, bucketMinutes),
        fullLabel: formatBucketFull(date, bucketMinutes),
        requests: pt.requests,
        errors4xx: pt.errors_4xx,
        errors5xx: pt.errors_5xx,
        inputTokens: pt.input_tokens,
        outputTokens: pt.output_tokens,
        reasoningTokens: pt.reasoning_tokens,
        cachedTokens: pt.cached_tokens,
        avgLatency: pt.avg_latency > 0 ? Math.round(pt.avg_latency) : null,
        avgTtft: pt.avg_first_token_ms && pt.avg_first_token_ms > 0 ? Math.round(pt.avg_first_token_ms) : null,
        userBilled: Number((pt.user_billed ?? 0).toFixed(4)),
        accountBilled: Number((pt.account_billed ?? 0).toFixed(4)),
      }
    })
  }, [chartData, bucketMinutes])

  const totals = useMemo(() => {
    let requests = 0
    let errors4xx = 0
    let errors5xx = 0
    let inputTokens = 0
    let outputTokens = 0
    let cachedTokens = 0
    let userBilled = 0
    let accountBilled = 0
    for (const pt of seriesData) {
      requests += pt.requests
      errors4xx += pt.errors4xx
      errors5xx += pt.errors5xx
      inputTokens += pt.inputTokens
      outputTokens += pt.outputTokens
      cachedTokens += pt.cachedTokens
      userBilled += pt.userBilled
      accountBilled += pt.accountBilled
    }
    return {
      requests,
      errors4xx,
      errors5xx,
      inputTokens,
      outputTokens,
      cachedTokens,
      userBilled,
      accountBilled,
      margin: userBilled - accountBilled,
    }
  }, [seriesData])

  const summaryBlocks = useMemo(() => {
    switch (activeTab) {
      case 'traffic':
        return [
          { key: 'requests', label: t('dashboard.seriesRequests'), value: totals.requests.toLocaleString(), dot: 'bg-primary', accent: 'ring-primary/35 bg-primary/[0.04]' },
          { key: 'errors4xx', label: t('dashboard.series4xxErrors'), value: totals.errors4xx.toLocaleString(), dot: 'bg-amber-500', accent: 'ring-amber-500/35 bg-amber-500/[0.05]' },
          { key: 'errors5xx', label: t('dashboard.series5xxErrors'), value: totals.errors5xx.toLocaleString(), dot: 'bg-destructive', accent: 'ring-destructive/35 bg-destructive/[0.05]' },
        ]
      case 'tokens':
        return [
          { key: 'inputTokens', label: t('dashboard.seriesInputTokens'), value: formatCompact(totals.inputTokens), dot: 'bg-sky-500', accent: 'ring-sky-500/35 bg-sky-500/[0.05]' },
          { key: 'outputTokens', label: t('dashboard.seriesOutputTokens'), value: formatCompact(totals.outputTokens), dot: 'bg-emerald-500', accent: 'ring-emerald-500/35 bg-emerald-500/[0.05]' },
          { key: 'cachedTokens', label: t('dashboard.seriesCachedTokens'), value: formatCompact(totals.cachedTokens), dot: 'bg-violet-500', accent: 'ring-violet-500/35 bg-violet-500/[0.05]' },
        ]
      case 'latency':
        return [
          { key: 'avgTtft', label: t('dashboard.avgFirstTokenLatency'), value: formatDurationMs(usageStats?.avg_first_token_ms), dot: 'bg-emerald-500', accent: 'ring-emerald-500/35 bg-emerald-500/[0.05]' },
          { key: 'avgLatency', label: t('dashboard.avgCompletionLatency'), value: formatDurationMs(usageStats?.avg_duration_ms), dot: 'bg-primary', accent: 'ring-primary/35 bg-primary/[0.04]' },
          { key: 'rpm', label: 'RPM / TPM', value: `${Math.round(usageStats?.rpm ?? 0)} / ${formatCompact(usageStats?.tpm ?? 0)}`, dot: 'bg-amber-500', accent: '', nonInteractive: true },
        ]
      case 'billing':
        return [
          { key: 'userBilled', label: t('dashboard.trendUserBilled'), value: formatUsd(totals.userBilled), dot: 'bg-emerald-500', accent: 'ring-emerald-500/35 bg-emerald-500/[0.05]' },
          { key: 'accountBilled', label: t('dashboard.trendAccountCost'), value: formatUsd(totals.accountBilled), dot: 'bg-sky-500', accent: 'ring-sky-500/35 bg-sky-500/[0.05]' },
          { key: 'margin', label: t('dashboard.trendEstMargin'), value: formatUsd(Math.max(0, totals.margin)), dot: 'bg-violet-500', accent: '', nonInteractive: true },
        ]
    }
  }, [activeTab, t, totals, usageStats])

  const seriesOpacity = (key: string) => (!pinnedSeries || pinnedSeries === key ? 1 : 0.16)

  const tabs: Array<{ id: DashboardTrendTab; label: string; icon: typeof Activity }> = [
    { id: 'traffic', label: t('dashboard.trendTabTraffic'), icon: Activity },
    { id: 'tokens', label: t('dashboard.trendTabTokens'), icon: Layers },
    { id: 'latency', label: t('dashboard.trendTabLatency'), icon: Timer },
    { id: 'billing', label: t('dashboard.trendTabBilling'), icon: Coins },
  ]

  const hasSamples = totals.requests > 0

  return (
    <Card className="flex h-full flex-col rounded-2xl border-border/75 bg-card py-0 shadow-2xs">
      <CardContent className="flex flex-1 flex-col p-5">
        {/* Single-line Header + Segmented Switcher */}
        <div className="flex flex-wrap items-center justify-between gap-3">
          <div className="flex min-w-0 items-center gap-2.5">
            <h3 className="text-[15.5px] font-semibold tracking-tight text-foreground">
              {t('dashboard.primaryTrendTitle')}
            </h3>
            {isLive ? (
              <span className="inline-flex shrink-0 items-center gap-1.5 rounded-full border border-emerald-500/20 bg-emerald-500/10 px-2 py-0.5 text-[11px] font-medium text-emerald-700 tabular-nums dark:text-emerald-300">
                <span className="size-1.5 rounded-full bg-emerald-500 animate-pulse" />
                {formatClock(refreshedAt)}
              </span>
            ) : null}
          </div>

          <div
            role="tablist"
            aria-label={t('dashboard.primaryTrendTitle')}
            className="inline-flex max-w-full shrink-0 flex-wrap items-center gap-0.5 rounded-xl border border-border/70 bg-muted/45 p-0.5"
          >
            {tabs.map((tab) => {
              const Icon = tab.icon
              const active = activeTab === tab.id
              return (
                <button
                  key={tab.id}
                  type="button"
                  role="tab"
                  aria-selected={active}
                  onClick={() => {
                    setActiveTab(tab.id)
                    setPinnedSeries(null)
                  }}
                  className={cn(
                    'inline-flex items-center gap-1 rounded-lg px-2.5 py-1 text-[11.5px] font-medium whitespace-nowrap transition-all',
                    active
                      ? 'bg-card text-foreground shadow-2xs ring-1 ring-border/60 font-semibold'
                      : 'text-muted-foreground hover:text-foreground',
                  )}
                >
                  <Icon className="size-3" />
                  <span>{tab.label}</span>
                </button>
              )
            })}
          </div>
        </div>

        {/* Interactive 3-tile summary bar */}
        <div className="mt-4 grid grid-cols-3 gap-2.5">
          {summaryBlocks.map((item) => {
            const isPinned = pinnedSeries === item.key
            const interactive = !item.nonInteractive
            return (
              <button
                key={item.key}
                type="button"
                disabled={!interactive}
                aria-pressed={interactive ? isPinned : undefined}
                onClick={() => {
                  if (!interactive) return
                  setPinnedSeries((curr) => (curr === item.key ? null : item.key))
                }}
                className={cn(
                  'flex min-w-0 items-center justify-between gap-2 rounded-xl border border-border/60 bg-muted/30 px-3.5 py-2.5 text-left transition-all',
                  interactive && 'cursor-pointer hover:border-border hover:bg-muted/55',
                  isPinned && cn('border-transparent ring-1', item.accent),
                )}
              >
                <div className="min-w-0">
                  <div className="flex items-center gap-1.5">
                    <span className={cn('size-2 shrink-0 rounded-full', item.dot)} aria-hidden="true" />
                    <span className="truncate text-[11px] font-medium text-muted-foreground">
                      {item.label}
                    </span>
                  </div>
                  <strong className="mt-1 block truncate font-sans text-[19px] leading-tight font-semibold tracking-[-0.025em] text-foreground tabular-nums">
                    {item.value}
                  </strong>
                </div>
              </button>
            )
          })}
        </div>

        {/* Chart Canvas */}
        <div className="mt-4 min-h-[290px] flex-1">
          {loading && !hasSamples ? (
            <div className="flex h-[290px] items-end gap-2 px-3 pb-4">
              {[35, 62, 48, 78, 52, 70, 42, 65, 84, 58, 72, 60].map((h, i) => (
                <div
                  key={i}
                  className="flex-1 animate-pulse rounded-t-md bg-muted/55"
                  style={{ height: `${h}%`, animationDelay: `${i * 60}ms` }}
                />
              ))}
            </div>
          ) : !hasSamples ? (
            <div className="flex h-[290px] items-center justify-center">
              <StateShell
                variant="section"
                isEmpty
                emptyTitle={t('dashboard.chartsEmptyTitle')}
                emptyDescription={t('dashboard.chartsEmptyDesc')}
              >
                <></>
              </StateShell>
            </div>
          ) : (
            <div className="h-[290px] w-full">
              <ResponsiveContainer width="100%" height="100%">
                {activeTab === 'traffic' ? (
                  <ComposedChart data={seriesData} margin={chartMargin}>
                    <defs>
                      <linearGradient id="dash-req-fill" x1="0" y1="0" x2="0" y2="1">
                        <stop offset="0%" stopColor="var(--color-primary)" stopOpacity={0.22} />
                        <stop offset="85%" stopColor="var(--color-primary)" stopOpacity={0.02} />
                        <stop offset="100%" stopColor="var(--color-primary)" stopOpacity={0} />
                      </linearGradient>
                    </defs>
                    <CartesianGrid vertical={false} stroke={gridColor} strokeDasharray="3 3" strokeOpacity={0.5} />
                    <XAxis dataKey="label" tick={{ fill: axisColor, fontSize: 11 }} axisLine={false} tickLine={false} minTickGap={22} tickMargin={8} />
                    <YAxis tickFormatter={formatCompact} tick={{ fill: axisColor, fontSize: 11 }} axisLine={false} tickLine={false} allowDecimals={false} width={40} />
                    <Tooltip content={(props) => <StudioChartTooltip active={props.active} payload={props.payload} formatter={(v) => v.toLocaleString()} />} />
                    <Area
                      type="monotone"
                      dataKey="requests"
                      name={t('dashboard.seriesRequests')}
                      stroke="var(--color-primary)"
                      fill="url(#dash-req-fill)"
                      strokeWidth={2.2}
                      isAnimationActive={false}
                      strokeOpacity={seriesOpacity('requests')}
                      fillOpacity={seriesOpacity('requests')}
                    />
                    <Line
                      type="monotone"
                      dataKey="errors4xx"
                      name={t('dashboard.series4xxErrors')}
                      stroke="#f59e0b"
                      strokeWidth={1.9}
                      dot={false}
                      isAnimationActive={false}
                      strokeOpacity={seriesOpacity('errors4xx')}
                    />
                    <Line
                      type="monotone"
                      dataKey="errors5xx"
                      name={t('dashboard.series5xxErrors')}
                      stroke="var(--color-destructive)"
                      strokeWidth={1.9}
                      dot={false}
                      isAnimationActive={false}
                      strokeOpacity={seriesOpacity('errors5xx')}
                    />
                  </ComposedChart>
                ) : activeTab === 'tokens' ? (
                  <BarChart data={seriesData} margin={chartMargin}>
                    <CartesianGrid vertical={false} stroke={gridColor} strokeDasharray="3 3" strokeOpacity={0.5} />
                    <XAxis dataKey="label" tick={{ fill: axisColor, fontSize: 11 }} axisLine={false} tickLine={false} minTickGap={22} tickMargin={8} />
                    <YAxis tickFormatter={formatCompact} tick={{ fill: axisColor, fontSize: 11 }} axisLine={false} tickLine={false} width={44} />
                    <Tooltip content={(props) => <StudioChartTooltip active={props.active} payload={props.payload} formatter={(v) => v.toLocaleString()} />} />
                    <Bar dataKey="inputTokens" stackId="t" name={t('dashboard.seriesInputTokens')} fill="#0ea5e9" fillOpacity={seriesOpacity('inputTokens')} isAnimationActive={false} radius={[0, 0, 3, 3]} maxBarSize={24} />
                    <Bar dataKey="outputTokens" stackId="t" name={t('dashboard.seriesOutputTokens')} fill="#10b981" fillOpacity={seriesOpacity('outputTokens')} isAnimationActive={false} maxBarSize={24} />
                    <Bar dataKey="reasoningTokens" stackId="t" name={t('dashboard.seriesReasoningTokens')} fill="#f59e0b" fillOpacity={pinnedSeries ? 0.2 : 0.9} isAnimationActive={false} maxBarSize={24} />
                    <Bar dataKey="cachedTokens" stackId="t" name={t('dashboard.seriesCachedTokens')} fill="#8b5cf6" fillOpacity={seriesOpacity('cachedTokens')} isAnimationActive={false} radius={[3, 3, 0, 0]} maxBarSize={24} />
                  </BarChart>
                ) : activeTab === 'latency' ? (
                  <LineChart data={seriesData} margin={chartMargin}>
                    <CartesianGrid vertical={false} stroke={gridColor} strokeDasharray="3 3" strokeOpacity={0.5} />
                    <XAxis dataKey="label" tick={{ fill: axisColor, fontSize: 11 }} axisLine={false} tickLine={false} minTickGap={22} tickMargin={8} />
                    <YAxis tickFormatter={(v) => formatDurationMs(Number(v))} tick={{ fill: axisColor, fontSize: 11 }} axisLine={false} tickLine={false} width={48} />
                    <Tooltip content={(props) => <StudioChartTooltip active={props.active} payload={props.payload} formatter={(v) => formatDurationMs(v)} />} />
                    <Line type="monotone" dataKey="avgLatency" name={t('dashboard.avgCompletionLatency')} stroke="var(--color-primary)" strokeWidth={2.2} dot={false} connectNulls isAnimationActive={false} strokeOpacity={seriesOpacity('avgLatency')} />
                    <Line type="monotone" dataKey="avgTtft" name={t('dashboard.avgFirstTokenLatency')} stroke="#10b981" strokeWidth={2.2} dot={false} connectNulls isAnimationActive={false} strokeOpacity={seriesOpacity('avgTtft')} />
                  </LineChart>
                ) : (
                  <AreaChart data={seriesData} margin={chartMargin}>
                    <defs>
                      <linearGradient id="dash-bill-fill" x1="0" y1="0" x2="0" y2="1">
                        <stop offset="0%" stopColor="#10b981" stopOpacity={0.22} />
                        <stop offset="100%" stopColor="#10b981" stopOpacity={0} />
                      </linearGradient>
                    </defs>
                    <CartesianGrid vertical={false} stroke={gridColor} strokeDasharray="3 3" strokeOpacity={0.5} />
                    <XAxis dataKey="label" tick={{ fill: axisColor, fontSize: 11 }} axisLine={false} tickLine={false} minTickGap={22} tickMargin={8} />
                    <YAxis tickFormatter={(v) => `$${Number(v) >= 1 ? Number(v).toFixed(1) : Number(v).toFixed(3)}`} tick={{ fill: axisColor, fontSize: 11 }} axisLine={false} tickLine={false} width={50} />
                    <Tooltip content={(props) => <StudioChartTooltip active={props.active} payload={props.payload} formatter={(v) => formatUsd(v)} />} />
                    <Area type="monotone" dataKey="userBilled" name={t('dashboard.trendUserBilled')} stroke="#10b981" fill="url(#dash-bill-fill)" strokeWidth={2.2} isAnimationActive={false} strokeOpacity={seriesOpacity('userBilled')} fillOpacity={seriesOpacity('userBilled')} />
                    <Line type="monotone" dataKey="accountBilled" name={t('dashboard.trendAccountCost')} stroke="#0ea5e9" strokeWidth={2} dot={false} isAnimationActive={false} strokeOpacity={seriesOpacity('accountBilled')} />
                  </AreaChart>
                )}
              </ResponsiveContainer>
            </div>
          )}
        </div>

        {/* Subtle footer caption */}
        <div className="mt-3 flex items-center justify-between gap-2 border-t border-border/50 pt-2.5 text-[11px] text-muted-foreground">
          <span>{t('dashboard.usageChartsDesc', { count: totals.requests.toLocaleString() })}</span>
          {isLive ? (
            <span className="tabular-nums">
              {t('dashboard.liveHeartbeat', { seconds: Math.round(refreshIntervalMs / 1000), time: formatClock(refreshedAt) })}
            </span>
          ) : null}
        </div>
      </CardContent>
    </Card>
  )
}
