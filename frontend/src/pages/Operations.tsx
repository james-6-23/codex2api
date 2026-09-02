import { useCallback, useEffect, useState } from 'react'
import {
  Activity,
  AlertTriangle,
  BarChart3,
  Clock3,
  Cpu,
  Database,
  HardDrive,
  RefreshCw,
  Server,
  Users,
  Zap,
  Gauge,
  Layers,
} from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api } from '../api'
import PageHeader from '../components/PageHeader'
import { StatTile } from '../components/StatTile'
import OpsTabs from '../components/OpsTabs'
import StateShell from '../components/StateShell'
import { useDataLoader } from '../hooks/useDataLoader'
import type { OpsOverviewResponse } from '../types'
import { cacheUtilizationPercent, formatIECBytes } from '../lib/responseCacheMetrics'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { cn } from '@/lib/utils'

type MetricTone = 'normal' | 'warning' | 'danger' | 'info'

export default function Operations() {
  const { t } = useTranslation()
  const loadOperationsData = useCallback(() => api.getOpsOverview(), [])

  const { data: overview, loading, error, reload, reloadSilently } = useDataLoader<OpsOverviewResponse | null>({
    initialData: null,
    load: loadOperationsData,
  })

  const [countdown, setCountdown] = useState(15)

  const triggerManualReload = useCallback(async () => {
    setCountdown(15)
    await reload()
  }, [reload])

  useEffect(() => {
    const timer = window.setInterval(() => {
      setCountdown((prev) => {
        if (prev <= 1) {
          void reloadSilently()
          return 15
        }
        return prev - 1
      })
    }, 1000)

    return () => window.clearInterval(timer)
  }, [reloadSilently])

  const updatedLabel = overview?.updated_at ? formatTimeLabel(overview.updated_at) : '--:--:--'

  return (
    <StateShell
      variant="page"
      loading={loading}
      error={error}
      onRetry={() => void triggerManualReload()}
      loadingTitle={t('ops.loadingTitle')}
      loadingDescription={t('ops.loadingDesc')}
      errorTitle={t('ops.errorTitle')}
    >
      <>
        <PageHeader
          title={t('ops.title')}
          description={t('ops.description')}
          actions={
            <div className="flex items-center gap-3 max-sm:w-full max-sm:flex-col max-sm:items-stretch">
              <div className="flex items-center gap-2 rounded-md border border-border/60 bg-muted/40 px-2.5 py-1.5 text-xs text-muted-foreground max-sm:justify-center">
                <span className="relative flex size-2">
                  <span className="absolute inline-flex h-full w-full animate-ping rounded-full bg-emerald-400 opacity-75" />
                  <span className="relative inline-flex size-2 rounded-full bg-emerald-500" />
                </span>
                <span>{t('ops.autoRefreshing', { sec: countdown })}</span>
              </div>
              <span className="text-xs text-muted-foreground max-sm:text-center">
                {t('ops.lastUpdated', { time: updatedLabel })}
              </span>
              <Button variant="outline" size="sm" onClick={() => void triggerManualReload()} disabled={loading}>
                <RefreshCw className={cn('size-3.5', loading && 'animate-spin')} />
                {loading ? t('ops.refreshing') : t('common.refresh')}
              </Button>
            </div>
          }
        />
        <OpsTabs />

        {overview ? (
          <>
            <div className="mb-6 grid grid-cols-1 gap-3 min-[420px]:grid-cols-2 xl:grid-cols-4 sm:gap-4">
              <StatTile
                label={t('ops.uptime')}
                value={formatUptime(overview.uptime_seconds, t)}
                icon={<Clock3 className="size-4" />}
                tone="neutral"
              />
              <StatTile
                label={t('ops.accountPool')}
                value={`${overview.runtime.available_accounts} / ${overview.runtime.total_accounts}`}
                icon={<Users className="size-4" />}
                tone="info"
              />
              <StatTile
                label={t('ops.todayRequests')}
                value={formatNumber(overview.traffic.today_requests)}
                icon={<BarChart3 className="size-4" />}
                tone="info"
              />
              <StatTile
                label={t('ops.todayErrorRate')}
                value={`${overview.traffic.error_rate.toFixed(1)}%`}
                icon={<AlertTriangle className="size-4" />}
                tone={overview.traffic.error_rate >= 5 ? 'danger' : overview.traffic.error_rate > 0 ? 'warning' : 'success'}
              />
            </div>

            <div className="space-y-6">
              {/* 分组 1：基础硬件与依赖 */}
              <div className="space-y-3">
                <div className="flex items-center gap-2 text-sm font-semibold text-foreground">
                  <Cpu className="size-4 text-primary" />
                  <span>{t('ops.sections.infrastructure')}</span>
                </div>
                <div className="grid grid-cols-1 gap-4 min-[420px]:grid-cols-2 lg:grid-cols-4">
                  <OpsMetricCard
                    label={t('ops.cpu')}
                    value={`${overview.cpu.percent.toFixed(1)}%`}
                    sub={t('ops.cpuCores', { count: overview.cpu.cores })}
                    icon={<Cpu className="size-5" />}
                    tone={getPercentTone(overview.cpu.percent, 70, 90)}
                    progressPercent={overview.cpu.percent}
                    t={t}
                  />
                  <OpsMetricCard
                    label={t('ops.memory')}
                    value={`${overview.memory.percent.toFixed(1)}%`}
                    sub={
                      <dl className="grid grid-cols-2 gap-x-2 gap-y-1 text-xs">
                        <MemoryDetail
                          label={t('ops.memorySystem')}
                          value={`${formatIECBytes(overview.memory.used_bytes)} / ${formatIECBytes(overview.memory.total_bytes)}`}
                        />
                        <MemoryDetail
                          label={t('ops.memoryRss')}
                          value={formatIECBytes(overview.memory.process_bytes)}
                        />
                        {typeof overview.memory.heap_alloc_bytes === 'number' ? (
                          <>
                            <MemoryDetail
                              label={t('ops.heapAllocated')}
                              value={formatIECBytes(overview.memory.heap_alloc_bytes)}
                            />
                            <MemoryDetail
                              label={t('ops.heapInUse')}
                              value={formatIECBytes(overview.memory.heap_inuse_bytes ?? 0)}
                            />
                            <MemoryDetail
                              label={t('ops.heapReleased')}
                              value={formatIECBytes(overview.memory.heap_released_bytes ?? 0)}
                            />
                            <MemoryDetail
                              label={t('ops.gcRuns')}
                              value={formatNumber(overview.memory.num_gc ?? 0)}
                            />
                          </>
                        ) : null}
                      </dl>
                    }
                    icon={<HardDrive className="size-5" />}
                    tone={getPercentTone(overview.memory.percent, 75, 90)}
                    progressPercent={overview.memory.percent}
                    secondaryProgress={
                      typeof overview.memory.container_percent === 'number'
                        ? {
                            label:
                              overview.memory.container_source === 'cgroup'
                                ? t('ops.memoryContainer')
                                : t('ops.memoryProcess'),
                            value: `${formatIECBytes(overview.memory.container_used_bytes ?? 0)} / ${formatIECBytes(
                              overview.memory.container_limit_bytes || overview.memory.total_bytes,
                            )} (${overview.memory.container_percent.toFixed(1)}%)`,
                            percent: overview.memory.container_percent,
                            tone: getPercentTone(overview.memory.container_percent, 75, 90),
                          }
                        : undefined
                    }
                    t={t}
                  />
                  <OpsMetricCard
                    label={overview.database_label || t('ops.postgres')}
                    value={`${overview.postgres.usage_percent.toFixed(1)}%`}
                    sub={t('ops.pgConn', { open: overview.postgres.open, max: overview.postgres.max_open || '∞' })}
                    icon={<Database className="size-5" />}
                    tone={getDatabaseTone(overview)}
                    progressPercent={overview.database_driver === 'sqlite' ? undefined : overview.postgres.usage_percent}
                    t={t}
                  />
                  <OpsMetricCard
                    label={overview.cache_label || t('ops.redis')}
                    value={`${overview.redis.usage_percent.toFixed(1)}%`}
                    sub={t('ops.redisConn', { open: overview.redis.total_conns, max: overview.redis.pool_size || '-' })}
                    icon={<Server className="size-5" />}
                    tone={getCacheTone(overview)}
                    progressPercent={overview.cache_driver === 'memory' ? undefined : overview.redis.usage_percent}
                    t={t}
                  />
                </div>
              </div>

              {/* 分组 2：流量与吞吐表现 */}
              <div className="space-y-3">
                <div className="flex items-center gap-2 text-sm font-semibold text-foreground">
                  <Gauge className="size-4 text-primary" />
                  <span>{t('ops.sections.traffic')}</span>
                </div>
                <div className="grid grid-cols-1 gap-4 min-[420px]:grid-cols-2 lg:grid-cols-4">
                  <OpsMetricCard
                    label={t('ops.qps')}
                    value={overview.traffic.qps.toFixed(1)}
                    sub={t('ops.qpsPeak', { value: overview.traffic.qps_peak.toFixed(1) })}
                    icon={<BarChart3 className="size-5" />}
                    tone="info"
                    t={t}
                  />
                  <OpsMetricCard
                    label={t('ops.tps')}
                    value={formatNumber(Math.round(overview.traffic.tps))}
                    sub={t('ops.tpsPeak', { value: formatNumber(Math.round(overview.traffic.tps_peak)) })}
                    icon={<Zap className="size-5" />}
                    tone="info"
                    t={t}
                  />
                  <OpsMetricCard
                    label={t('ops.rpm')}
                    value={formatNumber(Math.round(overview.traffic.rpm))}
                    sub={overview.traffic.rpm_limit > 0 ? t('ops.rpmLimit', { value: formatNumber(overview.traffic.rpm_limit) }) : t('ops.rpmNoLimit')}
                    icon={<Clock3 className="size-5" />}
                    tone={overview.traffic.rpm_limit > 0 && overview.traffic.rpm >= overview.traffic.rpm_limit * 0.8 ? 'warning' : 'normal'}
                    progressPercent={overview.traffic.rpm_limit > 0 ? (overview.traffic.rpm / overview.traffic.rpm_limit) * 100 : undefined}
                    t={t}
                  />
                  <OpsMetricCard
                    label={t('ops.tpm')}
                    value={formatNumber(Math.round(overview.traffic.tpm))}
                    sub={t('ops.tpmToday', { value: formatNumber(overview.traffic.today_tokens) })}
                    icon={<AlertTriangle className="size-5" />}
                    tone={overview.traffic.error_rate >= 5 ? 'warning' : 'normal'}
                    t={t}
                  />
                </div>
              </div>

              {/* 分组 3：并发与运行时负载 */}
              <div className="space-y-3">
                <div className="flex items-center gap-2 text-sm font-semibold text-foreground">
                  <Layers className="size-4 text-primary" />
                  <span>{t('ops.sections.concurrency')}</span>
                </div>
                <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
                  <OpsMetricCard
                    label={t('ops.activeRequests')}
                    value={formatNumber(overview.requests.active)}
                    sub={t('ops.totalRequestsAccum', { count: formatNumber(overview.requests.total) })}
                    icon={<Activity className="size-5" />}
                    tone={overview.requests.active >= Math.max(50, overview.runtime.total_accounts * 0.5) ? 'warning' : 'normal'}
                    t={t}
                  />
                  <OpsMetricCard
                    label={t('ops.goroutines')}
                    value={formatNumber(overview.runtime.goroutines)}
                    sub={t('ops.goroutinesPool', { available: overview.runtime.available_accounts, total: overview.runtime.total_accounts })}
                    icon={<Users className="size-5" />}
                    tone={overview.runtime.goroutines >= Math.max(1000, overview.runtime.total_accounts * 3) ? 'danger' : overview.runtime.goroutines >= Math.max(500, overview.runtime.total_accounts * 1.5) ? 'warning' : 'normal'}
                    t={t}
                  />
                </div>
                {overview.scheduler ? (
                  <div className="grid grid-cols-1 gap-4 min-[420px]:grid-cols-2 lg:grid-cols-5">
                    <OpsMetricCard
                      label={t('ops.schedulerEngine')}
                      value={formatSchedulerEngine(overview.scheduler.engine, t)}
                      sub={overview.scheduler.engine === 'shadow'
                        ? t('ops.schedulerShadowChecks', { checks: formatNumber(overview.scheduler.shadow_checks), mismatches: formatNumber(overview.scheduler.shadow_mismatches) })
                        : t('ops.schedulerSelections', { count: formatNumber(overview.scheduler.selection_total) })}
                      icon={<Gauge className="size-5" />}
                      tone={overview.scheduler.engine === 'indexed' ? 'normal' : overview.scheduler.engine === 'shadow' ? 'info' : 'warning'}
                      t={t}
                    />
                    <OpsMetricCard
                      label={t('ops.schedulerFastHit')}
                      value={`${schedulerFastHitRate(overview.scheduler).toFixed(1)}%`}
                      sub={t('ops.schedulerSlowScans', { count: formatNumber(overview.scheduler.slow_scanned_accounts) })}
                      icon={<Zap className="size-5" />}
                      tone={overview.scheduler.slow_scanned_accounts > 0 && overview.scheduler.engine === 'indexed' ? 'warning' : 'normal'}
                      t={t}
                    />
                    <OpsMetricCard
                      label={t('ops.schedulerWaiters')}
                      value={formatNumber(overview.scheduler.waiters)}
                      sub={t('ops.schedulerWakeups', { count: formatNumber(overview.scheduler.wait_wakeups) })}
                      icon={<Clock3 className="size-5" />}
                      tone={overview.scheduler.waiters > Math.max(100, overview.runtime.total_accounts) ? 'warning' : 'normal'}
                      t={t}
                    />
                    <OpsMetricCard
                      label={t('ops.schedulerRoutingCache')}
                      value={formatNumber(overview.scheduler.routing_cache_entries)}
                      sub={t('ops.schedulerRoutingCacheStats', { hits: formatNumber(overview.scheduler.routing_cache_hits), accounts: formatNumber(overview.scheduler.routing_cache_accounts) })}
                      icon={<Layers className="size-5" />}
                      tone={overview.scheduler.routing_cache_evictions > 0 ? 'info' : 'normal'}
                      t={t}
                    />
                    <OpsMetricCard
                      label={t('ops.schedulerOutbox')}
                      value={formatNumber(overview.scheduler.outbox_backlog)}
                      sub={t('ops.schedulerOutboxLag', { ms: formatNumber(overview.scheduler.outbox_lag_ms), errors: formatNumber(overview.scheduler.outbox_errors) })}
                      icon={<Database className="size-5" />}
                      tone={overview.scheduler.outbox_errors > 0 ? 'danger' : overview.scheduler.outbox_backlog > 1000 || overview.scheduler.outbox_lag_ms > 5000 ? 'warning' : 'normal'}
                      t={t}
                    />
                  </div>
                ) : null}
              </div>
            </div>

            <ResponseCacheCard cache={overview.response_cache} t={t} />
          </>
        ) : null}
      </>
    </StateShell>
  )
}

function schedulerFastHitRate(scheduler: NonNullable<OpsOverviewResponse['scheduler']>) {
  const decided = scheduler.selection_fast_hit + scheduler.selection_slow_hit
  return decided > 0 ? (scheduler.selection_fast_hit / decided) * 100 : 0
}

type ResponseCacheOverview = NonNullable<OpsOverviewResponse['response_cache']>

function ResponseCacheCard({
  cache,
  t,
}: {
  cache?: ResponseCacheOverview
  t: (key: string, options?: Record<string, unknown>) => string
}) {
  if (!cache) {
    return (
      <Card className="mt-6">
        <CardContent className="p-6">
          <h3 className="text-base font-semibold text-foreground">{t('ops.responseCache.title')}</h3>
          <p className="mt-1 text-sm text-muted-foreground">{t('ops.responseCache.unavailable')}</p>
        </CardContent>
      </Card>
    )
  }

  const maxBytes = cache.max_bytes || cache.applied_config.local_max_bytes
  const utilization = cacheUtilizationPercent(cache.current_bytes, maxBytes)
  const syncTone: MetricTone = cache.last_config_sync_error
    ? 'danger'
    : cache.effective_config.generation !== cache.applied_config.generation
      ? 'warning'
      : cache.last_config_sync_at
        ? 'normal'
        : 'info'
  const syncLabel = cache.last_config_sync_error
    ? t('ops.responseCache.syncError')
    : cache.effective_config.generation !== cache.applied_config.generation
      ? t('ops.responseCache.syncPending')
      : cache.last_config_sync_at
        ? t('ops.responseCache.syncHealthy')
        : t('ops.responseCache.syncWaiting')
  const syncStyle = {
    normal: 'bg-[hsl(var(--success-bg))] text-[hsl(var(--success))]',
    warning: 'bg-amber-500/10 text-amber-600',
    danger: 'bg-destructive/10 text-destructive',
    info: 'bg-primary/10 text-primary',
  }[syncTone]

  const localHitRate = calculateHitRatePercent(cache.local_hits, cache.local_misses)
  const remoteHitRate = calculateHitRatePercent(cache.remote_hits, cache.remote_misses)

  return (
    <Card className="mt-6">
      <CardContent className="p-6">
        <div className="flex flex-wrap items-start justify-between gap-3">
          <div>
            <h3 className="text-base font-semibold text-foreground">{t('ops.responseCache.title')}</h3>
            <p className="mt-1 max-w-3xl text-sm text-muted-foreground">{t('ops.responseCache.description')}</p>
          </div>
          <span className={`inline-flex items-center gap-1.5 rounded-full px-2.5 py-1 text-xs font-semibold ${syncStyle}`}>
            <span className={cn('size-2 rounded-full', syncTone === 'danger' ? 'bg-destructive' : syncTone === 'warning' ? 'bg-amber-500' : syncTone === 'info' ? 'bg-primary' : 'bg-emerald-500')} />
            {syncLabel}
          </span>
        </div>

        <div className="mt-5 grid gap-3 md:grid-cols-3">
          <div className="rounded-lg border border-border/80 bg-muted/20 p-4 md:col-span-2">
            <div className="flex items-center justify-between gap-3 text-sm">
              <span className="font-semibold text-foreground">{t('ops.responseCache.logicalBudget')}</span>
              <span className="font-semibold tabular-nums text-foreground">
                {formatIECBytes(cache.current_bytes)} / {formatIECBytes(maxBytes)}
              </span>
            </div>
            <div
              className="mt-3 h-2 overflow-hidden rounded-full bg-muted"
              role="progressbar"
              aria-label={t('ops.responseCache.logicalBudget')}
              aria-valuemin={0}
              aria-valuemax={100}
              aria-valuenow={Math.round(utilization)}
            >
              <div
                className={cn(
                  'h-full rounded-full transition-[width] duration-300',
                  utilization >= 90 ? 'bg-destructive' : utilization >= 75 ? 'bg-amber-500' : 'bg-primary',
                )}
                style={{ width: `${utilization}%` }}
              />
            </div>
            <p className="mt-2 text-xs text-muted-foreground">
              {t('ops.responseCache.logicalBudgetHint', { percent: utilization.toFixed(1) })}
            </p>
          </div>
          <div className="rounded-lg border border-border/80 bg-muted/20 p-4">
            <div className="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
              {t('ops.responseCache.entries')}
            </div>
            <div className="mt-2 text-2xl font-bold tabular-nums text-foreground">
              {formatNumber(cache.entries)} / {formatNumber(cache.max_entries)}
            </div>
            <p className="mt-2 text-xs text-muted-foreground">
              {t('ops.responseCache.generations', {
                effective: cache.effective_config.generation,
                applied: cache.applied_config.generation,
              })}
            </p>
          </div>
        </div>

        <div className="mt-4 grid gap-3 sm:grid-cols-2 lg:grid-cols-4">
          <CacheMetricTile
            label={t('ops.responseCache.highWater')}
            value={formatIECBytes(cache.high_water_bytes)}
            sub={t('ops.responseCache.highWaterSub', { max: formatIECBytes(maxBytes) })}
          />
          <CacheMetricTile
            label={t('ops.responseCache.largestEntry')}
            value={formatIECBytes(cache.largest_entry_bytes)}
            sub={t('ops.responseCache.entryLimit', { max: formatIECBytes(cache.applied_config.local_max_entry_bytes) })}
          />
          <CacheMetricTile
            label={t('ops.responseCache.localLookup')}
            value={`${formatNumber(cache.local_hits)} / ${formatNumber(cache.local_misses)}`}
            sub={t('ops.responseCache.hitMiss')}
            badge={
              localHitRate !== null ? (
                <span className="rounded bg-emerald-500/10 px-1.5 py-0.5 text-[11px] font-semibold text-emerald-600 dark:bg-emerald-500/20 dark:text-emerald-400">
                  {t('ops.hitRate', { rate: localHitRate })}
                </span>
              ) : undefined
            }
          />
          <CacheMetricTile
            label={t('ops.responseCache.remoteLookup')}
            value={`${formatNumber(cache.remote_hits)} / ${formatNumber(cache.remote_misses)}`}
            sub={t('ops.responseCache.hitMiss')}
            badge={
              remoteHitRate !== null ? (
                <span className="rounded bg-blue-500/10 px-1.5 py-0.5 text-[11px] font-semibold text-blue-600 dark:bg-blue-500/20 dark:text-blue-400">
                  {t('ops.hitRate', { rate: remoteHitRate })}
                </span>
              ) : undefined
            }
          />
          <CacheMetricTile
            label={t('ops.responseCache.expirations')}
            value={formatNumber(cache.expirations)}
            sub={t('ops.responseCache.absoluteTTL')}
          />
          <CacheMetricTile
            label={t('ops.responseCache.evictions')}
            value={`${formatNumber(cache.count_evictions)} / ${formatNumber(cache.byte_evictions)}`}
            sub={t('ops.responseCache.countByte')}
          />
          <CacheMetricTile
            label={t('ops.responseCache.oversize')}
            value={`${formatNumber(cache.oversize_bypasses)} / ${formatNumber(cache.oversize_rejections)}`}
            sub={t('ops.responseCache.bypassRejection')}
          />
          <CacheMetricTile
            label={t('ops.responseCache.unavailableErrors')}
            value={formatNumber(cache.known_unavailable_errors)}
            sub={
              cache.last_config_sync_at
                ? t('ops.responseCache.lastSync', { time: formatDateTimeLabel(cache.last_config_sync_at) })
                : t('ops.responseCache.neverSynced')
            }
          />
        </div>

        {cache.last_config_sync_error ? (
          <p role="alert" className="mt-4 rounded-lg border border-destructive/20 bg-destructive/5 px-3 py-2 text-xs text-destructive">
            {t('ops.responseCache.syncErrorDetail', { error: cache.last_config_sync_error })}
          </p>
        ) : null}
      </CardContent>
    </Card>
  )
}

function CacheMetricTile({
  label,
  value,
  sub,
  badge,
}: {
  label: string
  value: string
  sub: string
  badge?: React.ReactNode
}) {
  return (
    <div className="rounded-lg border border-border/70 bg-card px-3.5 py-3 transition-colors hover:border-primary/30">
      <div className="flex items-center justify-between gap-2">
        <div className="text-xs font-semibold text-muted-foreground">{label}</div>
        {badge}
      </div>
      <div className="mt-2 text-lg font-bold tabular-nums text-foreground">{value}</div>
      <div className="mt-1 text-xs leading-relaxed text-muted-foreground">{sub}</div>
    </div>
  )
}

function MemoryDetail({ label, value }: { label: string; value: string }) {
  return (
    <div className="inline-flex min-w-0 items-baseline gap-1 overflow-hidden whitespace-nowrap">
      <dt className="text-[11px] font-medium text-muted-foreground">{label}:</dt>
      <dd className="truncate text-[11px] font-semibold tabular-nums text-foreground/90">{value}</dd>
    </div>
  )
}

const METRIC_TONE_DOT: Record<MetricTone, string> = {
  normal: 'bg-emerald-500',
  warning: 'bg-amber-500',
  danger: 'bg-destructive',
  info: 'bg-primary',
}

function OpsMetricCard({
  label,
  value,
  sub,
  icon,
  tone,
  progressPercent,
  secondaryProgress,
  t,
}: {
  label: string
  value: string
  sub: React.ReactNode
  icon: React.ReactNode
  tone: MetricTone
  progressPercent?: number
  secondaryProgress?: { label: string; value: string; percent: number; tone: MetricTone }
  t: (key: string) => string
}) {
  const toneStyle = {
    normal: {
      badge: 'bg-[hsl(var(--success-bg))] text-[hsl(var(--success))]',
      dot: 'bg-emerald-500',
      icon: 'bg-[hsl(var(--success-bg))] text-[hsl(var(--success))]',
      label: t('common.normal'),
    },
    warning: {
      badge: 'bg-amber-500/10 text-amber-600',
      dot: 'bg-amber-500',
      icon: 'bg-amber-500/10 text-amber-600',
      label: t('common.warning'),
    },
    danger: {
      badge: 'bg-destructive/10 text-destructive',
      dot: 'bg-destructive',
      icon: 'bg-destructive/10 text-destructive',
      label: t('common.danger'),
    },
    info: {
      badge: 'bg-primary/10 text-primary',
      dot: 'bg-primary',
      icon: 'bg-primary/10 text-primary',
      label: t('common.info'),
    },
  }[tone]

  return (
    <Card className="py-0 transition-all duration-200 hover:border-primary/30 hover:shadow-sm">
      <CardContent className="p-4">
        <div className="flex items-center justify-between gap-3">
          <span className="text-[13px] font-semibold text-muted-foreground">{label}</span>
          {tone !== 'normal' ? (
            <span className={`inline-flex items-center gap-1 rounded-full px-2.5 py-1 text-[12px] font-semibold ${toneStyle.badge}`}>
              <span className={`size-2 rounded-full ${toneStyle.dot}`} />
              {toneStyle.label}
            </span>
          ) : null}
        </div>

        <div className="mt-4">
          <div className="flex items-center justify-between gap-3">
            <div className="min-w-0 text-[28px] font-bold leading-none tracking-tight tabular-nums text-foreground">{value}</div>
            <div
              className={`flex size-10 shrink-0 items-center justify-center rounded-lg ${toneStyle.icon}`}
              aria-hidden="true"
            >
              {icon}
            </div>
          </div>

          {typeof progressPercent === 'number' && !Number.isNaN(progressPercent) ? (
            <div className="mt-3.5 h-1.5 w-full overflow-hidden rounded-full bg-muted/60">
              <div
                className={cn('h-full rounded-full transition-all duration-500', toneStyle.dot)}
                style={{ width: `${Math.min(100, Math.max(0, progressPercent))}%` }}
              />
            </div>
          ) : null}

          {secondaryProgress ? (
            <div className="mt-2.5">
              <div className="flex items-center justify-between gap-2 text-[11px] text-muted-foreground">
                <span className="truncate">{secondaryProgress.label}</span>
                <span className="shrink-0 tabular-nums">{secondaryProgress.value}</span>
              </div>
              <div className="mt-1 h-1.5 w-full overflow-hidden rounded-full bg-muted/60">
                <div
                  className={cn('h-full rounded-full transition-all duration-500', METRIC_TONE_DOT[secondaryProgress.tone])}
                  style={{ width: `${Math.min(100, Math.max(0, secondaryProgress.percent))}%` }}
                />
              </div>
            </div>
          ) : null}

          <div className="mt-3 min-w-0 text-[12px] leading-relaxed text-muted-foreground">{sub}</div>
        </div>
      </CardContent>
    </Card>
  )
}

function getPercentTone(value: number, warningThreshold: number, dangerThreshold: number): MetricTone {
  if (value >= dangerThreshold) return 'danger'
  if (value >= warningThreshold) return 'warning'
  return 'normal'
}

function getDatabaseTone(overview: OpsOverviewResponse): MetricTone {
  if (!overview.postgres.healthy) return 'danger'
  if (overview.database_driver === 'sqlite') return 'normal'
  return getPercentTone(overview.postgres.usage_percent, 75, 90)
}

function getCacheTone(overview: OpsOverviewResponse): MetricTone {
  if (!overview.redis.healthy) return 'danger'
  if (overview.cache_driver === 'memory') return 'normal'
  return getPercentTone(overview.redis.usage_percent, 70, 90)
}

function calculateHitRatePercent(hits: number, misses: number): string | null {
  const total = hits + misses
  if (total === 0) return null
  return ((hits / total) * 100).toFixed(1)
}

function formatNumber(value: number): string {
  return value.toLocaleString()
}

function formatSchedulerEngine(engine: string, t: (key: string) => string): string {
  switch (engine) {
    case 'legacy':
      return t('settings.schedulerEngineLegacy')
    case 'shadow':
      return t('settings.schedulerEngineShadow')
    case 'indexed':
      return t('settings.schedulerEngineIndexed')
    default:
      return engine
  }
}

function formatDateTimeLabel(iso: string): string {
  const date = new Date(iso)
  if (Number.isNaN(date.getTime())) {
    return '--'
  }
  return date.toLocaleString()
}

function formatTimeLabel(iso: string): string {
  const date = new Date(iso)
  if (Number.isNaN(date.getTime())) {
    return '--:--:--'
  }
  return date.toLocaleTimeString('zh-CN', {
    hour12: false,
  })
}

function formatUptime(seconds: number, t: (key: string) => string): string {
  if (seconds <= 0) return t('ops.justStarted')

  const days = Math.floor(seconds / 86400)
  const hours = Math.floor((seconds % 86400) / 3600)
  const minutes = Math.floor((seconds % 3600) / 60)

  if (days > 0) {
    return `${days}${t('ops.days')} ${hours}${t('ops.hours')}`
  }
  if (hours > 0) {
    return `${hours}${t('ops.hours')} ${minutes}${t('ops.minutes')}`
  }
  return `${minutes}${t('ops.minutes')}`
}
