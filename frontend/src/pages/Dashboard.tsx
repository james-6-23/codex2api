import type { ReactNode } from 'react'
import { lazy, Suspense, useCallback, useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { api } from '../api'
import { getTimeRangeISO, getBucketConfig, type TimeRangeKey } from '../lib/timeRange'
import PageHeader from '../components/PageHeader'
import StateShell from '../components/StateShell'
import StatCard from '../components/StatCard'
import UsageStatsSummary from '../components/UsageStatsSummary'
import TimeRangeSelector from '../components/TimeRangeSelector'
import SystemHealthBar from '../components/SystemHealthBar'
import type { StatsResponse, SystemSettings, UsageStats, ChartAggregation } from '../types'
import { useDataLoader } from '../hooks/useDataLoader'
import { Card, CardContent } from '@/components/ui/card'
import { Users, CheckCircle, Gauge, XCircle, Activity } from 'lucide-react'

const DashboardUsageCharts = lazy(() => import('../components/DashboardUsageCharts'))

const DASHBOARD_REFRESH_INTERVAL_MS = 15_000

function ChartsSkeleton() {
  return (
    <div className="grid grid-cols-1 gap-4 xl:grid-cols-2">
      {[0, 1, 2, 3].map((i) => (
        <Card key={i} className="py-0">
          <CardContent className="p-6">
            <div className="mb-5 space-y-2">
              <div className="h-4 w-32 rounded-md bg-muted animate-pulse" />
              <div className="h-3 w-48 rounded-md bg-muted/60 animate-pulse" />
            </div>
            <div className="h-[280px] flex items-end gap-2 px-4 pb-4">
              {[40, 65, 30, 80, 55, 70, 45, 60, 35, 75, 50, 68].map((h, j) => (
                <div
                  key={j}
                  className="flex-1 rounded-t-md bg-muted/50 animate-pulse"
                  style={{ height: `${h}%`, animationDelay: `${j * 80}ms` }}
                />
              ))}
            </div>
          </CardContent>
        </Card>
      ))}
    </div>
  )
}

export default function Dashboard() {
  const { t } = useTranslation()
  const [timeRange, setTimeRange] = useState<TimeRangeKey>('1h')
  const [chartData, setChartData] = useState<ChartAggregation | null>(null)
  const [chartRefreshedAt, setChartRefreshedAt] = useState<number | null>(null)
  const [chartLoading, setChartLoading] = useState(true)
  const chartAbort = useRef<AbortController | null>(null)
  const timeRangeRef = useRef<TimeRangeKey>(timeRange)
  const usageStatsRangeInitialized = useRef(false)

  // 仅加载轻量级统计数据（秒级响应）
  const loadDashboardStats = useCallback(async () => {
    const { start, end } = getTimeRangeISO(timeRangeRef.current)
    const [stats, usageStats, settings] = await Promise.all([
      api.getStats(),
      api.getUsageStats({ start, end }),
      api.getSettings().catch((): SystemSettings | null => null),
    ])
    return { stats, usageStats, settings }
  }, [])

  const { data, loading, error, reload, reloadSilently } = useDataLoader<{
    stats: StatsResponse | null
    usageStats: UsageStats | null
    settings: SystemSettings | null
  }>({
    initialData: { stats: null, usageStats: null, settings: null },
    load: loadDashboardStats,
  })

  useEffect(() => {
    timeRangeRef.current = timeRange
    if (!usageStatsRangeInitialized.current) {
      usageStatsRangeInitialized.current = true
      return
    }
    void reloadSilently()
  }, [timeRange, reloadSilently])

  // 加载服务端聚合的图表数据（12~48 个聚合点，非原始行）
  const loadChartData = useCallback(async () => {
    chartAbort.current?.abort()
    const controller = new AbortController()
    chartAbort.current = controller
    setChartLoading(true)
    try {
      const { start, end } = getTimeRangeISO(timeRange)
      const { bucketMinutes } = getBucketConfig(timeRange)
      const res = await api.getChartData({ start, end, bucketMinutes })
      if (!controller.signal.aborted) {
        setChartData(res)
        setChartRefreshedAt(Date.now())
      }
    } catch {
      // 静默容错
    } finally {
      if (!controller.signal.aborted) {
        setChartLoading(false)
      }
    }
  }, [timeRange])

  // 首次加载 + timeRange 变更时重新拉取图表数据
  useEffect(() => {
    void loadChartData()
  }, [loadChartData])

  // 仅在 1h（实时）模式下启用自动刷新
  useEffect(() => {
    if (timeRange !== '1h') return

    const timer = window.setInterval(() => {
      if (document.visibilityState !== 'visible') return
      void reloadSilently()
      void loadChartData()
    }, DASHBOARD_REFRESH_INTERVAL_MS)

    return () => window.clearInterval(timer)
  }, [reloadSilently, timeRange, loadChartData])

  const { stats, usageStats, settings } = data
  const showFullUsageNumbers = settings?.show_full_usage_numbers ?? false
  const total = stats?.total ?? 0
  const available = stats?.available ?? 0
  const rateLimited = stats?.rate_limited ?? 0
  const errorCount = stats?.error ?? 0
  const todayRequests = stats?.today_requests ?? 0

  const icons: Record<string, ReactNode> = {
    total: <Users className="size-[22px]" />,
    available: <CheckCircle className="size-[22px]" />,
    rateLimited: <Gauge className="size-[22px]" />,
    error: <XCircle className="size-[22px]" />,
    requests: <Activity className="size-[22px]" />,
  }

  return (
    <StateShell
      variant="page"
      loading={loading}
      error={error}
      onRetry={() => { void reload(); void loadChartData() }}
      loadingTitle={t('dashboard.loadingTitle')}
      loadingDescription={t('dashboard.loadingDesc')}
      errorTitle={t('dashboard.errorTitle')}
    >
      <>
        <PageHeader
          title={t('dashboard.title')}
          description={t('dashboard.description')}
          onRefresh={() => { void reload(); void loadChartData() }}
          actions={
            <TimeRangeSelector
              timeRange={timeRange}
              onTimeRangeChange={setTimeRange}
            />
          }
        />

        {/* Hero summary */}
        <div className="relative mb-5 overflow-hidden rounded-2xl border border-border/80 bg-card p-4 shadow-sm sm:mb-6 sm:p-5">
          <div
            aria-hidden
            className="pointer-events-none absolute inset-0 bg-[radial-gradient(ellipse_at_top_left,color-mix(in_oklab,var(--color-primary)_12%,transparent),transparent_55%)]"
          />
          <div className="relative z-10 flex flex-col gap-4 lg:flex-row lg:items-center lg:justify-between">
            <div className="min-w-0">
              <div className="text-[11px] font-bold uppercase tracking-wide text-muted-foreground">
                {t('dashboard.heroLabel')}
              </div>
              <div className="mt-1 flex flex-wrap items-end gap-x-3 gap-y-1">
                <div className="text-3xl font-bold tabular-nums tracking-tight text-foreground sm:text-4xl">
                  {available}
                  <span className="text-lg font-semibold text-muted-foreground sm:text-xl">/{total}</span>
                </div>
                <div className="pb-1 text-sm text-muted-foreground">
                  {t('dashboard.heroAvailable')}
                </div>
              </div>
              <div className="mt-2 flex flex-wrap items-center gap-2 text-xs text-muted-foreground">
                <span className="inline-flex items-center gap-1.5 rounded-full bg-emerald-500/12 px-2.5 py-1 font-semibold text-emerald-700 dark:text-emerald-300">
                  <span className="size-1.5 rounded-full bg-emerald-500" />
                  {total > 0
                    ? t('dashboard.heroAvailability', {
                        rate: Math.round((available / Math.max(total, 1)) * 100),
                      })
                    : t('dashboard.heroNoAccounts')}
                </span>
                <span className="inline-flex items-center rounded-full bg-muted/80 px-2.5 py-1 font-medium">
                  {t('dashboard.heroTodayRequests', { count: todayRequests })}
                </span>
                {errorCount > 0 ? (
                  <span className="inline-flex items-center rounded-full bg-destructive/12 px-2.5 py-1 font-semibold text-destructive">
                    {t('dashboard.heroErrors', { count: errorCount })}
                  </span>
                ) : null}
                {rateLimited > 0 ? (
                  <span className="inline-flex items-center rounded-full bg-amber-500/12 px-2.5 py-1 font-semibold text-amber-700 dark:text-amber-300">
                    {t('dashboard.heroRateLimited', { count: rateLimited })}
                  </span>
                ) : null}
              </div>
            </div>
            {total === 0 ? (
              <div className="rounded-xl border border-dashed border-border bg-background/70 px-4 py-3 text-left text-sm text-muted-foreground lg:max-w-sm">
                <div className="font-semibold text-foreground">{t('dashboard.heroEmptyTitle')}</div>
                <p className="mt-1 leading-relaxed">{t('dashboard.heroEmptyDesc')}</p>
              </div>
            ) : null}
          </div>
        </div>

        {/* Account status */}
        <div className="mb-6 grid grid-cols-1 gap-3 min-[420px]:grid-cols-2 xl:grid-cols-5 sm:gap-4">
          <StatCard icon={icons.total} iconClass="blue" label={t('dashboard.totalAccounts')} value={total} />
          <StatCard
            icon={icons.available}
            iconClass="green"
            label={t('dashboard.available')}
            value={available}
          />
          <StatCard
            icon={icons.rateLimited}
            iconClass="amber"
            label={t('dashboard.rateLimited')}
            value={rateLimited}
          />
          <StatCard icon={icons.error} iconClass="red" label={t('dashboard.error')} value={errorCount} />
          <StatCard icon={icons.requests} iconClass="purple" label={t('dashboard.todayRequests')} value={todayRequests} />
        </div>

        {/* System health */}
        <div className="mb-6">
          <SystemHealthBar chartData={chartData} timeRange={timeRange} loading={chartLoading} />
        </div>

        {/* Usage stats */}
        {usageStats && (
          <div className="space-y-6">
            <UsageStatsSummary
              stats={usageStats}
              rangeLabel={t(`dashboard.timeRange${timeRange.toUpperCase()}`)}
              showFullUsageNumbers={showFullUsageNumbers}
            />
            <Suspense fallback={<ChartsSkeleton />}>
              <DashboardUsageCharts
                chartData={chartData}
                refreshedAt={chartRefreshedAt}
                refreshIntervalMs={DASHBOARD_REFRESH_INTERVAL_MS}
                timeRange={timeRange}
                loading={chartLoading}
              />
            </Suspense>
          </div>
        )}
      </>
    </StateShell>
  )
}
