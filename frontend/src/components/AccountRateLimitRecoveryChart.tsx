import { useEffect, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import {
  Bar,
  CartesianGrid,
  Cell,
  ComposedChart,
  Line,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts'
import { CircleHelp, TimerReset } from 'lucide-react'
import { Card, CardContent } from '@/components/ui/card'
import { Tooltip as UITooltip, TooltipContent, TooltipProvider, TooltipTrigger } from '@/components/ui/tooltip'
import type { AccountRow } from '../types'
import { formatBeijingTime } from '../utils/time'

interface AccountRateLimitRecoveryChartProps {
  accounts: AccountRow[]
  currentRpm?: number
  rpmLimit?: number
  avgDurationMs?: number
  className?: string
  compact?: boolean
}

interface RecoveryCandidate {
  id: number
  label: string
  recoveryAt: number
  secondsUntil: number
  reason: RecoveryReason
}

type RecoveryReason = '5h' | '7d' | 'cooldown'
type RecoveryWindow = '5h' | '7d'
type RecoveryViewMode = 'recovery' | 'reset'

interface RecoveryGroup {
  key: string
  startAt: number
  endAt: number
  label: string
  fullLabel: string
  count: number
  fill: string
}

interface PressureForecast {
  sampled: number
  threshold: number
  predictedAt: number | null
  predictedCount: number
  unknown: number
  rpm: number
  effectiveRpmLimit: number
  rpmPressure: number | null
  activePressure: number
  rateLimitPressure: number
  dispatchableAccounts: number
  avgConcurrency: number
  highPressureAt: number | null
  supplyShortageAt: number | null
  riskLevel: 'low' | 'medium' | 'high'
  confidence: number
}

interface ResetCandidate {
  id: number
  label: string
  resetAt: number
}

interface ResetStats {
  candidates: ResetCandidate[]
  points: RecoveryGroup[]
  total: number
  unknown: number
}

interface SupplyEvent {
  at: number
  concurrency: number
  delta: 1 | -1
  // True when this +1 replenishment pairs with a prior -1 from the same
  // account. Unpaired +1 events (currently rate-limited accounts that recover
  // at their reset_at) restore capacity but shouldn't decrement the
  // "simultaneously exhausted" count used for bulk-limit detection.
  paired?: boolean
}

interface SupplyPressurePoint {
  highPressureAt: number | null
  supplyShortageAt: number | null
}

const recoveryWindows: RecoveryWindow[] = ['5h', '7d']
const recoveryViewModes: RecoveryViewMode[] = ['recovery', 'reset']
const recoveryReasonFill: Record<RecoveryReason, string> = {
  '5h': 'var(--color-primary)',
  '7d': 'hsl(30 82% 44%)',
  cooldown: 'hsl(var(--info))',
}

const chartMargin = { top: 8, right: 8, left: -18, bottom: 0 }
const resetChartMargin = { top: 8, right: 8, left: -4, bottom: 0 }
const gridColor = 'var(--color-border)'
const axisColor = 'var(--color-muted-foreground)'
const tooltipContentStyle = {
  backgroundColor: 'var(--color-card)',
  border: '1px solid var(--color-border)',
  borderRadius: '12px',
  boxShadow: '0 18px 40px rgba(0, 0, 0, 0.12)',
}
const tooltipLabelStyle = { color: 'var(--color-foreground)', fontWeight: 600 }
const tooltipItemStyle = { color: 'var(--color-foreground)' }
// Default throughput per concurrency slot when no avg_duration_ms is provided
// (6 rpm ≈ 10s avg request duration). At runtime we adapt this to actual
// avg_duration_ms via getRpmPerSlot — see usage sites below.
const RPM_PER_CONCURRENCY_SLOT_DEFAULT = 6
// Adaptive bounds: avg_duration_ms outside [2s, 60s] is treated as noisy and
// clamped, so the slot rate stays in [1, 30] rpm/slot.
const RPM_PER_CONCURRENCY_SLOT_MIN = 1
const RPM_PER_CONCURRENCY_SLOT_MAX = 30
// Fraction of dispatchable accounts with a 429 in the recent window that we
// consider "fully saturated" (=> pressure 1). Replaces the prior lifetime
// rate_limit_attempts/total_requests ratio which never moved for old accounts.
const RATE_LIMIT_SATURATION_FRACTION = 0.3
// Treat a 429 as "currently active" only if it happened within this window.
const RECENT_RATE_LIMIT_WINDOW_MS = 60 * 60_000
// Bulk-limit threshold: how many sampled accounts must be projected to exhaust
// before we treat it as a pool-wide event. min 3, otherwise 30% of sample.
const BULK_LIMIT_RATIO = 0.3
const BULK_LIMIT_MIN_COUNT = 3
// Pressure factor: 1.0 = no acceleration, capped at 2.5 (= predicted time / 2.5).
const PRESSURE_FACTOR_MAX = 2.5
// Boost weights for the dominant pressure axis vs the secondary one. Switching
// from sum-of-all-three (could double-count correlated signals like rpm/active)
// to "dominant + 0.5 × secondMax" gives more stable acceleration.
const PRESSURE_BOOST_DOMINANT = 1.0
const PRESSURE_BOOST_SECONDARY = 0.5
// Threshold above which an axis starts contributing to acceleration.
const PRESSURE_THRESHOLD_RPM = 0.75
const PRESSURE_THRESHOLD_ACTIVE = 0.75
// Confidence floor: predictions made with <40% known samples are downgraded.
const LOW_CONFIDENCE_THRESHOLD = 0.4
// Minimum elapsed-window ratio before we trust a single account's burn
// extrapolation. Below this, a freshly-rotated account at usage=1% can blow up
// burn rate (because elapsed is tiny) and predict exhaustion within minutes.
const BURN_MIN_ELAPSED_RATIO = 0.05
// Fraction of the active window that counts as "soon" for risk-escalation.
// 20% is the same for 5h (→ 1h) and 7d (→ 33.6h); the previous values were
// inconsistent (5h:1h=20% vs 7d:24h=14%) leaving 7d under-warned.
const SOON_WINDOW_RATIO = 0.2

export default function AccountRateLimitRecoveryChart({ accounts, currentRpm = 0, rpmLimit = 0, avgDurationMs = 0, className = '', compact = false }: AccountRateLimitRecoveryChartProps) {
  const { t } = useTranslation()
  const [nowMs, setNowMs] = useState(() => Date.now())
  const [windowKey, setWindowKey] = useState<RecoveryWindow>('5h')
  const [viewMode, setViewMode] = useState<RecoveryViewMode>('recovery')

  useEffect(() => {
    const timer = window.setInterval(() => setNowMs(Date.now()), 60_000)
    return () => window.clearInterval(timer)
  }, [])

  const recovery = useMemo(() => {
    const candidates: RecoveryCandidate[] = []
    let unknown = 0

    for (const account of accounts) {
      const candidate = getAccountRecoveryCandidate(account, nowMs, windowKey)
      if (candidate) {
        candidates.push(candidate)
      } else if (isWindowRateLimitLike(account, windowKey)) {
        unknown += 1
      }
    }

    candidates.sort((a, b) => a.recoveryAt - b.recoveryAt)

    return {
      candidates,
      points: createRecoveryPoints(candidates, windowKey, nowMs),
      unknown,
      forecast: estimatePressureForecast(accounts, windowKey, nowMs, currentRpm, rpmLimit, avgDurationMs),
    }
  }, [accounts, avgDurationMs, currentRpm, nowMs, rpmLimit, windowKey])

  const resetStats = useMemo(() => createResetStats(accounts, nowMs), [accounts, nowMs])
  const limitedTotal = recovery.candidates.length + recovery.unknown
  const nextRecovery = recovery.candidates[0]
  const nextReset = resetStats.candidates[0]
  const chartPoints = viewMode === 'recovery' ? recovery.points : resetStats.points
  const yAxisConfig = getCountAxisConfig(chartPoints)
  const currentTitle = viewMode === 'recovery' ? t('accounts.recoveryDistributionTitle') : t('accounts.quotaResetDistributionTitle')
  const currentDescription = viewMode === 'recovery'
    ? t('accounts.recoveryDistributionDesc', {
      recoverable: recovery.candidates.length,
      limited: limitedTotal,
    })
    : t('accounts.quotaResetDistributionDesc', {
      known: resetStats.candidates.length,
      total: resetStats.total,
    })

  return (
    <Card className={`${compact ? 'lg:h-[430px]' : 'mb-4'} py-0 ${className}`}>
      <CardContent className={compact ? 'flex h-full flex-col p-4' : 'p-4 sm:p-5'}>
        <div className="mb-2 flex flex-wrap items-start justify-between gap-3">
          <div className="min-w-0">
            <div className="flex items-center gap-2">
              <TimerReset className="size-4 text-primary" />
              <h3 className="text-base font-semibold text-foreground">{currentTitle}</h3>
            </div>
            <p className="mt-1 text-sm text-muted-foreground">
              {currentDescription}
            </p>
          </div>
          <div className="flex flex-wrap justify-end gap-2">
            <div className="inline-flex rounded-lg border border-border bg-muted/50 p-0.5">
              {recoveryViewModes.map((mode) => (
                <button
                  key={mode}
                  type="button"
                  onClick={() => setViewMode(mode)}
                  className={`rounded-md px-2.5 py-1.5 text-xs font-medium transition-all ${
                    viewMode === mode
                      ? 'border border-border bg-background text-foreground shadow-sm'
                      : 'text-muted-foreground hover:text-foreground'
                  }`}
                >
                  {t(mode === 'recovery' ? 'accounts.recoveryModeRecovery' : 'accounts.recoveryModeReset')}
                </button>
              ))}
            </div>
            {viewMode === 'recovery' ? (
              <div className="inline-flex rounded-lg border border-border bg-muted/50 p-0.5">
                {recoveryWindows.map((key) => (
                  <button
                    key={key}
                    type="button"
                    onClick={() => setWindowKey(key)}
                    className={`rounded-md px-3 py-1.5 text-xs font-medium transition-all ${
                      windowKey === key
                        ? 'border border-border bg-background text-foreground shadow-sm'
                        : 'text-muted-foreground hover:text-foreground'
                    }`}
                  >
                    {key}
                  </button>
                ))}
              </div>
            ) : (
              <div className="inline-flex rounded-lg border border-border bg-muted/50 p-0.5">
                <span className="rounded-md border border-border bg-background px-3 py-1.5 text-xs font-medium text-foreground shadow-sm">
                  7d
                </span>
              </div>
            )}
          </div>
        </div>

        <div className={compact ? 'mb-3 grid grid-cols-2 gap-2 sm:grid-cols-4' : 'mb-4 grid grid-cols-2 gap-2 sm:grid-cols-4'}>
          {viewMode === 'recovery' ? (
            <>
              <RecoveryMetric label={t('accounts.recoveryLimitedTotal')} value={limitedTotal} tone={limitedTotal > 0 ? 'warning' : 'success'} compact={compact} />
              <RecoveryMetric label={t('accounts.recoveryRecoverable')} value={recovery.candidates.length} compact={compact} />
              <RecoveryMetric label={t('accounts.recoveryNext')} value={nextRecovery ? formatChartTime(nextRecovery.recoveryAt) : '-'} tone={nextRecovery ? 'success' : 'neutral'} compact={compact} />
              <RecoveryMetric label={t('accounts.recoveryUnknown')} value={recovery.unknown} tone={recovery.unknown > 0 ? 'warning' : 'neutral'} compact={compact} />
            </>
          ) : (
            <>
              <RecoveryMetric label={t('accounts.quotaResetTotal')} value={resetStats.total} compact={compact} />
              <RecoveryMetric label={t('accounts.quotaResetKnown')} value={resetStats.candidates.length} compact={compact} />
              <RecoveryMetric label={t('accounts.quotaResetNext')} value={nextReset ? formatChartTime(nextReset.resetAt) : '-'} tone={nextReset ? 'success' : 'neutral'} compact={compact} />
              <RecoveryMetric label={t('accounts.quotaResetUnknown')} value={resetStats.unknown} tone={resetStats.unknown > 0 ? 'warning' : 'neutral'} compact={compact} />
            </>
          )}
        </div>

        <div className={compact ? 'grid min-h-0 flex-1 grid-rows-[200px_auto] gap-3 lg:grid-rows-[minmax(116px,1fr)_94px]' : 'grid gap-3'}>
          <div className={compact ? 'min-h-0' : 'h-[260px]'}>
            <ResponsiveContainer width="100%" height="100%">
              <ComposedChart data={chartPoints} margin={viewMode === 'reset' ? resetChartMargin : chartMargin}>
                <CartesianGrid vertical={false} stroke={gridColor} strokeDasharray="4 4" />
                <XAxis
                  dataKey="label"
                  tick={{ fill: axisColor, fontSize: compact ? 11 : 12 }}
                  axisLine={{ stroke: gridColor }}
                  tickLine={{ stroke: gridColor }}
                  tickMargin={6}
                  minTickGap={compact ? 4 : 8}
                  interval={0}
                />
                <YAxis
                  tick={{ fill: axisColor, fontSize: compact ? 11 : 12 }}
                  axisLine={{ stroke: gridColor }}
                  tickLine={{ stroke: gridColor }}
                  allowDecimals={false}
                  domain={yAxisConfig.domain}
                  ticks={yAxisConfig.ticks}
                  tickFormatter={(value) => String(Math.round(Number(value)))}
                  width={viewMode === 'reset' ? (compact ? 44 : 50) : (compact ? 34 : 44)}
                />
                <Tooltip
                  formatter={(value) => [t('accounts.recoveryTooltipCount', { count: Number(value) }), t('accounts.recoveryAccountCount')]}
                  labelFormatter={(_, payload) => {
                    const point = payload?.[0]?.payload as RecoveryGroup | undefined
                    return t(viewMode === 'recovery' ? 'accounts.recoveryTooltipTime' : 'accounts.quotaResetTooltipTime', { time: point?.fullLabel ?? '' })
                  }}
                  contentStyle={tooltipContentStyle}
                  labelStyle={tooltipLabelStyle}
                  itemStyle={tooltipItemStyle}
                />
                <Bar
                  dataKey="count"
                  name={t('accounts.recoveryAccountCount')}
                  radius={[6, 6, 0, 0]}
                  maxBarSize={compact ? 34 : 46}
                >
                  {chartPoints.map((entry) => (
                    <Cell key={entry.key} fill={entry.fill} />
                  ))}
                </Bar>
                {viewMode === 'reset' ? (
                  <Line
                    type="monotone"
                    dataKey="count"
                    name={t('accounts.quotaResetTrend')}
                    stroke="var(--color-foreground)"
                    strokeWidth={2.5}
                    dot={{ r: 3, fill: 'var(--color-card)', stroke: 'var(--color-foreground)', strokeWidth: 2 }}
                    activeDot={{ r: 5 }}
                  />
                ) : null}
              </ComposedChart>
            </ResponsiveContainer>
          </div>
          {viewMode === 'recovery'
            ? <PressureForecastCard forecast={recovery.forecast} t={t} />
            : <QuotaResetSummaryCard stats={resetStats} t={t} />}
        </div>
      </CardContent>
    </Card>
  )
}

function RecoveryMetric({ label, value, tone = 'neutral', compact = false }: { label: string; value: number | string; tone?: 'neutral' | 'warning' | 'danger' | 'success'; compact?: boolean }) {
  const toneClass = {
    neutral: 'text-foreground',
    warning: 'text-amber-600 dark:text-amber-400',
    danger: 'text-red-600 dark:text-red-400',
    success: 'text-emerald-600 dark:text-emerald-400',
  }[tone]

  return (
    <div className={`min-w-0 rounded-lg border border-border bg-muted/20 ${compact ? 'px-2.5 py-1.5' : 'px-3 py-2.5'}`}>
      <div className="truncate text-[11px] font-medium text-muted-foreground">{label}</div>
      <div className={`${compact ? 'mt-0.5 text-base' : 'mt-1 text-lg'} font-semibold ${toneClass}`}>{value}</div>
    </div>
  )
}

function PressureForecastCard({ forecast, t }: { forecast: PressureForecast; t: (key: string, options?: Record<string, unknown>) => string }) {
  const pressureAt = forecast.supplyShortageAt ?? forecast.highPressureAt ?? forecast.predictedAt
  const predictedText = pressureAt
    ? formatChartTime(pressureAt)
    : t('accounts.pressureForecastNone')
  const stateText = forecast.supplyShortageAt
    ? t('accounts.pressureForecastShortage')
    : forecast.highPressureAt
      ? t('accounts.pressureForecastHigh')
      : forecast.predictedAt
        ? t('accounts.pressureForecastLimitRisk')
        : t('accounts.pressureForecastStable')
  const tone = forecast.riskLevel === 'high'
    ? 'text-red-600 dark:text-red-400'
    : forecast.riskLevel === 'medium'
      ? 'text-amber-600 dark:text-amber-400'
      : 'text-emerald-600 dark:text-emerald-400'
  const hasSamples = forecast.sampled + forecast.unknown > 0
  const lowConfidence = hasSamples && forecast.confidence < LOW_CONFIDENCE_THRESHOLD
  const logicText = t('accounts.pressureForecastLogic')
  const descText = t('accounts.pressureForecastDesc', {
    state: stateText,
    threshold: forecast.threshold,
    sampled: forecast.sampled,
    count: forecast.predictedCount,
    rpm: formatWholeNumber(forecast.rpm),
    rpmLimit: forecast.effectiveRpmLimit > 0 ? formatWholeNumber(forecast.effectiveRpmLimit) : '-',
    rpmPressure: formatPercentText(forecast.rpmPressure),
    dispatchable: forecast.dispatchableAccounts,
    activePressure: formatPercentText(forecast.activePressure),
    rateLimitPressure: formatPercentText(forecast.rateLimitPressure),
  })

  return (
    <TooltipProvider>
      <div className="min-h-0 overflow-hidden rounded-lg border border-border bg-muted/20 px-3 py-2">
        <div className="flex items-start justify-between gap-3">
          <div className="min-w-0">
            <div className="flex items-center gap-1.5 text-xs font-semibold text-foreground">
              <span>{t('accounts.pressureForecastTitle')}</span>
              <UITooltip>
                <TooltipTrigger asChild>
                  <button
                    type="button"
                    className="inline-flex size-4 items-center justify-center rounded-full text-muted-foreground transition-colors hover:bg-muted hover:text-foreground focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                    aria-label={t('accounts.pressureForecastHelp')}
                  >
                    <CircleHelp className="size-3.5" />
                  </button>
                </TooltipTrigger>
                <TooltipContent side="top" sideOffset={6} className="max-w-[340px] whitespace-normal text-left leading-relaxed">
                  {logicText}
                </TooltipContent>
              </UITooltip>
              {lowConfidence ? (
                <UITooltip>
                  <TooltipTrigger asChild>
                    <span
                      className="inline-flex items-center rounded-md border border-amber-500/30 bg-amber-500/10 px-1.5 py-0 text-[10px] font-medium text-amber-700 dark:text-amber-300"
                      tabIndex={0}
                    >
                      {t('accounts.pressureForecastLowConfidence', {
                        percent: Math.round(forecast.confidence * 100),
                      })}
                    </span>
                  </TooltipTrigger>
                  <TooltipContent side="top" sideOffset={6} className="max-w-[280px] whitespace-normal text-left leading-relaxed">
                    {t('accounts.pressureForecastLowConfidenceHint', {
                      sampled: forecast.sampled,
                      unknown: forecast.unknown,
                    })}
                  </TooltipContent>
                </UITooltip>
              ) : null}
            </div>
            <div className={`mt-1 truncate text-xs font-semibold ${tone}`}>{stateText}</div>
          </div>
          <div className="shrink-0 text-right">
            <div className="text-[11px] font-medium text-muted-foreground">{t('accounts.pressureForecastEta')}</div>
            <div className={`text-sm font-semibold ${tone}`}>{predictedText}</div>
          </div>
        </div>
        <div className="mt-1 truncate text-[11px] text-muted-foreground" title={descText}>
          {descText}
        </div>
      </div>
    </TooltipProvider>
  )
}

function QuotaResetSummaryCard({ stats, t }: { stats: ResetStats; t: (key: string, options?: Record<string, unknown>) => string }) {
  const nextReset = stats.candidates[0]
  const nextText = nextReset
    ? formatChartTime(nextReset.resetAt)
    : t('accounts.quotaResetSummaryNone')
  const futureCount = stats.points.reduce((sum, point) => sum + point.count, 0)
  const tone = nextReset ? 'text-emerald-600 dark:text-emerald-400' : 'text-muted-foreground'
  const descText = t('accounts.quotaResetSummaryDesc', {
    count: futureCount,
    known: stats.candidates.length,
    total: stats.total,
    unknown: stats.unknown,
  })

  return (
    <div className="min-h-0 overflow-hidden rounded-lg border border-border bg-muted/20 px-3 py-2">
      <div className="flex items-start justify-between gap-3">
        <div className="min-w-0">
          <div className="text-xs font-semibold text-foreground">{t('accounts.quotaResetSummaryTitle')}</div>
          <div className="mt-1 truncate text-[11px] text-muted-foreground" title={descText}>
            {descText}
          </div>
        </div>
        <div className="shrink-0 text-right">
          <div className="text-[11px] font-medium text-muted-foreground">{t('accounts.quotaResetNext')}</div>
          <div className={`text-sm font-semibold ${tone}`}>{nextText}</div>
        </div>
      </div>
      <div className="mt-1 truncate text-[11px] text-muted-foreground">
        {t('accounts.quotaResetSummaryKnown', {
          known: stats.candidates.length,
          total: stats.total,
          unknown: stats.unknown,
        })}
      </div>
    </div>
  )
}

function getAccountRecoveryCandidate(account: AccountRow, nowMs: number, windowKey: RecoveryWindow): RecoveryCandidate | null {
  const reset5h = futureTimestamp(account.reset_5h_at, nowMs)
  const reset7d = futureTimestamp(account.reset_7d_at, nowMs)
  const cooldownUntil = futureTimestamp(account.cooldown_until, nowMs)

  if (windowKey === '5h') {
    if (isPremiumUsagePlan(account.plan_type) && isUsageExhausted(account.usage_percent_5h) && reset5h) {
      return buildRecoveryCandidate(account, reset5h, nowMs, '5h')
    }
    if (cooldownUntil && isShortRateLimitLike(account)) {
      return buildRecoveryCandidate(account, cooldownUntil, nowMs, 'cooldown')
    }
    return null
  }

  if (isUsageExhausted(account.usage_percent_7d) && reset7d) {
    return buildRecoveryCandidate(account, reset7d, nowMs, '7d')
  }
  return null
}

function buildRecoveryCandidate(account: AccountRow, recoveryAt: number, nowMs: number, reason: RecoveryReason): RecoveryCandidate {
  return {
    id: account.id,
    label: account.email || account.name || `ID ${account.id}`,
    recoveryAt,
    secondsUntil: Math.max(0, Math.ceil((recoveryAt - nowMs) / 1000)),
    reason,
  }
}

function isWindowRateLimitLike(account: AccountRow, windowKey: RecoveryWindow): boolean {
  if (windowKey === '5h') {
    return (isPremiumUsagePlan(account.plan_type) && isUsageExhausted(account.usage_percent_5h)) || isShortRateLimitLike(account)
  }
  const status = (account.status || '').toLowerCase()
  const reason = (account.cooldown_reason || '').toLowerCase()
  return isUsageExhausted(account.usage_percent_7d) ||
    status === 'usage_exhausted' ||
    status === 'rate_limited_7d' ||
    reason === 'rate_limited_7d'
}

function isShortRateLimitLike(account: AccountRow): boolean {
  const status = (account.status || '').toLowerCase()
  const reason = (account.cooldown_reason || '').toLowerCase()
  if (status === 'rate_limited' || status === 'rate_limited_5h' || status === 'cooldown') {
    return true
  }
  if (reason === 'rate_limited' || reason === 'rate_limited_5h') {
    return true
  }
  return false
}

function createResetStats(accounts: AccountRow[], nowMs: number): ResetStats {
  const candidates: ResetCandidate[] = []
  let total = 0
  let unknown = 0

  for (const account of accounts) {
    if (!hasBurnPrediction(account, '7d')) {
      continue
    }
    total += 1
    const resetAt = futureTimestamp(account.reset_7d_at, nowMs)
    if (!resetAt) {
      unknown += 1
      continue
    }
    candidates.push({
      id: account.id,
      label: account.email || account.name || `ID ${account.id}`,
      resetAt,
    })
  }

  candidates.sort((a, b) => a.resetAt - b.resetAt)

  return {
    candidates,
    points: createResetPoints(candidates, nowMs),
    total,
    unknown,
  }
}

function createResetPoints(candidates: ResetCandidate[], nowMs: number): RecoveryGroup[] {
  const bucketCount = 7
  const startOfToday = startOfBeijingDay(nowMs)
  const points: RecoveryGroup[] = Array.from({ length: bucketCount }, (_, index) => {
    const startAt = startOfToday + index * 24 * 60 * 60_000
    const endAt = startAt + 24 * 60 * 60_000
    return {
      key: `7d-reset-${index}`,
      startAt,
      endAt,
      label: formatRecoveryPointLabel(startAt, '7d'),
      fullLabel: formatRecoveryPointRange(startAt, endAt, '7d'),
      count: 0,
      fill: recoveryReasonFill['7d'],
    }
  })

  for (const candidate of candidates) {
    const point = points.find((item) => candidate.resetAt >= item.startAt && candidate.resetAt < item.endAt)
    if (!point) {
      continue
    }
    point.count += 1
  }

  return points
}

function startOfBeijingDay(timestamp: number): number {
  const day = formatBeijingTime(new Date(timestamp).toISOString()).slice(0, 10)
  return new Date(`${day}T00:00:00+08:00`).getTime()
}

function createRecoveryPoints(candidates: RecoveryCandidate[], windowKey: RecoveryWindow, nowMs: number): RecoveryGroup[] {
  const bucketCount = windowKey === '5h' ? 5 : 7
  const bucketMs = windowKey === '5h' ? 60 * 60_000 : 24 * 60 * 60_000
  const points: RecoveryGroup[] = Array.from({ length: bucketCount }, (_, index) => {
    const startAt = nowMs + index * bucketMs
    const endAt = startAt + bucketMs
    return {
      key: `${windowKey}-${index}`,
      startAt,
      endAt,
      label: formatRecoveryPointLabel(endAt, windowKey),
      fullLabel: formatRecoveryPointRange(startAt, endAt, windowKey),
      count: 0,
      fill: recoveryReasonFill[windowKey],
    }
  })

  for (const candidate of candidates) {
    const point = points.find((item) => candidate.recoveryAt >= item.startAt && candidate.recoveryAt < item.endAt)
    if (!point) {
      continue
    }
    point.count += 1
    if (candidate.reason === 'cooldown') {
      point.fill = recoveryReasonFill.cooldown
    }
  }

  return points
}

function getCountAxisConfig(points: RecoveryGroup[]): { domain: [number, number]; ticks: number[] } {
  const maxCount = Math.max(0, ...points.map((point) => point.count))
  if (maxCount <= 4) {
    return {
      domain: [0, 4],
      ticks: [0, 1, 2, 3, 4],
    }
  }

  const step = getNiceTickStep(maxCount / 4)
  const top = Math.max(step, Math.ceil(maxCount / step) * step)
  const tickCount = Math.floor(top / step) + 1
  return {
    domain: [0, top],
    ticks: Array.from({ length: tickCount }, (_, index) => index * step),
  }
}

function getNiceTickStep(rawStep: number): number {
  if (!Number.isFinite(rawStep) || rawStep <= 1) {
    return 1
  }
  const magnitude = 10 ** Math.floor(Math.log10(rawStep))
  const normalized = rawStep / magnitude
  if (normalized <= 1.5) return magnitude
  if (normalized <= 3) return 2 * magnitude
  if (normalized <= 7) return 5 * magnitude
  return 10 * magnitude
}

function estimatePressureForecast(accounts: AccountRow[], windowKey: RecoveryWindow, nowMs: number, currentRpm: number, rpmLimit: number, avgDurationMs: number): PressureForecast {
  const windowMs = getWindowMs(windowKey)
  const burnMinElapsedMs = windowMs * BURN_MIN_ELAPSED_RATIO
  const rpmPerSlot = getRpmPerSlot(avgDurationMs)
  const projectedLimitTimes: number[] = []
  const supplyEvents: SupplyEvent[] = []
  const dispatchableAccounts = accounts.filter((account) => isInSupplyPool(account, windowKey))
  const totalConcurrency = dispatchableAccounts.reduce((sum, account) => sum + getEffectiveConcurrency(account), 0)
  const avgConcurrency = dispatchableAccounts.length > 0 ? totalConcurrency / dispatchableAccounts.length : 0
  const activeRequests = dispatchableAccounts.reduce((sum, account) => sum + normalizeNumber(account.active_requests), 0)
  const activePressure = totalConcurrency > 0 ? clamp(activeRequests / totalConcurrency, 0, 3) : 0
  // Real-time 429 signal: include both (a) accounts currently in a window
  // rate-limit (not in dispatchable pool right now) and (b) dispatchable
  // accounts that hit a 429 within RECENT_RATE_LIMIT_WINDOW_MS. Denominator is
  // the full eligible pool — dispatchable plus those currently rate-limited.
  // Replaces the prior sum(rate_limit_attempts)/sum(total_requests) which used
  // lifetime counters and was therefore nearly static once accounts had history.
  const currentlyRateLimited = accounts.filter((account) => {
    const status = (account.status || '').toLowerCase()
    if (status === 'unauthorized') return false
    if (account.enabled === false) return false
    return isWindowRateLimitLike(account, windowKey)
  })
  const recentlyRateLimitedFromPool = dispatchableAccounts.filter((account) => {
    if (!account.last_rate_limited_at) return false
    const ts = new Date(account.last_rate_limited_at).getTime()
    return Number.isFinite(ts) && ts > 0 && nowMs - ts <= RECENT_RATE_LIMIT_WINDOW_MS
  })
  const rateLimitedSignalDenominator = dispatchableAccounts.length + currentlyRateLimited.length
  const rateLimitedSignalNumerator = recentlyRateLimitedFromPool.length + currentlyRateLimited.length
  const recentRateLimitedFraction = rateLimitedSignalDenominator > 0
    ? rateLimitedSignalNumerator / rateLimitedSignalDenominator
    : 0
  const rateLimitPressure = clamp(recentRateLimitedFraction / RATE_LIMIT_SATURATION_FRACTION, 0, 1)
  const normalizedRpm = normalizeNumber(currentRpm)
  const configuredRpmLimit = normalizeNumber(rpmLimit)
  // Simplified from accounts.length × avgConcurrency × slot: the product is
  // mathematically equal to totalConcurrency × slot but the latter makes intent
  // explicit ("each concurrency slot contributes ~6 rpm of headroom").
  const concurrencyRpmLimit = totalConcurrency > 0
    ? Math.max(1, Math.round(totalConcurrency * rpmPerSlot))
    : 0
  const effectiveRpmLimit = getEffectiveRpmLimit(configuredRpmLimit, concurrencyRpmLimit)
  const rpmPressure = effectiveRpmLimit > 0 ? normalizedRpm / effectiveRpmLimit : null
  const pressureFactor = getPressureFactor(rpmPressure, activePressure, rateLimitPressure)
  let sampled = 0
  let unknown = 0

  for (const account of accounts) {
    // Burn prediction requires a quota window — skip Responses API accounts
    // and 5h-on-non-premium entirely (they still contribute to supply via the
    // dispatchableAccounts filter above, just not to burn forecasting).
    if (!hasBurnPrediction(account, windowKey)) {
      continue
    }
    const inSupply = isInSupplyPool(account, windowKey)
    const concurrency = getEffectiveConcurrency(account)
    const usage = windowKey === '5h' ? account.usage_percent_5h : account.usage_percent_7d
    const rawResetAt = windowKey === '5h' ? account.reset_5h_at : account.reset_7d_at
    // Fallback: if upstream reset_at is missing but usage is readable, assume a
    // fresh window starting now. This keeps newly-rotated accounts in the
    // supply pool instead of dropping them into "unknown" where they can't
    // influence the forecast.
    const knownResetAt = futureTimestamp(rawResetAt, nowMs)
    const resetAt = knownResetAt ?? (nowMs + windowMs)

    // Currently rate-limited (or otherwise out of supply pool right now) but
    // burn-predictable: not in totalConcurrency, but will replenish at reset.
    // Schedule an unpaired +1 event so the supply curve models recovery, while
    // findBulkLimitTime won't decrement exhaust count for it.
    if (!inSupply) {
      if (knownResetAt) {
        supplyEvents.push({ at: knownResetAt, concurrency, delta: 1 })
      }
      if (typeof usage !== 'number' || !Number.isFinite(usage)) {
        unknown += 1
      }
      continue
    }

    if (typeof usage !== 'number' || !Number.isFinite(usage)) {
      unknown += 1
      continue
    }

    sampled += 1
    const usedPercent = clamp(usage, 0, 100)
    if (usedPercent >= 100) {
      projectedLimitTimes.push(nowMs)
      supplyEvents.push({ at: nowMs, concurrency, delta: -1 })
      supplyEvents.push({ at: resetAt, concurrency, delta: 1, paired: true })
      continue
    }

    const windowStartAt = resetAt - windowMs
    const elapsedMs = Math.max(60_000, nowMs - windowStartAt)
    // Demand a minimum elapsed slice before trusting linear extrapolation —
    // otherwise a freshly-rotated account at usage=1% with elapsed=2min would
    // predict exhaustion in minutes. Account still stays in supply pool.
    if (elapsedMs < burnMinElapsedMs) {
      continue
    }
    const burnRatePerMs = usedPercent / elapsedMs
    if (burnRatePerMs <= 0) {
      unknown += 1
      continue
    }
    const predictedAt = nowMs + ((100 - usedPercent) / burnRatePerMs)
    if (Number.isFinite(predictedAt) && predictedAt <= resetAt) {
      projectedLimitTimes.push(predictedAt)
      supplyEvents.push({ at: predictedAt, concurrency, delta: -1 })
      supplyEvents.push({ at: resetAt, concurrency, delta: 1, paired: true })
    }
  }

  projectedLimitTimes.sort((a, b) => a - b)
  supplyEvents.sort((a, b) => a.at - b.at)
  const supplyPressurePoint = estimateSupplyPressurePoint(
    supplyEvents,
    normalizedRpm,
    configuredRpmLimit,
    totalConcurrency,
    nowMs,
    rpmPerSlot,
  )
  // Capacity-driven threshold: how many accounts can vanish before remaining
  // concurrency can no longer absorb currentRpm at the current rpm/slot rate.
  // Falls back to the historical 30% ratio when there's no traffic.
  const minAccountsForRpm = (normalizedRpm > 0 && avgConcurrency > 0)
    ? Math.ceil(normalizedRpm / (avgConcurrency * rpmPerSlot))
    : 0
  const capacityThreshold = minAccountsForRpm > 0 && dispatchableAccounts.length > 0
    ? Math.max(BULK_LIMIT_MIN_COUNT, dispatchableAccounts.length - minAccountsForRpm)
    : Math.max(BULK_LIMIT_MIN_COUNT, Math.ceil(sampled * BULK_LIMIT_RATIO))
  const threshold = sampled > 0 ? Math.min(sampled, capacityThreshold) : 0
  const quotaPredictedAt = findBulkLimitTime(supplyEvents, threshold)
  const predictedAt = quotaPredictedAt
    ? nowMs + ((quotaPredictedAt - nowMs) / pressureFactor)
    : null
  const totalEligible = sampled + unknown
  const confidence = totalEligible > 0 ? sampled / totalEligible : 0
  const riskLevel = getForecastRiskLevel(
    predictedAt,
    supplyPressurePoint.highPressureAt,
    supplyPressurePoint.supplyShortageAt,
    nowMs,
    windowKey,
    rpmPressure,
    activePressure,
    rateLimitPressure,
    confidence,
  )

  return {
    sampled,
    threshold,
    predictedAt,
    predictedCount: quotaPredictedAt ? projectedLimitTimes.filter((item) => item <= quotaPredictedAt).length : projectedLimitTimes.length,
    unknown,
    rpm: normalizedRpm,
    effectiveRpmLimit,
    rpmPressure,
    activePressure,
    rateLimitPressure,
    dispatchableAccounts: dispatchableAccounts.length,
    avgConcurrency,
    highPressureAt: supplyPressurePoint.highPressureAt,
    supplyShortageAt: supplyPressurePoint.supplyShortageAt,
    riskLevel,
    confidence,
  }
}

// Walks the sorted supply event stream and finds the earliest time at which
// at least `threshold` accounts are simultaneously exhausted. Only +1 events
// tagged as `paired` (i.e. recovery of a previously exhausted account)
// decrement the exhaust count; unpaired +1 events (currently rate-limited
// accounts recovering) restore capacity but were never in the exhaust count.
function findBulkLimitTime(events: SupplyEvent[], threshold: number): number | null {
  if (threshold <= 0) return null
  let exhausted = 0
  for (const event of events) {
    if (event.delta === -1) {
      exhausted += 1
      if (exhausted >= threshold) return event.at
    } else if (event.paired) {
      exhausted = Math.max(0, exhausted - 1)
    }
  }
  return null
}

// Account contributes RPM/concurrency to the supply pool. We include all
// account classes that actually serve proxy traffic — OAuth + Responses API +
// non-premium-on-5h — and only filter out hard-disabled/unauthorized or
// currently rate-limited entries. This corrects a prior bias that dropped
// Responses accounts from totalConcurrency and made mixed deployments look
// far smaller than they really are.
function isInSupplyPool(account: AccountRow, windowKey: RecoveryWindow): boolean {
  const status = (account.status || '').toLowerCase()
  if (status === 'unauthorized') return false
  if (account.enabled === false) return false
  if (isWindowRateLimitLike(account, windowKey)) return false
  return true
}

// Account has a quota window we can linearly extrapolate to an exhaustion
// time. Excludes Responses API accounts (no per-user quota tracking) and 5h
// for non-premium plans (no 5h quota cap). Unauthorized is also excluded
// since burn data won't be refreshed.
function hasBurnPrediction(account: AccountRow, windowKey: RecoveryWindow): boolean {
  const status = (account.status || '').toLowerCase()
  if (status === 'unauthorized') return false
  if (account.openai_responses_api) return false
  if (windowKey === '5h') return isPremiumUsagePlan(account.plan_type)
  return true
}

function getEffectiveConcurrency(account: AccountRow): number {
  const value = account.dynamic_concurrency_limit ??
    account.base_concurrency_effective ??
    account.base_concurrency_override ??
    1
  return clamp(normalizeNumber(value), 1, 50)
}

// Adaptive RPM/slot: 60000ms / avgDurationMs gives "requests a slot can finish
// per minute". Falls back to the historical 6 (≈10s/req) when avg_duration_ms
// is unavailable, and is clamped to [1, 30] so a freakishly fast/slow sample
// (single request, batched timeouts) doesn't dominate the supply estimate.
function getRpmPerSlot(avgDurationMs: number): number {
  if (!avgDurationMs || avgDurationMs <= 0 || !Number.isFinite(avgDurationMs)) {
    return RPM_PER_CONCURRENCY_SLOT_DEFAULT
  }
  return clamp(60_000 / avgDurationMs, RPM_PER_CONCURRENCY_SLOT_MIN, RPM_PER_CONCURRENCY_SLOT_MAX)
}

function getEffectiveRpmLimit(configuredRpmLimit: number, concurrencyRpmLimit: number): number {
  if (concurrencyRpmLimit <= 0) {
    return 0
  }
  if (configuredRpmLimit > 0 && concurrencyRpmLimit > 0) {
    return Math.min(configuredRpmLimit, concurrencyRpmLimit)
  }
  return concurrencyRpmLimit
}

function getPressureFactor(rpmPressure: number | null, activePressure: number, rateLimitPressure: number): number {
  // RPM and active-request signals are typically correlated (high RPM saturates
  // concurrency), so summing all three boosts double-counts. Use dominant +
  // weighted second-largest instead — softer, stays under PRESSURE_FACTOR_MAX
  // even when every signal is maxed out.
  const rpmBoost = Math.max(0, (rpmPressure ?? 0) - PRESSURE_THRESHOLD_RPM)
  const activeBoost = Math.max(0, activePressure - PRESSURE_THRESHOLD_ACTIVE)
  const rateLimitBoost = rateLimitPressure
  const boosts = [rpmBoost, activeBoost, rateLimitBoost].sort((a, b) => b - a)
  const composite = boosts[0] * PRESSURE_BOOST_DOMINANT + boosts[1] * PRESSURE_BOOST_SECONDARY
  return clamp(1 + composite, 1, PRESSURE_FACTOR_MAX)
}

function estimateSupplyPressurePoint(events: SupplyEvent[], currentRpm: number, configuredRpmLimit: number, totalConcurrency: number, nowMs: number, rpmPerSlot: number): SupplyPressurePoint {
  if (currentRpm <= 0) {
    return { highPressureAt: null, supplyShortageAt: null }
  }

  let remainingConcurrency = totalConcurrency
  let capacity = getEffectiveRpmLimit(configuredRpmLimit, Math.round(remainingConcurrency * rpmPerSlot))
  let pressure = capacity > 0 ? currentRpm / capacity : Number.POSITIVE_INFINITY
  let highPressureAt = pressure >= 0.9 ? nowMs : null
  let supplyShortageAt = pressure >= 1 ? nowMs : null

  for (const event of events) {
    // Delta-based: -1 events (exhaustion) shrink the pool, +1 events
    // (replenishment from reset) grow it. Pool can transiently dip below the
    // RPM ceiling and then recover — that's expected, the forecast surfaces the
    // earliest crossing only.
    remainingConcurrency = Math.max(0, remainingConcurrency + event.delta * event.concurrency)
    capacity = getEffectiveRpmLimit(configuredRpmLimit, Math.round(remainingConcurrency * rpmPerSlot))
    pressure = capacity > 0 ? currentRpm / capacity : Number.POSITIVE_INFINITY

    if (!highPressureAt && pressure >= 0.9) {
      highPressureAt = event.at
    }
    if (!supplyShortageAt && pressure >= 1) {
      supplyShortageAt = event.at
      break
    }
  }

  return { highPressureAt, supplyShortageAt }
}

function getWindowMs(windowKey: RecoveryWindow): number {
  return windowKey === '5h' ? 5 * 60 * 60_000 : 7 * 24 * 60 * 60_000
}

function getForecastRiskLevel(
  predictedAt: number | null,
  highPressureAt: number | null,
  supplyShortageAt: number | null,
  nowMs: number,
  windowKey: RecoveryWindow,
  rpmPressure: number | null,
  activePressure: number,
  rateLimitPressure: number,
  confidence: number,
): PressureForecast['riskLevel'] {
  const soonWindowMs = getWindowMs(windowKey) * SOON_WINDOW_RATIO
  // Real-time signals (RPM, active queue, historical 429s) are always reliable,
  // so they can independently raise risk. The quota-burn projection however
  // depends on sample coverage — gate predictedAt-based escalation behind a
  // minimum confidence so a single observed account can't flip the badge.
  const burnSignalReliable = confidence >= LOW_CONFIDENCE_THRESHOLD
  if (
    (supplyShortageAt && supplyShortageAt - nowMs <= soonWindowMs) ||
    (burnSignalReliable && predictedAt && predictedAt - nowMs <= soonWindowMs) ||
    (rpmPressure ?? 0) >= 1 ||
    activePressure >= 0.9 ||
    rateLimitPressure >= 0.8
  ) {
    return 'high'
  }
  if (highPressureAt || (burnSignalReliable && predictedAt) || (rpmPressure ?? 0) >= 0.7 || activePressure >= 0.7 || rateLimitPressure >= 0.4) {
    return 'medium'
  }
  return 'low'
}

function futureTimestamp(value: string | undefined, nowMs: number): number | null {
  if (!value) return null
  const timestamp = new Date(value).getTime()
  if (!Number.isFinite(timestamp) || timestamp <= nowMs) {
    return null
  }
  return timestamp
}

function isUsageExhausted(value?: number | null): boolean {
  return typeof value === 'number' && Number.isFinite(value) && value >= 100
}

function clamp(value: number, min: number, max: number): number {
  return Math.min(max, Math.max(min, value))
}

function normalizeNumber(value?: number | null): number {
  return typeof value === 'number' && Number.isFinite(value) ? value : 0
}

function formatWholeNumber(value: number): string {
  return Number.isFinite(value) ? String(Math.round(value)) : '-'
}

function formatPercentText(value: number | null): string {
  if (typeof value !== 'number' || !Number.isFinite(value)) {
    return '-'
  }
  return `${Math.round(value * 100)}%`
}

function normalizePlanType(planType?: string): string {
  const raw = (planType || '').toLowerCase().trim()
  if (raw === 'prolite' || raw === 'pro_lite' || raw === 'pro-lite') return 'pro'
  return raw
}

// Plans that carry a rolling 5h usage window (mirrors Go isPremium5hPlan).
function isPremiumUsagePlan(planType?: string): boolean {
  return ['plus', 'pro', 'team', 'teamplus', 'k12', 'edu', 'education', 'go'].includes(normalizePlanType(planType))
}

function formatChartTime(timestamp: number): string {
  return formatBeijingTime(new Date(timestamp).toISOString()).slice(5, 16)
}

function formatRecoveryPointLabel(timestamp: number, windowKey: RecoveryWindow): string {
  const value = formatBeijingTime(new Date(timestamp).toISOString())
  return windowKey === '5h' ? value.slice(11, 16) : value.slice(5, 10)
}

function formatRecoveryPointRange(startAt: number, endAt: number, windowKey: RecoveryWindow): string {
  const start = formatBeijingTime(new Date(startAt).toISOString())
  const end = formatBeijingTime(new Date(endAt).toISOString())
  if (windowKey === '5h') {
    return `${start.slice(5, 16)} - ${end.slice(11, 16)}`
  }
  return `${start.slice(5, 10)} - ${end.slice(5, 10)}`
}
