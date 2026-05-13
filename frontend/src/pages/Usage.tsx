import { useCallback, useEffect, useRef, useState, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'
import { Cell, Pie, PieChart, ResponsiveContainer, Tooltip as RechartsTooltip } from 'recharts'
import { api } from '../api'
import { getTimeRangeISO, type TimeRangeKey } from '../lib/timeRange'
import PageHeader from '../components/PageHeader'
import Pagination from '../components/Pagination'
import StateShell from '../components/StateShell'
import ToastNotice from '../components/ToastNotice'
import { useDataLoader } from '../hooks/useDataLoader'
import { useConfirmDialog } from '../hooks/useConfirmDialog'
import { useToast } from '../hooks/useToast'
import type { APIKeyRow, UsageAPIKeyStat, UsageEndpointStat, UsageFeatureStats, UsageLog, UsageModelStat, UsageStats } from '../types'
import { formatCompactEmail } from '../lib/utils'
import { formatBeijingTime } from '../utils/time'
import { Card, CardContent } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from '@/components/ui/tooltip'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { Activity, Box, Clock, Zap, AlertTriangle, Search, Brain, DatabaseZap, X, Image as ImageIcon, Info, CircleDollarSign, BarChart3, KeyRound, Route, SlidersHorizontal } from 'lucide-react'
import { Input } from '@/components/ui/input'
import { Select } from '@/components/ui/select'

function formatTokens(value?: number | null): string {
  if (value === undefined || value === null) return '0'
  return value.toLocaleString()
}

function getStatusBadgeClassName(statusCode: number): string {
  if (statusCode === 200) {
    return 'border-transparent bg-emerald-500/14 text-emerald-600 dark:bg-emerald-500/20 dark:text-emerald-300'
  }
  if (statusCode === 401) {
    return 'border-transparent bg-red-500/14 text-red-600 dark:bg-red-500/20 dark:text-red-300'
  }
  if (statusCode === 429) {
    return 'border-transparent bg-amber-500/14 text-amber-600 dark:bg-amber-500/20 dark:text-amber-300'
  }
  if (statusCode >= 500) {
    return 'border-transparent bg-red-500/14 text-red-600 dark:bg-red-500/20 dark:text-red-300'
  }
  if (statusCode >= 400) {
    return 'border-transparent bg-amber-500/14 text-amber-600 dark:bg-amber-500/20 dark:text-amber-300'
  }
  return 'border-transparent bg-slate-500/14 text-slate-600 dark:bg-slate-500/20 dark:text-slate-300'
}

const TIME_RANGE_OPTIONS: TimeRangeKey[] = ['1h', '6h', '24h', '7d', '30d']

function formatAPIKeyOptionLabel(apiKey: APIKeyRow): string {
  return apiKey.name ? `${apiKey.name} · ${apiKey.key}` : apiKey.key
}

function formatUsageAPIKeyLabel(name?: string, maskedKey?: string): string {
  const trimmedName = name?.trim() ?? ''
  if (trimmedName) {
    return trimmedName
  }

  const trimmedKey = maskedKey?.trim() ?? ''
  if (!trimmedKey) {
    return ''
  }

  if (trimmedKey.length <= 8) {
    return trimmedKey
  }

  return `${trimmedKey.slice(0, 4)}...${trimmedKey.slice(-4)}`
}

function isImageUsageLog(log: UsageLog): boolean {
  const endpoint = log.inbound_endpoint || log.endpoint || ''
  return endpoint.includes('/images/') || log.model?.startsWith('gpt-image-') || (log.image_count ?? 0) > 0
}

function formatImageBytes(bytes?: number | null): string {
  if (!bytes || bytes <= 0) return ''
  if (bytes < 1024) return `${bytes} B`
  if (bytes < 1024 * 1024) return `${(bytes / 1024).toFixed(1)} KB`
  return `${(bytes / 1024 / 1024).toFixed(2)} MB`
}

function imageResolution(log: UsageLog): string {
  if (log.image_width > 0 && log.image_height > 0) {
    return `${log.image_width}×${log.image_height}`
  }
  return log.image_size || ''
}

function safeNumber(value?: number | null): number {
  return typeof value === 'number' && Number.isFinite(value) ? value : 0
}

function formatUSD(value?: number | null, digits = 6): string {
  return `$${safeNumber(value).toFixed(digits)}`
}

function formatCostCardValue(value?: number | null): string {
  const amount = safeNumber(value)
  if (amount >= 100) {
    return `$${amount.toLocaleString(undefined, { maximumFractionDigits: 2 })}`
  }
  if (amount >= 1) {
    return `$${amount.toFixed(2)}`
  }
  if (amount >= 0.01) {
    return `$${amount.toFixed(4)}`
  }
  return `$${amount.toFixed(6)}`
}

function formatPercent(value: number, total: number): string {
  if (total <= 0) return '0.0%'
  return `${((value / total) * 100).toFixed(1)}%`
}

function formatTokenPricePerMillion(value?: number | null): string {
  return `$${safeNumber(value).toFixed(4)} / 1M Token`
}

function UsageCostCell({ log }: { log: UsageLog }) {
  const { t } = useTranslation()
  const accountBilled = safeNumber(log.account_billed)
  const userBilled = safeNumber(log.user_billed)
  const totalCost = safeNumber(log.total_cost)
  const displayCost = userBilled > 0 ? userBilled : accountBilled
  const hasCostContext = log.status_code < 400 && (
    accountBilled > 0 ||
    userBilled > 0 ||
    totalCost > 0 ||
    log.input_tokens > 0 ||
    log.output_tokens > 0 ||
    log.cached_tokens > 0
  )

  if (!hasCostContext) {
    return <span className={`${usageTableMonoClass} text-muted-foreground`}>-</span>
  }

  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <button
          type="button"
          className="group inline-flex cursor-help items-center gap-1.5 rounded-md px-1.5 py-1 text-left transition-colors hover:bg-muted/60 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
        >
          <span className="text-[13px] font-semibold leading-none tabular-nums text-emerald-600 antialiased dark:text-emerald-400">
            {formatUSD(displayCost)}
          </span>
          <Info className="size-3.5 shrink-0 text-muted-foreground transition-colors group-hover:text-blue-500" />
        </button>
      </TooltipTrigger>
      <TooltipContent side="right" sideOffset={8} className="w-72 max-w-none whitespace-nowrap rounded-lg border border-slate-700 bg-slate-950 px-3 py-2.5 text-xs text-slate-50 shadow-xl">
        <div className="space-y-1.5">
          <div className="mb-1 text-xs font-semibold text-slate-300">{t('usage.costDetails')}</div>
          {log.input_cost > 0 && (
            <CostTooltipRow label={t('usage.inputCost')} value={formatUSD(log.input_cost)} />
          )}
          {log.output_cost > 0 && (
            <CostTooltipRow label={t('usage.outputCost')} value={formatUSD(log.output_cost)} />
          )}
          {log.cached_tokens > 0 && (
            <CostTooltipRow label={t('usage.cacheReadCost')} value={formatUSD(log.cache_read_cost)} />
          )}
          {log.input_tokens > 0 && (
            <CostTooltipRow label={t('usage.inputUnitPrice')} value={formatTokenPricePerMillion(log.input_price_per_mtoken)} valueClassName="text-sky-300" />
          )}
          {log.output_tokens > 0 && (
            <CostTooltipRow label={t('usage.outputUnitPrice')} value={formatTokenPricePerMillion(log.output_price_per_mtoken)} valueClassName="text-violet-300" />
          )}
          {log.cached_tokens > 0 && log.cache_read_price_per_mtoken > 0 && (
            <CostTooltipRow label={t('usage.cacheReadUnitPrice')} value={formatTokenPricePerMillion(log.cache_read_price_per_mtoken)} valueClassName="text-cyan-300" />
          )}
        </div>
      </TooltipContent>
    </Tooltip>
  )
}

function CostTooltipRow({ label, value, valueClassName = 'font-medium text-white' }: { label: string; value: string; valueClassName?: string }) {
  return (
    <div className="flex items-center justify-between gap-6">
      <span className="text-slate-400">{label}</span>
      <span className={`font-geist-mono tabular-nums ${valueClassName}`}>{value}</span>
    </div>
  )
}

interface ModelPieDatum {
  model: string
  value: number
  requests: number
  amount: number
  share: number
}

function buildModelPieData(stats: UsageModelStat[], useAmount: boolean, otherLabel: string): ModelPieDatum[] {
  const base = stats
    .map((item) => ({
      model: item.model || 'unknown',
      value: useAmount ? safeNumber(item.user_billed) : safeNumber(item.requests),
      requests: safeNumber(item.requests),
      amount: safeNumber(item.user_billed),
      share: 0,
    }))
    .filter((item) => item.value > 0)

  const total = base.reduce((sum, item) => sum + item.value, 0)
  if (total <= 0) return []

  const visible = base.slice(0, 4)
  const overflow = base.slice(4)
  if (overflow.length > 0) {
    visible.push({
      model: otherLabel,
      value: overflow.reduce((sum, item) => sum + item.value, 0),
      requests: overflow.reduce((sum, item) => sum + item.requests, 0),
      amount: overflow.reduce((sum, item) => sum + item.amount, 0),
      share: 0,
    })
  }

  return visible.map((item) => ({
    ...item,
    share: (item.value / total) * 100,
  }))
}

function ModelSharePie({ stats }: { stats: UsageModelStat[] }) {
  const { t } = useTranslation()
  const totalAmount = stats.reduce((sum, item) => sum + safeNumber(item.user_billed), 0)
  const totalRequests = stats.reduce((sum, item) => sum + safeNumber(item.requests), 0)
  const useAmount = totalAmount > 0
  const pieData = buildModelPieData(stats, useAmount, t('usage.modelStatsOther'))
  const centerValue = useAmount ? formatCostCardValue(totalAmount) : formatTokens(totalRequests)
  const metricLabel = useAmount ? t('usage.modelPieAmount') : t('usage.modelPieRequests')

  if (pieData.length === 0) {
    return (
      <div className={modelPieShellClass}>
        <div className="flex min-h-[150px] flex-1 items-center justify-center px-3 text-center text-sm text-muted-foreground">
          {t('usage.noModelStats')}
        </div>
      </div>
    )
  }

  return (
    <div className={modelPieShellClass}>
      <div className="mb-1.5 flex items-start justify-between gap-3">
        <div>
          <div className="text-[13px] font-semibold text-foreground">{t('usage.modelPieTitle')}</div>
          <div className="mt-0.5 text-xs text-muted-foreground">{metricLabel}</div>
        </div>
      </div>
      <div className="relative h-[150px] max-xl:h-[140px]">
        <ResponsiveContainer width="100%" height="100%">
          <PieChart>
            <Pie
              data={pieData}
              dataKey="value"
              nameKey="model"
              cx="50%"
              cy="50%"
              innerRadius="54%"
              outerRadius="78%"
              paddingAngle={2}
              stroke="var(--color-card)"
              strokeWidth={2}
            >
              {pieData.map((_, index) => (
                <Cell key={index} fill={modelPieColors[index % modelPieColors.length]} />
              ))}
            </Pie>
            <RechartsTooltip
              formatter={(value, name) => [
                useAmount ? formatCostCardValue(Number(value ?? 0)) : formatTokens(Number(value ?? 0)),
                String(name ?? ''),
              ]}
              contentStyle={{
                backgroundColor: 'var(--color-card)',
                border: '1px solid var(--color-border)',
                borderRadius: 12,
                boxShadow: '0 16px 36px rgba(15, 23, 42, 0.14)',
                fontSize: 12,
              }}
              itemStyle={{ color: 'var(--color-foreground)' }}
            />
          </PieChart>
        </ResponsiveContainer>
        <div className="pointer-events-none absolute inset-0 flex items-center justify-center">
          <div className="max-w-[112px] text-center">
            <div className="text-[11px] font-medium text-muted-foreground">{metricLabel}</div>
            <div className="mt-0.5 truncate font-geist-mono text-[13px] font-semibold tabular-nums text-foreground">
              {centerValue}
            </div>
          </div>
        </div>
      </div>
      <div className="mt-2 grid grid-cols-2 gap-x-3 gap-y-1 max-sm:grid-cols-1">
        {pieData.map((item, index) => (
          <div key={`${item.model}-${index}`} className="flex items-center gap-2 text-xs">
            <span className="size-2 shrink-0 rounded-full" style={{ background: modelPieColors[index % modelPieColors.length] }} />
            <span className="min-w-0 flex-1 truncate font-medium text-foreground" title={item.model}>{item.model}</span>
            <span className="shrink-0 font-geist-mono tabular-nums text-muted-foreground">{item.share.toFixed(1)}%</span>
          </div>
        ))}
      </div>
    </div>
  )
}

function ModelStatsPanel({ stats }: { stats: UsageModelStat[] }) {
  const { t } = useTranslation()
  const totalRequests = stats.reduce((sum, item) => sum + safeNumber(item.requests), 0)
  const maxRequests = Math.max(1, ...stats.map((item) => safeNumber(item.requests)))

  return (
    <Card className="py-0">
      <CardContent className="flex flex-col p-4">
        <div className="mb-4 flex items-start justify-between gap-3">
          <div className="min-w-0">
            <h3 className="text-base font-semibold text-foreground">{t('usage.modelStatsTitle')}</h3>
            <p className="mt-1 text-xs text-muted-foreground">{t('usage.modelStatsDesc')}</p>
          </div>
          <div className="size-10 flex shrink-0 items-center justify-center rounded-xl bg-blue-500/12 text-blue-600 dark:bg-blue-500/20 dark:text-blue-300">
            <BarChart3 className="size-[18px]" />
          </div>
        </div>

        {stats.length === 0 ? (
          <div className="rounded-lg border border-dashed border-border bg-muted/30 px-3 py-8 text-center text-sm text-muted-foreground">
            {t('usage.noModelStats')}
          </div>
        ) : (
          <div className="grid grid-cols-[minmax(0,1fr)_minmax(220px,260px)] gap-4 max-lg:grid-cols-1">
            <div className="space-y-2.5">
              {stats.slice(0, 5).map((item) => {
                const share = totalRequests > 0 ? (item.requests / totalRequests) * 100 : 0
                const width = `${Math.max(4, Math.min(100, (item.requests / maxRequests) * 100))}%`
                return (
                  <div key={item.model} className="space-y-1">
                    <div className="flex items-start justify-between gap-3">
                      <div className="min-w-0">
                        <div className="truncate font-geist-mono text-[13px] font-semibold leading-tight text-foreground" title={item.model}>
                          {item.model}
                        </div>
                        <div className="mt-0.5 flex flex-wrap items-center gap-x-2.5 gap-y-0.5 text-xs text-muted-foreground">
                          <span>{t('usage.modelStatsRequests')}: {formatTokens(item.requests)}</span>
                          <span>{t('usage.modelStatsTokens')}: {formatTokens(item.tokens)}</span>
                          {item.error_count > 0 && (
                            <span className="text-amber-600 dark:text-amber-400">{t('usage.modelStatsErrors')}: {formatTokens(item.error_count)}</span>
                          )}
                        </div>
                      </div>
                      <div className="shrink-0 text-right">
                        <div className="font-geist-mono text-[13px] font-semibold tabular-nums text-emerald-600 dark:text-emerald-400">
                          {formatCostCardValue(item.user_billed)}
                        </div>
                        <div className="mt-0.5 text-xs text-muted-foreground">{share.toFixed(1)}%</div>
                      </div>
                    </div>
                    <div className="h-1.5 overflow-hidden rounded-full bg-muted">
                      <div className="h-full rounded-full bg-blue-500/70" style={{ width }} />
                    </div>
                  </div>
                )
              })}
            </div>
            <ModelSharePie stats={stats} />
          </div>
        )}
      </CardContent>
    </Card>
  )
}

function FeatureStatsPanel({ stats, totalRequests }: { stats?: UsageFeatureStats; totalRequests: number }) {
  const { t } = useTranslation()
  const safeStats = stats ?? {
    stream_requests: 0,
    sync_requests: 0,
    fast_requests: 0,
    cache_hit_requests: 0,
    reasoning_requests: 0,
    image_requests: 0,
    retry_requests: 0,
    error_requests: 0,
  }
  const items = [
    { label: t('usage.featureStream'), value: safeStats.stream_requests, color: '#6366f1' },
    { label: t('usage.featureSync'), value: safeStats.sync_requests, color: '#64748b' },
    { label: t('usage.featureFast'), value: safeStats.fast_requests, color: '#3b82f6' },
    { label: t('usage.featureCache'), value: safeStats.cache_hit_requests, color: '#06b6d4' },
    { label: t('usage.featureReasoning'), value: safeStats.reasoning_requests, color: '#f59e0b' },
    { label: t('usage.featureImage'), value: safeStats.image_requests, color: '#d946ef' },
    { label: t('usage.featureRetry'), value: safeStats.retry_requests, color: '#f97316' },
    { label: t('usage.featureError'), value: safeStats.error_requests, color: '#ef4444' },
  ]

  return (
    <Card className="py-0">
      <CardContent className="flex h-full flex-col p-4">
        <div className="mb-4 flex items-start justify-between gap-3">
          <div className="min-w-0">
            <h3 className="text-base font-semibold text-foreground">{t('usage.featureStatsTitle')}</h3>
            <p className="mt-1 text-xs text-muted-foreground">{t('usage.featureStatsDesc')}</p>
          </div>
          <div className="flex size-10 shrink-0 items-center justify-center rounded-xl bg-cyan-500/12 text-cyan-600 dark:bg-cyan-500/20 dark:text-cyan-300">
            <Activity className="size-[18px]" />
          </div>
        </div>

        <div className="grid flex-1 grid-cols-2 gap-2 max-sm:grid-cols-1">
          {items.map((item) => {
            const pct = totalRequests > 0 ? (item.value / totalRequests) * 100 : 0
            return (
              <div
                key={item.label}
                className="group relative overflow-hidden rounded-lg border px-3 py-2.5 transition-colors"
                style={{
                  background: `color-mix(in srgb, ${item.color} 10%, transparent)`,
                  borderColor: `color-mix(in srgb, ${item.color} 28%, transparent)`,
                }}
              >
                <div className="flex items-center justify-between gap-2">
                  <span className="truncate text-[12px] font-medium text-foreground/80">{item.label}</span>
                  <span className="font-geist-mono text-[10px] font-semibold tabular-nums text-foreground/60">
                    {pct.toFixed(1)}%
                  </span>
                </div>
                <div className="mt-0.5 font-geist-mono text-[20px] font-bold leading-tight tabular-nums text-foreground">
                  {formatTokens(item.value)}
                </div>
                <div className="mt-1.5 h-[3px] overflow-hidden rounded-full bg-foreground/5">
                  <div
                    className="h-full rounded-full transition-all"
                    style={{ width: `${Math.min(100, pct)}%`, background: item.color }}
                  />
                </div>
              </div>
            )
          })}
        </div>
      </CardContent>
    </Card>
  )
}

function EndpointStatsPanel({ stats, totalRequests }: { stats: UsageEndpointStat[]; totalRequests: number }) {
  const { t } = useTranslation()
  return (
    <DistributionPanel
      title={t('usage.endpointStatsTitle')}
      description={t('usage.endpointStatsDesc')}
      emptyText={t('usage.noEndpointStats')}
      icon={<Route className="size-[18px]" />}
      items={stats.map((item) => ({
        key: item.endpoint,
        label: item.endpoint,
        requests: item.requests,
        tokens: item.tokens,
        errors: item.error_count,
      }))}
      totalRequests={totalRequests}
    />
  )
}

function APIKeyStatsPanel({ stats, totalRequests }: { stats: UsageAPIKeyStat[]; totalRequests: number }) {
  const { t } = useTranslation()
  return (
    <DistributionPanel
      title={t('usage.apiKeyStatsTitle')}
      description={t('usage.apiKeyStatsDesc')}
      emptyText={t('usage.noApiKeyStats')}
      icon={<KeyRound className="size-[18px]" />}
      items={stats.map((item) => ({
        key: `${item.api_key_id}-${item.label}`,
        label: item.label,
        requests: item.requests,
        tokens: item.tokens,
        errors: item.error_count,
      }))}
      limit={3}
      totalRequests={totalRequests}
    />
  )
}

function DistributionPanel({
  title,
  description,
  emptyText,
  icon,
  items,
  limit = 6,
  totalRequests,
}: {
  title: string
  description: string
  emptyText: string
  icon: ReactNode
  items: Array<{ key: string; label: string; requests: number; tokens: number; errors: number }>
  limit?: number
  totalRequests: number
}) {
  const { t } = useTranslation()
  const maxRequests = Math.max(1, ...items.map((item) => safeNumber(item.requests)))

  return (
    <Card className="h-full py-0">
      <CardContent className="flex h-full flex-col p-4">
        <div className="mb-4 flex items-start justify-between gap-3">
          <div className="min-w-0">
            <h3 className="text-base font-semibold text-foreground">{title}</h3>
            <p className="mt-1 text-xs text-muted-foreground">{description}</p>
          </div>
          <div className="size-10 flex shrink-0 items-center justify-center rounded-xl bg-muted text-foreground">
            {icon}
          </div>
        </div>

        {items.length === 0 ? (
          <div className="flex min-h-[150px] flex-1 items-center justify-center rounded-lg border border-dashed border-border bg-muted/20 px-3 text-center text-sm text-muted-foreground">
            {emptyText}
          </div>
        ) : (
          <div className="space-y-3">
            {items.slice(0, limit).map((item) => {
              const width = `${Math.max(5, Math.min(100, (safeNumber(item.requests) / maxRequests) * 100))}%`
              return (
                <div key={item.key} className="space-y-1.5">
                  <div className="flex items-start justify-between gap-3">
                    <div className="min-w-0">
                      <div className="truncate font-geist-mono text-[13px] font-semibold text-foreground" title={item.label}>
                        {item.label}
                      </div>
                      <div className="mt-0.5 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground">
                        <span>{t('usage.modelStatsRequests')}: {formatTokens(item.requests)}</span>
                        <span>{t('usage.modelStatsTokens')}: {formatTokens(item.tokens)}</span>
                        {item.errors > 0 && (
                          <span className="text-amber-600 dark:text-amber-400">{t('usage.modelStatsErrors')}: {formatTokens(item.errors)}</span>
                        )}
                      </div>
                    </div>
                    <span className="shrink-0 font-geist-mono text-xs tabular-nums text-muted-foreground">
                      {formatPercent(item.requests, totalRequests)}
                    </span>
                  </div>
                  <div className="h-2 overflow-hidden rounded-full bg-muted">
                    <div className="h-full rounded-full bg-emerald-500/70" style={{ width }} />
                  </div>
                </div>
              )
            })}
          </div>
        )}
      </CardContent>
    </Card>
  )
}

function ImageUsageBadge({ log }: { log: UsageLog }) {
  const { t } = useTranslation()
  const rows = [
    { label: t('usage.imageTooltipCount'), value: log.image_count > 0 ? String(log.image_count) : '' },
    { label: t('usage.imageTooltipResolution'), value: imageResolution(log) },
    { label: t('usage.imageTooltipBytes'), value: formatImageBytes(log.image_bytes) },
    { label: t('usage.imageTooltipFormat'), value: log.image_format?.toUpperCase() || '' },
    { label: t('usage.imageTooltipRequestSize'), value: log.image_size || '' },
  ].filter((row) => row.value)
  const title = rows.length > 0
    ? rows.map((row) => `${row.label}: ${row.value}`).join('\n')
    : t('usage.imageTooltipNoDetails')

  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span
          aria-label={title}
          tabIndex={0}
          className="inline-flex w-fit shrink-0 cursor-help items-center justify-center gap-0.5 rounded-full border border-transparent bg-cyan-500/12 px-2 py-0.5 text-[11px] font-semibold whitespace-nowrap text-cyan-700 transition-colors dark:bg-cyan-500/20 dark:text-cyan-300 [&>svg]:pointer-events-none [&>svg]:size-3"
        >
          <ImageIcon className="size-3" />
          {t('usage.imageRequest')}
        </span>
      </TooltipTrigger>
      <TooltipContent side="top" sideOffset={6} className="max-w-64 p-2.5">
        <div className="space-y-1.5">
          <div className="font-semibold">{t('usage.imageTooltipTitle')}</div>
          {rows.length > 0 ? rows.map((row) => (
            <div key={row.label} className="flex min-w-44 items-center justify-between gap-4">
              <span className="text-background/70">{row.label}</span>
              <span className="font-geist-mono tabular-nums">{row.value}</span>
            </div>
          )) : (
            <div className="text-background/70">{t('usage.imageTooltipNoDetails')}</div>
          )}
        </div>
      </TooltipContent>
    </Tooltip>
  )
}

function StatusCodeBadge({ log }: { log: UsageLog }) {
  const { t } = useTranslation()
  const badge = (
    <Badge
      variant="outline"
      className={`${usageTableBadgeClass} ${getStatusBadgeClassName(log.status_code)} ${log.status_code !== 200 ? 'cursor-help ring-1 ring-inset ring-current/10' : ''}`}
    >
      {log.status_code}
    </Badge>
  )

  if (log.status_code === 200) {
    return badge
  }

  const message = log.error_message?.trim() || t('usage.statusErrorEmpty')
  const title = t('usage.statusErrorDetails')

  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <span tabIndex={0} aria-label={`${log.status_code} ${message}`} className="inline-flex focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring">
          {badge}
        </span>
      </TooltipTrigger>
      <TooltipContent side="right" sideOffset={8} className="max-w-[360px] rounded-lg border border-slate-700 bg-slate-950 px-3 py-2.5 text-xs text-slate-50 shadow-xl">
        <div className="space-y-1.5">
          <div className="font-semibold text-slate-300">{title}</div>
          <div className="font-geist-mono text-[11px] tabular-nums text-slate-400">HTTP {log.status_code}</div>
          <div className="whitespace-pre-wrap break-words leading-relaxed text-slate-50">{message}</div>
        </div>
      </TooltipContent>
    </Tooltip>
  )
}

const usageTableHeadClass = 'text-[12px] font-semibold'
const usageTableTextClass = 'text-[14px]'
const usageTableMonoClass = 'font-mono text-[13px] tabular-nums'
const usageTableBadgeClass = 'text-[13px]'
const modelPieColors = ['#2563eb', '#059669', '#f59e0b', '#dc2626', '#7c3aed', '#0891b2', '#db2777']
const modelPieShellClass = 'flex min-h-[196px] flex-col border-l border-border pl-4 max-lg:min-h-0 max-lg:border-l-0 max-lg:border-t max-lg:pl-0 max-lg:pt-3'

type UsageTableColumn = 'status' | 'model' | 'account' | 'apiKey' | 'endpoint' | 'type' | 'token' | 'cost' | 'cached' | 'firstToken' | 'duration' | 'time'

const USAGE_COLUMN_DEFINITIONS: Array<{ key: UsageTableColumn; labelKey: string }> = [
  { key: 'status', labelKey: 'usage.tableStatus' },
  { key: 'model', labelKey: 'usage.tableModel' },
  { key: 'account', labelKey: 'usage.tableAccount' },
  { key: 'apiKey', labelKey: 'usage.tableApiKey' },
  { key: 'endpoint', labelKey: 'usage.tableEndpoint' },
  { key: 'type', labelKey: 'usage.tableType' },
  { key: 'token', labelKey: 'usage.tableToken' },
  { key: 'cost', labelKey: 'usage.tableCost' },
  { key: 'cached', labelKey: 'usage.tableCached' },
  { key: 'firstToken', labelKey: 'usage.tableFirstToken' },
  { key: 'duration', labelKey: 'usage.tableDuration' },
  { key: 'time', labelKey: 'usage.tableTime' },
]

const USAGE_VISIBLE_COLUMNS_KEY = 'codex2api:usage:visible-columns'
const DEFAULT_USAGE_VISIBLE_COLUMNS: Record<UsageTableColumn, boolean> = {
  status: true,
  model: true,
  account: true,
  apiKey: true,
  endpoint: true,
  type: true,
  token: true,
  cost: true,
  cached: true,
  firstToken: true,
  duration: true,
  time: true,
}

function getInitialUsageVisibleColumns(): Record<UsageTableColumn, boolean> {
  try {
    const stored = localStorage.getItem(USAGE_VISIBLE_COLUMNS_KEY)
    if (stored) {
      const parsed = JSON.parse(stored)
      if (parsed && typeof parsed === 'object') {
        const defaults: Record<UsageTableColumn, boolean> = { ...DEFAULT_USAGE_VISIBLE_COLUMNS }
        for (const key of Object.keys(defaults) as UsageTableColumn[]) {
          if (key in parsed) defaults[key] = Boolean(parsed[key])
        }
        return defaults
      }
    }
  } catch { /* ignore */ }
  return { ...DEFAULT_USAGE_VISIBLE_COLUMNS }
}

function persistUsageVisibleColumns(columns: Record<UsageTableColumn, boolean>) {
  try { localStorage.setItem(USAGE_VISIBLE_COLUMNS_KEY, JSON.stringify(columns)) } catch { /* ignore */ }
}

function ColumnSettingsDropdown({
  open,
  columns,
  onOpenChange,
  onToggle,
}: {
  open: boolean
  columns: Record<UsageTableColumn, boolean>
  onOpenChange: (open: boolean) => void
  onToggle: (key: UsageTableColumn) => void
}) {
  const { t } = useTranslation()

  return (
    <div className="relative">
      <Button
        type="button"
        variant="outline"
        size="sm"
        onClick={() => onOpenChange(!open)}
        aria-haspopup="menu"
        aria-expanded={open}
      >
        <SlidersHorizontal className="size-3.5" />
        {t('accounts.columnSettings', { defaultValue: 'Columns' })}
      </Button>
      {open ? (
        <div
          role="menu"
          className="absolute right-0 z-20 mt-2 w-56 rounded-lg border border-border bg-popover p-2 text-popover-foreground shadow-lg"
        >
          <div className="mb-1 px-2 py-1 text-[11px] font-semibold uppercase text-muted-foreground">
            {t('accounts.columnSettings', { defaultValue: 'Columns' })}
          </div>
          {USAGE_COLUMN_DEFINITIONS.map((column) => (
            <label
              key={column.key}
              className="flex cursor-pointer items-center gap-2 rounded-md px-2 py-1.5 text-[13px] hover:bg-muted"
            >
              <input
                type="checkbox"
                className="size-3.5 rounded border-border"
                checked={columns[column.key]}
                onChange={() => onToggle(column.key)}
              />
              <span>{t(column.labelKey)}</span>
            </label>
          ))}
        </div>
      ) : null}
    </div>
  )
}

export default function Usage() {
  const { t } = useTranslation()
  const { toast, showToast } = useToast()
  const { confirm, confirmDialog } = useConfirmDialog()
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = useState(20)
  const [clearing, setClearing] = useState(false)
  const [timeRange, setTimeRange] = useState<TimeRangeKey>('1h')
  const [logs, setLogs] = useState<UsageLog[]>([])
  const [logsTotal, setLogsTotal] = useState(0)
  const [logsLoading, setLogsLoading] = useState(false)
  const [searchInput, setSearchInput] = useState('')
  const [searchEmail, setSearchEmail] = useState('')
  const [filterModel, setFilterModel] = useState('')
  const [filterEndpoint, setFilterEndpoint] = useState('')
  const [filterApiKeyId, setFilterApiKeyId] = useState('')
  const [filterFast, setFilterFast] = useState('')
  const [filterStream, setFilterStream] = useState<'' | 'true' | 'false'>('')
  const [apiKeys, setAPIKeys] = useState<APIKeyRow[]>([])
  const [modelOptions, setModelOptions] = useState<string[]>([])
  const [apiKeyLoadFailed, setAPIKeyLoadFailed] = useState(false)
  const showFastFilter = true
  const pageSizeOptions = [10, 20, 50, 100]
  const searchTimer = useRef<ReturnType<typeof setTimeout>>(null)
  const [visibleColumns, setVisibleColumns] = useState<Record<UsageTableColumn, boolean>>(getInitialUsageVisibleColumns)
  const [columnSettingsOpen, setColumnSettingsOpen] = useState(false)

  // 搜索防抖：输入停止 400ms 后触发查询
  const handleSearchChange = useCallback((value: string) => {
    setSearchInput(value)
    if (searchTimer.current) clearTimeout(searchTimer.current)
    searchTimer.current = setTimeout(() => {
      setSearchEmail(value)
      setPage(1)
    }, 400)
  }, [])

  // 仅加载轻量统计（秒级）
  const loadStats = useCallback(async () => {
    const stats = await api.getUsageStats()
    return { stats }
  }, [])

  const { data, loading, error, reload, reloadSilently } = useDataLoader<{
    stats: UsageStats | null
  }>({
    initialData: { stats: null },
    load: loadStats,
  })

  const loadAPIKeys = useCallback(async () => {
    try {
      const response = await api.getAPIKeys()
      setAPIKeys(response.keys ?? [])
      setAPIKeyLoadFailed(false)
    } catch {
      setAPIKeys([])
      setAPIKeyLoadFailed(true)
    }
  }, [])

  // 服务端分页加载日志
  const loadLogs = useCallback(async () => {
    setLogsLoading(true)
    try {
      const { start, end } = getTimeRangeISO(timeRange)
      const res = await api.getUsageLogsPaged({
        start, end, page, pageSize,
        email: searchEmail || undefined,
        model: filterModel || undefined,
        endpoint: filterEndpoint || undefined,
        apiKeyId: filterApiKeyId || undefined,
        fast: filterFast || undefined,
        stream: filterStream || undefined,
      })
      setLogs(res.logs ?? [])
      setLogsTotal(res.total ?? 0)
    } catch {
      // 静默容错
    } finally {
      setLogsLoading(false)
    }
  }, [timeRange, page, pageSize, searchEmail, filterModel, filterEndpoint, filterApiKeyId, filterFast, filterStream])

  // 首次加载 + timeRange/page 变更时重新拉取日志
  useEffect(() => {
    void loadLogs()
  }, [loadLogs])

  useEffect(() => {
    void loadAPIKeys()
  }, [loadAPIKeys])

  useEffect(() => {
    let active = true
    const loadModels = async () => {
      try {
        const response = await api.getModels()
        if (!active) return
        const models = response.items && response.items.length > 0
          ? response.items.filter((item) => item.enabled).map((item) => item.id)
          : response.models ?? []
        setModelOptions(models)
      } catch {
        if (active) setModelOptions([])
      }
    }
    void loadModels()
    return () => {
      active = false
    }
  }, [])

  useEffect(() => {
    const timer = window.setInterval(() => {
      void reloadSilently()
    }, 30000)
    return () => window.clearInterval(timer)
  }, [reloadSilently])

  useEffect(() => {
    persistUsageVisibleColumns(visibleColumns)
  }, [visibleColumns])

  const { stats } = data
  const totalPages = Math.max(1, Math.ceil(logsTotal / pageSize))
  const currentPage = Math.min(page, totalPages)

  useEffect(() => {
    if (page > totalPages) {
      setPage(totalPages)
    }
  }, [page, totalPages])

  const totalRequests = stats?.total_requests ?? 0
  const totalTokens = stats?.total_tokens ?? 0
  const totalPromptTokens = stats?.total_prompt_tokens ?? 0
  const totalCompletionTokens = stats?.total_completion_tokens ?? 0
  const totalAccountBilled = stats?.total_account_billed ?? 0
  const totalUserBilled = stats?.total_user_billed ?? 0
  const todayRequests = stats?.today_requests ?? 0
  const todayUserBilled = stats?.today_user_billed ?? 0
  const modelStats = stats?.model_stats ?? []
  const featureStats = stats?.feature_stats
  const endpointStats = stats?.endpoint_stats ?? []
  const apiKeyStats = stats?.api_key_stats ?? []
  const rpm = stats?.rpm ?? 0
  const tpm = stats?.tpm ?? 0
  const errorRate = stats?.error_rate ?? 0
  const avgDurationMs = stats?.avg_duration_ms ?? 0
  const successRequests = totalRequests - Math.round(totalRequests * errorRate / 100)
  const showAPIKeyFilter = !apiKeyLoadFailed && apiKeys.length > 0
  const hasActiveFilters = Boolean(searchInput || filterModel || filterEndpoint || filterApiKeyId || filterStream || filterFast)
  const apiKeyOptions = [
    { label: t('usage.allApiKeys'), value: '' },
    ...apiKeys.map((apiKey) => ({ label: formatAPIKeyOptionLabel(apiKey), value: String(apiKey.id) })),
  ]

  return (
    <StateShell
      variant="page"
      loading={loading}
      error={error}
      onRetry={() => { void reload(); void loadLogs(); void loadAPIKeys() }}
      loadingTitle={t('usage.loadingTitle')}
      loadingDescription={t('usage.loadingDesc')}
      errorTitle={t('usage.errorTitle')}
    >
      <>
        <PageHeader
          title={t('usage.title')}
          description={t('usage.description')}
          onRefresh={() => { void reload(); void loadLogs(); void loadAPIKeys() }}
        />

        <div className="space-y-6">
        {/* Stat overview: 6 metrics in a single row */}
        <div className="grid grid-cols-6 gap-3 max-xl:grid-cols-3 max-sm:grid-cols-2">
          <Card className="py-0">
            <CardContent className="flex flex-col gap-1.5 p-3">
              <div className="flex items-center justify-between gap-2">
                <span className="text-[11px] font-bold uppercase text-muted-foreground">{t('usage.totalRequestsCard')}</span>
                <div className="flex size-9 items-center justify-center rounded-lg bg-primary/12 text-primary">
                  <Activity className="size-4" />
                </div>
              </div>
              <div className="text-[22px] font-bold leading-none tabular-nums">
                {formatTokens(totalRequests)}
              </div>
              <div className="flex flex-wrap items-center gap-x-2 gap-y-0.5 text-[11px] text-muted-foreground leading-snug">
                <span className="text-[hsl(var(--success))]">● {t('usage.success')}: {formatTokens(successRequests)}</span>
                <span>● {t('usage.today')}: {formatTokens(todayRequests)}</span>
              </div>
            </CardContent>
          </Card>

          <Card className="py-0">
            <CardContent className="flex flex-col gap-1.5 p-3">
              <div className="flex items-center justify-between gap-2">
                <span className="text-[11px] font-bold uppercase text-muted-foreground">{t('usage.totalTokensCard')}</span>
                <div className="flex size-9 items-center justify-center rounded-lg bg-[hsl(var(--info-bg))] text-[hsl(var(--info))]">
                  <Box className="size-4" />
                </div>
              </div>
              <div className="text-[22px] font-bold leading-none tabular-nums">
                {formatTokens(totalTokens)}
              </div>
              <div className="flex flex-wrap items-center gap-x-2 gap-y-0.5 text-[11px] text-muted-foreground leading-snug">
                <span>{t('usage.inputTokens')}: {formatTokens(totalPromptTokens)}</span>
                <span>{t('usage.outputTokens')}: {formatTokens(totalCompletionTokens)}</span>
              </div>
            </CardContent>
          </Card>

          <Card className="py-0">
            <CardContent className="flex flex-col gap-1.5 p-3">
              <div className="flex items-center justify-between gap-2">
                <span className="text-[11px] font-bold uppercase text-muted-foreground">{t('usage.totalCostCard')}</span>
                <div className="flex size-9 items-center justify-center rounded-lg bg-emerald-500/12 text-emerald-600 dark:bg-emerald-500/20 dark:text-emerald-300">
                  <CircleDollarSign className="size-4" />
                </div>
              </div>
              <div className="text-[22px] font-bold leading-none tabular-nums text-emerald-600 dark:text-emerald-400">
                {formatCostCardValue(totalUserBilled)}
              </div>
              <div className="flex flex-wrap items-center gap-x-2 gap-y-0.5 text-[11px] text-muted-foreground leading-snug">
                <span>{t('usage.todayCost')}: {formatCostCardValue(todayUserBilled)}</span>
                <span>{t('usage.accountCost')}: {formatCostCardValue(totalAccountBilled)}</span>
              </div>
            </CardContent>
          </Card>

          <Card className="py-0">
            <CardContent className="flex flex-col gap-1.5 p-3">
              <div className="flex items-center justify-between gap-2">
                <span className="text-[11px] font-bold uppercase text-muted-foreground">RPM</span>
                <div className="flex size-9 items-center justify-center rounded-lg bg-[hsl(var(--success-bg))] text-[hsl(var(--success))]">
                  <Clock className="size-4" />
                </div>
              </div>
              <div className="text-[22px] font-bold leading-none tabular-nums">
                {Math.round(rpm)}
              </div>
              <div className="text-[11px] text-muted-foreground leading-snug">{t('usage.rpmDesc')}</div>
            </CardContent>
          </Card>

          <Card className="py-0">
            <CardContent className="flex flex-col gap-1.5 p-3">
              <div className="flex items-center justify-between gap-2">
                <span className="text-[11px] font-bold uppercase text-muted-foreground">TPM</span>
                <div className="flex size-9 items-center justify-center rounded-lg bg-destructive/12 text-destructive">
                  <Zap className="size-4" />
                </div>
              </div>
              <div className="text-[22px] font-bold leading-none tabular-nums">
                {formatTokens(tpm)}
              </div>
              <div className="text-[11px] text-muted-foreground leading-snug">{t('usage.tpmDesc')}</div>
            </CardContent>
          </Card>

          <Card className="py-0">
            <CardContent className="flex flex-col gap-1.5 p-3">
              <div className="flex items-center justify-between gap-2">
                <span className="text-[11px] font-bold uppercase text-muted-foreground">{t('usage.errorRateCard')}</span>
                <div className="flex size-9 items-center justify-center rounded-lg bg-[hsl(36_72%_40%/0.12)] text-[hsl(36,72%,40%)]">
                  <AlertTriangle className="size-4" />
                </div>
              </div>
              <div className="text-[22px] font-bold leading-none tabular-nums">
                {errorRate.toFixed(1)}%
              </div>
              <div className="text-[11px] text-muted-foreground leading-snug">{t('usage.avgLatencyInline', { value: Math.round(avgDurationMs) })}</div>
            </CardContent>
          </Card>
        </div>

        <div className="grid grid-cols-[minmax(0,0.5fr)_minmax(360px,0.5fr)] gap-3 max-lg:grid-cols-1">
          <ModelStatsPanel stats={modelStats} />
          <FeatureStatsPanel stats={featureStats} totalRequests={totalRequests} />
        </div>

        <div className="grid grid-cols-2 gap-3 max-lg:grid-cols-1">
          <EndpointStatsPanel stats={endpointStats} totalRequests={totalRequests} />
          <APIKeyStatsPanel stats={apiKeyStats} totalRequests={totalRequests} />
        </div>

        {/* Logs table */}
        <Card>
          <CardContent className="p-4">
            <div className="mb-4 flex items-center justify-between gap-3 overflow-x-auto">
              <div className="flex shrink-0 items-center gap-3">
                <h3 className="whitespace-nowrap text-base font-semibold text-foreground">{t('usage.requestLogs')}</h3>
                <div className="inline-flex shrink-0 rounded-lg border border-border bg-muted/50 p-0.5">
                  {TIME_RANGE_OPTIONS.map((key) => (
                    <button
                      key={key}
                      type="button"
                      onClick={() => { setTimeRange(key); setPage(1) }}
                      className={`whitespace-nowrap px-2.5 py-1 text-xs font-medium rounded-md transition-all duration-200 ${
                        timeRange === key
                          ? 'bg-background text-foreground shadow-sm border border-border'
                          : 'text-muted-foreground hover:text-foreground'
                      }`}
                    >
                      {t(`dashboard.timeRange${key.toUpperCase()}`)}
                    </button>
                  ))}
                </div>
              </div>
              <div className="flex shrink-0 items-center gap-3">
                <span className="whitespace-nowrap text-xs text-muted-foreground">{logsLoading ? t('common.loading') : t('usage.recordsCount', { count: logsTotal })}</span>
                <Button
                  variant="destructive"
                  size="sm"
                  disabled={clearing || logs.length === 0}
                  onClick={async () => {
                    const confirmed = await confirm({
                      title: t('usage.clearLogsTitle'),
                      description: t('usage.clearLogsDesc'),
                      confirmText: t('usage.clearLogsConfirm'),
                      tone: 'destructive',
                      confirmVariant: 'destructive',
                    })
                    if (!confirmed) return
                    setClearing(true)
                    try {
                      await api.clearUsageLogs()
                      showToast(t('usage.clearLogsSuccess'))
                      setPage(1)
                      void reload()
                      void loadLogs()
                    } catch {
                      showToast(t('usage.clearLogsFailed'), 'error')
                    } finally {
                      setClearing(false)
                    }
                  }}
                >
                  {clearing ? t('usage.clearingLogs') : t('usage.clearLogs')}
                </Button>
              </div>
            </div>

            {/* 筛选栏 */}
            <div className="toolbar-surface mb-4 flex items-center gap-2 overflow-x-auto whitespace-nowrap">
              {/* 搜索框 */}
              <div className="relative w-60 shrink-0 max-sm:w-full">
                <Search className="absolute left-2.5 top-1/2 -translate-y-1/2 size-3.5 text-muted-foreground pointer-events-none" />
                <Input
                  className="pl-8 h-8 rounded-lg text-[13px]"
                  placeholder={t('usage.searchEmail')}
                  value={searchInput}
                  onChange={(e: React.ChangeEvent<HTMLInputElement>) => handleSearchChange(e.target.value)}
                />
              </div>

              {/* 模型下拉 */}
              <Select
                className="w-36 shrink-0"
                compact
                value={filterModel}
                onValueChange={(v) => { setFilterModel(v); setPage(1) }}
                placeholder={t('usage.allModels')}
                options={[
                  { label: t('usage.allModels'), value: '' },
                  ...modelOptions.map((m) => ({ label: m, value: m })),
                ]}
              />

              {/* 端点下拉 */}
              <Select
                className="w-44 shrink-0"
                compact
                value={filterEndpoint}
                onValueChange={(v) => { setFilterEndpoint(v); setPage(1) }}
                placeholder={t('usage.allEndpoints')}
                options={[
                  { label: t('usage.allEndpoints'), value: '' },
                  { label: '/v1/chat/completions', value: '/v1/chat/completions' },
                  { label: '/v1/responses', value: '/v1/responses' },
                  { label: '/v1/images/generations', value: '/v1/images/generations' },
                  { label: '/v1/images/edits', value: '/v1/images/edits' },
                  { label: '/v1/messages', value: '/v1/messages' },
                ]}
              />

              {showAPIKeyFilter && (
                <Select
                  className="w-48 shrink-0"
                  compact
                  value={filterApiKeyId}
                  onValueChange={(v) => { setFilterApiKeyId(v); setPage(1) }}
                  placeholder={t('usage.allApiKeys')}
                  options={apiKeyOptions}
                />
              )}

              {/* 类型下拉 */}
              <Select
                className="w-28 shrink-0"
                compact
                value={filterStream}
                onValueChange={(v) => { setFilterStream(v as '' | 'true' | 'false'); setPage(1) }}
                placeholder={t('usage.allTypes')}
                options={[
                  { label: t('usage.allTypes'), value: '' },
                  { label: 'Stream', value: 'true' },
                  { label: 'Sync', value: 'false' },
                ]}
              />

              {showFastFilter && (
                <button
                  type="button"
                  onClick={() => { setFilterFast(filterFast === 'true' ? '' : 'true'); setPage(1) }}
                  className={`h-8 shrink-0 px-2.5 rounded-lg border text-[13px] font-medium transition-colors inline-flex items-center gap-1 whitespace-nowrap ${
                    filterFast === 'true'
                      ? 'border-blue-500/40 bg-blue-500/12 text-blue-600 dark:bg-blue-500/20 dark:text-blue-400'
                      : 'border-border bg-background text-muted-foreground hover:text-foreground hover:bg-muted/50'
                  }`}
                >
                  <Zap className="size-3.5" />
                  Fast
                </button>
              )}

              {/* 清除筛选 */}
              {hasActiveFilters && (
                <button
                  type="button"
                  onClick={() => {
                    setSearchInput(''); setSearchEmail('')
                    setFilterModel(''); setFilterEndpoint('')
                    setFilterApiKeyId('')
                    setFilterStream(''); setFilterFast('')
                    setPage(1)
                  }}
                  className="h-8 shrink-0 px-2.5 rounded-lg border border-border bg-background text-[13px] text-muted-foreground hover:text-foreground hover:bg-muted/50 transition-colors inline-flex items-center gap-1 whitespace-nowrap"
                >
                  <X className="size-3.5" />
                  {t('usage.clearFilters')}
                </button>
              )}

              <div className="ml-auto shrink-0">
                <ColumnSettingsDropdown
                  open={columnSettingsOpen}
                  columns={visibleColumns}
                  onOpenChange={setColumnSettingsOpen}
                  onToggle={(key) => setVisibleColumns((current) => ({ ...current, [key]: !current[key] }))}
                />
              </div>
            </div>

            <StateShell
              variant="section"
              isEmpty={logs.length === 0}
              emptyTitle={t('usage.emptyTitle')}
              emptyDescription={hasActiveFilters ? t('usage.emptyFilteredDesc') : t('usage.emptyDesc')}
            >
              <div className="data-table-shell">
                <TooltipProvider>
                <Table>
                  <TableHeader>
                    <TableRow>
                      {visibleColumns.status && <TableHead className={usageTableHeadClass}>{t('usage.tableStatus')}</TableHead>}
                      {visibleColumns.model && <TableHead className={usageTableHeadClass}>{t('usage.tableModel')}</TableHead>}
                      {visibleColumns.account && <TableHead className={usageTableHeadClass}>{t('usage.tableAccount')}</TableHead>}
                      {visibleColumns.apiKey && <TableHead className={usageTableHeadClass}>{t('usage.tableApiKey')}</TableHead>}
                      {visibleColumns.endpoint && <TableHead className={usageTableHeadClass}>{t('usage.tableEndpoint')}</TableHead>}
                      {visibleColumns.type && <TableHead className={usageTableHeadClass}>{t('usage.tableType')}</TableHead>}
                      {visibleColumns.token && <TableHead className={usageTableHeadClass}>{t('usage.tableToken')}</TableHead>}
                      {visibleColumns.cost && <TableHead className={usageTableHeadClass}>{t('usage.tableCost')}</TableHead>}
                      {visibleColumns.cached && <TableHead className={usageTableHeadClass}>{t('usage.tableCached')}</TableHead>}
                      {visibleColumns.firstToken && <TableHead className={usageTableHeadClass}>{t('usage.tableFirstToken')}</TableHead>}
                      {visibleColumns.duration && <TableHead className={usageTableHeadClass}>{t('usage.tableDuration')}</TableHead>}
                      {visibleColumns.time && <TableHead className={usageTableHeadClass}>{t('usage.tableTime')}</TableHead>}
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {logs.map((log: UsageLog) => {
                      return (
                      <TableRow key={log.id}>
                        {visibleColumns.status && <TableCell>
                          <StatusCodeBadge log={log} />
                        </TableCell>}
                        {visibleColumns.model && <TableCell>
                          <div className="flex items-center gap-1.5 flex-wrap">
                            <Badge variant="outline" className={usageTableBadgeClass}>
                              {log.model || '-'}
                            </Badge>
                            {log.effective_model && log.effective_model !== log.model && (
                              <Badge variant="outline" className="text-[11px] font-medium border-transparent bg-blue-500/10 text-blue-600 dark:bg-blue-500/20 dark:text-blue-400">
                                → {log.effective_model}
                              </Badge>
                            )}
                            {log.reasoning_effort && (
                              <Badge
                                variant="outline"
                                className={`text-[11px] font-medium border-transparent ${
                                  log.reasoning_effort === 'xhigh' || log.reasoning_effort === 'high'
                                    ? 'bg-red-500/12 text-red-600 dark:bg-red-500/20 dark:text-red-400'
                                    : log.reasoning_effort === 'medium'
                                      ? 'bg-amber-500/12 text-amber-600 dark:bg-amber-500/20 dark:text-amber-400'
                                      : 'bg-emerald-500/12 text-emerald-600 dark:bg-emerald-500/20 dark:text-emerald-400'
                                }`}
                              >
                                {log.reasoning_effort}
                              </Badge>
                            )}
                            {isImageUsageLog(log) && (
                              <ImageUsageBadge log={log} />
                            )}
                            {(log.service_tier === 'fast' || log.service_tier === 'priority') && (
                              <Badge
                                variant="outline"
                                className="text-[11px] font-semibold gap-0.5 border-transparent bg-blue-500/12 text-blue-600 dark:bg-blue-500/20 dark:text-blue-400"
                              >
                                <Zap className="size-3" />
                                Fast
                              </Badge>
                            )}
                          </div>
                        </TableCell>}
                        {visibleColumns.account && <TableCell className={`${usageTableTextClass} text-muted-foreground`}>
                          {formatCompactEmail(log.account_email)}
                        </TableCell>}
                        {visibleColumns.apiKey && <TableCell className={`${usageTableTextClass} text-muted-foreground`}>
                          <span className="block max-w-[180px] truncate whitespace-nowrap font-mono text-[12px]" title={formatUsageAPIKeyLabel(log.api_key_name, log.api_key_masked) || t('usage.unknownApiKey')}>
                            {formatUsageAPIKeyLabel(log.api_key_name, log.api_key_masked) || t('usage.unknownApiKey')}
                          </span>
                        </TableCell>}
                        {visibleColumns.endpoint && <TableCell>
                          <div className={`${usageTableMonoClass} leading-relaxed`}>
                            <span className="text-muted-foreground">
                              {log.inbound_endpoint || log.endpoint || '-'}
                            </span>
                            {log.upstream_endpoint && log.upstream_endpoint !== log.inbound_endpoint && (
                              <span className="text-muted-foreground"> → {log.upstream_endpoint}</span>
                            )}
                          </div>
                        </TableCell>}
                        {visibleColumns.type && <TableCell>
                          <Badge
                            variant="outline"
                            className={usageTableBadgeClass}
                            style={{
                              background: log.stream ? 'rgba(99, 102, 241, 0.12)' : 'rgba(107, 114, 128, 0.12)',
                              color: log.stream ? '#6366f1' : '#6b7280',
                              borderColor: 'transparent',
                            }}
                          >
                            {log.stream ? 'stream' : 'sync'}
                          </Badge>
                        </TableCell>}
                        {visibleColumns.token && <TableCell>
                          {log.status_code < 400 && (log.input_tokens > 0 || log.output_tokens > 0) ? (
                            <div className={`${usageTableMonoClass} leading-relaxed`}>
                              <span className="text-blue-500">↓{formatTokens(log.input_tokens)}</span>
                              <span className="mx-1 text-border">|</span>
                              <span className="text-emerald-500">↑{formatTokens(log.output_tokens)}</span>
                              {log.reasoning_tokens > 0 && (
                                <>
                                  <span className="mx-1 text-border">|</span>
                                  <span className="text-amber-500 inline-flex items-center gap-0.5"><Brain className="size-3.5 inline" />{formatTokens(log.reasoning_tokens)}</span>
                                </>
                              )}
                            </div>
                          ) : (
                            <span className={`${usageTableMonoClass} text-muted-foreground`}>-</span>
                          )}
                        </TableCell>}
                        {visibleColumns.cost && <TableCell>
                          <UsageCostCell log={log} />
                        </TableCell>}
                        {visibleColumns.cached && <TableCell>
                          {log.cached_tokens > 0 ? (
                            <Badge variant="outline" className={`${usageTableBadgeClass} gap-1 border-transparent bg-indigo-500/10 text-indigo-600 dark:bg-indigo-500/20 dark:text-indigo-400`}>
                              <DatabaseZap className="size-3.5" />
                              {formatTokens(log.cached_tokens)}
                            </Badge>
                          ) : (
                            <span className={`${usageTableMonoClass} text-muted-foreground`}>-</span>
                          )}
                        </TableCell>}
                        {visibleColumns.firstToken && <TableCell>
                          {log.first_token_ms > 0 ? (
                            <span className={`${usageTableMonoClass} ${log.first_token_ms > 5000 ? 'text-red-500' : log.first_token_ms > 2000 ? 'text-amber-500' : 'text-emerald-500'}`}>
                              {log.first_token_ms > 1000 ? `${(log.first_token_ms / 1000).toFixed(1)}s` : `${log.first_token_ms}ms`}
                            </span>
                          ) : <span className={`${usageTableMonoClass} text-muted-foreground`}>-</span>}
                        </TableCell>}
                        {visibleColumns.duration && <TableCell>
                          <span className={`${usageTableMonoClass} ${log.duration_ms > 30000 ? 'text-red-500' : log.duration_ms > 10000 ? 'text-amber-500' : 'text-muted-foreground'}`}>
                            {log.duration_ms > 1000 ? `${(log.duration_ms / 1000).toFixed(1)}s` : `${log.duration_ms}ms`}
                          </span>
                        </TableCell>}
                        {visibleColumns.time && <TableCell className={`${usageTableMonoClass} text-muted-foreground whitespace-nowrap`}>
                          {formatBeijingTime(log.created_at)}
                        </TableCell>}
                      </TableRow>
                      )
                    })}
                  </TableBody>
                </Table>
                </TooltipProvider>
              </div>
              <Pagination
                page={currentPage}
                totalPages={totalPages}
                onPageChange={setPage}
                totalItems={logsTotal}
                pageSize={pageSize}
                pageSizeOptions={pageSizeOptions}
                onPageSizeChange={(nextPageSize) => {
                  setPageSize(nextPageSize)
                  setPage(1)
                }}
              />
            </StateShell>
          </CardContent>
        </Card>
        </div>

        <ToastNotice toast={toast} />
        {confirmDialog}
      </>
    </StateShell>
  )
}
