import { useTranslation } from 'react-i18next'
import { KeyRound, Layers, ShieldCheck } from 'lucide-react'
import { Card, CardContent } from '@/components/ui/card'
import type { OpsOverviewResponse } from '../../types'
import { formatIECBytes } from '../../lib/responseCacheMetrics'
import { cn } from '@/lib/utils'

interface OpsGuardsDeckProps {
  overview: OpsOverviewResponse
}

function barTone(pct: number, warn = 75, danger = 90) {
  if (pct >= danger) return 'bg-destructive'
  if (pct >= warn) return 'bg-amber-500'
  return 'bg-primary'
}

export default function OpsGuardsDeck({ overview }: OpsGuardsDeckProps) {
  const { t } = useTranslation()
  const authCache = overview.api_key_auth_cache
  const reqMem = overview.request_memory
  const writer = overview.response_cache_writer

  if (!authCache && !reqMem && !writer) return null

  const authTotalLookups = authCache ? authCache.local_hits + authCache.remote_hits + authCache.misses : 0
  const authHitRate =
    authCache && authTotalLookups > 0
      ? (((authCache.local_hits + authCache.remote_hits) / authTotalLookups) * 100).toFixed(1)
      : null
  const authBytesPct =
    authCache && authCache.max_bytes > 0 ? Math.min(100, (authCache.bytes / authCache.max_bytes) * 100) : 0

  const reqMemPct =
    reqMem && reqMem.limit_bytes > 0 ? Math.min(100, (reqMem.used_bytes / reqMem.limit_bytes) * 100) : 0

  const writerActivePct =
    writer && writer.max_active_writes > 0 ? Math.min(100, (writer.active_writes / writer.max_active_writes) * 100) : 0
  const writerBytesPct =
    writer && writer.max_logical_bytes > 0
      ? Math.min(100, (writer.inflight_logical_bytes / writer.max_logical_bytes) * 100)
      : 0

  return (
    <section className="space-y-3">
      <div className="flex items-center gap-2 text-sm font-semibold text-foreground">
        <ShieldCheck className="size-4 text-primary" />
        <span>{t('ops.guardsSection')}</span>
      </div>

      <div className="grid grid-cols-1 gap-4 lg:grid-cols-3">
        {/* 1. API Key Auth Cache */}
        {authCache ? (
          <Card className="flex h-full flex-col rounded-2xl border-border/75 bg-card py-0 shadow-2xs">
            <CardContent className="flex flex-1 flex-col gap-3.5 p-5">
              <div className="flex items-start justify-between gap-2">
                <div className="min-w-0">
                  <h4 className="flex items-center gap-2 text-[14px] font-semibold text-foreground">
                    <span className="flex size-7 shrink-0 items-center justify-center rounded-lg bg-violet-500/12 text-violet-600 dark:text-violet-400">
                      <KeyRound className="size-3.5" />
                    </span>
                    <span className="truncate">{t('ops.authCache.title')}</span>
                  </h4>
                  <p className="mt-1 text-xs text-muted-foreground">{t('ops.authCache.desc')}</p>
                </div>
                <span
                  className={cn(
                    'shrink-0 rounded-full px-2 py-0.5 text-[11px] font-medium',
                    !authCache.enabled
                      ? 'bg-muted text-muted-foreground'
                      : authCache.errors > 0
                        ? 'bg-amber-500/12 text-amber-700 dark:text-amber-300'
                        : 'bg-emerald-500/12 text-emerald-700 dark:text-emerald-300',
                  )}
                >
                  {!authCache.enabled
                    ? t('ops.authCache.disabled')
                    : authCache.shared
                      ? t('ops.authCache.enabledShared')
                      : t('ops.authCache.enabledLocal')}
                </span>
              </div>

              <div className="flex items-baseline justify-between gap-2">
                <div>
                  <span className="block text-[11px] text-muted-foreground">{t('ops.authCache.hitRate')}</span>
                  <strong
                    className={cn(
                      'font-sans text-[26px] leading-tight font-semibold tracking-[-0.03em] tabular-nums',
                      authHitRate !== null ? 'text-foreground' : 'text-muted-foreground/55',
                    )}
                  >
                    {authHitRate !== null ? `${authHitRate}%` : '--'}
                  </strong>
                </div>
                <div className="text-right">
                  <span className="block text-[11px] text-muted-foreground">{t('ops.authCache.entries')}</span>
                  <span className="font-sans text-[13.5px] font-medium text-foreground tabular-nums">
                    {authCache.entries.toLocaleString()}
                    <span className="text-muted-foreground/75">
                      {authCache.max_entries > 0 ? ` / ${authCache.max_entries.toLocaleString()}` : ''}
                    </span>
                  </span>
                </div>
              </div>

              <div>
                <div className="flex items-center justify-between text-[11px] text-muted-foreground">
                  <span>{t('ops.authCache.bytes')}</span>
                  <span className="font-sans tabular-nums text-foreground/90">
                    {formatIECBytes(authCache.bytes)}
                    {authCache.max_bytes > 0 ? ` / ${formatIECBytes(authCache.max_bytes)}` : ''}
                  </span>
                </div>
                <div className="mt-1.5 h-1.5 w-full overflow-hidden rounded-full bg-muted/80">
                  <div
                    className={cn('h-full rounded-full transition-all duration-500', barTone(authBytesPct))}
                    style={{ width: `${authBytesPct}%` }}
                  />
                </div>
              </div>

              <div className="mt-auto grid grid-cols-2 gap-2 border-t border-border/55 pt-3">
                <MiniKv
                  label={t('ops.authCache.localRemote')}
                  value={`${authCache.local_hits.toLocaleString()} / ${authCache.remote_hits.toLocaleString()}`}
                />
                <MiniKv
                  label={t('ops.authCache.missNegative')}
                  value={`${authCache.misses.toLocaleString()} / ${authCache.negative_hits.toLocaleString()}`}
                />
                <MiniKv
                  label={t('ops.authCache.dbLoads')}
                  value={`${authCache.db_loads.toLocaleString()} / ${authCache.invalidations.toLocaleString()}`}
                />
                <MiniKv
                  label={t('ops.authCache.evictErrors')}
                  value={`${authCache.evictions.toLocaleString()} / ${authCache.errors.toLocaleString()}`}
                  tone={authCache.errors > 0 ? 'danger' : 'default'}
                />
              </div>
            </CardContent>
          </Card>
        ) : null}

        {/* 2. Request Memory Guard */}
        {reqMem ? (
          <Card className="flex h-full flex-col rounded-2xl border-border/75 bg-card py-0 shadow-2xs">
            <CardContent className="flex flex-1 flex-col gap-3.5 p-5">
              <div className="flex items-start justify-between gap-2">
                <div className="min-w-0">
                  <h4 className="flex items-center gap-2 text-[14px] font-semibold text-foreground">
                    <span className="flex size-7 shrink-0 items-center justify-center rounded-lg bg-emerald-500/12 text-emerald-600 dark:text-emerald-400">
                      <ShieldCheck className="size-3.5" />
                    </span>
                    <span className="truncate">{t('ops.requestMemory.title')}</span>
                  </h4>
                  <p className="mt-1 text-xs text-muted-foreground">{t('ops.requestMemory.desc')}</p>
                </div>
                <span
                  className={cn(
                    'shrink-0 rounded-full px-2 py-0.5 font-sans text-[11px] font-medium tabular-nums',
                    reqMem.rejections > 0
                      ? 'bg-destructive/12 text-destructive'
                      : 'bg-emerald-500/12 text-emerald-700 dark:text-emerald-300',
                  )}
                >
                  {reqMemPct.toFixed(1)}%
                </span>
              </div>

              <div className="flex items-baseline justify-between gap-2">
                <div>
                  <span className="block text-[11px] text-muted-foreground">{t('ops.requestMemory.budget')}</span>
                  <strong className="font-sans text-[24px] leading-tight font-semibold tracking-[-0.03em] text-foreground tabular-nums">
                    {formatIECBytes(reqMem.used_bytes)}
                  </strong>
                  <span className="ml-1 font-sans text-xs text-muted-foreground tabular-nums">
                    / {reqMem.limit_bytes > 0 ? formatIECBytes(reqMem.limit_bytes) : t('ops.requestMemory.unlimited')}
                  </span>
                </div>
              </div>

              <div className="h-1.5 w-full overflow-hidden rounded-full bg-muted/80">
                <div
                  className={cn('h-full rounded-full transition-all duration-500', barTone(reqMemPct, 70, 90))}
                  style={{ width: `${reqMemPct}%` }}
                />
              </div>

              <div className="mt-auto grid grid-cols-2 gap-2 border-t border-border/55 pt-3">
                <MiniKv label={t('ops.requestMemory.highWater')} value={formatIECBytes(reqMem.high_water_bytes)} />
                <MiniKv label={t('ops.requestMemory.active')} value={reqMem.active_reservations.toLocaleString()} />
                <div className="col-span-2">
                  <MiniKv
                    label={t('ops.requestMemory.rejections')}
                    value={reqMem.rejections.toLocaleString()}
                    tone={reqMem.rejections > 0 ? 'danger' : 'success'}
                  />
                </div>
              </div>
            </CardContent>
          </Card>
        ) : null}

        {/* 3. Response Cache Async Writer */}
        {writer ? (
          <Card className="flex h-full flex-col rounded-2xl border-border/75 bg-card py-0 shadow-2xs">
            <CardContent className="flex flex-1 flex-col gap-3.5 p-5">
              <div className="flex items-start justify-between gap-2">
                <div className="min-w-0">
                  <h4 className="flex items-center gap-2 text-[14px] font-semibold text-foreground">
                    <span className="flex size-7 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-primary">
                      <Layers className="size-3.5" />
                    </span>
                    <span className="truncate">{t('ops.cacheWriter.title')}</span>
                  </h4>
                  <p className="mt-1 text-xs text-muted-foreground">{t('ops.cacheWriter.desc')}</p>
                </div>
                {writer.oversize_active ? (
                  <span className="shrink-0 rounded-full bg-amber-500/12 px-2 py-0.5 text-[11px] font-medium text-amber-700 dark:text-amber-300">
                    {t('ops.cacheWriter.oversizeActive')}
                  </span>
                ) : null}
              </div>

              <div className="space-y-2.5">
                <div>
                  <div className="flex items-center justify-between text-[11.5px]">
                    <span className="text-muted-foreground">{t('ops.cacheWriter.active')}</span>
                    <span className="font-sans font-medium text-foreground tabular-nums">
                      {writer.active_writes.toLocaleString()} / {writer.max_active_writes || '∞'}
                    </span>
                  </div>
                  <div className="mt-1 h-1.5 w-full overflow-hidden rounded-full bg-muted/80">
                    <div
                      className={cn('h-full rounded-full transition-all duration-500', barTone(writerActivePct))}
                      style={{ width: `${writerActivePct}%` }}
                    />
                  </div>
                </div>

                <div>
                  <div className="flex items-center justify-between text-[11.5px]">
                    <span className="text-muted-foreground">{t('ops.cacheWriter.inflightBytes')}</span>
                    <span className="font-sans font-medium text-foreground tabular-nums">
                      {formatIECBytes(writer.inflight_logical_bytes)} / {formatIECBytes(writer.max_logical_bytes)}
                    </span>
                  </div>
                  <div className="mt-1 h-1.5 w-full overflow-hidden rounded-full bg-muted/80">
                    <div
                      className={cn('h-full rounded-full transition-all duration-500', barTone(writerBytesPct))}
                      style={{ width: `${writerBytesPct}%` }}
                    />
                  </div>
                </div>
              </div>

              <div className="mt-auto grid grid-cols-2 gap-2 border-t border-border/55 pt-3">
                <MiniKv
                  label={t('ops.cacheWriter.waiting')}
                  value={`${writer.waiting_writes.toLocaleString()} · ${formatIECBytes(writer.waiting_logical_bytes)}`}
                  tone={writer.waiting_writes > 0 ? 'warn' : 'default'}
                />
                <MiniKv
                  label={t('ops.cacheWriter.highWater')}
                  value={`${writer.high_water_active_writes} · ${formatIECBytes(writer.high_water_logical_bytes)}`}
                />
                <div className="col-span-2">
                  <MiniKv
                    label={t('ops.cacheWriter.rejectTimeout')}
                    value={`${writer.queue_rejections.toLocaleString()} / ${writer.wait_timeouts.toLocaleString()}`}
                    tone={writer.queue_rejections > 0 || writer.wait_timeouts > 0 ? 'danger' : 'success'}
                  />
                </div>
              </div>
            </CardContent>
          </Card>
        ) : null}
      </div>
    </section>
  )
}

function MiniKv({
  label,
  value,
  tone = 'default',
}: {
  label: string
  value: string
  tone?: 'default' | 'warn' | 'danger' | 'success'
}) {
  return (
    <div className="min-w-0 rounded-lg bg-muted/30 px-2.5 py-2">
      <span className="block truncate text-[10.5px] text-muted-foreground">{label}</span>
      <span
        className={cn(
          'mt-0.5 block truncate font-sans text-[12.5px] font-medium tabular-nums',
          tone === 'danger'
            ? 'text-destructive'
            : tone === 'warn'
              ? 'text-amber-600 dark:text-amber-400'
              : tone === 'success'
                ? 'text-emerald-600 dark:text-emerald-400'
                : 'text-foreground',
        )}
        title={value}
      >
        {value}
      </span>
    </div>
  )
}
