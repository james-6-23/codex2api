import { useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import {
  Area,
  AreaChart,
  Bar,
  BarChart,
  CartesianGrid,
  Line,
  LineChart,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts'
import { Activity, Coins, ShieldCheck, Timer } from 'lucide-react'
import { Card, CardContent } from '@/components/ui/card'
import ModelLogo from '../ModelLogo'
import type { ChartAggregation, UsageAPIKeyStat, UsageEndpointStat, UsageModelStat, UsageStats } from '../../types'
import { getTimezone } from '../../utils/time'
import { cn } from '@/lib/utils'

interface UsageInsightsDeckProps {
  chartData: ChartAggregation | null
  chartLoading?: boolean
  usageStats: UsageStats | null
  modelStats: UsageModelStat[]
  endpointStats: UsageEndpointStat[]
  apiKeyStats: UsageAPIKeyStat[]
  bucketMinutes: number
  onSelectModel?: (model: string) => void
  onSelectEndpoint?: (endpoint: string) => void
  onSelectApiKey?: (apiKeyId: number) => void
}

type DiagnosticDimension = 'model' | 'endpoint' | 'apiKey'
type CostView = 'billing' | 'cache'

const chartMargin = { top: 6, right: 8, left: 0, bottom: 0 }
const gridColor = 'var(--color-border)'
const axisColor = 'var(--color-muted-foreground)'
const tooltipStyle = {
  backgroundColor: 'var(--color-card)',
  border: '1px solid var(--color-border)',
  borderRadius: '10px',
  boxShadow: '0 10px 24px rgba(0, 0, 0, 0.14)',
  padding: '8px 12px',
  fontSize: '12px',
}

const compactFmt = new Intl.NumberFormat(undefined, { notation: 'compact', maximumFractionDigits: 1 })

function formatCompact(v: number): string {
  if (!Number.isFinite(v)) return '0'
  if (Math.abs(v) < 1000) return String(Math.round(v))
  return compactFmt.format(v)
}

function formatDuration(ms: number | null | undefined): string {
  if (!ms || !Number.isFinite(ms) || ms <= 0) return '-'
  if (ms >= 1000) return `${(ms / 1000).toFixed(1)}s`
  return `${Math.round(ms)}ms`
}

function formatUsd(v: number): string {
  if (!Number.isFinite(v) || v <= 0) return '$0.00'
  if (v >= 100) return `$${v.toLocaleString(undefined, { maximumFractionDigits: 1 })}`
  if (v >= 1) return `$${v.toFixed(2)}`
  return `$${v.toFixed(4)}`
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

export default function UsageInsightsDeck({
  chartData,
  chartLoading = false,
  usageStats,
  modelStats,
  endpointStats,
  apiKeyStats,
  bucketMinutes,
  onSelectModel,
  onSelectEndpoint,
  onSelectApiKey,
}: UsageInsightsDeckProps) {
  const { t } = useTranslation()
  const [diagDim, setDiagDim] = useState<DiagnosticDimension>('model')
  const [costView, setCostView] = useState<CostView>('billing')

  const points = useMemo(() => {
    const timeline = chartData?.timeline ?? []
    return timeline.map((pt) => {
      const date = new Date(pt.bucket)
      const errors4xx = pt.errors_4xx ?? 0
      const errors5xx = pt.errors_5xx ?? 0
      const success = Math.max(0, pt.requests - errors4xx - errors5xx)
      const cacheDenom = pt.input_tokens + pt.cached_tokens
      const cacheRate = cacheDenom > 0 ? Number(((pt.cached_tokens / cacheDenom) * 100).toFixed(1)) : 0
      return {
        label: formatBucketAxis(date, bucketMinutes),
        fullLabel: formatBucketFull(date, bucketMinutes),
        requests: pt.requests,
        success,
        errors4xx,
        errors5xx,
        avgLatency: pt.avg_latency > 0 ? Math.round(pt.avg_latency) : null,
        avgTtft: pt.avg_first_token_ms && pt.avg_first_token_ms > 0 ? Math.round(pt.avg_first_token_ms) : null,
        userBilled: Number((pt.user_billed ?? 0).toFixed(4)),
        accountBilled: Number((pt.account_billed ?? 0).toFixed(4)),
        cacheRate,
      }
    })
  }, [chartData, bucketMinutes])

  const healthSummary = useMemo(() => {
    let success = 0
    let errors4xx = 0
    let errors5xx = 0
    let peakLatency = 0
    for (const p of points) {
      success += p.success
      errors4xx += p.errors4xx
      errors5xx += p.errors5xx
      if (p.avgLatency && p.avgLatency > peakLatency) peakLatency = p.avgLatency
    }
    const serviceTotal = success + errors5xx
    const serviceRate = serviceTotal > 0 ? (success / serviceTotal) * 100 : null
    return { success, errors4xx, errors5xx, serviceRate, peakLatency }
  }, [points])

  const hasChartData = points.some((p) => p.requests > 0)

  const rangeUserBilled = usageStats?.today_user_billed ?? 0
  const rangeAccountBilled = usageStats?.today_account_billed ?? 0
  const rangeMargin = Math.max(0, rangeUserBilled - rangeAccountBilled)
  const rangeCacheRate = usageStats?.today_cache_rate ?? 0

  const diagRows = useMemo(() => {
    if (diagDim === 'model') {
      return modelStats.slice(0, 8).map((m) => ({
        key: m.model,
        label: m.model,
        requests: m.requests,
        errors: m.error_count,
        tokens: m.tokens,
        cost: m.user_billed,
        isModel: true,
        mono: false,
        onClick: onSelectModel ? () => onSelectModel(m.model) : undefined,
      }))
    }
    if (diagDim === 'endpoint') {
      return endpointStats.slice(0, 8).map((e) => ({
        key: e.endpoint,
        label: e.endpoint,
        requests: e.requests,
        errors: e.error_count,
        tokens: e.tokens,
        cost: e.user_billed,
        isModel: false,
        mono: true,
        onClick: onSelectEndpoint ? () => onSelectEndpoint(e.endpoint) : undefined,
      }))
    }
    return apiKeyStats.slice(0, 8).map((k) => ({
      key: `${k.api_key_id}-${k.label}`,
      label: k.label,
      requests: k.requests,
      errors: k.error_count,
      tokens: k.tokens,
      cost: k.user_billed,
      isModel: false,
      mono: false,
      onClick: onSelectApiKey && k.api_key_id > 0 ? () => onSelectApiKey(k.api_key_id) : undefined,
    }))
  }, [apiKeyStats, diagDim, endpointStats, modelStats, onSelectApiKey, onSelectEndpoint, onSelectModel])

  return (
    <section
      aria-label={t('usage.insightsDeckLabel')}
      className="grid grid-cols-1 gap-4 xl:grid-cols-2"
    >
      {/* 1. Request Health Stacked Bars */}
      <Card className="border-border/75 bg-card py-0 shadow-2xs">
        <CardContent className="flex h-full flex-col p-4 sm:p-5">
          <div className="flex items-start justify-between gap-3">
            <div className="min-w-0">
              <div className="flex items-center gap-2">
                <ShieldCheck className="size-4 text-emerald-600 dark:text-emerald-400" />
                <h3 className="text-sm font-semibold text-foreground">{t('usage.insightsHealthTitle')}</h3>
              </div>
              <p className="mt-0.5 text-xs text-muted-foreground">{t('usage.insightsHealthDesc')}</p>
            </div>
            <span
              className={cn(
                'inline-flex shrink-0 items-center rounded-full px-2.5 py-0.5 font-sans text-xs font-semibold tabular-nums',
                healthSummary.serviceRate === null
                  ? 'bg-muted text-muted-foreground'
                  : healthSummary.serviceRate >= 98
                    ? 'bg-emerald-500/12 text-emerald-700 dark:text-emerald-300'
                    : healthSummary.serviceRate >= 90
                      ? 'bg-amber-500/12 text-amber-700 dark:text-amber-300'
                      : 'bg-destructive/12 text-destructive',
              )}
            >
              {healthSummary.serviceRate === null ? '--' : `${healthSummary.serviceRate.toFixed(1)}%`}
            </span>
          </div>

          <div className="mt-3 flex flex-wrap items-center gap-x-4 gap-y-1 text-xs text-muted-foreground">
            <span className="inline-flex items-center gap-1.5">
              <span className="size-2 rounded-full bg-emerald-500" />
              {t('usage.success')} <b className="font-sans font-medium text-foreground tabular-nums">{healthSummary.success.toLocaleString()}</b>
            </span>
            <span className="inline-flex items-center gap-1.5">
              <span className="size-2 rounded-full bg-amber-500" />
              4xx <b className="font-sans font-medium text-foreground tabular-nums">{healthSummary.errors4xx.toLocaleString()}</b>
            </span>
            <span className="inline-flex items-center gap-1.5">
              <span className="size-2 rounded-full bg-rose-500" />
              5xx <b className="font-sans font-medium text-foreground tabular-nums">{healthSummary.errors5xx.toLocaleString()}</b>
            </span>
          </div>

          <div className="mt-3 h-[210px] w-full">
            {chartLoading && !hasChartData ? (
              <div className="h-full w-full animate-pulse rounded-lg bg-muted/45" />
            ) : !hasChartData ? (
              <div className="flex h-full items-center justify-center rounded-lg border border-dashed border-border/70 text-xs text-muted-foreground">
                {t('dashboard.chartsEmptyTitle')}
              </div>
            ) : (
              <ResponsiveContainer width="100%" height="100%">
                <BarChart data={points} margin={chartMargin}>
                  <CartesianGrid vertical={false} stroke={gridColor} strokeDasharray="3 3" strokeOpacity={0.7} />
                  <XAxis dataKey="label" tick={{ fill: axisColor, fontSize: 11 }} axisLine={false} tickLine={false} minTickGap={18} />
                  <YAxis tickFormatter={formatCompact} tick={{ fill: axisColor, fontSize: 11 }} axisLine={false} tickLine={false} allowDecimals={false} width={40} />
                  <Tooltip
                    contentStyle={tooltipStyle}
                    labelFormatter={(_, payload) => (payload?.[0]?.payload as { fullLabel?: string } | undefined)?.fullLabel ?? ''}
                    formatter={(val) => Number(val ?? 0).toLocaleString()}
                  />
                  <Bar dataKey="success" stackId="h" name={t('usage.success')} fill="#10b981" fillOpacity={0.82} radius={[0, 0, 2, 2]} maxBarSize={22} />
                  <Bar dataKey="errors4xx" stackId="h" name="4xx" fill="#f59e0b" fillOpacity={0.9} maxBarSize={22} />
                  <Bar dataKey="errors5xx" stackId="h" name="5xx" fill="#ef4444" fillOpacity={0.9} radius={[2, 2, 0, 0]} maxBarSize={22} />
                </BarChart>
              </ResponsiveContainer>
            )}
          </div>
        </CardContent>
      </Card>

      {/* 2. Multi-Dimensional Cross Diagnostics */}
      <Card className="border-border/75 bg-card py-0 shadow-2xs">
        <CardContent className="flex h-full flex-col p-4 sm:p-5">
          <div className="flex flex-wrap items-start justify-between gap-3">
            <div className="min-w-0">
              <div className="flex items-center gap-2">
                <Activity className="size-4 text-primary" />
                <h3 className="text-sm font-semibold text-foreground">{t('usage.insightsDiagnosticsTitle')}</h3>
              </div>
              <p className="mt-0.5 text-xs text-muted-foreground">{t('usage.insightsDiagnosticsDesc')}</p>
            </div>
            <div className="inline-flex rounded-lg border border-border/70 bg-muted/45 p-0.5">
              {([
                { id: 'model', label: t('usage.tableModel') },
                { id: 'endpoint', label: t('usage.tableEndpoint') },
                { id: 'apiKey', label: t('usage.tableApiKey') },
              ] as const).map((item) => (
                <button
                  key={item.id}
                  type="button"
                  onClick={() => setDiagDim(item.id)}
                  className={cn(
                    'rounded-md px-2.5 py-1 text-[11.5px] font-medium transition-all',
                    diagDim === item.id
                      ? 'bg-card text-foreground shadow-2xs ring-1 ring-border/60 font-semibold'
                      : 'text-muted-foreground hover:text-foreground',
                  )}
                >
                  {item.label}
                </button>
              ))}
            </div>
          </div>

          <div className="mt-3 max-h-[246px] flex-1 overflow-auto">
            {diagRows.length === 0 ? (
              <div className="flex h-[210px] items-center justify-center rounded-lg border border-dashed border-border/70 text-xs text-muted-foreground">
                {t('dashboard.chartsEmptyTitle')}
              </div>
            ) : (
              <table className="w-full min-w-[420px] border-collapse text-left text-xs">
                <thead>
                  <tr className="border-b border-border/70 text-[11px] font-medium text-muted-foreground">
                    <th className="py-1.5 pr-2 font-medium">{t('usage.diagnosticsNameCol')}</th>
                    <th className="px-2 py-1.5 text-right font-medium">{t('usage.modelStatsRequests')}</th>
                    <th className="px-2 py-1.5 text-right font-medium">{t('usage.modelStatsErrors')}</th>
                    <th className="px-2 py-1.5 text-right font-medium">Tokens</th>
                    <th className="py-1.5 pl-2 text-right font-medium">{t('usage.tableCost')}</th>
                  </tr>
                </thead>
                <tbody className="divide-y divide-border/45">
                  {diagRows.map((row) => {
                    const errRate = row.requests > 0 ? (row.errors / row.requests) * 100 : 0
                    return (
                      <tr
                        key={row.key}
                        onClick={row.onClick}
                        className={cn(
                          'transition-colors hover:bg-muted/40',
                          row.onClick && 'cursor-pointer',
                        )}
                      >
                        <td className="max-w-[180px] py-2 pr-2">
                          <div className="flex items-center gap-1.5">
                            {row.isModel ? <ModelLogo model={row.label} size={14} className="shrink-0" /> : null}
                            <span
                              className={cn('truncate font-medium text-foreground', row.mono && 'font-mono text-[11px]')}
                              title={row.label}
                            >
                              {row.label}
                            </span>
                          </div>
                        </td>
                        <td className="px-2 py-2 text-right font-sans font-medium text-foreground tabular-nums">
                          {row.requests.toLocaleString()}
                        </td>
                        <td className="px-2 py-2 text-right font-sans tabular-nums">
                          {row.errors > 0 ? (
                            <span className="text-amber-600 dark:text-amber-400">
                              {row.errors} <span className="text-[10.5px] opacity-80">({errRate.toFixed(0)}%)</span>
                            </span>
                          ) : (
                            <span className="text-muted-foreground/60">0</span>
                          )}
                        </td>
                        <td className="px-2 py-2 text-right font-sans text-muted-foreground tabular-nums">
                          {formatCompact(row.tokens)}
                        </td>
                        <td className="py-2 pl-2 text-right font-sans font-medium text-emerald-600 tabular-nums dark:text-emerald-400">
                          {formatUsd(row.cost)}
                        </td>
                      </tr>
                    )
                  })}
                </tbody>
              </table>
            )}
          </div>
        </CardContent>
      </Card>

      {/* 3. Performance & TTFT Trend */}
      <Card className="border-border/75 bg-card py-0 shadow-2xs">
        <CardContent className="flex h-full flex-col p-4 sm:p-5">
          <div className="flex items-start justify-between gap-3">
            <div className="min-w-0">
              <div className="flex items-center gap-2">
                <Timer className="size-4 text-sky-600 dark:text-sky-400" />
                <h3 className="text-sm font-semibold text-foreground">{t('usage.insightsPerformanceTitle')}</h3>
              </div>
              <p className="mt-0.5 text-xs text-muted-foreground">{t('usage.insightsPerformanceDesc')}</p>
            </div>
          </div>

          <div className="mt-3 grid grid-cols-3 gap-2 rounded-lg bg-muted/40 px-3 py-2 text-xs">
            <div className="min-w-0">
              <span className="block truncate text-[10.5px] text-muted-foreground">{t('dashboard.avgFirstTokenLatency')}</span>
              <strong className="font-sans text-[15px] font-semibold tracking-tight text-emerald-600 tabular-nums dark:text-emerald-400">
                {formatDuration(usageStats?.avg_first_token_ms)}
              </strong>
            </div>
            <div className="min-w-0 border-l border-border/60 pl-2.5">
              <span className="block truncate text-[10.5px] text-muted-foreground">{t('dashboard.avgCompletionLatency')}</span>
              <strong className="font-sans text-[15px] font-semibold tracking-tight text-foreground tabular-nums">
                {formatDuration(usageStats?.avg_duration_ms)}
              </strong>
            </div>
            <div className="min-w-0 border-l border-border/60 pl-2.5">
              <span className="block truncate text-[10.5px] text-muted-foreground">{t('usage.insightsPeakLatency')}</span>
              <strong className="font-sans text-[15px] font-semibold tracking-tight text-foreground tabular-nums">
                {formatDuration(healthSummary.peakLatency)}
              </strong>
            </div>
          </div>

          <div className="mt-3 h-[196px] w-full">
            {chartLoading && !hasChartData ? (
              <div className="h-full w-full animate-pulse rounded-lg bg-muted/45" />
            ) : !hasChartData ? (
              <div className="flex h-full items-center justify-center rounded-lg border border-dashed border-border/70 text-xs text-muted-foreground">
                {t('dashboard.chartsEmptyTitle')}
              </div>
            ) : (
              <ResponsiveContainer width="100%" height="100%">
                <LineChart data={points} margin={chartMargin}>
                  <CartesianGrid vertical={false} stroke={gridColor} strokeDasharray="3 3" strokeOpacity={0.7} />
                  <XAxis dataKey="label" tick={{ fill: axisColor, fontSize: 11 }} axisLine={false} tickLine={false} minTickGap={18} />
                  <YAxis tickFormatter={(v) => formatDuration(Number(v))} tick={{ fill: axisColor, fontSize: 11 }} axisLine={false} tickLine={false} width={48} />
                  <Tooltip
                    contentStyle={tooltipStyle}
                    labelFormatter={(_, payload) => (payload?.[0]?.payload as { fullLabel?: string } | undefined)?.fullLabel ?? ''}
                    formatter={(val) => formatDuration(Number(val ?? 0))}
                  />
                  <Line type="monotone" dataKey="avgLatency" name={t('dashboard.avgCompletionLatency')} stroke="var(--color-primary)" strokeWidth={2.2} dot={false} connectNulls />
                  <Line type="monotone" dataKey="avgTtft" name={t('dashboard.avgFirstTokenLatency')} stroke="#10b981" strokeWidth={2.2} dot={false} connectNulls />
                </LineChart>
              </ResponsiveContainer>
            )}
          </div>
        </CardContent>
      </Card>

      {/* 4. Cost Efficiency & Dual-Ledger / Cache Trend */}
      <Card className="border-border/75 bg-card py-0 shadow-2xs">
        <CardContent className="flex h-full flex-col p-4 sm:p-5">
          <div className="flex flex-wrap items-start justify-between gap-3">
            <div className="min-w-0">
              <div className="flex items-center gap-2">
                <Coins className="size-4 text-emerald-600 dark:text-emerald-400" />
                <h3 className="text-sm font-semibold text-foreground">{t('usage.insightsCostTitle')}</h3>
              </div>
              <p className="mt-0.5 text-xs text-muted-foreground">{t('usage.insightsCostDesc')}</p>
            </div>
            <div className="inline-flex rounded-lg border border-border/70 bg-muted/45 p-0.5">
              {([
                { id: 'billing', label: t('usage.insightsCostTabBilling') },
                { id: 'cache', label: t('usage.insightsCostTabCache') },
              ] as const).map((item) => (
                <button
                  key={item.id}
                  type="button"
                  onClick={() => setCostView(item.id)}
                  className={cn(
                    'rounded-md px-2.5 py-1 text-[11.5px] font-medium transition-all',
                    costView === item.id
                      ? 'bg-card text-foreground shadow-2xs ring-1 ring-border/60 font-semibold'
                      : 'text-muted-foreground hover:text-foreground',
                  )}
                >
                  {item.label}
                </button>
              ))}
            </div>
          </div>

          <div className="mt-3 grid grid-cols-2 gap-2 rounded-lg bg-muted/40 px-3 py-2 text-xs sm:grid-cols-4">
            <div className="min-w-0">
              <span className="block truncate text-[10.5px] text-muted-foreground">{t('dashboard.trendUserBilled')}</span>
              <strong className="font-sans text-[15px] font-semibold tracking-tight text-emerald-600 tabular-nums dark:text-emerald-400">
                {formatUsd(rangeUserBilled)}
              </strong>
            </div>
            <div className="min-w-0 sm:border-l sm:border-border/60 sm:pl-2.5">
              <span className="block truncate text-[10.5px] text-muted-foreground">{t('dashboard.trendAccountCost')}</span>
              <strong className="font-sans text-[15px] font-semibold tracking-tight text-foreground tabular-nums">
                {formatUsd(rangeAccountBilled)}
              </strong>
            </div>
            <div className="min-w-0 sm:border-l sm:border-border/60 sm:pl-2.5">
              <span className="block truncate text-[10.5px] text-muted-foreground">{t('dashboard.trendEstMargin')}</span>
              <strong className="font-sans text-[15px] font-semibold tracking-tight text-violet-600 tabular-nums dark:text-violet-400">
                {formatUsd(rangeMargin)}
              </strong>
            </div>
            <div className="min-w-0 sm:border-l sm:border-border/60 sm:pl-2.5">
              <span className="block truncate text-[10.5px] text-muted-foreground">{t('dashboard.kpiCached')}</span>
              <strong className="font-sans text-[15px] font-semibold tracking-tight text-foreground tabular-nums">
                {rangeCacheRate.toFixed(1)}%
              </strong>
            </div>
          </div>

          <div className="mt-3 h-[196px] w-full">
            {chartLoading && !hasChartData ? (
              <div className="h-full w-full animate-pulse rounded-lg bg-muted/45" />
            ) : !hasChartData ? (
              <div className="flex h-full items-center justify-center rounded-lg border border-dashed border-border/70 text-xs text-muted-foreground">
                {t('dashboard.chartsEmptyTitle')}
              </div>
            ) : (
              <ResponsiveContainer width="100%" height="100%">
                {costView === 'billing' ? (
                  <AreaChart data={points} margin={chartMargin}>
                    <defs>
                      <linearGradient id="usage-bill-grad" x1="0" y1="0" x2="0" y2="1">
                        <stop offset="5%" stopColor="#10b981" stopOpacity={0.25} />
                        <stop offset="95%" stopColor="#10b981" stopOpacity={0} />
                      </linearGradient>
                    </defs>
                    <CartesianGrid vertical={false} stroke={gridColor} strokeDasharray="3 3" strokeOpacity={0.7} />
                    <XAxis dataKey="label" tick={{ fill: axisColor, fontSize: 11 }} axisLine={false} tickLine={false} minTickGap={18} />
                    <YAxis tickFormatter={(v) => `$${Number(v) >= 1 ? Number(v).toFixed(1) : Number(v).toFixed(3)}`} tick={{ fill: axisColor, fontSize: 11 }} axisLine={false} tickLine={false} width={50} />
                    <Tooltip
                      contentStyle={tooltipStyle}
                      labelFormatter={(_, payload) => (payload?.[0]?.payload as { fullLabel?: string } | undefined)?.fullLabel ?? ''}
                      formatter={(val) => formatUsd(Number(val ?? 0))}
                    />
                    <Area type="monotone" dataKey="userBilled" name={t('dashboard.trendUserBilled')} stroke="#10b981" fill="url(#usage-bill-grad)" strokeWidth={2.2} />
                    <Line type="monotone" dataKey="accountBilled" name={t('dashboard.trendAccountCost')} stroke="#0ea5e9" strokeWidth={2} dot={false} />
                  </AreaChart>
                ) : (
                  <AreaChart data={points} margin={chartMargin}>
                    <defs>
                      <linearGradient id="usage-cache-grad" x1="0" y1="0" x2="0" y2="1">
                        <stop offset="5%" stopColor="#8b5cf6" stopOpacity={0.25} />
                        <stop offset="95%" stopColor="#8b5cf6" stopOpacity={0} />
                      </linearGradient>
                    </defs>
                    <CartesianGrid vertical={false} stroke={gridColor} strokeDasharray="3 3" strokeOpacity={0.7} />
                    <XAxis dataKey="label" tick={{ fill: axisColor, fontSize: 11 }} axisLine={false} tickLine={false} minTickGap={18} />
                    <YAxis domain={[0, 100]} tickFormatter={(v) => `${v}%`} tick={{ fill: axisColor, fontSize: 11 }} axisLine={false} tickLine={false} width={40} />
                    <Tooltip
                      contentStyle={tooltipStyle}
                      labelFormatter={(_, payload) => (payload?.[0]?.payload as { fullLabel?: string } | undefined)?.fullLabel ?? ''}
                      formatter={(val) => `${Number(val ?? 0).toFixed(1)}%`}
                    />
                    <Area type="monotone" dataKey="cacheRate" name={t('dashboard.seriesCachedTokens')} stroke="#8b5cf6" fill="url(#usage-cache-grad)" strokeWidth={2.2} />
                  </AreaChart>
                )}
              </ResponsiveContainer>
            )}
          </div>
        </CardContent>
      </Card>
    </section>
  )
}
