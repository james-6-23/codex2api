import { useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import {
  Bar,
  CartesianGrid,
  Cell,
  ComposedChart,
  Legend,
  Line,
  ResponsiveContainer,
  Tooltip,
  XAxis,
  YAxis,
} from 'recharts'
import { BarChart3, RefreshCw } from 'lucide-react'
import { Card, CardContent } from '@/components/ui/card'
import { api } from '../api'
import { getErrorMessage } from '../utils/error'
import type { AccountRow } from '../types'

type QuotaWindow = '5h' | '7d'

interface AccountQuotaDistributionChartProps {
  accounts: AccountRow[]
  className?: string
  compact?: boolean
  onProbeStarted?: () => void
  onProbeError?: (message: string) => void
}

interface DistributionBucket {
  key: string
  label: string
  count: number
  bucketPercent: number
  fill: string
}

interface SampledAccountQuota {
  used: number
}

const quotaWindows: QuotaWindow[] = ['5h', '7d']

const quotaBuckets = [
  { key: '0-10', min: 0, max: 10, fill: 'hsl(var(--success))' },
  { key: '10-20', min: 10, max: 20, fill: 'hsl(164 58% 36%)' },
  { key: '20-30', min: 20, max: 30, fill: 'hsl(178 56% 38%)' },
  { key: '30-40', min: 30, max: 40, fill: 'hsl(var(--info))' },
  { key: '40-50', min: 40, max: 50, fill: 'var(--color-primary)' },
  { key: '50-60', min: 50, max: 60, fill: 'hsl(47 78% 44%)' },
  { key: '60-70', min: 60, max: 70, fill: 'hsl(var(--warning))' },
  { key: '70-80', min: 70, max: 80, fill: 'hsl(30 82% 44%)' },
  { key: '80-90', min: 80, max: 90, fill: 'hsl(24 85% 48%)' },
  { key: '90-100', min: 90, max: 100, fill: 'var(--color-destructive)' },
]

const chartMargin = { top: 8, right: 12, left: -12, bottom: 0 }
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
const legendWrapperStyle = { paddingTop: 4, fontSize: 12, color: axisColor }

export default function AccountQuotaDistributionChart({
  accounts,
  className = '',
  compact = false,
  onProbeStarted,
  onProbeError,
}: AccountQuotaDistributionChartProps) {
  const { t } = useTranslation()
  const [windowKey, setWindowKey] = useState<QuotaWindow>('7d')
  const [probing, setProbing] = useState(false)

  const handleProbe = async () => {
    if (probing) return
    setProbing(true)
    try {
      await api.forceUsageProbe()
      onProbeStarted?.()
    } catch (err) {
      onProbeError?.(getErrorMessage(err))
    } finally {
      setProbing(false)
    }
  }

  const distribution = useMemo(() => {
    const eligibleAccounts = accounts.filter((account) => isEligibleForQuotaWindow(account, windowKey))
    const sampledQuotas: SampledAccountQuota[] = []

    for (const account of eligibleAccounts) {
      const value = windowKey === '5h' ? account.usage_percent_5h : account.usage_percent_7d
      if (typeof value !== 'number' || !Number.isFinite(value)) {
        continue
      }
      const used = clamp(value, 0, 100)
      sampledQuotas.push({ used })
    }

    const buckets: DistributionBucket[] = quotaBuckets.map((bucket) => ({
      key: bucket.key,
      label: `${bucket.min}-${bucket.max}%`,
      count: 0,
      bucketPercent: 0,
      fill: bucket.fill,
    }))

    for (const quota of sampledQuotas) {
      const bucketIndex = findBucketIndex(quota.used)
      buckets[bucketIndex].count += 1
    }

    for (const bucket of buckets) {
      bucket.bucketPercent = sampledQuotas.length > 0
        ? Number(((bucket.count / sampledQuotas.length) * 100).toFixed(1))
        : 0
    }

    const averageUsed = sampledQuotas.length > 0
      ? sampledQuotas.reduce((sum, quota) => sum + quota.used, 0) / sampledQuotas.length
      : null

    return {
      buckets,
      total: eligibleAccounts.length,
      sampled: sampledQuotas.length,
      unsampled: eligibleAccounts.length - sampledQuotas.length,
      highUsage: sampledQuotas.filter((quota) => quota.used >= 90).length,
      exhausted: sampledQuotas.filter((quota) => quota.used >= 100).length,
      averageUsed,
    }
  }, [accounts, windowKey])

  const hasChartData = distribution.sampled > 0

  return (
    <Card className={`${compact ? 'h-[430px]' : 'mb-4'} py-0 ${className}`}>
      <CardContent className={compact ? 'flex h-full flex-col p-4' : 'p-4 sm:p-5'}>
        <div className="mb-3 flex flex-wrap items-start justify-between gap-3">
          <div className="min-w-0">
            <div className="flex items-center gap-2">
              <BarChart3 className="size-4 text-primary" />
              <h3 className="text-base font-semibold text-foreground">{t('accounts.quotaDistributionTitle')}</h3>
            </div>
            <p className="mt-1 text-sm text-muted-foreground">
              {t('accounts.quotaDistributionDesc', {
                sampled: distribution.sampled,
                total: distribution.total,
              })}
            </p>
          </div>
          <div className="flex items-center gap-2">
            <button
              type="button"
              onClick={handleProbe}
              disabled={probing}
              title={t('accounts.quotaDistributionRefreshTitle')}
              className="inline-flex items-center gap-1 rounded-md border border-border bg-background px-2.5 py-1.5 text-xs font-medium text-muted-foreground transition-all hover:text-foreground disabled:opacity-50"
            >
              <RefreshCw className={`size-3.5 ${probing ? 'animate-spin' : ''}`} />
              <span>{probing ? t('accounts.quotaDistributionRefreshing') : t('accounts.quotaDistributionRefresh')}</span>
            </button>
            <div className="inline-flex rounded-lg border border-border bg-muted/50 p-0.5">
              {quotaWindows.map((key) => (
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
          </div>
        </div>

        <div className={compact ? 'flex min-h-0 flex-1 flex-col gap-3' : 'grid gap-4 xl:grid-cols-[minmax(0,1fr)_320px]'}>
          <div className={`${compact ? 'min-h-0 flex-1' : 'h-[260px]'} min-w-0`}>
            {hasChartData ? (
              <ResponsiveContainer width="100%" height="100%">
                <ComposedChart data={distribution.buckets} margin={chartMargin}>
                  <CartesianGrid vertical={false} stroke={gridColor} strokeDasharray="4 4" />
                  <XAxis
                    dataKey="label"
                    tick={{ fill: axisColor, fontSize: 12 }}
                    axisLine={{ stroke: gridColor }}
                    tickLine={{ stroke: gridColor }}
                    tickMargin={8}
                    minTickGap={8}
                  />
                  <YAxis
                    yAxisId="count"
                    tick={{ fill: axisColor, fontSize: 12 }}
                    axisLine={{ stroke: gridColor }}
                    tickLine={{ stroke: gridColor }}
                    allowDecimals={false}
                    width={44}
                  />
                  <YAxis
                    yAxisId="percent"
                    orientation="right"
                    domain={[0, 100]}
                    tickFormatter={(value) => `${value}%`}
                    tick={{ fill: axisColor, fontSize: 12 }}
                    axisLine={{ stroke: gridColor }}
                    tickLine={{ stroke: gridColor }}
                    width={48}
                  />
                  <Tooltip
                    formatter={(value, name, item) => {
                      const payload = item.payload as DistributionBucket | undefined
                      if (name === t('accounts.quotaDistributionBucketPercent')) {
                        return [`${Number(value).toFixed(1)}%`, name]
                      }
                      return [
                        t('accounts.quotaDistributionTooltipBucket', {
                          count: Number(value),
                          percent: (payload?.bucketPercent ?? 0).toFixed(1),
                        }),
                        name,
                      ]
                    }}
                    labelFormatter={(label) => t('accounts.quotaDistributionTooltipRange', { range: label })}
                    contentStyle={tooltipContentStyle}
                    labelStyle={tooltipLabelStyle}
                    itemStyle={tooltipItemStyle}
                  />
                  <Legend wrapperStyle={legendWrapperStyle} />
                  <Bar
                    yAxisId="count"
                    dataKey="count"
                    radius={[6, 6, 0, 0]}
                    maxBarSize={compact ? 28 : 42}
                    name={t('accounts.quotaDistributionAccountCount')}
                  >
                    {distribution.buckets.map((entry) => (
                      <Cell key={entry.key} fill={entry.fill} />
                    ))}
                  </Bar>
                  <Line
                    yAxisId="percent"
                    type="monotone"
                    dataKey="bucketPercent"
                    name={t('accounts.quotaDistributionBucketPercent')}
                    stroke="var(--color-foreground)"
                    strokeWidth={2.5}
                    dot={{ r: 3, fill: 'var(--color-card)', stroke: 'var(--color-foreground)', strokeWidth: 2 }}
                    activeDot={{ r: 5 }}
                  />
                </ComposedChart>
              </ResponsiveContainer>
            ) : (
              <div className="flex h-full items-center justify-center rounded-lg border border-dashed border-border bg-muted/20 px-4 text-center text-sm text-muted-foreground">
                {distribution.total > 0
                  ? t('accounts.quotaDistributionNoSample')
                  : t('accounts.quotaDistributionEmpty')}
              </div>
            )}
          </div>

          <div className={compact ? 'grid grid-cols-2 gap-2 sm:grid-cols-3 2xl:grid-cols-6' : 'grid grid-cols-2 gap-2 sm:grid-cols-3 xl:grid-cols-2'}>
            <QuotaMetric label={t('accounts.quotaDistributionEligible')} value={distribution.total} compact={compact} />
            <QuotaMetric label={t('accounts.quotaDistributionSampled')} value={distribution.sampled} compact={compact} />
            <QuotaMetric label={t('accounts.quotaDistributionUnsampled')} value={distribution.unsampled} tone={distribution.unsampled > 0 ? 'warning' : 'neutral'} compact={compact} />
            <QuotaMetric label={t('accounts.quotaDistributionHighUsage')} value={distribution.highUsage} tone={distribution.highUsage > 0 ? 'danger' : 'neutral'} compact={compact} />
            <QuotaMetric label={t('accounts.quotaDistributionExhausted')} value={distribution.exhausted} tone={distribution.exhausted > 0 ? 'danger' : 'neutral'} compact={compact} />
            <QuotaMetric
              label={t('accounts.quotaDistributionAverageUsed')}
              value={distribution.averageUsed === null ? '-' : `${distribution.averageUsed.toFixed(1)}%`}
              tone={getAverageUsedTone(distribution.averageUsed)}
              compact={compact}
            />
          </div>
        </div>
      </CardContent>
    </Card>
  )
}

function QuotaMetric({ label, value, tone = 'neutral', compact = false }: { label: string; value: number | string; tone?: 'neutral' | 'warning' | 'danger' | 'success'; compact?: boolean }) {
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

function normalizePlanType(planType?: string): string {
  const raw = (planType || '').toLowerCase().trim()
  if (raw === 'prolite' || raw === 'pro_lite' || raw === 'pro-lite') return 'pro'
  return raw
}

// Plans that carry a rolling 5h usage window (mirrors Go isPremium5hPlan).
function isPremiumUsagePlan(planType?: string): boolean {
  return ['plus', 'pro', 'team', 'teamplus', 'k12', 'edu', 'education', 'go'].includes(normalizePlanType(planType))
}

function isEligibleForQuotaWindow(account: AccountRow, windowKey: QuotaWindow): boolean {
  const status = (account.status || '').toLowerCase()
  if (status === 'unauthorized' || account.openai_responses_api) {
    return false
  }
  if (windowKey === '5h') {
    return isPremiumUsagePlan(account.plan_type)
  }
  return true
}

function findBucketIndex(used: number): number {
  const value = clamp(used, 0, 100)
  const index = quotaBuckets.findIndex((bucket) => {
    if (bucket.max === 100) {
      return value >= bucket.min && value <= bucket.max
    }
    return value >= bucket.min && value < bucket.max
  })
  return index >= 0 ? index : 0
}

function getAverageUsedTone(value: number | null): 'neutral' | 'warning' | 'danger' | 'success' {
  if (value === null) return 'neutral'
  if (value >= 90) return 'danger'
  if (value >= 70) return 'warning'
  if (value < 30) return 'success'
  return 'neutral'
}

function clamp(value: number, min: number, max: number): number {
  return Math.min(max, Math.max(min, value))
}
