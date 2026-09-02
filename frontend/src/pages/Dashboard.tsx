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
import ChannelFilter, { useUsageChannel, type UsageChannel } from '../components/ChannelFilter'
import ChannelLogo from '../components/ChannelLogo'
import SystemHealthBar from '../components/SystemHealthBar'
import type {
  AccountAnalysisResponse,
  StatsResponse,
  StatsChannelCounts,
  SystemSettings,
  UsageStats,
  ChartAggregation,
} from '../types'
import { useDataLoader } from '../hooks/useDataLoader'
import { Card, CardContent } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { BarChart3, Users, CheckCircle, Gauge, XCircle, Activity } from 'lucide-react'
import PoolRunwayCard from '../components/PoolRunwayCard'

const DashboardUsageCharts = lazy(() => import('../components/DashboardUsageCharts'))

const DASHBOARD_REFRESH_INTERVAL_MS = 15_000
const DASHBOARD_POOL_REFRESH_INTERVAL_MS = 60_000
const DASHBOARD_POOL_RUNWAY_VISIBILITY_KEY = 'codex2api:dashboard:pool-runway-visible'

function getInitialPoolRunwayVisibility(): boolean {
  try {
    return window.localStorage.getItem(DASHBOARD_POOL_RUNWAY_VISIBILITY_KEY) !== 'false'
  } catch {
    return true
  }
}

function persistPoolRunwayVisibility(visible: boolean) {
  try {
    window.localStorage.setItem(
      DASHBOARD_POOL_RUNWAY_VISIBILITY_KEY,
      visible ? 'true' : 'false',
    )
  } catch {
    // Restricted browser modes may block localStorage; keep in-memory toggle working.
  }
}

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
  const [channel, setChannel] = useUsageChannel()
  const channelRef = useRef<UsageChannel>(channel)
  const [showPoolRunway, setShowPoolRunway] = useState(getInitialPoolRunwayVisibility)
  const [chartData, setChartData] = useState<ChartAggregation | null>(null)
  const [chartRefreshedAt, setChartRefreshedAt] = useState<number | null>(null)
  const [chartLoading, setChartLoading] = useState(true)
  const [chartError, setChartError] = useState<string | null>(null)
  const chartAbort = useRef<AbortController | null>(null)
  const statsAbort = useRef<AbortController | null>(null)
  const poolAbort = useRef<AbortController | null>(null)
  const poolRequestGenerationRef = useRef(0)
  const timeRangeRef = useRef<TimeRangeKey>(timeRange)
  const usageStatsRangeInitialized = useRef(false)
  const showPoolRunwayRef = useRef(showPoolRunway)
  const poolDataRef = useRef<AccountAnalysisResponse | null>(null)

  // 核心统计与号池分析解耦：百万级日志下账号聚合变慢时，不能阻塞整个仪表盘首屏。
  const loadDashboardStats = useCallback(async () => {
    statsAbort.current?.abort()
    const controller = new AbortController()
    statsAbort.current = controller
    const { start, end } = getTimeRangeISO(timeRangeRef.current)
    const [stats, usageStats, settings] = await Promise.all([
      api.getStats(),
      api.getUsageStats({
        start,
        end,
        channel: channelRef.current || undefined,
        detail: 'summary',
        signal: controller.signal,
      }),
      api.getSettings().catch((): SystemSettings | null => null),
    ])
    return {
      stats,
      usageStats,
      settings,
      accountAnalysis: poolDataRef.current,
    }
  }, [])

  const { data, loading, error, reload, reloadSilently, setData } = useDataLoader<{
    stats: StatsResponse | null
    usageStats: UsageStats | null
    settings: SystemSettings | null
    accountAnalysis: AccountAnalysisResponse | null
  }>({
    initialData: {
      stats: null,
      usageStats: null,
      settings: null,
      accountAnalysis: null,
    },
    load: loadDashboardStats,
  })

  const loadPoolRunwayData = useCallback(async () => {
    const requestedChannel = channel
    if (!showPoolRunwayRef.current || !requestedChannel) return
    poolAbort.current?.abort()
    const controller = new AbortController()
    poolAbort.current = controller
    const generation = poolRequestGenerationRef.current + 1
    poolRequestGenerationRef.current = generation
    let accountAnalysis: AccountAnalysisResponse | null = null
    try {
      accountAnalysis = await api.getAccountAnalysis(requestedChannel, controller.signal)
    } catch {
      if (controller.signal.aborted) return
    }
    if (
      controller.signal.aborted ||
      generation !== poolRequestGenerationRef.current ||
      channelRef.current !== requestedChannel ||
      !showPoolRunwayRef.current
    ) return
    poolDataRef.current = accountAnalysis
    setData((prev) => ({ ...prev, accountAnalysis }))
  }, [channel, setData])

  // 偏好持久化 + 号池独立加载。号池失败只影响号池卡片，不拖死核心统计。
  useEffect(() => {
    channelRef.current = channel
    showPoolRunwayRef.current = showPoolRunway
    persistPoolRunwayVisibility(showPoolRunway)
    poolAbort.current?.abort()
    poolRequestGenerationRef.current += 1
    poolDataRef.current = null
    setData((prev) => ({ ...prev, accountAnalysis: null }))
    if (!showPoolRunway || !channel) return
    void loadPoolRunwayData()
  }, [channel, showPoolRunway, loadPoolRunwayData, setData])

  useEffect(() => {
    if (!showPoolRunway || !channel) return
    const timer = window.setInterval(() => {
      if (document.visibilityState === 'visible') void loadPoolRunwayData()
    }, DASHBOARD_POOL_REFRESH_INTERVAL_MS)
    return () => window.clearInterval(timer)
  }, [channel, loadPoolRunwayData, showPoolRunway])

  useEffect(() => {
    timeRangeRef.current = timeRange
    channelRef.current = channel
    if (!usageStatsRangeInitialized.current) {
      usageStatsRangeInitialized.current = true
      return
    }
    void reloadSilently()
  }, [timeRange, channel, reloadSilently])

  // 加载服务端聚合的图表数据（12~48 个聚合点，非原始行）
  const loadChartData = useCallback(async () => {
    chartAbort.current?.abort()
    const controller = new AbortController()
    chartAbort.current = controller
    setChartLoading(true)
    setChartError(null)
    try {
      const { start, end } = getTimeRangeISO(timeRange)
      const { bucketMinutes } = getBucketConfig(timeRange)
      const res = await api.getChartData({
        start,
        end,
        bucketMinutes,
        channel: channel || undefined,
        signal: controller.signal,
      })
      if (!controller.signal.aborted) {
        setChartData(res)
        setChartRefreshedAt(Date.now())
      }
    } catch (err) {
      if (!controller.signal.aborted) {
        setChartError(err instanceof Error ? err.message : t('common.loadFailed'))
      }
    } finally {
      if (!controller.signal.aborted) {
        setChartLoading(false)
      }
    }
  }, [timeRange, channel, t])

  useEffect(() => () => {
    chartAbort.current?.abort()
    statsAbort.current?.abort()
    poolAbort.current?.abort()
    poolRequestGenerationRef.current += 1
  }, [])

  const handleChannelChange = useCallback((nextChannel: UsageChannel) => {
    if (nextChannel === channel) return
    channelRef.current = nextChannel
    poolAbort.current?.abort()
    poolRequestGenerationRef.current += 1
    poolDataRef.current = null
    setData((prev) => ({ ...prev, accountAnalysis: null }))
    setChannel(nextChannel)
  }, [channel, setChannel, setData])

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

  const { stats, usageStats, settings, accountAnalysis } = data
  const showFullUsageNumbers = settings?.show_full_usage_numbers ?? false
  // 渠道视图下账号池概览与统计卡切换为该渠道的计数；全部视图保持总量并展示分渠道徽标。
  // 旧后端响应无 channels 字段时回退全量，有字段但该渠道无账号时如实显示 0。
  const emptyChannelCounts: StatsChannelCounts = {
    total: 0, available: 0, rate_limited: 0, error: 0, today_requests: 0,
  }
  const effectiveCounts = channel && stats?.channels
    ? (stats.channels[channel] ?? emptyChannelCounts)
    : stats
  const total = effectiveCounts?.total ?? 0
  const available = effectiveCounts?.available ?? 0
  const rateLimited = effectiveCounts?.rate_limited ?? 0
  const errorCount = effectiveCounts?.error ?? 0
  const todayRequests = effectiveCounts?.today_requests ?? 0
  const channelBreakdown = !channel && stats?.channels
    ? (['codex', 'grok', 'antigravity', 'claude'] as const)
        .map((key) => ({ key, counts: stats.channels?.[key] }))
        .filter((item): item is { key: 'codex' | 'grok' | 'antigravity' | 'claude'; counts: StatsChannelCounts } =>
          Boolean(item.counts && item.counts.total > 0))
    : []

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
          titleAdornment={<ChannelFilter value={channel} onChange={handleChannelChange} />}
          actions={
            <div className="flex flex-wrap items-center gap-2">
              {channel ? (
                <Button
                  type="button"
                  variant="outline"
                  size="sm"
                  className="shrink-0"
                  aria-pressed={showPoolRunway}
                  onClick={() => setShowPoolRunway((visible) => !visible)}
                  title={
                    showPoolRunway
                      ? t('dashboard.hidePoolRunway')
                      : t('dashboard.showPoolRunway')
                  }
                >
                  <BarChart3 className="size-3.5" />
                  <span className="hidden sm:inline">
                    {showPoolRunway
                      ? t('dashboard.hidePoolRunway')
                      : t('dashboard.showPoolRunway')}
                  </span>
                </Button>
              ) : (
                <span className="max-w-44 text-center text-xs font-medium leading-tight text-muted-foreground">
                  {t('dashboard.poolRunwaySingleChannelOnly')}
                </span>
              )}
              <TimeRangeSelector
                timeRange={timeRange}
                onTimeRangeChange={setTimeRange}
              />
            </div>
          }
        />

        {/* 渠道切换时整块内容淡入过渡（key 变化触发重播） */}
        <div key={channel || 'all'} className="animate-channel-switch-in">
        {/* Hero summary */}
        <div className="relative mb-5 overflow-hidden rounded-xl border border-border/80 bg-gradient-to-r from-primary/5 via-card to-card p-5 shadow-2xs sm:mb-6 sm:p-6">
          <div
            aria-hidden
            className="pointer-events-none absolute inset-0 bg-[radial-gradient(ellipse_at_top_left,color-mix(in_oklab,var(--color-primary)_10%,transparent),transparent_60%)]"
          />
          <div className="relative z-10 flex flex-col gap-4 lg:flex-row lg:items-center lg:justify-between">
            <div className="min-w-0 space-y-2">
              <div className="text-[11px] font-bold uppercase tracking-wider text-muted-foreground/90">
                {t('dashboard.heroLabel')}
              </div>
              <div className="flex flex-wrap items-baseline gap-x-3 gap-y-1">
                <div className="text-3xl font-extrabold tabular-nums tracking-tight text-foreground sm:text-4xl">
                  {available}
                  <span className="text-lg font-bold text-muted-foreground/80 sm:text-xl">/{total}</span>
                </div>
                <div className="pb-0.5 text-sm font-semibold text-muted-foreground">
                  {t('dashboard.heroAvailable')}
                </div>
              </div>
              <div className="flex flex-wrap items-center gap-2 pt-1 text-xs text-muted-foreground">
                <span className="inline-flex items-center gap-1.5 rounded-full bg-emerald-500/12 px-3 py-1 text-xs font-bold text-emerald-700 dark:text-emerald-300 ring-1 ring-emerald-500/20">
                  <span className="size-1.5 rounded-full bg-emerald-500 animate-pulse" />
                  {total > 0
                    ? t('dashboard.heroAvailability', {
                        rate: Math.round((available / Math.max(total, 1)) * 100),
                      })
                    : t('dashboard.heroNoAccounts')}
                </span>
                <span className="inline-flex items-center gap-1.5 rounded-full bg-muted/80 px-3 py-1 font-semibold text-foreground ring-1 ring-border/50">
                  <Activity className="size-3 text-primary" />
                  {t('dashboard.heroTodayRequests', { count: todayRequests })}
                </span>
                {channelBreakdown.map(({ key, counts }) => (
                  <span
                    key={key}
                    className="inline-flex items-center gap-1.5 rounded-full bg-muted/80 px-3 py-1 font-semibold text-foreground ring-1 ring-border/50"
                    title={t('dashboard.heroChannelTitle', {
                      // Preserve the provider identity in the tooltip for every channel.
                      channel: key === 'claude' ? 'Claude' : key === 'grok' ? 'Grok' : key === 'antigravity' ? 'Antigravity' : 'Codex',
                      available: counts.available,
                      total: counts.total,
                      requests: counts.today_requests,
                    })}
                  >
                    <ChannelLogo channel={key} size={13} />
                    <span className="tabular-nums">
                      {counts.available}/{counts.total}
                    </span>
                    <span className="text-muted-foreground/60">·</span>
                    <span className="tabular-nums">{counts.today_requests}</span>
                  </span>
                ))}
                {errorCount > 0 ? (
                  <span className="inline-flex items-center rounded-full bg-destructive/12 px-3 py-1 font-bold text-destructive ring-1 ring-destructive/20">
                    {t('dashboard.heroErrors', { count: errorCount })}
                  </span>
                ) : null}
                {rateLimited > 0 ? (
                  <span className="inline-flex items-center rounded-full bg-amber-500/12 px-3 py-1 font-bold text-amber-700 dark:text-amber-300 ring-1 ring-amber-500/20">
                    {t('dashboard.heroRateLimited', { count: rateLimited })}
                  </span>
                ) : null}
              </div>
            </div>
            {total === 0 ? (
              <div className="rounded-xl border border-dashed border-border bg-background/80 px-4 py-3.5 text-left text-sm text-muted-foreground lg:max-w-sm shadow-2xs">
                <div className="font-bold text-foreground">{t('dashboard.heroEmptyTitle')}</div>
                <p className="mt-1 leading-relaxed text-xs">{t('dashboard.heroEmptyDesc')}</p>
              </div>
            ) : null}
          </div>
        </div>

        {/* Account status */}
        <div className="mb-6 grid grid-cols-2 gap-2.5 sm:gap-4 md:grid-cols-3 xl:grid-cols-5">
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
          <StatCard icon={icons.requests} iconClass="purple" label={t('dashboard.todayRequests')} value={todayRequests} className="col-span-2 min-[420px]:col-span-1 md:col-span-1" />
        </div>

        {/* Pool runway（可开关）+ system health */}
        <div className="mb-6 space-y-3">
          {showPoolRunway && accountAnalysis ? (
            <PoolRunwayCard analysis={accountAnalysis} />
          ) : null}
          <SystemHealthBar chartData={chartData} timeRange={timeRange} loading={chartLoading} />
          {chartError ? (
            <div className="flex items-center justify-between gap-3 rounded-lg border border-destructive/30 bg-destructive/5 px-3 py-2 text-sm text-destructive">
              <span>{chartError}</span>
              <Button variant="outline" size="sm" onClick={() => void loadChartData()}>
                {t('common.retry')}
              </Button>
            </div>
          ) : null}
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
        </div>
      </>
    </StateShell>
  )
}
