import { useCallback, useEffect } from 'react'
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
} from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api } from '../api'
import PageHeader from '../components/PageHeader'
import OpsTabs from '../components/OpsTabs'
import StateShell from '../components/StateShell'
import { useDataLoader } from '../hooks/useDataLoader'
import type { OpsOverviewResponse } from '../types'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'

type MetricTone = 'normal' | 'warning' | 'danger' | 'info'

export default function Operations() {
  const { t } = useTranslation()
  const loadOperationsData = useCallback(() => api.getOpsOverview(), [])

  const { data: overview, loading, error, reload, reloadSilently } = useDataLoader<OpsOverviewResponse | null>({
    initialData: null,
    load: loadOperationsData,
  })

  useEffect(() => {
    const timer = window.setInterval(() => {
      void reloadSilently()
    }, 15000)

    return () => window.clearInterval(timer)
  }, [reloadSilently])

  const updatedLabel = overview?.updated_at ? formatTimeLabel(overview.updated_at) : '--:--:--'

  return (
    <StateShell
      variant="page"
      loading={loading}
      error={error}
      onRetry={() => void reload()}
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
              <span className="text-sm text-muted-foreground max-sm:text-center">{t('ops.lastUpdated', { time: updatedLabel })}</span>
              <Button variant="outline" onClick={() => void reload()}>
                <RefreshCw className="size-3.5" />
                {t('common.refresh')}
              </Button>
            </div>
          }
        />
        <OpsTabs />

        {overview ? (
          <>
            <div className="mb-6 grid grid-cols-1 gap-3 min-[420px]:grid-cols-2 xl:grid-cols-4 sm:gap-4">
              <SummaryPill label={t('ops.uptime')} value={formatUptime(overview.uptime_seconds, t)} />
              <SummaryPill label={t('ops.accountPool')} value={`${overview.runtime.available_accounts} / ${overview.runtime.total_accounts}`} />
              <SummaryPill label={t('ops.todayRequests')} value={formatNumber(overview.traffic.today_requests)} />
              <SummaryPill label={t('ops.todayErrorRate')} value={`${overview.traffic.error_rate.toFixed(1)}%`} />
            </div>

            <Card>
              <CardContent className="p-6">
                <div className="mb-5 flex items-center justify-between gap-4">
                  <div>
                    <h3 className="text-base font-semibold text-foreground">{t('ops.overview')}</h3>
                    <p className="mt-1 text-sm text-muted-foreground">{t('ops.overviewDesc')}</p>
                  </div>
                </div>

                <div className="grid grid-cols-1 gap-4 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-5">
                  <OpsMetricCard
                    label={t('ops.cpu')}
                    value={`${overview.cpu.percent.toFixed(1)}%`}
                    sub={t('ops.cpuCores', { count: overview.cpu.cores })}
                    icon={<Cpu className="size-5" />}
                    tone={getPercentTone(overview.cpu.percent, 70, 90)}
                    t={t}
                  />
                  <OpsMetricCard
                    label={t('ops.memory')}
                    value={`${overview.memory.percent.toFixed(1)}%`}
                    sub={`${t('ops.memoryUsage', { used: formatBytes(overview.memory.used_bytes), total: formatBytes(overview.memory.total_bytes) })} · ${t('ops.processMemory', { size: formatBytes(overview.memory.process_bytes) })}`}
                    icon={<HardDrive className="size-5" />}
                    tone={getPercentTone(overview.memory.percent, 75, 90)}
                    t={t}
                  />
                  <OpsMetricCard
                    label={overview.database_label || t('ops.postgres')}
                    value={`${overview.postgres.usage_percent.toFixed(1)}%`}
                    sub={t('ops.pgConn', { open: overview.postgres.open, max: overview.postgres.max_open || '∞' })}
                    icon={<Database className="size-5" />}
                    tone={getDatabaseTone(overview)}
                    t={t}
                  />
                  <OpsMetricCard
                    label={overview.cache_label || t('ops.redis')}
                    value={`${overview.redis.usage_percent.toFixed(1)}%`}
                    sub={t('ops.redisConn', { open: overview.redis.total_conns, max: overview.redis.pool_size || '-' })}
                    icon={<Server className="size-5" />}
                    tone={getCacheTone(overview)}
                    t={t}
                  />
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
              </CardContent>
            </Card>
          </>
        ) : null}
      </>
    </StateShell>
  )
}

function OpsMetricCard({
  label,
  value,
  sub,
  icon,
  tone,
  t,
}: {
  label: string
  value: string
  sub: string
  icon: React.ReactNode
  tone: MetricTone
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
    <Card className="py-0 transition-all duration-150 hover:-translate-y-0.5 hover:shadow-md">
      <CardContent className="p-4">
        <div className="flex items-center justify-between gap-3">
          <span className="text-[13px] font-semibold text-muted-foreground">{label}</span>
          <span className={`inline-flex items-center gap-1 rounded-full px-2.5 py-1 text-[12px] font-semibold ${toneStyle.badge}`}>
            <span className={`size-2 rounded-full ${toneStyle.dot}`} />
            {toneStyle.label}
          </span>
        </div>

        <div className="mt-5 flex items-end justify-between gap-3">
          <div className="min-w-0">
            <div className="text-[32px] font-bold leading-none text-foreground">{value}</div>
            <div className="mt-3 text-[13px] leading-relaxed text-muted-foreground">{sub}</div>
          </div>
          <div className={`flex size-11 shrink-0 items-center justify-center rounded-lg ${toneStyle.icon}`}>
            {icon}
          </div>
        </div>
      </CardContent>
    </Card>
  )
}

function SummaryPill({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded-lg border border-border bg-card/85 px-3 py-2.5 shadow-sm">
      <div className="text-[12px] font-bold uppercase text-muted-foreground">{label}</div>
      <div className="mt-2 text-[20px] font-bold text-foreground">{value}</div>
    </div>
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

function formatBytes(bytes: number): string {
  if (!bytes) return '0 B'
  const units = ['B', 'KB', 'MB', 'GB', 'TB']
  let value = bytes
  let unitIndex = 0
  while (value >= 1024 && unitIndex < units.length - 1) {
    value /= 1024
    unitIndex++
  }
  return `${value.toFixed(unitIndex === 0 ? 0 : 1)} ${units[unitIndex]}`
}

function formatNumber(value: number): string {
  return value.toLocaleString()
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
