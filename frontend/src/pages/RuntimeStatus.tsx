import { useCallback, useEffect } from 'react'
import type { ReactNode } from 'react'
import {
  Activity,
  CheckCircle2,
  Database,
  HardDrive,
  KeyRound,
  RefreshCw,
  Server,
  ShieldAlert,
  Signal,
  Users,
} from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api } from '../api'
import OpsTabs from '../components/OpsTabs'
import PageHeader from '../components/PageHeader'
import StateShell from '../components/StateShell'
import { useDataLoader } from '../hooks/useDataLoader'
import type { RuntimeCheck, RuntimeHealthStatus, RuntimeStatusResponse } from '../types'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'

type StatusTone = 'ok' | 'degraded' | 'error'

export default function RuntimeStatus() {
  const { t } = useTranslation()
  const loadRuntimeStatus = useCallback(() => api.getRuntimeStatus(), [])

  const { data: status, loading, error, reload, reloadSilently } = useDataLoader<RuntimeStatusResponse | null>({
    initialData: null,
    load: loadRuntimeStatus,
  })

  useEffect(() => {
    const timer = window.setInterval(() => {
      void reloadSilently()
    }, 15000)

    return () => window.clearInterval(timer)
  }, [reloadSilently])

  const updatedLabel = status?.updated_at ? formatTimeLabel(status.updated_at) : '--:--:--'

  return (
    <StateShell
      variant="page"
      loading={loading}
      error={error}
      onRetry={() => void reload()}
      loadingTitle={t('runtime.loadingTitle')}
      loadingDescription={t('runtime.loadingDesc')}
      errorTitle={t('runtime.errorTitle')}
    >
      <>
        <PageHeader
          title={t('runtime.title')}
          description={t('runtime.description')}
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

        {status ? (
          <>
            <div className="mb-6 grid grid-cols-1 gap-3 min-[420px]:grid-cols-2 xl:grid-cols-4 sm:gap-4">
              <SummaryPill label={t('runtime.overall')} value={t(`runtime.status.${normalStatus(status.status)}`)} tone={normalStatus(status.status)} />
              <SummaryPill label={t('runtime.uptime')} value={formatUptime(status.service.uptime_seconds, t)} />
              <SummaryPill label={t('runtime.databaseCache')} value={`${status.database.label || status.database.driver} / ${status.cache.label || status.cache.driver}`} />
              <SummaryPill label={t('runtime.accountPool')} value={`${status.accounts.available} / ${status.accounts.total}`} tone={status.accounts.status} />
            </div>

            <Card className="mb-6">
              <CardContent className="p-6">
                <div className="mb-5 flex items-center justify-between gap-4">
                  <div>
                    <h3 className="text-base font-semibold text-foreground">{t('runtime.checksTitle')}</h3>
                    <p className="mt-1 text-sm text-muted-foreground">{t('runtime.checksDesc')}</p>
                  </div>
                  <StatusBadge status={status.status} />
                </div>

                <div className="grid gap-3 md:grid-cols-2 xl:grid-cols-3">
                  {status.checks.map((check) => (
                    <CheckRow key={`${check.component}:${check.code}`} check={check} />
                  ))}
                </div>
              </CardContent>
            </Card>

            <div className="grid gap-4 lg:grid-cols-2">
              <StatusPanel
                title={status.database.label || t('runtime.database')}
                status={status.database.status}
                icon={<Database className="size-5" />}
                rows={[
                  [t('runtime.driver'), status.database.driver || '-'],
                  [t('runtime.location'), status.database.location || '-'],
                  [t('runtime.connections'), `${status.database.open} / ${status.database.max_open || '∞'}`],
                  [t('runtime.inUseIdle'), `${status.database.in_use} / ${status.database.idle}`],
                  [t('runtime.waitCount'), formatNumber(status.database.wait_count)],
                  [t('runtime.poolUsage'), `${status.database.usage_percent.toFixed(1)}%`],
                ]}
                error={status.database.error}
              />

              <StatusPanel
                title={status.cache.label || t('runtime.cache')}
                status={status.cache.status}
                icon={<Server className="size-5" />}
                rows={[
                  [t('runtime.driver'), status.cache.driver || '-'],
                  [t('runtime.connections'), `${status.cache.total_conns} / ${status.cache.pool_size || '-'}`],
                  [t('runtime.idleStale'), `${status.cache.idle_conns} / ${status.cache.stale_conns}`],
                  [t('runtime.poolUsage'), `${status.cache.usage_percent.toFixed(1)}%`],
                ]}
                error={status.cache.error}
              />

              <StatusPanel
                title={t('runtime.usageLog')}
                status={status.usage_log.status}
                icon={<Activity className="size-5" />}
                rows={[
                  [t('runtime.mode'), status.usage_log.mode],
                  [t('runtime.enabled'), status.usage_log.enabled ? t('common.enabled') : t('common.disabled')],
                  [t('runtime.batchSize'), formatNumber(status.usage_log.batch_size)],
                  [t('runtime.flushInterval'), t('runtime.seconds', { count: status.usage_log.flush_interval_seconds })],
                  [t('runtime.buffer'), `${status.usage_log.buffer_length} / ${status.usage_log.buffer_capacity || '-'}`],
                ]}
              />

              <StatusPanel
                title={t('runtime.probes')}
                status={status.probes.status}
                icon={<Signal className="size-5" />}
                rows={[
                  [t('runtime.lazyMode'), status.probes.lazy_mode ? t('common.enabled') : t('common.disabled')],
                  [t('runtime.backgroundRefresh'), t('runtime.minutesValue', { count: status.probes.background_refresh_interval_minutes })],
                  [t('runtime.usageProbeMaxAge'), t('runtime.minutesValue', { count: status.probes.usage_probe_max_age_minutes })],
                  [t('runtime.usageProbeConcurrency'), formatNumber(status.probes.usage_probe_concurrency)],
                  [t('runtime.recoveryProbeInterval'), t('runtime.minutesValue', { count: status.probes.recovery_probe_interval_minutes })],
                  [t('runtime.runningJobs'), runningJobs(status, t)],
                ]}
              />

              <StatusPanel
                title={t('runtime.accounts')}
                status={status.accounts.status}
                icon={<Users className="size-5" />}
                rows={[
                  [t('runtime.availableTotal'), `${status.accounts.available} / ${status.accounts.total}`],
                  [t('runtime.activeRequests'), formatNumber(status.accounts.active_requests)],
                  [t('runtime.totalRequests'), formatNumber(status.accounts.total_requests)],
                  [t('runtime.statusCounts'), formatStatusCounts(status.accounts.status_counts, t)],
                ]}
              />

              <StatusPanel
                title={t('runtime.imageStorage')}
                status={status.image_storage.status}
                icon={<HardDrive className="size-5" />}
                rows={[
                  [t('runtime.backend'), status.image_storage.backend || '-'],
                  [t('runtime.localDir'), status.image_storage.local_dir || '-'],
                  [t('runtime.bucket'), status.image_storage.bucket || '-'],
                  [t('runtime.prefix'), status.image_storage.prefix || '-'],
                ]}
                error={status.image_storage.error}
              />

              <StatusPanel
                title={t('runtime.adminAuth')}
                status={status.admin_auth.status}
                icon={<KeyRound className="size-5" />}
                rows={[
                  [t('runtime.source'), status.admin_auth.source],
                  [t('runtime.configured'), status.admin_auth.configured ? t('common.enabled') : t('common.disabled')],
                ]}
              />

              <StatusPanel
                title={t('runtime.service')}
                status={status.service.status}
                icon={<CheckCircle2 className="size-5" />}
                rows={[
                  [t('runtime.serviceUrl'), status.service.service_url || '-'],
                  [t('runtime.adminUrl'), status.service.admin_url || '-'],
                  [t('runtime.apiBaseUrl'), status.service.api_base_url || '-'],
                  [t('runtime.goVersion'), status.service.go_version],
                  [t('runtime.platform'), `${status.service.os}/${status.service.arch}`],
                  [t('runtime.pid'), String(status.service.pid)],
                ]}
              />
            </div>
          </>
        ) : null}
      </>
    </StateShell>
  )
}

function StatusPanel({
  title,
  status,
  icon,
  rows,
  error,
}: {
  title: string
  status: RuntimeHealthStatus
  icon: ReactNode
  rows: Array<[string, string]>
  error?: string
}) {
  const tone = normalStatus(status)
  return (
    <Card>
      <CardContent className="p-5">
        <div className="mb-4 flex items-start justify-between gap-4">
          <div className="flex min-w-0 items-center gap-3">
            <div className={`flex size-10 shrink-0 items-center justify-center rounded-lg ${iconToneClass(tone)}`}>
              {icon}
            </div>
            <h3 className="truncate text-base font-semibold text-foreground">{title}</h3>
          </div>
          <StatusBadge status={status} />
        </div>

        <div className="space-y-2">
          {rows.map(([label, value]) => (
            <div key={label} className="grid grid-cols-[150px_minmax(0,1fr)] gap-3 rounded-md bg-muted/45 px-3 py-2 text-sm max-sm:grid-cols-1 max-sm:gap-1">
              <span className="font-semibold text-muted-foreground">{label}</span>
              <span className="break-words text-foreground">{value}</span>
            </div>
          ))}
        </div>

        {error ? (
          <div className="mt-3 rounded-md border border-destructive/25 bg-destructive/10 px-3 py-2 text-sm text-destructive">
            {error}
          </div>
        ) : null}
      </CardContent>
    </Card>
  )
}

function CheckRow({ check }: { check: RuntimeCheck }) {
  const { t } = useTranslation()
  const tone = normalStatus(check.status)
  return (
    <div className="flex min-h-[74px] items-start gap-3 rounded-lg border border-border bg-card/75 p-3">
      <div className={`mt-0.5 flex size-8 shrink-0 items-center justify-center rounded-lg ${iconToneClass(tone)}`}>
        {tone === 'error' ? <ShieldAlert className="size-4" /> : <CheckCircle2 className="size-4" />}
      </div>
      <div className="min-w-0 flex-1">
        <div className="flex items-center justify-between gap-2">
          <span className="text-sm font-semibold text-foreground">{t(`runtime.components.${check.component}`, { defaultValue: check.component })}</span>
          <StatusBadge status={check.status} />
        </div>
        <p className="mt-1 text-sm leading-relaxed text-muted-foreground">
          {t(`runtime.checks.${check.code}`, { defaultValue: check.message })}
        </p>
      </div>
    </div>
  )
}

function StatusBadge({ status }: { status: RuntimeHealthStatus }) {
  const { t } = useTranslation()
  const tone = normalStatus(status)
  const variant = tone === 'error' ? 'destructive' : tone === 'degraded' ? 'secondary' : 'default'
  return (
    <Badge variant={variant} className="shrink-0 gap-1.5 text-[12px]">
      <span className={`size-1.5 rounded-full ${dotToneClass(tone)}`} />
      {t(`runtime.status.${tone}`)}
    </Badge>
  )
}

function SummaryPill({ label, value, tone = 'ok' }: { label: string; value: string; tone?: RuntimeHealthStatus }) {
  const normalized = normalStatus(tone)
  return (
    <div className={`rounded-lg border px-3 py-2.5 shadow-sm ${summaryToneClass(normalized)}`}>
      <div className="text-[12px] font-bold uppercase text-muted-foreground">{label}</div>
      <div className="mt-2 text-[20px] font-bold text-foreground">{value}</div>
    </div>
  )
}

function normalStatus(status: RuntimeHealthStatus): StatusTone {
  if (status === 'error') return 'error'
  if (status === 'degraded') return 'degraded'
  return 'ok'
}

function iconToneClass(tone: StatusTone): string {
  if (tone === 'error') return 'bg-destructive/10 text-destructive'
  if (tone === 'degraded') return 'bg-amber-500/10 text-amber-600'
  return 'bg-[hsl(var(--success-bg))] text-[hsl(var(--success))]'
}

function dotToneClass(tone: StatusTone): string {
  if (tone === 'error') return 'bg-destructive'
  if (tone === 'degraded') return 'bg-amber-500'
  return 'bg-emerald-500'
}

function summaryToneClass(tone: StatusTone): string {
  if (tone === 'error') return 'border-destructive/20 bg-destructive/10'
  if (tone === 'degraded') return 'border-amber-500/25 bg-amber-500/10'
  return 'border-border bg-card/85'
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

function runningJobs(status: RuntimeStatusResponse, t: (key: string) => string): string {
  const jobs = [
    status.probes.usage_probe_running ? t('runtime.jobUsageProbe') : '',
    status.probes.recovery_probe_running ? t('runtime.jobRecoveryProbe') : '',
    status.probes.auto_cleanup_running ? t('runtime.jobAutoCleanup') : '',
  ].filter(Boolean)
  return jobs.length > 0 ? jobs.join(', ') : '-'
}

function formatStatusCounts(counts: Record<string, number>, t: (key: string, options?: Record<string, unknown>) => string): string {
  const entries = Object.entries(counts)
    .filter(([, count]) => count > 0)
    .sort((left, right) => right[1] - left[1])
  if (entries.length === 0) {
    return '-'
  }
  return entries
    .map(([status, count]) => `${t(`status.${status}`, { defaultValue: status })}: ${count}`)
    .join(' · ')
}
