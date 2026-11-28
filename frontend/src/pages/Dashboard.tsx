import { lazy, Suspense, useCallback, useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { api } from '../api'
import { getTimeRangeISO, getBucketConfig, type TimeRangeKey } from '../lib/timeRange'
import PageHeader from '../components/PageHeader'
import StateShell from '../components/StateShell'
import TimeRangeSelector from '../components/TimeRangeSelector'
import ChannelFilter, { useUsageChannel, type UsageChannel } from '../components/ChannelFilter'
import { useVisibleChannels } from '../visibleChannels'
import SystemHealthBar from '../components/SystemHealthBar'
import DashboardKpiGrid from '../components/dashboard/DashboardKpiGrid'
import DashboardPoolChannelCard, { type ChannelBreakdownEntry } from '../components/dashboard/DashboardPoolChannelCard'
import DashboardRecentRequestsCard from '../components/dashboard/DashboardRecentRequestsCard'
import type {
  AccountAnalysisResponse,
  StatsResponse,
  StatsChannelCounts,
  SystemSettings,
  UsageLog,
  UsageStats,
  ChartAggregation,
} from '../types'
import { useDataLoader } from '../hooks/useDataLoader'
import { Card, CardContent } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { BarChart3 } from 'lucide-react'
import PoolRunwayCard from '../components/PoolRunwayCard'

const DashboardPrimaryTrendCard = lazy(() => import('../components/dashboard/DashboardPrimaryTrendCard'))

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

function PrimaryTrendSkeleton() {
  return (
    <Card className="h-full border-border/75 py-0 shadow-2xs">
      <CardContent className="p-5">
        <div className="mb-4 space-y-2">
          <div className="h-4 w-36 animate-pulse rounded-md bg-muted" />
          <div className="h-3 w-52 animate-pulse rounded-md bg-muted/60" />
        </div>
        <div className="mb-4 grid grid-cols-3 gap-2">
          {[0, 1, 2].map((i) => (
            <div key={i} className="h-14 animate-pulse rounded-xl bg-muted/45" />
          ))}
        </div>
        <div className="flex h-[270px] items-end gap-2 px-3 pb-4">
          {[40, 65, 30, 80, 55, 70, 45, 60, 35, 75, 50, 68].map((h, j) => (
            <div
              key={j}
              className="flex-1 animate-pulse rounded-t-md bg-muted/50"
              style={{ height: `${h}%`, animationDelay: `${j * 70}ms` }}
            />
          ))}
        </div>
      </CardContent>
    </Card>
  )
}

export default function Dashboard() {
  const { t } = useTranslation()
  const [timeRange, setTimeRange] = useState<TimeRangeKey>('1h')
  const [channel, setChannel] = useUsageChannel()
  const { isChannelVisible } = useVisibleChannels()
  const channelRef = useRef<UsageChannel>(channel)
  const [showPoolRunway, setShowPoolRunway] = useState(getInitialPoolRunwayVisibility)
  const [chartData, setChartData] = useState<ChartAggregation | null>(null)
  const [chartRefreshedAt, setChartRefreshedAt] = useState<number | null>(null)
  const [chartLoading, setChartLoading] = useState(true)
  const [chartError, setChartError] = useState<string | null>(null)
  const [recentLogs, setRecentLogs] = useState<UsageLog[]>([])
  const [recentLogsLoading, setRecentLogsLoading] = useState(true)
  const chartAbort = useRef<AbortController | null>(null)
  const statsAbort = useRef<AbortController | null>(null)
  const poolAbort = useRef<AbortController | null>(null)
  const recentLogsGenerationRef = useRef(0)
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

  // 概览底部最近请求流（轻量 8 行，不阻塞核心统计）
  const loadRecentLogs = useCallback(async () => {
    const generation = ++recentLogsGenerationRef.current
    setRecentLogsLoading(true)
    try {
      const { start, end } = getTimeRangeISO(timeRange)
      const res = await api.getUsageLogsPaged({
        start,
        end,
        channel: channel || undefined,
        page: 1,
        pageSize: 8,
      })
      if (generation === recentLogsGenerationRef.current) {
        setRecentLogs(res.logs ?? [])
      }
    } catch {
      // 静默容错，不影响概览主体
    } finally {
      if (generation === recentLogsGenerationRef.current) {
        setRecentLogsLoading(false)
      }
    }
  }, [timeRange, channel])

  useEffect(() => () => {
    chartAbort.current?.abort()
    statsAbort.current?.abort()
    poolAbort.current?.abort()
    poolRequestGenerationRef.current += 1
    recentLogsGenerationRef.current += 1
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

  // 首次加载 + timeRange / channel 变更时重新拉取图表与最近请求
  useEffect(() => {
    void loadChartData()
    void loadRecentLogs()
  }, [loadChartData, loadRecentLogs])

  // 仅在 1h（实时）模式下启用自动刷新
  useEffect(() => {
    if (timeRange !== '1h') return

    const timer = window.setInterval(() => {
      if (document.visibilityState !== 'visible') return
      void reloadSilently()
      void loadChartData()
      void loadRecentLogs()
    }, DASHBOARD_REFRESH_INTERVAL_MS)

    return () => window.clearInterval(timer)
  }, [reloadSilently, timeRange, loadChartData, loadRecentLogs])

  const { stats, usageStats, settings, accountAnalysis } = data
  const showFullUsageNumbers = settings?.show_full_usage_numbers ?? false
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
  const rangeLabel = t(`dashboard.timeRange${timeRange.toUpperCase()}`)

  const channelBreakdown: ChannelBreakdownEntry[] = !channel && stats?.channels
    ? (['codex', 'grok', 'antigravity', 'claude', 'kiro', 'opencode'] as const)
        .filter((key) => isChannelVisible(key))
        .map((key) => {
          const channelLabel = key === 'claude'
            ? 'Claude'
            : key === 'grok'
              ? 'Grok'
              : key === 'antigravity'
                ? 'Antigravity'
                : key === 'kiro'
                  ? 'Kiro'
                  : key === 'opencode'
                    ? 'OpenCode'
                    : 'Codex'
          return {
            key,
            label: channelLabel,
            counts: stats.channels?.[key],
            title: t('dashboard.heroChannelTitle', {
              channel: key === 'claude' ? 'Claude' : channelLabel,
              available: stats.channels?.[key]?.available ?? 0,
              total: stats.channels?.[key]?.total ?? 0,
              requests: stats.channels?.[key]?.today_requests ?? 0,
            }),
          }
        })
        .filter((item): item is ChannelBreakdownEntry & { title: string } =>
          Boolean(item.counts && item.counts.total > 0))
    : []

  const refreshAll = () => {
    void reload()
    void loadChartData()
    void loadRecentLogs()
  }

  return (
    <StateShell
      variant="page"
      loading={loading}
      error={error}
      onRetry={refreshAll}
      loadingTitle={t('dashboard.loadingTitle')}
      loadingDescription={t('dashboard.loadingDesc')}
      errorTitle={t('dashboard.errorTitle')}
    >
      <>
        <PageHeader
          title={t('dashboard.title')}
          description={t('dashboard.description')}
          onRefresh={refreshAll}
          className="mb-3 sm:mb-4"
          actions={
            <TimeRangeSelector
              timeRange={timeRange}
              onTimeRangeChange={setTimeRange}
            />
          }
        />

        {/* Unified Studio Filter Toolbar */}
        <div className="mb-5 flex flex-wrap items-center justify-between gap-2.5">
          <ChannelFilter value={channel} onChange={handleChannelChange} />
          <div className="flex items-center gap-2">
            {channel ? (
              <Button
                type="button"
                variant="outline"
                size="sm"
                className="shrink-0 rounded-lg"
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
              <span className="inline-flex items-center gap-1.5 rounded-lg border border-border/60 bg-muted/35 px-2.5 py-1.5 text-[11.5px] font-medium text-muted-foreground">
                <BarChart3 className="size-3.5 opacity-65" />
                {t('dashboard.poolRunwaySingleChannelOnly')}
              </span>
            )}
          </div>
        </div>

        {/* 渠道切换时整块内容淡入过渡 */}
        <div key={channel || 'all'} className="space-y-5 animate-channel-switch-in">
          {/* Empty-pool onboarding callout */}
          {total === 0 ? (
            <div className="rounded-xl border border-dashed border-border bg-card/80 px-5 py-4 text-sm text-muted-foreground shadow-2xs">
              <div className="font-semibold text-foreground">{t('dashboard.heroEmptyTitle')}</div>
              <p className="mt-1 text-xs leading-relaxed">{t('dashboard.heroEmptyDesc')}</p>
            </div>
          ) : null}

          {/* Row 1: 4 high-density KPI cards with sparklines */}
          <DashboardKpiGrid
            total={total}
            available={available}
            rateLimited={rateLimited}
            errorCount={errorCount}
            todayRequests={todayRequests}
            usageStats={usageStats}
            chartData={chartData}
            rangeLabel={rangeLabel}
            showFullUsageNumbers={showFullUsageNumbers}
          />

          {/* Row 2: Asymmetric 2-column command center (62% interactive trend + 38% pool/channels/models) */}
          <div className="grid grid-cols-1 gap-4 xl:grid-cols-[minmax(0,1.6fr)_minmax(0,1fr)]">
            <Suspense fallback={<PrimaryTrendSkeleton />}>
              <DashboardPrimaryTrendCard
                chartData={chartData}
                usageStats={usageStats}
                timeRange={timeRange}
                refreshedAt={chartRefreshedAt}
                refreshIntervalMs={DASHBOARD_REFRESH_INTERVAL_MS}
                loading={chartLoading}
              />
            </Suspense>

            <DashboardPoolChannelCard
              total={total}
              available={available}
              rateLimited={rateLimited}
              errorCount={errorCount}
              activeChannel={channel}
              channelBreakdown={channelBreakdown}
              models={chartData?.models ?? []}
              onSelectChannel={handleChannelChange}
            />
          </div>

          {chartError ? (
            <div className="flex items-center justify-between gap-3 rounded-lg border border-destructive/30 bg-destructive/5 px-3 py-2 text-sm text-destructive">
              <span>{chartError}</span>
              <Button variant="outline" size="sm" onClick={() => void loadChartData()}>
                {t('common.retry')}
              </Button>
            </div>
          ) : null}

          {/* Row 3: Pool runway forecast (when single channel) + System health timeline bar */}
          <div className="space-y-3">
            {showPoolRunway && accountAnalysis ? (
              <PoolRunwayCard analysis={accountAnalysis} />
            ) : null}
            <SystemHealthBar chartData={chartData} timeRange={timeRange} loading={chartLoading} />
          </div>

          {/* Row 4: Live recent requests feed */}
          <DashboardRecentRequestsCard logs={recentLogs} loading={recentLogsLoading} />
        </div>
      </>
    </StateShell>
  )
}
