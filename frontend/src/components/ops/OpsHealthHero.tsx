import type { ReactNode } from 'react'
import { useId, useMemo } from 'react'
import { Link } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
import {
  Activity,
  AlertCircle,
  AlertTriangle,
  ArrowUpRight,
  CheckCircle2,
  Clock3,
  Gauge,
  ServerCog,
  ShieldAlert,
  Users,
  Workflow,
} from 'lucide-react'
import type { OpsOverviewResponse } from '../../types'
import { cn } from '@/lib/utils'

export interface OpsSamplePoint {
  ts: number
  cpu: number
  memory: number
  qps: number
  active: number
  goroutines: number
}

interface OpsHealthHeroProps {
  overview: OpsOverviewResponse
  samples: OpsSamplePoint[]
}

type CardTone = 'primary' | 'emerald' | 'violet' | 'amber' | 'rose'

const TONE_STYLES: Record<CardTone, { icon: string; stroke: string; fill: string; wash: string }> = {
  primary: {
    icon: 'bg-primary/10 text-primary ring-1 ring-primary/15',
    stroke: 'var(--color-primary)',
    fill: 'var(--color-primary)',
    wash: 'bg-[radial-gradient(ellipse_at_top_right,color-mix(in_oklab,var(--color-primary)_9%,transparent),transparent_68%)]',
  },
  emerald: {
    icon: 'bg-emerald-500/12 text-emerald-600 dark:text-emerald-400 ring-1 ring-emerald-500/20',
    stroke: '#10b981',
    fill: '#10b981',
    wash: 'bg-[radial-gradient(ellipse_at_top_right,rgba(16,185,129,0.09),transparent_68%)]',
  },
  violet: {
    icon: 'bg-violet-500/12 text-violet-600 dark:text-violet-400 ring-1 ring-violet-500/20',
    stroke: '#8b5cf6',
    fill: '#8b5cf6',
    wash: 'bg-[radial-gradient(ellipse_at_top_right,rgba(139,92,246,0.09),transparent_68%)]',
  },
  amber: {
    icon: 'bg-amber-500/12 text-amber-600 dark:text-amber-400 ring-1 ring-amber-500/20',
    stroke: '#f59e0b',
    fill: '#f59e0b',
    wash: 'bg-[radial-gradient(ellipse_at_top_right,rgba(245,158,11,0.10),transparent_68%)]',
  },
  rose: {
    icon: 'bg-rose-500/12 text-rose-600 dark:text-rose-400 ring-1 ring-rose-500/20',
    stroke: '#ef4444',
    fill: '#ef4444',
    wash: 'bg-[radial-gradient(ellipse_at_top_right,rgba(239,68,68,0.10),transparent_68%)]',
  },
}

function buildSmoothPath(coords: Array<{ x: number; y: number }>): string {
  if (coords.length === 0) return ''
  if (coords.length === 1) return `M${coords[0].x.toFixed(1)},${coords[0].y.toFixed(1)}`
  let d = `M${coords[0].x.toFixed(1)},${coords[0].y.toFixed(1)}`
  for (let i = 0; i < coords.length - 1; i += 1) {
    const p0 = coords[Math.max(0, i - 1)]
    const p1 = coords[i]
    const p2 = coords[i + 1]
    const p3 = coords[Math.min(coords.length - 1, i + 2)]
    const cp1x = p1.x + (p2.x - p0.x) / 6
    const cp1y = p1.y + (p2.y - p0.y) / 6
    const cp2x = p2.x - (p3.x - p1.x) / 6
    const cp2y = p2.y - (p3.y - p1.y) / 6
    d += ` C${cp1x.toFixed(1)},${cp1y.toFixed(1)} ${cp2x.toFixed(1)},${cp2y.toFixed(1)} ${p2.x.toFixed(1)},${p2.y.toFixed(1)}`
  }
  return d
}

export function OpsSparkline({ values, tone, width = 96, height = 40 }: { values: number[]; tone: CardTone; width?: number; height?: number }) {
  const gradId = useId()
  const geometry = useMemo(() => {
    const series = values.length >= 2 ? values.slice(-20) : values.length === 1 ? [values[0], values[0]] : []
    if (series.length < 2) return null
    const max = Math.max(...series)
    const min = Math.min(...series)
    const span = max - min
    const padX = 3
    const padY = 5
    const usableW = width - padX * 2
    const usableH = height - padY * 2
    const coords = series.map((val, idx) => {
      const x = padX + (idx / (series.length - 1)) * usableW
      const norm = span > 0 ? (val - min) / span : max > 0 ? 0.55 : 0.18
      const y = height - padY - norm * usableH
      return { x, y }
    })
    const linePath = buildSmoothPath(coords)
    const first = coords[0]
    const last = coords[coords.length - 1]
    const areaPath = `${linePath} L${last.x.toFixed(1)},${height} L${first.x.toFixed(1)},${height} Z`
    return { linePath, areaPath, last }
  }, [values, width, height])

  if (!geometry) return null
  const palette = TONE_STYLES[tone]

  return (
    <svg viewBox={`0 0 ${width} ${height}`} className="shrink-0 overflow-visible" style={{ width, height }} aria-hidden="true">
      <defs>
        <linearGradient id={gradId} x1="0" y1="0" x2="0" y2="1">
          <stop offset="0%" stopColor={palette.fill} stopOpacity="0.22" />
          <stop offset="85%" stopColor={palette.fill} stopOpacity="0.02" />
          <stop offset="100%" stopColor={palette.fill} stopOpacity="0" />
        </linearGradient>
      </defs>
      <path d={geometry.areaPath} fill={`url(#${gradId})`} />
      <path
        d={geometry.linePath}
        fill="none"
        stroke={palette.stroke}
        strokeWidth="2"
        strokeLinecap="round"
        strokeLinejoin="round"
      />
      <circle cx={geometry.last.x} cy={geometry.last.y} r="5" fill={palette.stroke} opacity="0.16" />
      <circle cx={geometry.last.x} cy={geometry.last.y} r="2.3" fill={palette.stroke} />
    </svg>
  )
}

export function OpsRingGauge({
  percent,
  hasData = true,
  size = 46,
  invertSeverity = false,
}: {
  percent: number
  hasData?: boolean
  size?: number
  invertSeverity?: boolean
}) {
  const stroke = 4.2
  const radius = (size - stroke) / 2
  const circumference = 2 * Math.PI * radius
  const clamped = hasData ? Math.max(0, Math.min(100, percent)) : 0
  const dashOffset = circumference - (clamped / 100) * circumference

  let strokeColor = 'var(--color-muted-foreground)'
  if (hasData) {
    if (invertSeverity) {
      strokeColor = clamped >= 90 ? '#ef4444' : clamped >= 75 ? '#f59e0b' : '#10b981'
    } else {
      strokeColor = clamped >= 80 ? '#10b981' : clamped >= 50 ? '#f59e0b' : '#ef4444'
    }
  }

  return (
    <div className="relative flex shrink-0 items-center justify-center" style={{ width: size, height: size }}>
      <svg width={size} height={size} viewBox={`0 0 ${size} ${size}`} className="-rotate-90" aria-hidden="true">
        <circle
          cx={size / 2}
          cy={size / 2}
          r={radius}
          fill="none"
          stroke="currentColor"
          strokeWidth={stroke}
          className="text-muted/80"
        />
        <circle
          cx={size / 2}
          cy={size / 2}
          r={radius}
          fill="none"
          stroke={strokeColor}
          strokeWidth={stroke}
          strokeLinecap="round"
          strokeDasharray={circumference}
          strokeDashoffset={dashOffset}
          className="transition-[stroke-dashoffset] duration-500 ease-out"
        />
      </svg>
      <span className="absolute font-sans text-[10.5px] font-semibold tracking-tight text-foreground tabular-nums">
        {hasData ? `${Math.round(clamped)}%` : '--'}
      </span>
    </div>
  )
}

function HeroKpiCard({
  icon,
  tone,
  label,
  value,
  suffix,
  visual,
  leftDetail,
  rightDetail,
}: {
  icon: ReactNode
  tone: CardTone
  label: string
  value: string
  suffix?: string
  visual?: ReactNode
  leftDetail: { label: string; value: string; tone?: 'default' | 'danger' | 'warn' | 'success' }
  rightDetail: { label: string; value: string; tone?: 'default' | 'danger' | 'warn' | 'success' }
}) {
  const styles = TONE_STYLES[tone]

  return (
    <article className="group relative flex min-w-0 flex-col justify-between overflow-hidden rounded-2xl border border-border/75 bg-card p-5 shadow-2xs transition-all duration-200 hover:border-border hover:shadow-xs">
      <div aria-hidden="true" className={cn('pointer-events-none absolute inset-0 opacity-90', styles.wash)} />

      <div className="relative z-10 flex items-center justify-between gap-2">
        <span className="truncate text-xs font-medium text-muted-foreground" title={label}>
          {label}
        </span>
        <span className={cn('flex size-8 shrink-0 items-center justify-center rounded-lg', styles.icon)} aria-hidden="true">
          {icon}
        </span>
      </div>

      <div className="relative z-10 mt-3 flex items-end justify-between gap-3">
        <div className="min-w-0">
          <div className="flex items-baseline gap-1">
            <strong
              className="truncate font-sans text-[27px] leading-none font-semibold tracking-[-0.03em] text-foreground tabular-nums sm:text-[30px]"
              title={value}
            >
              {value}
            </strong>
            {suffix ? (
              <span className="font-sans text-[14.5px] font-normal text-muted-foreground/70 tabular-nums">
                {suffix}
              </span>
            ) : null}
          </div>
        </div>
        {visual}
      </div>

      <div className="relative z-10 mt-4 grid grid-cols-2 gap-3 border-t border-border/55 pt-3">
        {[leftDetail, rightDetail].map((detail, index) => (
          <div key={detail.label} className={cn('min-w-0', index === 1 && 'border-l border-border/55 pl-3')}>
            <span className="block truncate text-[11px] text-muted-foreground">{detail.label}</span>
            <span
              className={cn(
                'mt-0.5 block truncate font-sans text-[13px] font-medium tracking-[-0.01em] tabular-nums',
                detail.tone === 'danger'
                  ? 'text-destructive'
                  : detail.tone === 'warn'
                    ? 'text-amber-600 dark:text-amber-400'
                    : detail.tone === 'success'
                      ? 'text-emerald-600 dark:text-emerald-400'
                      : 'text-foreground',
              )}
              title={detail.value}
            >
              {detail.value}
            </span>
          </div>
        ))}
      </div>
    </article>
  )
}

function formatDurationMs(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return '-'
  if (ms >= 1000) return `${(ms / 1000).toFixed(1)}s`
  return `${Math.round(ms)}ms`
}

function formatCompact(n: number): string {
  if (!Number.isFinite(n)) return '0'
  if (Math.abs(n) >= 1_000_000) return `${(n / 1_000_000).toFixed(1)}M`
  if (Math.abs(n) >= 10_000) return `${(n / 1_000).toFixed(1)}k`
  return Math.round(n).toLocaleString()
}

function formatUptime(seconds: number, t: (key: string) => string): string {
  if (seconds <= 0) return t('ops.justStarted')
  const days = Math.floor(seconds / 86400)
  const hours = Math.floor((seconds % 86400) / 3600)
  const minutes = Math.floor((seconds % 3600) / 60)
  if (days > 0) return `${days}${t('ops.days')} ${hours}${t('ops.hours')}`
  if (hours > 0) return `${hours}${t('ops.hours')} ${minutes}${t('ops.minutes')}`
  return `${minutes}${t('ops.minutes')}`
}

export default function OpsHealthHero({ overview, samples }: OpsHealthHeroProps) {
  const { t } = useTranslation()

  const diagnostics = useMemo(() => {
    const critical: string[] = []
    const warnings: string[] = []

    if (!overview.postgres.healthy) critical.push(t('ops.healthBanner.issueDbDown'))
    if (!overview.redis.healthy) critical.push(t('ops.healthBanner.issueCacheDown'))
    if (overview.cpu.percent >= 90) {
      critical.push(t('ops.healthBanner.issueCpuHigh', { percent: overview.cpu.percent.toFixed(1) }))
    } else if (overview.cpu.percent >= 75) {
      warnings.push(t('ops.healthBanner.issueCpuHigh', { percent: overview.cpu.percent.toFixed(1) }))
    }
    if (overview.memory.percent >= 90) {
      critical.push(t('ops.healthBanner.issueMemHigh', { percent: overview.memory.percent.toFixed(1) }))
    } else if (overview.memory.percent >= 78) {
      warnings.push(t('ops.healthBanner.issueMemHigh', { percent: overview.memory.percent.toFixed(1) }))
    }
    if (overview.traffic.error_rate >= 5) {
      warnings.push(t('ops.healthBanner.issueHighErrorRate', { rate: overview.traffic.error_rate.toFixed(1) }))
    }
    if (overview.runtime.total_accounts > 0 && overview.runtime.available_accounts === 0) {
      warnings.push(t('ops.healthBanner.issuePoolEmpty'))
    }
    if (overview.scheduler) {
      if (overview.scheduler.outbox_errors > 0) {
        critical.push(t('ops.healthBanner.issueOutboxErrors', { count: overview.scheduler.outbox_errors }))
      } else if (overview.scheduler.outbox_backlog > 1000 || overview.scheduler.outbox_lag_ms > 5000) {
        warnings.push(
          t('ops.healthBanner.issueOutboxLag', {
            count: overview.scheduler.outbox_backlog,
            ms: overview.scheduler.outbox_lag_ms,
          }),
        )
      }
    }
    if (overview.request_memory && overview.request_memory.rejections > 0) {
      warnings.push(t('ops.healthBanner.issueReqMemRejected', { count: overview.request_memory.rejections }))
    }
    if (overview.api_key_auth_cache && overview.api_key_auth_cache.errors > 0) {
      warnings.push(t('ops.healthBanner.issueAuthCacheErrors', { count: overview.api_key_auth_cache.errors }))
    }

    const level: 'healthy' | 'warning' | 'danger' =
      critical.length > 0 ? 'danger' : warnings.length > 0 ? 'warning' : 'healthy'
    return { level, issues: [...critical, ...warnings] }
  }, [overview, t])

  const poolPercent =
    overview.runtime.total_accounts > 0
      ? (overview.runtime.available_accounts / overview.runtime.total_accounts) * 100
      : 0

  const qpsSeries = useMemo(() => samples.map((s) => s.qps), [samples])
  const errorTone: CardTone =
    overview.traffic.error_rate >= 5 ? 'rose' : overview.traffic.error_rate > 0 ? 'amber' : 'emerald'

  return (
    <div className="space-y-4">
      {/* System Health Verdict & Quick Navigation Strip */}
      <div
        className={cn(
          'flex flex-wrap items-center justify-between gap-3 rounded-2xl border px-4 py-3 shadow-2xs transition-colors',
          diagnostics.level === 'danger'
            ? 'border-destructive/30 bg-destructive/[0.06]'
            : diagnostics.level === 'warning'
              ? 'border-amber-500/30 bg-amber-500/[0.06]'
              : 'border-emerald-500/25 bg-emerald-500/[0.05]',
        )}
      >
        <div className="flex min-w-0 items-center gap-3">
          <div
            className={cn(
              'flex size-9 shrink-0 items-center justify-center rounded-xl',
              diagnostics.level === 'danger'
                ? 'bg-destructive/15 text-destructive'
                : diagnostics.level === 'warning'
                  ? 'bg-amber-500/15 text-amber-600 dark:text-amber-400'
                  : 'bg-emerald-500/15 text-emerald-600 dark:text-emerald-400',
            )}
          >
            {diagnostics.level === 'danger' ? (
              <ShieldAlert className="size-4.5" />
            ) : diagnostics.level === 'warning' ? (
              <AlertTriangle className="size-4.5" />
            ) : (
              <CheckCircle2 className="size-4.5" />
            )}
          </div>
          <div className="min-w-0">
            <div className="flex flex-wrap items-center gap-2">
              <span className="text-[13.5px] font-semibold text-foreground">
                {diagnostics.level === 'danger'
                  ? t('ops.healthBanner.danger', { count: diagnostics.issues.length })
                  : diagnostics.level === 'warning'
                    ? t('ops.healthBanner.warning', { count: diagnostics.issues.length })
                    : t('ops.healthBanner.healthy')}
              </span>
              <span className="rounded-md border border-border/60 bg-background/75 px-1.5 py-0.5 font-sans text-[11px] font-medium text-muted-foreground">
                {overview.database_label || overview.database_driver} · {overview.cache_label || overview.cache_driver}
              </span>
            </div>
            <p className="mt-0.5 truncate text-xs text-muted-foreground">
              {diagnostics.issues.length > 0
                ? diagnostics.issues.join(' · ')
                : t('ops.healthBanner.healthyDesc')}
            </p>
          </div>
        </div>

        <div className="flex flex-wrap items-center gap-1.5">
          <Link
            to="/ops/runtime"
            className="inline-flex items-center gap-1 rounded-lg border border-border/70 bg-background/80 px-2.5 py-1.5 text-xs font-medium text-foreground transition-colors hover:border-border hover:bg-background"
          >
            <ServerCog className="size-3.5 text-muted-foreground" />
            {t('ops.healthBanner.quickRuntime')}
            <ArrowUpRight className="size-3 text-muted-foreground" />
          </Link>
          <Link
            to="/ops/errors"
            className="inline-flex items-center gap-1 rounded-lg border border-border/70 bg-background/80 px-2.5 py-1.5 text-xs font-medium text-foreground transition-colors hover:border-border hover:bg-background"
          >
            <AlertCircle className="size-3.5 text-muted-foreground" />
            {t('ops.healthBanner.quickErrors')}
            <ArrowUpRight className="size-3 text-muted-foreground" />
          </Link>
          <Link
            to="/ops/scheduler"
            className="inline-flex items-center gap-1 rounded-lg border border-border/70 bg-background/80 px-2.5 py-1.5 text-xs font-medium text-foreground transition-colors hover:border-border hover:bg-background"
          >
            <Workflow className="size-3.5 text-muted-foreground" />
            {t('ops.healthBanner.quickScheduler')}
            <ArrowUpRight className="size-3 text-muted-foreground" />
          </Link>
        </div>
      </div>

      {/* 4 Studio Hero KPI Cards */}
      <section className="grid grid-cols-1 gap-3.5 sm:grid-cols-2 xl:grid-cols-4">
        <HeroKpiCard
          icon={<Clock3 className="size-4" />}
          tone="emerald"
          label={t('ops.uptime')}
          value={formatUptime(overview.uptime_seconds, t)}
          leftDetail={{
            label: t('ops.activeRequests'),
            value: overview.requests.active.toLocaleString(),
            tone: overview.requests.active >= Math.max(50, overview.runtime.total_accounts * 0.5) ? 'warn' : 'default',
          }}
          rightDetail={{
            label: t('ops.goroutines'),
            value: overview.runtime.goroutines.toLocaleString(),
            tone:
              overview.runtime.goroutines >= Math.max(1000, overview.runtime.total_accounts * 3)
                ? 'danger'
                : overview.runtime.goroutines >= Math.max(500, overview.runtime.total_accounts * 1.5)
                  ? 'warn'
                  : 'default',
          }}
        />

        <HeroKpiCard
          icon={<Users className="size-4" />}
          tone={overview.runtime.total_accounts > 0 && overview.runtime.available_accounts === 0 ? 'amber' : 'primary'}
          label={t('ops.accountPool')}
          value={String(overview.runtime.available_accounts)}
          suffix={`/${overview.runtime.total_accounts}`}
          visual={<OpsRingGauge percent={poolPercent} hasData={overview.runtime.total_accounts > 0} />}
          leftDetail={{
            label: t('ops.todayRequests'),
            value: overview.traffic.today_requests.toLocaleString(),
          }}
          rightDetail={{
            label: t('ops.runtimeTotalShort'),
            value: formatCompact(overview.requests.total),
          }}
        />

        <HeroKpiCard
          icon={<Gauge className="size-4" />}
          tone="violet"
          label={t('ops.qps')}
          value={overview.traffic.qps.toFixed(1)}
          visual={<OpsSparkline values={qpsSeries} tone="violet" />}
          leftDetail={{
            label: t('ops.qpsPeak', { value: '' }).trim(),
            value: overview.traffic.qps_peak.toFixed(1),
          }}
          rightDetail={{
            label: 'TPS / Peak',
            value: `${formatCompact(overview.traffic.tps)} · ${formatCompact(overview.traffic.tps_peak)}`,
          }}
        />

        <HeroKpiCard
          icon={<Activity className="size-4" />}
          tone={errorTone}
          label={t('ops.todayErrorRate')}
          value={`${overview.traffic.error_rate.toFixed(1)}%`}
          leftDetail={{
            label: t('ops.avgDuration'),
            value: formatDurationMs(overview.traffic.avg_duration_ms),
          }}
          rightDetail={{
            label: 'RPM / TPM',
            value: `${formatCompact(overview.traffic.rpm)} · ${formatCompact(overview.traffic.tpm)}`,
          }}
        />
      </section>
    </div>
  )
}
