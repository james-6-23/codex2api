import { useMemo } from 'react'
import { useTranslation } from 'react-i18next'
import { Cpu, Database, Gauge, HardDrive, Server } from 'lucide-react'
import { Card, CardContent } from '@/components/ui/card'
import type { OpsOverviewResponse } from '../../types'
import { formatIECBytes } from '../../lib/responseCacheMetrics'
import { OpsRingGauge, OpsSparkline, type OpsSamplePoint } from './OpsHealthHero'
import { cn } from '@/lib/utils'

interface OpsInfraDeckProps {
  overview: OpsOverviewResponse
  samples: OpsSamplePoint[]
}

function progressToneClass(percent: number, warn = 75, danger = 90) {
  if (percent >= danger) return 'bg-destructive'
  if (percent >= warn) return 'bg-amber-500'
  return 'bg-emerald-500'
}

function formatCompact(n: number): string {
  if (!Number.isFinite(n)) return '0'
  if (Math.abs(n) >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`
  if (Math.abs(n) >= 10_000) return `${(n / 1_000).toFixed(1)}k`
  return Math.round(n).toLocaleString()
}

export default function OpsInfraDeck({ overview, samples }: OpsInfraDeckProps) {
  const { t } = useTranslation()

  const cpuSeries = useMemo(() => samples.map((s) => s.cpu), [samples])
  const memSeries = useMemo(() => samples.map((s) => s.memory), [samples])

  const cpuTone = overview.cpu.percent >= 90 ? 'rose' : overview.cpu.percent >= 70 ? 'amber' : 'primary'
  const memTone = overview.memory.percent >= 90 ? 'rose' : overview.memory.percent >= 75 ? 'amber' : 'emerald'

  const rpmPct =
    overview.traffic.rpm_limit > 0
      ? Math.min(100, Math.max(0, (overview.traffic.rpm / overview.traffic.rpm_limit) * 100))
      : null

  return (
    <div className="grid grid-cols-1 gap-4 xl:grid-cols-[minmax(0,1.22fr)_minmax(0,1fr)]">
      {/* Left: Compute & Go Runtime */}
      <Card className="flex h-full flex-col rounded-2xl border-border/75 bg-card py-0 shadow-2xs">
        <CardContent className="flex flex-1 flex-col gap-5 p-5">
          <div className="flex flex-wrap items-start justify-between gap-2">
            <div>
              <h3 className="flex items-center gap-2 text-[15px] font-semibold tracking-tight text-foreground">
                <Cpu className="size-4 text-primary" />
                {t('ops.computeCard.title')}
              </h3>
              <p className="mt-0.5 text-xs text-muted-foreground">{t('ops.computeCard.desc')}</p>
            </div>
            {samples.length > 1 ? (
              <span className="rounded-md bg-muted/60 px-2 py-0.5 font-sans text-[11px] font-medium text-muted-foreground tabular-nums">
                {t('ops.computeCard.liveSamples', { count: samples.length })}
              </span>
            ) : null}
          </div>

          {/* CPU + Memory dual gauges */}
          <div className="grid flex-1 grid-cols-1 gap-3.5 sm:grid-cols-2">
            {/* CPU Panel */}
            <div className="flex flex-col justify-between rounded-xl border border-border/65 bg-muted/20 p-4">
              <div className="flex items-center justify-between gap-2">
                <span className="inline-flex items-center gap-1.5 text-xs font-medium text-muted-foreground">
                  <Cpu className="size-3.5 text-primary" />
                  {t('ops.cpu')}
                </span>
                <span className="rounded-md bg-background/80 px-1.5 py-0.5 font-sans text-[11px] font-medium text-muted-foreground tabular-nums ring-1 ring-border/50">
                  {t('ops.cpuCores', { count: overview.cpu.cores })}
                </span>
              </div>

              <div className="my-4 flex items-center justify-between gap-2">
                <div className="flex min-w-0 items-center gap-3">
                  <OpsRingGauge percent={overview.cpu.percent} invertSeverity size={50} />
                  <div className="min-w-0">
                    <div className="font-sans text-[24px] leading-none font-semibold tracking-[-0.03em] text-foreground tabular-nums">
                      {overview.cpu.percent.toFixed(1)}%
                    </div>
                    <span className="mt-1 block text-[11px] text-muted-foreground">
                      {overview.cpu.percent >= 90
                        ? t('common.danger')
                        : overview.cpu.percent >= 70
                          ? t('common.warning')
                          : t('common.normal')}
                    </span>
                  </div>
                </div>
                <OpsSparkline values={cpuSeries} tone={cpuTone} width={64} height={32} />
              </div>

              <div>
                <div className="mb-1.5 flex items-center justify-between text-[11px] text-muted-foreground">
                  <span>{t('ops.cpu')}</span>
                  <span className="font-sans font-medium text-foreground/85 tabular-nums">
                    {overview.cpu.percent.toFixed(1)}% / 100%
                  </span>
                </div>
                <div className="h-1.5 w-full overflow-hidden rounded-full bg-muted/80">
                  <div
                    className={cn('h-full rounded-full transition-all duration-500', progressToneClass(overview.cpu.percent, 70, 90))}
                    style={{ width: `${Math.min(100, Math.max(0, overview.cpu.percent))}%` }}
                  />
                </div>
              </div>
            </div>

            {/* Memory Panel */}
            <div className="flex flex-col justify-between rounded-xl border border-border/65 bg-muted/20 p-4">
              <div className="flex items-center justify-between gap-2">
                <span className="inline-flex items-center gap-1.5 text-xs font-medium text-muted-foreground">
                  <HardDrive className="size-3.5 text-emerald-600 dark:text-emerald-400" />
                  {t('ops.memory')}
                </span>
                <span className="rounded-md bg-background/80 px-1.5 py-0.5 font-sans text-[11px] font-medium text-muted-foreground tabular-nums ring-1 ring-border/50">
                  {t('ops.memoryRss')} {formatIECBytes(overview.memory.process_bytes)}
                </span>
              </div>

              <div className="my-4 flex items-center justify-between gap-2">
                <div className="flex min-w-0 items-center gap-3">
                  <OpsRingGauge percent={overview.memory.percent} invertSeverity size={50} />
                  <div className="min-w-0">
                    <div className="font-sans text-[24px] leading-none font-semibold tracking-[-0.03em] text-foreground tabular-nums">
                      {overview.memory.percent.toFixed(1)}%
                    </div>
                    <span className="mt-1 block truncate font-sans text-[11px] text-muted-foreground tabular-nums">
                      {formatIECBytes(overview.memory.used_bytes)} / {formatIECBytes(overview.memory.total_bytes)}
                    </span>
                  </div>
                </div>
                <OpsSparkline values={memSeries} tone={memTone} width={64} height={32} />
              </div>

              {typeof overview.memory.container_percent === 'number' ? (
                <div>
                  <div className="mb-1.5 flex items-center justify-between gap-2 text-[11px] text-muted-foreground">
                    <span className="shrink-0">
                      {overview.memory.container_source === 'cgroup'
                        ? t('ops.memoryContainer')
                        : t('ops.memoryProcess')}
                    </span>
                    <span className="truncate font-sans font-medium text-foreground/85 tabular-nums">
                      {formatIECBytes(overview.memory.container_used_bytes ?? 0)} /{' '}
                      {formatIECBytes(overview.memory.container_limit_bytes || overview.memory.total_bytes)} (
                      {overview.memory.container_percent.toFixed(1)}%)
                    </span>
                  </div>
                  <div className="h-1.5 w-full overflow-hidden rounded-full bg-muted/80">
                    <div
                      className={cn(
                        'h-full rounded-full transition-all duration-500',
                        progressToneClass(overview.memory.container_percent, 75, 90),
                      )}
                      style={{ width: `${Math.min(100, Math.max(0, overview.memory.container_percent))}%` }}
                    />
                  </div>
                </div>
              ) : (
                <div className="h-1.5 w-full overflow-hidden rounded-full bg-muted/80">
                  <div
                    className={cn('h-full rounded-full transition-all duration-500', progressToneClass(overview.memory.percent, 75, 90))}
                    style={{ width: `${Math.min(100, Math.max(0, overview.memory.percent))}%` }}
                  />
                </div>
              )}
            </div>
          </div>

          {/* Go Heap Profile Strip */}
          {typeof overview.memory.heap_alloc_bytes === 'number' ? (
            <div className="rounded-xl border border-border/60 bg-muted/15 p-3.5">
              <div className="mb-2.5 flex items-center justify-between gap-2">
                <span className="text-xs font-medium text-muted-foreground">{t('ops.computeCard.heapTitle')}</span>
                <span className="font-sans text-[11px] text-muted-foreground tabular-nums">
                  {t('ops.memorySystem')}: {formatIECBytes(overview.memory.used_bytes)} / {formatIECBytes(overview.memory.total_bytes)}
                </span>
              </div>
              <div className="grid grid-cols-2 gap-2.5 sm:grid-cols-4">
                {[
                  { label: t('ops.heapAllocated'), value: formatIECBytes(overview.memory.heap_alloc_bytes) },
                  { label: t('ops.heapInUse'), value: formatIECBytes(overview.memory.heap_inuse_bytes ?? 0) },
                  { label: t('ops.heapReleased'), value: formatIECBytes(overview.memory.heap_released_bytes ?? 0) },
                  { label: t('ops.gcRuns'), value: (overview.memory.num_gc ?? 0).toLocaleString() },
                ].map((item) => (
                  <div key={item.label} className="rounded-lg bg-background/75 px-3 py-2 ring-1 ring-border/50">
                    <span className="block truncate text-[11px] text-muted-foreground">{item.label}</span>
                    <span className="mt-0.5 block font-sans text-[14px] font-semibold tracking-[-0.02em] text-foreground tabular-nums">
                      {item.value}
                    </span>
                  </div>
                ))}
              </div>
            </div>
          ) : null}
        </CardContent>
      </Card>

      {/* Right: Storage Dependencies & Rate Limit Watermark */}
      <Card className="flex h-full flex-col rounded-2xl border-border/75 bg-card py-0 shadow-2xs">
        <CardContent className="flex flex-1 flex-col gap-4 p-5">
          <div>
            <h3 className="flex items-center gap-2 text-[15px] font-semibold tracking-tight text-foreground">
              <Database className="size-4 text-primary" />
              {t('ops.storageCard.title')}
            </h3>
            <p className="mt-0.5 text-xs text-muted-foreground">{t('ops.storageCard.desc')}</p>
          </div>

          {/* Database Pool Row */}
          <div className="rounded-xl border border-border/65 bg-muted/20 p-3.5">
            <div className="flex items-center justify-between gap-2">
              <div className="flex items-center gap-2">
                <span
                  className={cn(
                    'flex size-7 items-center justify-center rounded-lg',
                    overview.postgres.healthy
                      ? 'bg-emerald-500/12 text-emerald-600 dark:text-emerald-400'
                      : 'bg-destructive/12 text-destructive',
                  )}
                >
                  <Database className="size-3.5" />
                </span>
                <div>
                  <span className="text-[13px] font-semibold text-foreground">
                    {overview.database_label || t('ops.postgres')}
                  </span>
                  <span className="ml-2 font-sans text-[11.5px] text-muted-foreground tabular-nums">
                    {overview.postgres.open} / {overview.postgres.max_open || '∞'}
                  </span>
                </div>
              </div>
              <div className="flex items-center gap-2">
                <span className="font-sans text-[13px] font-semibold text-foreground tabular-nums">
                  {overview.postgres.usage_percent.toFixed(1)}%
                </span>
                <span
                  className={cn(
                    'inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-[11px] font-medium',
                    overview.postgres.healthy
                      ? 'bg-emerald-500/12 text-emerald-700 dark:text-emerald-300'
                      : 'bg-destructive/12 text-destructive',
                  )}
                >
                  <span className={cn('size-1.5 rounded-full', overview.postgres.healthy ? 'bg-emerald-500' : 'bg-destructive')} />
                  {overview.postgres.healthy ? t('ops.storageCard.healthy') : t('ops.storageCard.unhealthy')}
                </span>
              </div>
            </div>

            {overview.database_driver !== 'sqlite' ? (
              <div className="mt-2.5 h-1.5 w-full overflow-hidden rounded-full bg-muted/80">
                <div
                  className={cn(
                    'h-full rounded-full transition-all duration-500',
                    !overview.postgres.healthy
                      ? 'bg-destructive'
                      : progressToneClass(overview.postgres.usage_percent, 75, 90),
                  )}
                  style={{ width: `${Math.min(100, Math.max(0, overview.postgres.usage_percent))}%` }}
                />
              </div>
            ) : null}

            <div className="mt-2.5 grid grid-cols-4 gap-1.5 text-center">
              {[
                { label: t('ops.storageCard.open'), value: overview.postgres.open.toLocaleString() },
                { label: t('ops.storageCard.inUse'), value: overview.postgres.in_use.toLocaleString() },
                { label: t('ops.storageCard.idle'), value: overview.postgres.idle.toLocaleString() },
                {
                  label: t('ops.storageCard.waitCount'),
                  value: overview.postgres.wait_count.toLocaleString(),
                  warn: overview.postgres.wait_count > 0,
                },
              ].map((cell) => (
                <div key={cell.label} className="rounded-lg bg-background/70 px-2 py-1.5 ring-1 ring-border/45">
                  <span className="block truncate text-[10.5px] text-muted-foreground">{cell.label}</span>
                  <span
                    className={cn(
                      'mt-0.5 block font-sans text-[12.5px] font-medium tabular-nums',
                      cell.warn ? 'text-amber-600 dark:text-amber-400' : 'text-foreground',
                    )}
                  >
                    {cell.value}
                  </span>
                </div>
              ))}
            </div>
          </div>

          {/* Cache Pool Row */}
          <div className="rounded-xl border border-border/65 bg-muted/20 p-3.5">
            <div className="flex items-center justify-between gap-2">
              <div className="flex items-center gap-2">
                <span
                  className={cn(
                    'flex size-7 items-center justify-center rounded-lg',
                    overview.redis.healthy
                      ? 'bg-emerald-500/12 text-emerald-600 dark:text-emerald-400'
                      : 'bg-destructive/12 text-destructive',
                  )}
                >
                  <Server className="size-3.5" />
                </span>
                <div>
                  <span className="text-[13px] font-semibold text-foreground">
                    {overview.cache_label || t('ops.redis')}
                  </span>
                  <span className="ml-2 font-sans text-[11.5px] text-muted-foreground tabular-nums">
                    {overview.redis.total_conns} / {overview.redis.pool_size || '-'}
                  </span>
                </div>
              </div>
              <div className="flex items-center gap-2">
                <span className="font-sans text-[13px] font-semibold text-foreground tabular-nums">
                  {overview.redis.usage_percent.toFixed(1)}%
                </span>
                <span
                  className={cn(
                    'inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-[11px] font-medium',
                    overview.redis.healthy
                      ? 'bg-emerald-500/12 text-emerald-700 dark:text-emerald-300'
                      : 'bg-destructive/12 text-destructive',
                  )}
                >
                  <span className={cn('size-1.5 rounded-full', overview.redis.healthy ? 'bg-emerald-500' : 'bg-destructive')} />
                  {overview.redis.healthy ? t('ops.storageCard.healthy') : t('ops.storageCard.unhealthy')}
                </span>
              </div>
            </div>

            {overview.cache_driver !== 'memory' ? (
              <div className="mt-2.5 h-1.5 w-full overflow-hidden rounded-full bg-muted/80">
                <div
                  className={cn(
                    'h-full rounded-full transition-all duration-500',
                    !overview.redis.healthy
                      ? 'bg-destructive'
                      : progressToneClass(overview.redis.usage_percent, 70, 90),
                  )}
                  style={{ width: `${Math.min(100, Math.max(0, overview.redis.usage_percent))}%` }}
                />
              </div>
            ) : null}

            <div className="mt-2.5 grid grid-cols-4 gap-1.5 text-center">
              {[
                { label: t('ops.storageCard.idle'), value: overview.redis.idle_conns.toLocaleString() },
                {
                  label: t('ops.storageCard.stale'),
                  value: overview.redis.stale_conns.toLocaleString(),
                  warn: overview.redis.stale_conns > 0,
                },
                {
                  label: t('ops.storageCard.timeouts'),
                  value: (overview.redis.timeouts ?? 0).toLocaleString(),
                  warn: (overview.redis.timeouts ?? 0) > 0,
                },
                {
                  label: t('ops.storageCard.pending'),
                  value: (overview.redis.pending_requests ?? 0).toLocaleString(),
                  warn: (overview.redis.pending_requests ?? 0) > 0,
                },
              ].map((cell) => (
                <div key={cell.label} className="rounded-lg bg-background/70 px-2 py-1.5 ring-1 ring-border/45">
                  <span className="block truncate text-[10.5px] text-muted-foreground">{cell.label}</span>
                  <span
                    className={cn(
                      'mt-0.5 block font-sans text-[12.5px] font-medium tabular-nums',
                      cell.warn ? 'text-amber-600 dark:text-amber-400' : 'text-foreground',
                    )}
                  >
                    {cell.value}
                  </span>
                </div>
              ))}
            </div>
          </div>

          {/* Live Throughput & Rate Limit */}
          <div className="mt-auto rounded-xl border border-border/60 bg-muted/15 px-3.5 py-3">
            <div className="flex items-center justify-between gap-2 text-xs">
              <span className="inline-flex items-center gap-1.5 font-medium text-muted-foreground">
                <Gauge className="size-3.5 text-primary" />
                {t('ops.storageCard.rateLimitTitle')}
              </span>
              <span className="font-sans text-[11.5px] text-muted-foreground tabular-nums">
                {t('ops.storageCard.todayTokensShort')}:{' '}
                <strong className="font-medium text-foreground">{formatCompact(overview.traffic.today_tokens)}</strong>
              </span>
            </div>
            <div className="mt-2 flex items-baseline justify-between gap-2">
              <span className="font-sans text-[13px] font-medium text-foreground tabular-nums">
                RPM {Math.round(overview.traffic.rpm).toLocaleString()}
                <span className="ml-1.5 text-xs font-normal text-muted-foreground">
                  {overview.traffic.rpm_limit > 0
                    ? `/ ${overview.traffic.rpm_limit.toLocaleString()}`
                    : `(${t('ops.rpmNoLimit')})`}
                </span>
              </span>
              <span className="font-sans text-[12px] text-muted-foreground tabular-nums">
                TPM {formatCompact(overview.traffic.tpm)}
              </span>
            </div>
            {rpmPct !== null ? (
              <div className="mt-2 h-1.5 w-full overflow-hidden rounded-full bg-muted/80">
                <div
                  className={cn('h-full rounded-full transition-all duration-500', progressToneClass(rpmPct, 80, 95))}
                  style={{ width: `${rpmPct}%` }}
                />
              </div>
            ) : null}
          </div>
        </CardContent>
      </Card>
    </div>
  )
}
