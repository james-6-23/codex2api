import type { ReactNode } from 'react'
import { lazy, Suspense, useCallback, useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { api } from '../api'
import { getTimeRangeISO, getBucketConfig, type TimeRangeKey } from '../lib/timeRange'
import PageHeader from '../components/PageHeader'
import StateShell from '../components/StateShell'
import StatCard from '../components/StatCard'
import UsageStatsSummary from '../components/UsageStatsSummary'
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

  // 仅加载轻量级统计数据（秒级响应）
  const loadDashboardStats = useCallback(async () => {
    const [stats, usageStats, settings] = await Promise.all([
      api.getStats(),
      api.getUsageStats(),
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
        />

        {/* Account status */}
        <div className="grid grid-cols-[repeat(auto-fit,minmax(220px,1fr))] gap-4 mb-6">
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

        {/* Usage stats */}
        {usageStats && (
          <div className="space-y-6">
            <UsageStatsSummary stats={usageStats} showFullUsageNumbers={showFullUsageNumbers} />
            <Suspense fallback={<ChartsSkeleton />}>
              <DashboardUsageCharts
                chartData={chartData}
                refreshedAt={chartRefreshedAt}
                refreshIntervalMs={DASHBOARD_REFRESH_INTERVAL_MS}
                timeRange={timeRange}
                onTimeRangeChange={setTimeRange}
                loading={chartLoading}
              />
            </Suspense>
          </div>
        )}
      </>
    </StateShell>
  )
}
