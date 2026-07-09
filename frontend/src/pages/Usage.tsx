import { useCallback, useEffect, useLayoutEffect, useRef, useState, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'
import { createPortal } from 'react-dom'
import { Cell, Pie, PieChart, ResponsiveContainer, Tooltip as RechartsTooltip } from 'recharts'
import { api } from '../api'
import { getTimeRangeISO, type TimeRangeKey } from '../lib/timeRange'
import PageHeader from '../components/PageHeader'
import Pagination from '../components/Pagination'
import Modal from '../components/Modal'
import StateShell from '../components/StateShell'
import { useDataLoader } from '../hooks/useDataLoader'
import { useConfirmDialog } from '../hooks/useConfirmDialog'
import { useToast } from '../hooks/useToast'
import { DEFAULT_PAGE_SIZE_OPTIONS, usePersistedPageSize } from '../hooks/usePersistedPageSize'
import type { APIKeyRow, SystemSettings, UsageAPIKeyStat, UsageEndpointStat, UsageFeatureStats, UsageLog, UsageModelStat, UsageStats, PromptFilterLog } from '../types'
import { formatCompactEmail } from '../lib/utils'
import { formatUsageNumber as formatTokens } from '../lib/usageFormat'
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
import { Activity, Box, Clock, Zap, AlertTriangle, Search, Brain, DatabaseZap, X, Image as ImageIcon, Info, CircleDollarSign, BarChart3, KeyRound, Route, SlidersHorizontal, ShieldAlert } from 'lucide-react'
import { Input } from '@/components/ui/input'
import { Select } from '@/components/ui/select'

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

type UsagePresetRangeKey = 'today' | TimeRangeKey
const USAGE_TIME_RANGE_OPTIONS: UsagePresetRangeKey[] = ['today', '1h', '6h', '24h', '7d', '30d']

// 本页面局部的"自定义"区间标记。不污染全局 TimeRangeKey 类型 (Dashboard 等仍只识别预设档)。
type UsageTimeRangeKey = UsagePresetRangeKey | 'custom'
interface CustomRange {
  start: string // RFC3339 with offset
  end: string
}
const CUSTOM_RANGE_MAX_DAYS = 90
const CUSTOM_RANGE_MAX_MS = CUSTOM_RANGE_MAX_DAYS * 24 * 60 * 60 * 1000

// datetime-local input 的字面值 ↔ Date 转换。input 本身没有时区,按本地时间解释。
function dateToLocalInputValue(date: Date): string {
  const pad = (n: number) => String(n).padStart(2, '0')
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`
}
function localInputValueToDate(value: string): Date | null {
  if (!value) return null
  const d = new Date(value)
  return Number.isNaN(d.getTime()) ? null : d
}
function dateToLocalRFC3339(date: Date): string {
  const pad = (n: number) => String(n).padStart(2, '0')
  const offset = date.getTimezoneOffset()
  const sign = offset <= 0 ? '+' : '-'
  const absOffset = Math.abs(offset)
  return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}${sign}${pad(Math.floor(absOffset / 60))}:${pad(absOffset % 60)}`
}

function getTodayRangeISO(): { start: string; end: string } {
  const now = new Date()
  const start = new Date(now)
  start.setHours(0, 0, 0, 0)
  return { start: dateToLocalRFC3339(start), end: dateToLocalRFC3339(now) }
}

function resolveRangeISO(
  range: UsageTimeRangeKey,
  custom: CustomRange | null,
): { start: string; end: string } {
  if (range === 'custom' && custom) {
    return { start: custom.start, end: custom.end }
  }
  if (range === 'today') {
    return getTodayRangeISO()
  }
  return getTimeRangeISO((range === 'custom' ? '24h' : range) as TimeRangeKey)
}

function getInitialUsageSearchParams(): URLSearchParams {
  if (typeof window === 'undefined') return new URLSearchParams()
  return new URLSearchParams(window.location.search)
}

function getInitialUsageAccountID(): string {
  const raw = getInitialUsageSearchParams().get('account_id') || ''
  return /^\d+$/.test(raw) ? raw : ''
}

function getInitialUsageRange(): UsageTimeRangeKey {
  const params = getInitialUsageSearchParams()
  const range = params.get('range') || ''
  if (range === 'custom' && params.get('days')) return 'custom'
  if (range === '7d' || range === '30d') return range
  return 'today'
}

function getInitialUsageCustomRange(): CustomRange | null {
  const params = getInitialUsageSearchParams()
  const days = Number(params.get('days') || 0)
  if (params.get('range') !== 'custom' || !Number.isFinite(days) || days <= 0 || days > CUSTOM_RANGE_MAX_DAYS) {
    return null
  }
  const end = new Date()
  const start = new Date(end)
  start.setDate(start.getDate() - days)
  return {
    start: dateToLocalRFC3339(start),
    end: dateToLocalRFC3339(end),
  }
}

const USAGE_ANALYSIS_VISIBILITY_KEY = 'usage_analysis_visible'
const usageStatCardContentClass = 'flex min-w-0 flex-col gap-1.5 p-3'
const usageStatValueClass = 'min-w-0 break-words text-[20px] font-bold leading-tight tabular-nums sm:text-[22px]'

function getInitialAnalysisVisibility(): boolean {
  try {
    return window.localStorage.getItem(USAGE_ANALYSIS_VISIBILITY_KEY) !== 'false'
  } catch {
    return true
  }
}

function persistAnalysisVisibility(visible: boolean) {
  try {
    window.localStorage.setItem(USAGE_ANALYSIS_VISIBILITY_KEY, visible ? 'true' : 'false')
  } catch {}
}

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

function formatUsageAccountLabel(log: UsageLog): string {
  // 邮箱优先：身份账号一律显示邮箱，账号名仅作为无邮箱账号（如 relay API-key 账号）的兜底。
  // 避免 AT 导入未命名时的占位名（at-account-N 等）盖过真实邮箱身份。
  const accountEmail = log.account_email?.trim()
  if (accountEmail) {
    return formatCompactEmail(accountEmail)
  }

  const accountName = log.account_name?.trim()
  if (accountName) {
    return accountName
  }

  return log.account_id > 0 ? `ID ${log.account_id}` : '-'
}

function formatUsageAccountTitle(log: UsageLog): string {
  const accountEmail = log.account_email?.trim()
  const accountName = log.account_name?.trim()
  if (accountEmail && accountName && accountEmail !== accountName) {
    return `${accountEmail} · ${accountName}`
  }
  return accountEmail || accountName || (log.account_id > 0 ? `ID ${log.account_id}` : '-')
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

function isFastTier(tier?: string | null): boolean {
  const normalized = (tier || '').trim().toLowerCase()
  return normalized === 'fast' || normalized === 'priority'
}

function formatServiceTierLabel(t: ReturnType<typeof useTranslation>['t'], tier?: string | null): string {
  const normalized = (tier || '').trim().toLowerCase()
  if (!normalized) return '-'
  if (isFastTier(normalized)) return t('usage.billingTierFast')
  if (normalized === 'default') return t('usage.billingTierStandard')
  return normalized
}

function UsageCostCell({ log }: { log: UsageLog }) {
  const { t } = useTranslation()
  const accountBilled = safeNumber(log.account_billed)
  const userBilled = safeNumber(log.user_billed)
  const totalCost = safeNumber(log.total_cost)
  const displayCost = userBilled > 0 ? userBilled : accountBilled
  const longContextThreshold = safeNumber(log.long_context_threshold)
  const requestedTier = log.requested_service_tier || ''
  const actualTier = log.actual_service_tier || log.service_tier || ''
  const billingTier = log.billing_service_tier || log.service_tier || ''
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
      <TooltipContent side="right" sideOffset={8} className="w-96 max-w-none whitespace-nowrap rounded-lg border border-slate-700 bg-slate-950 px-3 py-2.5 text-xs text-slate-50 shadow-xl">
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
          {requestedTier && (
            <CostTooltipRow label={t('usage.requestedTier')} value={formatServiceTierLabel(t, requestedTier)} valueClassName="text-slate-200" />
          )}
          {actualTier && (
            <CostTooltipRow
              label={t('usage.actualTier')}
              value={formatServiceTierLabel(t, actualTier)}
              valueClassName={isFastTier(actualTier) ? 'text-amber-300' : 'text-slate-200'}
            />
          )}
          <CostTooltipRow
            label={t('usage.billingTier')}
            value={formatServiceTierLabel(t, billingTier)}
            valueClassName={isFastTier(billingTier) ? 'text-amber-300' : 'text-slate-200'}
          />
          {log.long_context && longContextThreshold > 0 && (
            <CostTooltipRow
              label={t('usage.billingContext')}
              value={t('usage.billingContextLong', {
                input: formatTokens(log.input_tokens, true),
                threshold: formatTokens(longContextThreshold, true),
              })}
              valueClassName="text-orange-300"
            />
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

function ModelSharePie({
  stats,
  showFullUsageNumbers,
}: {
  stats: UsageModelStat[]
  showFullUsageNumbers: boolean
}) {
  const { t } = useTranslation()
  const totalAmount = stats.reduce((sum, item) => sum + safeNumber(item.user_billed), 0)
  const totalRequests = stats.reduce((sum, item) => sum + safeNumber(item.requests), 0)
  const useAmount = totalAmount > 0
  const pieData = buildModelPieData(stats, useAmount, t('usage.modelStatsOther'))
  const centerValue = useAmount ? formatCostCardValue(totalAmount) : formatTokens(totalRequests, showFullUsageNumbers)
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
      <div className="mb-1.5 flex items-baseline justify-between gap-3">
        <div className="text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">{t('usage.modelPieTitle')}</div>
        <div className="text-[11px] font-medium text-muted-foreground/80">{metricLabel}</div>
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
              outerRadius="84%"
              paddingAngle={0}
              strokeWidth={0}
            >
              {pieData.map((_, index) => (
                <Cell key={index} fill={modelPieColors[index % modelPieColors.length]} />
              ))}
            </Pie>
            <RechartsTooltip
              cursor={false}
              formatter={(value, name) => [
                useAmount ? formatCostCardValue(Number(value ?? 0)) : formatTokens(Number(value ?? 0), showFullUsageNumbers),
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
            <div className="truncate font-geist-mono text-[15px] font-semibold tabular-nums tracking-tight text-foreground">
              {centerValue}
            </div>
            <div className="mt-0.5 text-[10px] font-medium uppercase tracking-wide text-muted-foreground">{metricLabel}</div>
          </div>
        </div>
      </div>
      <div className="mt-3 grid grid-cols-2 gap-x-4 gap-y-1.5 max-sm:grid-cols-1">
        {pieData.map((item, index) => (
          <div key={`${item.model}-${index}`} className="flex items-center gap-2 text-xs">
            <span
              className="size-2.5 shrink-0 rounded-full"
              style={{ background: modelPieColors[index % modelPieColors.length] }}
            />
            <span className="min-w-0 flex-1 truncate text-muted-foreground" title={item.model}>{item.model}</span>
            <span className="shrink-0 font-geist-mono text-[11px] font-medium tabular-nums text-foreground">{item.share.toFixed(1)}%</span>
          </div>
        ))}
      </div>
    </div>
  )
}

function ModelStatsPanel({
  stats,
  showFullUsageNumbers,
}: {
  stats: UsageModelStat[]
  showFullUsageNumbers: boolean
}) {
  const { t } = useTranslation()
  const accent: PanelAccentKey = 'blue'
  const totalRequests = stats.reduce((sum, item) => sum + safeNumber(item.requests), 0)
  const maxRequests = Math.max(1, ...stats.map((item) => safeNumber(item.requests)))

  return (
    <PanelShell>
      <PanelHeader
        accent={accent}
        icon={<BarChart3 />}
        title={t('usage.modelStatsTitle')}
        description={t('usage.modelStatsDesc')}
      />

      {stats.length === 0 ? (
        <EmptyPanel accent={accent} icon={<BarChart3 />} text={t('usage.noModelStats')} />
      ) : (
        <div className="grid grid-cols-[minmax(0,1fr)_minmax(220px,260px)] gap-4 max-lg:grid-cols-1">
          <div className="space-y-3">
            {stats.slice(0, 5).map((item) => {
              const share = totalRequests > 0 ? (item.requests / totalRequests) * 100 : 0
              return (
                <div key={item.model} className="space-y-1.5">
                  <div className="flex items-start justify-between gap-3">
                    <div className="min-w-0">
                      <div className="truncate font-geist-mono text-[13px] font-semibold leading-tight text-foreground" title={item.model}>
                        {item.model}
                      </div>
                      <div className="mt-0.5 flex flex-wrap items-center gap-x-2.5 gap-y-0.5 text-xs text-muted-foreground">
                        <span className="tabular-nums">{t('usage.modelStatsRequests')} {formatTokens(item.requests, showFullUsageNumbers)}</span>
                        <span aria-hidden="true" className="text-border">·</span>
                        <span className="tabular-nums">{t('usage.modelStatsTokens')} {formatTokens(item.tokens, showFullUsageNumbers)}</span>
                        {item.error_count > 0 && (
                          <>
                            <span aria-hidden="true" className="text-border">·</span>
                            <span className="tabular-nums text-amber-600 dark:text-amber-400">{t('usage.modelStatsErrors')} {formatTokens(item.error_count, showFullUsageNumbers)}</span>
                          </>
                        )}
                      </div>
                    </div>
                    <div className="shrink-0 text-right">
                      <div className="font-geist-mono text-[13px] font-semibold tabular-nums tracking-tight text-emerald-600 dark:text-emerald-400">
                        {formatCostCardValue(item.user_billed)}
                      </div>
                      <div className="mt-0.5 font-geist-mono text-[11px] tabular-nums text-muted-foreground">{share.toFixed(1)}%</div>
                    </div>
                  </div>
                  <AccentBar accent={accent} ratio={safeNumber(item.requests) / maxRequests} />
                </div>
              )
            })}
          </div>
          <ModelSharePie stats={stats} showFullUsageNumbers={showFullUsageNumbers} />
        </div>
      )}
    </PanelShell>
  )
}

function FeatureStatsPanel({
  stats,
  totalRequests,
  showFullUsageNumbers,
}: {
  stats?: UsageFeatureStats
  totalRequests: number
  showFullUsageNumbers: boolean
}) {
  const { t } = useTranslation()
  const accent: PanelAccentKey = 'cyan'
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
    <PanelShell>
      <PanelHeader
        accent={accent}
        icon={<Activity />}
        title={t('usage.featureStatsTitle')}
        description={t('usage.featureStatsDesc')}
      />

      <div className="grid flex-1 grid-cols-2 gap-2.5 max-sm:grid-cols-1">
        {items.map((item) => {
          const pct = totalRequests > 0 ? (item.value / totalRequests) * 100 : 0
          return (
            <div
              key={item.label}
              className="group/tile relative flex flex-col justify-between overflow-hidden rounded-xl border px-3 py-2.5 transition-all duration-200 hover:-translate-y-0.5"
              style={{
                background: `color-mix(in srgb, ${item.color} 9%, transparent)`,
                borderColor: `color-mix(in srgb, ${item.color} 26%, transparent)`,
              }}
            >
              <div className="flex items-center justify-between gap-2">
                <span className="flex min-w-0 items-center gap-1.5 text-[12px] font-medium text-foreground/80">
                  <span
                    aria-hidden="true"
                    className="size-1.5 shrink-0 rounded-full"
                    style={{ background: item.color }}
                  />
                  <span className="truncate">{item.label}</span>
                </span>
                <span className="shrink-0 font-geist-mono text-[10px] font-semibold tabular-nums text-foreground/55">
                  {pct.toFixed(1)}%
                </span>
              </div>
              <div className="mt-1 font-geist-mono text-[20px] font-bold leading-tight tabular-nums text-foreground">
                {formatTokens(item.value, showFullUsageNumbers)}
              </div>
              <div className="mt-2 h-[3px] overflow-hidden rounded-full bg-foreground/[0.06]">
                <div
                  className="h-full rounded-full transition-[width] duration-500 ease-out"
                  style={{
                    width: `${Math.min(100, pct)}%`,
                    background: `linear-gradient(90deg, color-mix(in srgb, ${item.color} 92%, transparent), color-mix(in srgb, ${item.color} 55%, transparent))`,
                  }}
                />
              </div>
            </div>
          )
        })}
      </div>
    </PanelShell>
  )
}

function EndpointStatsPanel({
  stats,
  totalRequests,
  showFullUsageNumbers,
}: {
  stats: UsageEndpointStat[]
  totalRequests: number
  showFullUsageNumbers: boolean
}) {
  const { t } = useTranslation()
  return (
    <DistributionPanel
      accent="violet"
      title={t('usage.endpointStatsTitle')}
      description={t('usage.endpointStatsDesc')}
      emptyText={t('usage.noEndpointStats')}
      icon={<Route />}
      items={stats.map((item) => ({
        key: item.endpoint,
        label: item.endpoint,
        requests: item.requests,
        tokens: item.tokens,
        errors: item.error_count,
      }))}
      totalRequests={totalRequests}
      showFullUsageNumbers={showFullUsageNumbers}
    />
  )
}

function APIKeyStatsPanel({
  stats,
  totalRequests,
  showFullUsageNumbers,
}: {
  stats: UsageAPIKeyStat[]
  totalRequests: number
  showFullUsageNumbers: boolean
}) {
  const { t } = useTranslation()
  return (
    <DistributionPanel
      accent="amber"
      title={t('usage.apiKeyStatsTitle')}
      description={t('usage.apiKeyStatsDesc')}
      emptyText={t('usage.noApiKeyStats')}
      icon={<KeyRound />}
      items={stats.map((item) => ({
        key: `${item.api_key_id}-${item.label}`,
        label: item.label,
        requests: item.requests,
        tokens: item.tokens,
        errors: item.error_count,
      }))}
      limit={3}
      totalRequests={totalRequests}
      showFullUsageNumbers={showFullUsageNumbers}
    />
  )
}

function DistributionPanel({
  accent,
  title,
  description,
  emptyText,
  icon,
  items,
  limit = 6,
  totalRequests,
  showFullUsageNumbers,
}: {
  accent: PanelAccentKey
  title: string
  description: string
  emptyText: string
  icon: ReactNode
  items: Array<{ key: string; label: string; requests: number; tokens: number; errors: number }>
  limit?: number
  totalRequests: number
  showFullUsageNumbers: boolean
}) {
  const { t } = useTranslation()
  const visibleItems = items.slice(0, limit)
  const maxRequests = Math.max(1, ...items.map((item) => safeNumber(item.requests)))

  return (
    <PanelShell>
      <PanelHeader accent={accent} icon={icon} title={title} description={description} />

      {visibleItems.length === 0 ? (
        <EmptyPanel accent={accent} icon={icon} text={emptyText} />
      ) : (
        <div className="space-y-3.5">
          {visibleItems.map((item, index) => (
            <div key={item.key} className="space-y-1.5">
              <div className="flex items-start justify-between gap-3">
                <div className="flex min-w-0 items-start gap-2.5">
                  <RankBadge accent={accent} rank={index + 1} />
                  <div className="min-w-0">
                    <div className="truncate font-geist-mono text-[13px] font-semibold leading-tight text-foreground" title={item.label}>
                      {item.label}
                    </div>
                    <div className="mt-0.5 flex flex-wrap items-center gap-x-2.5 gap-y-0.5 text-[11px] text-muted-foreground">
                      <span className="tabular-nums">{t('usage.modelStatsRequests')} {formatTokens(item.requests, showFullUsageNumbers)}</span>
                      <span aria-hidden="true" className="text-border">·</span>
                      <span className="tabular-nums">{t('usage.modelStatsTokens')} {formatTokens(item.tokens, showFullUsageNumbers)}</span>
                      {item.errors > 0 && (
                        <>
                          <span aria-hidden="true" className="text-border">·</span>
                          <span className="tabular-nums text-amber-600 dark:text-amber-400">{t('usage.modelStatsErrors')} {formatTokens(item.errors, showFullUsageNumbers)}</span>
                        </>
                      )}
                    </div>
                  </div>
                </div>
                <span className="ml-1 inline-block min-w-[3.25rem] shrink-0 text-right font-geist-mono text-[13px] font-semibold tabular-nums tracking-tight text-foreground">
                  {formatPercent(item.requests, totalRequests)}
                </span>
              </div>
              <div className="pl-[30px]">
                <AccentBar accent={accent} ratio={safeNumber(item.requests) / maxRequests} thickness="h-2" minWidth={5} />
              </div>
            </div>
          ))}
        </div>
      )}
    </PanelShell>
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

// CyberPolicyDetailButton: 对 cyber_policy 报错的请求，点击关联到提示词过滤日志，
// 展示触发拦截的完整请求内容（为什么 & 是什么）。
function CyberPolicyDetailButton({ log }: { log: UsageLog }) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  const [loading, setLoading] = useState(false)
  const [detail, setDetail] = useState<PromptFilterLog | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loaded, setLoaded] = useState(false)

  const handleOpen = async () => {
    setOpen(true)
    if (loaded || loading) return
    setLoading(true)
    setError(null)
    try {
      const res = await api.matchPromptFilterLog({
        at: log.created_at,
        endpoint: log.endpoint,
        apiKeyId: log.api_key_id || undefined,
      })
      setDetail(res.log)
      setLoaded(true)
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setLoading(false)
    }
  }

  const content = (detail?.full_text || detail?.text_preview || '').trim()

  return (
    <>
      <button
        type="button"
        onClick={() => void handleOpen()}
        title={t('usage.cyberPolicyViewContent')}
        className="inline-flex items-center gap-1 rounded-md border border-red-500/30 bg-red-500/10 px-1.5 py-0.5 text-[11px] font-medium text-red-600 transition-colors hover:bg-red-500/20 dark:text-red-300"
      >
        <ShieldAlert className="size-3" />
        cyber_policy
      </button>
      <Modal show={open} title={t('usage.cyberPolicyDetailTitle')} onClose={() => setOpen(false)}>
        <div className="space-y-3">
          <div className="flex flex-wrap gap-x-4 gap-y-1 text-xs text-muted-foreground">
            <span className="font-mono text-foreground">{log.endpoint || '-'}</span>
            <span className="font-mono text-foreground">{log.model || '-'}</span>
            <span>{formatBeijingTime(log.created_at)}</span>
          </div>
          {log.error_message ? (
            <div className="rounded-md border border-red-500/20 bg-red-500/5 px-3 py-2 text-xs leading-relaxed text-red-700 dark:text-red-300">
              {log.error_message}
            </div>
          ) : null}
          {loading ? (
            <div className="py-6 text-center text-sm text-muted-foreground">{t('common.loading')}</div>
          ) : error ? (
            <div className="py-4 text-sm text-red-500">{error}</div>
          ) : content ? (
            <div>
              <div className="mb-1.5 flex items-center justify-between">
                <span className="text-xs font-semibold text-muted-foreground">{t('usage.cyberPolicyRequestContent')}</span>
                <button type="button" onClick={() => void navigator.clipboard?.writeText(content)} className="text-xs font-medium text-primary hover:underline">{t('common.copy')}</button>
              </div>
              <pre className="max-h-[50vh] overflow-auto whitespace-pre-wrap break-words rounded-md border border-border bg-muted/30 p-3 text-xs leading-relaxed text-foreground">{content}</pre>
            </div>
          ) : (
            <div className="py-4 text-sm text-muted-foreground">{t('usage.cyberPolicyNoDetail')}</div>
          )}
        </div>
      </Modal>
    </>
  )
}

const usageTableHeadClass = 'text-[12px] font-semibold'
const usageTableTextClass = 'text-[14px]'
const usageTableMonoClass = 'font-mono text-[13px] tabular-nums'
const usageTableBadgeClass = 'text-[13px]'
// Premium Minimal: a single-accent (primary) ramp. Instead of 20 competing hues,
// the donut + legend read as one calm material with descending opacity, so it is
// automatically correct under every theme-* palette (it only ever uses --color-primary).
const modelPieColors = [
  'color-mix(in oklab, var(--color-primary) 92%, transparent)',
  'color-mix(in oklab, var(--color-primary) 70%, transparent)',
  'color-mix(in oklab, var(--color-primary) 50%, transparent)',
  'color-mix(in oklab, var(--color-primary) 34%, transparent)',
  'color-mix(in oklab, var(--color-primary) 22%, transparent)',
]
const modelPieShellClass = 'flex min-h-[196px] flex-col rounded-xl border border-border bg-muted/20 p-3 max-lg:min-h-0'

// ============================================================================
// Shared "Unified Accent System" infrastructure for the four analysis panels.
// Each panel carries one accent identity (model=blue, feature=cyan,
// endpoint=violet, apiKey=amber) flowing through its icon chip, header
// underline, gradient capsule bars and rank badges. Every accent is expressed
// only through theme-safe light+dark token pairs, so the panels stay correct
// across dark mode and every theme-* palette (no bare single-mode color).
// ============================================================================
type PanelAccent = {
  /** icon chip background + foreground (light + dark) */
  chip: string
  /** soft ring around the icon chip */
  ring: string
  /** thin header underline rule (gradient fades out to the right) */
  underline: string
  /** gradient fill for AccentBar capsules */
  bar: string
  /** rank chip background + foreground for the top rows */
  rank: string
}

const PANEL_ACCENTS: Record<'blue' | 'cyan' | 'violet' | 'amber', PanelAccent> = {
  blue: {
    chip: 'bg-blue-500/12 text-blue-600 dark:bg-blue-500/20 dark:text-blue-300',
    ring: 'ring-1 ring-inset ring-blue-500/20 dark:ring-blue-500/30',
    underline: 'from-blue-500/45 via-blue-500/20 to-transparent dark:from-blue-400/45 dark:via-blue-400/20',
    bar: 'from-blue-500/85 to-blue-500/45 dark:from-blue-400/90 dark:to-blue-400/45',
    rank: 'bg-blue-500/14 text-blue-600 ring-1 ring-inset ring-blue-500/20 dark:bg-blue-500/22 dark:text-blue-300 dark:ring-blue-500/30',
  },
  cyan: {
    chip: 'bg-cyan-500/12 text-cyan-600 dark:bg-cyan-500/20 dark:text-cyan-300',
    ring: 'ring-1 ring-inset ring-cyan-500/20 dark:ring-cyan-500/30',
    underline: 'from-cyan-500/45 via-cyan-500/20 to-transparent dark:from-cyan-400/45 dark:via-cyan-400/20',
    bar: 'from-cyan-500/85 to-cyan-500/45 dark:from-cyan-400/90 dark:to-cyan-400/45',
    rank: 'bg-cyan-500/14 text-cyan-600 ring-1 ring-inset ring-cyan-500/20 dark:bg-cyan-500/22 dark:text-cyan-300 dark:ring-cyan-500/30',
  },
  violet: {
    chip: 'bg-violet-500/12 text-violet-600 dark:bg-violet-500/20 dark:text-violet-300',
    ring: 'ring-1 ring-inset ring-violet-500/20 dark:ring-violet-500/30',
    underline: 'from-violet-500/45 via-violet-500/20 to-transparent dark:from-violet-400/45 dark:via-violet-400/20',
    bar: 'from-violet-500/85 to-violet-500/45 dark:from-violet-400/90 dark:to-violet-400/45',
    rank: 'bg-violet-500/14 text-violet-600 ring-1 ring-inset ring-violet-500/20 dark:bg-violet-500/22 dark:text-violet-300 dark:ring-violet-500/30',
  },
  amber: {
    chip: 'bg-amber-500/12 text-amber-600 dark:bg-amber-500/20 dark:text-amber-300',
    ring: 'ring-1 ring-inset ring-amber-500/20 dark:ring-amber-500/30',
    underline: 'from-amber-500/45 via-amber-500/20 to-transparent dark:from-amber-400/45 dark:via-amber-400/20',
    bar: 'from-amber-500/85 to-amber-500/45 dark:from-amber-400/90 dark:to-amber-400/45',
    rank: 'bg-amber-500/14 text-amber-600 ring-1 ring-inset ring-amber-500/20 dark:bg-amber-500/22 dark:text-amber-300 dark:ring-amber-500/30',
  },
}

type PanelAccentKey = keyof typeof PANEL_ACCENTS

// PanelShell — Card wrapper with the StatCard hover lift, shared by all panels.
// The Card primitive carries bg-card/border/shadow so glass mode + every
// theme-* palette adapt automatically.
function PanelShell({ className = '', children }: { className?: string; children: ReactNode }) {
  return (
    <Card className={`group/panel h-full py-0 transition-all duration-200 hover:-translate-y-0.5 hover:shadow-md ${className}`}>
      <CardContent className="flex h-full flex-col p-5">{children}</CardContent>
    </Card>
  )
}

// PanelHeader — pixel-consistent header: accent icon chip (with soft ring),
// title + description, and a thin accent underline rule beneath the row.
function PanelHeader({
  accent,
  icon,
  title,
  description,
  trailing,
}: {
  accent: PanelAccentKey
  icon: ReactNode
  title: string
  description: string
  trailing?: ReactNode
}) {
  const a = PANEL_ACCENTS[accent]
  return (
    <div className="mb-4">
      <div className="flex items-start justify-between gap-3">
        <div className="flex min-w-0 items-start gap-3">
          <div
            aria-hidden="true"
            className={`flex size-10 shrink-0 items-center justify-center rounded-xl transition-transform duration-200 group-hover/panel:scale-[1.04] ${a.chip} ${a.ring} [&_svg]:size-[18px]`}
          >
            {icon}
          </div>
          <div className="min-w-0">
            <h3 className="truncate text-[15px] font-semibold tracking-tight text-foreground">{title}</h3>
            <p className="mt-1 text-xs leading-relaxed text-muted-foreground">{description}</p>
          </div>
        </div>
        {trailing ? <div className="shrink-0">{trailing}</div> : null}
      </div>
      <div className={`mt-3 h-px w-full rounded-full bg-gradient-to-r ${a.underline}`} />
    </div>
  )
}

// AccentBar — the single unified bar treatment: a rounded-full gradient capsule
// on a neutral, slightly recessed bg-muted track with rounded caps.
function AccentBar({
  accent,
  ratio,
  thickness = 'h-1.5',
  minWidth = 4,
}: {
  accent: PanelAccentKey
  /** 0..1 fill ratio (clamped); width derived against the panel max */
  ratio: number
  thickness?: string
  minWidth?: number
}) {
  const pct = Math.max(minWidth, Math.min(100, ratio * 100))
  return (
    <div className={`${thickness} overflow-hidden rounded-full bg-muted ring-1 ring-inset ring-border/50`}>
      <div
        className={`h-full rounded-full bg-gradient-to-r transition-[width] duration-500 ease-out ${PANEL_ACCENTS[accent].bar}`}
        style={{ width: `${pct}%` }}
      />
    </div>
  )
}

// RankBadge — #1/#2/#3 markers in the panel accent; neutral chip for 4+.
function RankBadge({ accent, rank }: { accent: PanelAccentKey; rank: number }) {
  const isTop = rank <= 3
  const cls = isTop ? PANEL_ACCENTS[accent].rank : 'bg-muted text-muted-foreground'
  return (
    <span
      aria-hidden="true"
      className={`flex size-5 shrink-0 items-center justify-center rounded-md text-[11px] font-bold leading-none tabular-nums ${cls}`}
    >
      {rank}
    </span>
  )
}

// EmptyPanel — unified empty state used by every panel.
function EmptyPanel({ accent, icon, text }: { accent: PanelAccentKey; icon: ReactNode; text: string }) {
  return (
    <div className="flex min-h-[140px] flex-1 flex-col items-center justify-center gap-2.5 rounded-xl border border-dashed border-border/70 px-4 text-center">
      <div
        aria-hidden="true"
        className={`flex size-9 items-center justify-center rounded-lg opacity-70 ${PANEL_ACCENTS[accent].chip} [&_svg]:size-[16px]`}
      >
        {icon}
      </div>
      <p className="text-[13px] text-muted-foreground">{text}</p>
    </div>
  )
}

type UsageTableColumn = 'status' | 'model' | 'account' | 'apiKey' | 'clientIp' | 'endpoint' | 'type' | 'token' | 'cost' | 'cached' | 'firstToken' | 'duration' | 'time'

const USAGE_COLUMN_DEFINITIONS: Array<{ key: UsageTableColumn; labelKey: string }> = [
  { key: 'status', labelKey: 'usage.tableStatus' },
  { key: 'model', labelKey: 'usage.tableModel' },
  { key: 'account', labelKey: 'usage.tableAccount' },
  { key: 'apiKey', labelKey: 'usage.tableApiKey' },
  { key: 'clientIp', labelKey: 'usage.tableClientIP' },
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
  clientIp: true,
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
  const [pageSize, setPageSize] = usePersistedPageSize('usage_logs', 20, DEFAULT_PAGE_SIZE_OPTIONS)
  const [clearing, setClearing] = useState(false)
  const [timeRange, setTimeRange] = useState<UsageTimeRangeKey>(getInitialUsageRange)
  const [customRange, setCustomRange] = useState<CustomRange | null>(getInitialUsageCustomRange)
  const [showCustomPopover, setShowCustomPopover] = useState(false)
  const customChipRef = useRef<HTMLButtonElement>(null)
  const [logs, setLogs] = useState<UsageLog[]>([])
  const [logsTotal, setLogsTotal] = useState(0)
  const [logsLoading, setLogsLoading] = useState(false)
  const [searchInput, setSearchInput] = useState('')
  const [searchEmail, setSearchEmail] = useState('')
  const [filterModel, setFilterModel] = useState('')
  const [filterEndpoint, setFilterEndpoint] = useState('')
  const [filterApiKeyId, setFilterApiKeyId] = useState('')
  const [filterAccountId, setFilterAccountId] = useState(getInitialUsageAccountID)
  const [filterFast, setFilterFast] = useState('')
  const [filterStream, setFilterStream] = useState<'' | 'true' | 'false'>('')
  const [apiKeys, setAPIKeys] = useState<APIKeyRow[]>([])
  const [modelOptions, setModelOptions] = useState<string[]>([])
  const [apiKeyLoadFailed, setAPIKeyLoadFailed] = useState(false)
  const showFastFilter = true
  const pageSizeOptions = DEFAULT_PAGE_SIZE_OPTIONS
  const searchTimer = useRef<ReturnType<typeof setTimeout>>(null)
  const [visibleColumns, setVisibleColumns] = useState<Record<UsageTableColumn, boolean>>(getInitialUsageVisibleColumns)
  const [columnSettingsOpen, setColumnSettingsOpen] = useState(false)
  const [showAnalysis, setShowAnalysis] = useState(getInitialAnalysisVisibility)

  // 搜索防抖：输入停止 400ms 后触发查询
  const handleSearchChange = useCallback((value: string) => {
    setSearchInput(value)
    if (searchTimer.current) clearTimeout(searchTimer.current)
    searchTimer.current = setTimeout(() => {
      setSearchEmail(value)
      setPage(1)
    }, 400)
  }, [])

  // 仅加载轻量统计（秒级）—— 联动同页 timeRange,与下方请求记录的范围保持一致
  const loadStats = useCallback(async () => {
    const { start, end } = resolveRangeISO(timeRange, customRange)
    const [stats, settings] = await Promise.all([
      api.getUsageStats({ start, end }),
      api.getSettings().catch((): SystemSettings | null => null),
    ])
    return { stats, settings }
  }, [timeRange, customRange])

  const { data, loading, error, reload, reloadSilently } = useDataLoader<{
    stats: UsageStats | null
    settings: SystemSettings | null
  }>({
    initialData: { stats: null, settings: null },
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
      const { start, end } = resolveRangeISO(timeRange, customRange)
      const res = await api.getUsageLogsPaged({
        start, end, page, pageSize,
        email: searchEmail || undefined,
        model: filterModel || undefined,
        endpoint: filterEndpoint || undefined,
        apiKeyId: filterApiKeyId || undefined,
        accountId: filterAccountId || undefined,
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
  }, [timeRange, customRange, page, pageSize, searchEmail, filterModel, filterEndpoint, filterApiKeyId, filterAccountId, filterFast, filterStream])

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

  useEffect(() => {
    persistAnalysisVisibility(showAnalysis)
  }, [showAnalysis])

  const { stats, settings } = data
  const showFullUsageNumbers = settings?.show_full_usage_numbers ?? false
  const totalPages = Math.max(1, Math.ceil(logsTotal / pageSize))
  const currentPage = Math.min(page, totalPages)

  useEffect(() => {
    if (page > totalPages) {
      setPage(totalPages)
    }
  }, [page, totalPages])

  const cumulativeRequests = stats?.total_requests ?? 0
  const cumulativeTokens = stats?.total_tokens ?? 0
  const cumulativeAccountBilled = stats?.total_account_billed ?? 0
  const cumulativeUserBilled = stats?.total_user_billed ?? 0
  const rangeRequests = stats?.today_requests ?? 0
  const rangeTokens = stats?.today_tokens ?? 0
  const rangePromptTokens = stats?.today_prompt_tokens ?? 0
  const rangeCompletionTokens = stats?.today_completion_tokens ?? 0
  const rangeAccountBilled = stats?.today_account_billed ?? 0
  const rangeUserBilled = stats?.today_user_billed ?? 0
  const modelStats = stats?.model_stats ?? []
  const featureStats = stats?.feature_stats
  const endpointStats = stats?.endpoint_stats ?? []
  const apiKeyStats = stats?.api_key_stats ?? []
  const rpm = stats?.rpm ?? 0
  const tpm = stats?.tpm ?? 0
  const errorRate = stats?.error_rate ?? 0
  const avgDurationMs = stats?.avg_duration_ms ?? 0
  const successRequests = rangeRequests - Math.round(rangeRequests * errorRate / 100)
  const showAPIKeyFilter = !apiKeyLoadFailed && apiKeys.length > 0
  const hasActiveFilters = Boolean(searchInput || filterModel || filterEndpoint || filterApiKeyId || filterAccountId || filterStream || filterFast)
  const apiKeyOptions = [
    { label: t('usage.allApiKeys'), value: '' },
    ...apiKeys.map((apiKey) => ({ label: formatAPIKeyOptionLabel(apiKey), value: String(apiKey.id) })),
  ]
  // 顶部主卡片展示当前区间; total_* 保留为清空日志基线叠加后的累计值。
  const rangeLabel = timeRange === 'custom'
    ? t('usage.customRange')
    : timeRange === 'today'
      ? t('usage.today')
    : t(`dashboard.timeRange${timeRange.toUpperCase()}`)
  const rangeRequestsLabel = t('usage.rangeRequestsCard', { range: rangeLabel })
  const rangeTokensLabel = t('usage.rangeTokensCard', { range: rangeLabel })
  const rangeCostLabel = t('usage.rangeCostCard', { range: rangeLabel })

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
          actions={
            <Button
              variant="outline"
              aria-pressed={showAnalysis}
              onClick={() => setShowAnalysis((v) => !v)}
            >
              <BarChart3 className="size-3.5" />
              {showAnalysis ? t('usage.hideAnalysis') : t('usage.showAnalysis')}
            </Button>
          }
        />

        <div className="space-y-6">
        {/* Stat overview: 6 metrics in a single row */}
        <div className="grid grid-cols-1 gap-3 min-[560px]:grid-cols-2 md:grid-cols-3 xl:grid-cols-6">
          <Card className="min-w-0 py-0">
            <CardContent className={usageStatCardContentClass}>
              <div className="flex items-center justify-between gap-2">
                <span className="text-[11px] font-bold uppercase text-muted-foreground">{rangeRequestsLabel}</span>
                <div className="flex size-9 items-center justify-center rounded-lg bg-primary/12 text-primary">
                  <Activity className="size-4" />
                </div>
              </div>
              <div className={usageStatValueClass}>
                {formatTokens(rangeRequests, showFullUsageNumbers)}
              </div>
              <div className="flex flex-wrap items-center gap-x-2 gap-y-0.5 text-[11px] text-muted-foreground leading-snug">
                <span className="text-[hsl(var(--success))]">● {t('usage.success')}: {formatTokens(successRequests, showFullUsageNumbers)}</span>
                <span>● {t('usage.cumulative')}: {formatTokens(cumulativeRequests, showFullUsageNumbers)}</span>
              </div>
            </CardContent>
          </Card>

          <Card className="min-w-0 py-0">
            <CardContent className={usageStatCardContentClass}>
              <div className="flex items-center justify-between gap-2">
                <span className="text-[11px] font-bold uppercase text-muted-foreground">{rangeTokensLabel}</span>
                <div className="flex size-9 items-center justify-center rounded-lg bg-[hsl(var(--info-bg))] text-[hsl(var(--info))]">
                  <Box className="size-4" />
                </div>
              </div>
              <div className={usageStatValueClass}>
                {formatTokens(rangeTokens, showFullUsageNumbers)}
              </div>
              <div className="flex flex-wrap items-center gap-x-2 gap-y-0.5 text-[11px] text-muted-foreground leading-snug">
                <span>{t('usage.inputTokens')}: {formatTokens(rangePromptTokens, showFullUsageNumbers)}</span>
                <span>{t('usage.outputTokens')}: {formatTokens(rangeCompletionTokens, showFullUsageNumbers)}</span>
                <span>{t('usage.cumulative')}: {formatTokens(cumulativeTokens, showFullUsageNumbers)}</span>
              </div>
            </CardContent>
          </Card>

          <Card className="min-w-0 py-0">
            <CardContent className={usageStatCardContentClass}>
              <div className="flex items-center justify-between gap-2">
                <span className="text-[11px] font-bold uppercase text-muted-foreground">{rangeCostLabel}</span>
                <div className="flex size-9 items-center justify-center rounded-lg bg-emerald-500/12 text-emerald-600 dark:bg-emerald-500/20 dark:text-emerald-300">
                  <CircleDollarSign className="size-4" />
                </div>
              </div>
              <div className={`${usageStatValueClass} text-emerald-600 dark:text-emerald-400`}>
                {formatCostCardValue(rangeUserBilled)}
              </div>
              <div className="flex flex-wrap items-center gap-x-2 gap-y-0.5 text-[11px] text-muted-foreground leading-snug">
                <span>{t('usage.accountCost')}: {formatCostCardValue(rangeAccountBilled)}</span>
                <span>{t('usage.cumulative')}: {formatCostCardValue(cumulativeUserBilled)}</span>
                <span>{t('usage.cumulativeAccountCost')}: {formatCostCardValue(cumulativeAccountBilled)}</span>
              </div>
            </CardContent>
          </Card>

          <Card className="min-w-0 py-0">
            <CardContent className={usageStatCardContentClass}>
              <div className="flex items-center justify-between gap-2">
                <span className="text-[11px] font-bold uppercase text-muted-foreground">RPM</span>
                <div className="flex size-9 items-center justify-center rounded-lg bg-[hsl(var(--success-bg))] text-[hsl(var(--success))]">
                  <Clock className="size-4" />
                </div>
              </div>
              <div className={usageStatValueClass}>
                {Math.round(rpm)}
              </div>
              <div className="text-[11px] text-muted-foreground leading-snug">{t('usage.rpmDesc')}</div>
            </CardContent>
          </Card>

          <Card className="min-w-0 py-0">
            <CardContent className={usageStatCardContentClass}>
              <div className="flex items-center justify-between gap-2">
                <span className="text-[11px] font-bold uppercase text-muted-foreground">TPM</span>
                <div className="flex size-9 items-center justify-center rounded-lg bg-destructive/12 text-destructive">
                  <Zap className="size-4" />
                </div>
              </div>
              <div className={usageStatValueClass}>
                {formatTokens(tpm, showFullUsageNumbers)}
              </div>
              <div className="text-[11px] text-muted-foreground leading-snug">{t('usage.tpmDesc')}</div>
            </CardContent>
          </Card>

          <Card className="min-w-0 py-0">
            <CardContent className={usageStatCardContentClass}>
              <div className="flex items-center justify-between gap-2">
                <span className="text-[11px] font-bold uppercase text-muted-foreground">{t('usage.errorRateCard')}</span>
                <div className="flex size-9 items-center justify-center rounded-lg bg-[hsl(36_72%_40%/0.12)] text-[hsl(36,72%,40%)]">
                  <AlertTriangle className="size-4" />
                </div>
              </div>
              <div className={usageStatValueClass}>
                {errorRate.toFixed(1)}%
              </div>
              <div className="text-[11px] text-muted-foreground leading-snug">{t('usage.avgLatencyInline', { value: Math.round(avgDurationMs) })}</div>
            </CardContent>
          </Card>
        </div>

        {showAnalysis && (
          <>
            <div className="grid grid-cols-[minmax(0,0.5fr)_minmax(360px,0.5fr)] gap-3 max-lg:grid-cols-1">
              <ModelStatsPanel stats={modelStats} showFullUsageNumbers={showFullUsageNumbers} />
              <FeatureStatsPanel stats={featureStats} totalRequests={rangeRequests} showFullUsageNumbers={showFullUsageNumbers} />
            </div>

            <div className="grid grid-cols-2 gap-3 max-lg:grid-cols-1">
              <EndpointStatsPanel stats={endpointStats} totalRequests={rangeRequests} showFullUsageNumbers={showFullUsageNumbers} />
              <APIKeyStatsPanel stats={apiKeyStats} totalRequests={rangeRequests} showFullUsageNumbers={showFullUsageNumbers} />
            </div>
          </>
        )}

        {/* Logs table */}
        <Card>
          <CardContent className="p-4">
            <div className="mb-4 flex items-center justify-between gap-3 overflow-visible max-lg:flex-col max-lg:items-stretch max-lg:overflow-visible">
              <div className="flex min-w-0 flex-1 flex-col gap-2 sm:flex-row sm:items-center sm:gap-3">
                <h3 className="shrink-0 whitespace-nowrap text-base font-semibold text-foreground">{t('usage.requestLogs')}</h3>
                <div className="inline-flex max-w-full flex-wrap rounded-xl border border-border bg-muted/50 p-0.5">
                  {USAGE_TIME_RANGE_OPTIONS.map((key) => (
                    <button
                      key={key}
                      type="button"
                      onClick={() => {
                        setTimeRange(key)
                        setPage(1)
                        setShowCustomPopover(false)
                      }}
                      className={`whitespace-nowrap px-2.5 py-1.5 text-xs font-medium rounded-lg transition-all duration-200 ${
                        timeRange === key
                          ? 'bg-background text-foreground shadow-sm border border-border'
                          : 'text-muted-foreground hover:text-foreground'
                      }`}
                    >
                      {key === 'today' ? t('usage.today') : t(`dashboard.timeRange${key.toUpperCase()}`)}
                    </button>
                  ))}
                  <button
                    ref={customChipRef}
                    type="button"
                    onClick={() => setShowCustomPopover((v) => !v)}
                    className={`whitespace-nowrap px-2.5 py-1.5 text-xs font-medium rounded-lg transition-all duration-200 ${
                      timeRange === 'custom'
                        ? 'bg-background text-foreground shadow-sm border border-border'
                        : 'text-muted-foreground hover:text-foreground'
                    }`}
                  >
                    {timeRange === 'custom' && customRange
                      ? t('usage.customRangeChipApplied')
                      : t('usage.customRange')}
                  </button>
                </div>
                {showCustomPopover && (
                  <CustomRangePopover
                    anchorRef={customChipRef}
                    initial={customRange}
                    onCancel={() => setShowCustomPopover(false)}
                    onApply={(range) => {
                      setCustomRange(range)
                      setTimeRange('custom')
                      setPage(1)
                      setShowCustomPopover(false)
                    }}
                  />
                )}
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
            <div className="toolbar-surface mb-4 flex flex-wrap items-center gap-2 overflow-visible max-lg:gap-1.5">
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

              {filterAccountId && (
                <button
                  type="button"
                  onClick={() => { setFilterAccountId(''); setPage(1) }}
                  className="inline-flex h-8 shrink-0 items-center gap-1 rounded-lg border border-primary/30 bg-primary/10 px-2.5 text-[13px] font-medium text-primary transition-colors hover:bg-primary/15"
                  title={t('usage.accountIdFilterTitle', { id: filterAccountId })}
                >
                  {t('usage.accountIdFilter', { id: filterAccountId })}
                  <X className="size-3.5" />
                </button>
              )}

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
                    setFilterAccountId('')
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
              {/* Mobile log cards */}
              <TooltipProvider>
              <div className="grid gap-3 lg:hidden">
                {logs.map((log: UsageLog) => {
                  const durationLabel = log.duration_ms > 1000
                    ? `${(log.duration_ms / 1000).toFixed(1)}s`
                    : `${log.duration_ms}ms`
                  const firstTokenLabel = log.first_token_ms > 0
                    ? (log.first_token_ms > 1000
                      ? `${(log.first_token_ms / 1000).toFixed(1)}s`
                      : `${log.first_token_ms}ms`)
                    : null
                  return (
                    <div
                      key={log.id}
                      className="rounded-xl border border-border bg-background/70 p-3.5 shadow-sm"
                    >
                      <div className="flex items-start justify-between gap-2">
                        <div className="flex min-w-0 flex-wrap items-center gap-1.5">
                          <StatusCodeBadge log={log} />
                          {log.via_websocket ? (
                            <Badge
                              variant="outline"
                              className="border-transparent bg-cyan-500/12 text-[11px] font-semibold uppercase text-cyan-600 dark:bg-cyan-500/20 dark:text-cyan-400"
                            >
                              ws
                            </Badge>
                          ) : null}
                          <Badge variant="outline" className={usageTableBadgeClass}>
                            {log.model || '-'}
                          </Badge>
                          {isFastTier(log.billing_service_tier || log.service_tier) ? (
                            <Badge
                              variant="outline"
                              className="gap-0.5 border-transparent bg-blue-500/12 text-[11px] font-semibold text-blue-600 dark:bg-blue-500/20 dark:text-blue-400"
                            >
                              <Zap className="size-3" />
                              Fast
                            </Badge>
                          ) : null}
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
                        </div>
                        <div className="shrink-0 text-right text-[11px] tabular-nums text-muted-foreground">
                          {formatBeijingTime(log.created_at)}
                        </div>
                      </div>

                      <div className="mt-2.5 space-y-1 text-xs text-muted-foreground">
                        <div className="truncate" title={formatUsageAccountTitle(log)}>
                          <span className="font-semibold text-foreground/80">{t('usage.tableAccount')}: </span>
                          {formatUsageAccountLabel(log)}
                        </div>
                        <div className="truncate font-mono">
                          <span className="font-sans font-semibold text-foreground/80">{t('usage.tableEndpoint')}: </span>
                          {log.inbound_endpoint || log.endpoint || '-'}
                        </div>
                      </div>

                      <div className="mt-3 grid grid-cols-2 gap-2 text-xs sm:grid-cols-4">
                        <div className="rounded-lg border border-border/70 bg-card/60 px-2.5 py-2">
                          <div className="text-[11px] font-semibold text-muted-foreground">{t('usage.tableToken')}</div>
                          <div className="mt-1 font-mono tabular-nums">
                            {log.status_code < 400 && (log.input_tokens > 0 || log.output_tokens > 0) ? (
                              <>
                                <span className="text-blue-500">↓{formatTokens(log.input_tokens, true)}</span>
                                <span className="mx-0.5 text-border">/</span>
                                <span className="text-emerald-500">↑{formatTokens(log.output_tokens, true)}</span>
                              </>
                            ) : (
                              <span className="text-muted-foreground">-</span>
                            )}
                          </div>
                        </div>
                        <div className="rounded-lg border border-border/70 bg-card/60 px-2.5 py-2">
                          <div className="text-[11px] font-semibold text-muted-foreground">{t('usage.tableCost')}</div>
                          <div className="mt-1">
                            <UsageCostCell log={log} />
                          </div>
                        </div>
                        <div className="rounded-lg border border-border/70 bg-card/60 px-2.5 py-2">
                          <div className="text-[11px] font-semibold text-muted-foreground">{t('usage.tableDuration')}</div>
                          <div className={`mt-1 font-mono tabular-nums ${log.duration_ms > 30000 ? 'text-red-500' : log.duration_ms > 10000 ? 'text-amber-500' : 'text-foreground'}`}>
                            {durationLabel}
                          </div>
                        </div>
                        <div className="rounded-lg border border-border/70 bg-card/60 px-2.5 py-2">
                          <div className="text-[11px] font-semibold text-muted-foreground">{t('usage.tableFirstToken')}</div>
                          <div className={`mt-1 font-mono tabular-nums ${
                            !firstTokenLabel
                              ? 'text-muted-foreground'
                              : log.first_token_ms > 5000
                                ? 'text-red-500'
                                : log.first_token_ms > 2000
                                  ? 'text-amber-500'
                                  : 'text-emerald-500'
                          }`}>
                            {firstTokenLabel || '-'}
                          </div>
                        </div>
                      </div>
                    </div>
                  )
                })}
              </div>
              </TooltipProvider>

              {/* Desktop table */}
              <div className="data-table-shell hidden lg:block">
                <TooltipProvider>
                <Table>
                  <TableHeader>
                    <TableRow>
                      {visibleColumns.status && <TableHead className={usageTableHeadClass}>{t('usage.tableStatus')}</TableHead>}
                      {visibleColumns.model && <TableHead className={usageTableHeadClass}>{t('usage.tableModel')}</TableHead>}
                      {visibleColumns.account && <TableHead className={usageTableHeadClass}>{t('usage.tableAccount')}</TableHead>}
                      {visibleColumns.apiKey && <TableHead className={usageTableHeadClass}>{t('usage.tableApiKey')}</TableHead>}
                      {visibleColumns.clientIp && <TableHead className={usageTableHeadClass}>{t('usage.tableClientIP')}</TableHead>}
                      {visibleColumns.endpoint && <TableHead className={usageTableHeadClass}>{t('usage.tableEndpoint')}</TableHead>}
                      {visibleColumns.type && <TableHead className={usageTableHeadClass}>{t('usage.tableType')}</TableHead>}
                      {visibleColumns.token && <TableHead className={usageTableHeadClass}>{t('usage.tableToken')}</TableHead>}
                      {visibleColumns.cost && <TableHead className={usageTableHeadClass}>{t('usage.tableCost')}</TableHead>}
                      {visibleColumns.cached && <TableHead className={usageTableHeadClass}>{t('usage.tableCached')}</TableHead>}
                      {visibleColumns.firstToken && <TableHead className={usageTableHeadClass}><span title={t('usage.tableFirstTokenHint')} className="cursor-help underline decoration-dotted underline-offset-2">{t('usage.tableFirstToken')}</span></TableHead>}
                      {visibleColumns.duration && <TableHead className={usageTableHeadClass}>{t('usage.tableDuration')}</TableHead>}
                      {visibleColumns.time && <TableHead className={usageTableHeadClass}>{t('usage.tableTime')}</TableHead>}
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {logs.map((log: UsageLog) => {
                      return (
                      <TableRow key={log.id}>
                        {visibleColumns.status && <TableCell>
                          <div className="flex items-center gap-1.5">
                            <StatusCodeBadge log={log} />
                            {log.upstream_error_kind === 'cyber_policy' ? <CyberPolicyDetailButton log={log} /> : null}
                          </div>
                        </TableCell>}
                        {visibleColumns.model && <TableCell>
                          <div className="flex items-center gap-1.5 flex-wrap">
                            {log.via_websocket && (
                              <Badge
                                variant="outline"
                                title="WebSocket"
                                className="text-[11px] font-semibold uppercase border-transparent bg-cyan-500/12 text-cyan-600 dark:bg-cyan-500/20 dark:text-cyan-400"
                              >
                                ws
                              </Badge>
                            )}
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
                            {isFastTier(log.billing_service_tier || log.service_tier) && (
                              <Badge
                                variant="outline"
                                className="text-[11px] font-semibold gap-0.5 border-transparent bg-blue-500/12 text-blue-600 dark:bg-blue-500/20 dark:text-blue-400"
                                title={`${t('usage.billingTier')}: ${formatServiceTierLabel(t, log.billing_service_tier || log.service_tier)}`}
                              >
                                <Zap className="size-3" />
                                Fast
                              </Badge>
                            )}
                          </div>
                        </TableCell>}
                        {visibleColumns.account && <TableCell className={`${usageTableTextClass} text-muted-foreground`}>
                          <span className="block max-w-[180px] truncate whitespace-nowrap" title={formatUsageAccountTitle(log)}>
                            {formatUsageAccountLabel(log)}
                          </span>
                        </TableCell>}
                        {visibleColumns.apiKey && <TableCell className={`${usageTableTextClass} text-muted-foreground`}>
                          <span className="block max-w-[180px] truncate whitespace-nowrap font-mono text-[12px]" title={formatUsageAPIKeyLabel(log.api_key_name, log.api_key_masked) || t('usage.unknownApiKey')}>
                            {formatUsageAPIKeyLabel(log.api_key_name, log.api_key_masked) || t('usage.unknownApiKey')}
                          </span>
                        </TableCell>}
                        {visibleColumns.clientIp && <TableCell className={`${usageTableMonoClass} text-muted-foreground whitespace-nowrap`}>
                          <span title={log.client_ip || '-'}>
                            {log.client_ip || '-'}
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
                          <div className="flex flex-wrap items-center gap-1.5">
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
                            {log.compact && (
                              <Badge
                                variant="outline"
                                className="text-[11px] font-semibold gap-0.5 border-transparent bg-teal-500/12 text-teal-700 dark:bg-teal-500/20 dark:text-teal-300"
                                title={t('usage.compactRequestTooltip')}
                              >
                                <Box className="size-3" />
                                {t('usage.compactRequest')}
                              </Badge>
                            )}
                          </div>
                        </TableCell>}
                        {visibleColumns.token && <TableCell>
                          {log.status_code < 400 && (log.input_tokens > 0 || log.output_tokens > 0) ? (
                            <div className={`${usageTableMonoClass} leading-relaxed`}>
                              <span className="text-blue-500">↓{formatTokens(log.input_tokens, true)}</span>
                              <span className="mx-1 text-border">|</span>
                              <span className="text-emerald-500">↑{formatTokens(log.output_tokens, true)}</span>
                              {log.reasoning_tokens > 0 && (
                                <>
                                  <span className="mx-1 text-border">|</span>
                                  <span className="text-amber-500 inline-flex items-center gap-0.5"><Brain className="size-3.5 inline" />{formatTokens(log.reasoning_tokens, true)}</span>
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
                              {formatTokens(log.cached_tokens, true)}
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

        {confirmDialog}
      </>
    </StateShell>
  )
}

// CustomRangePopover 通过 React portal 渲染在 body 下,不受外层 overflow 裁切。
// 位置根据触发按钮 rect 计算,自动避开右边界。
function CustomRangePopover({
  anchorRef,
  initial,
  onApply,
  onCancel,
}: {
  anchorRef: React.RefObject<HTMLButtonElement | null>
  initial: CustomRange | null
  onApply: (range: CustomRange) => void
  onCancel: () => void
}) {
  const { t } = useTranslation()
  const now = new Date()
  const defaultEnd = initial ? new Date(initial.end) : now
  const defaultStart = initial
    ? new Date(initial.start)
    : new Date(now.getTime() - 24 * 60 * 60 * 1000)

  const [startStr, setStartStr] = useState(dateToLocalInputValue(defaultStart))
  const [endStr, setEndStr] = useState(dateToLocalInputValue(defaultEnd))
  const [error, setError] = useState<string | null>(null)
  const popoverRef = useRef<HTMLDivElement>(null)
  const [position, setPosition] = useState<{ top: number; left: number } | null>(null)
  const POPOVER_WIDTH = 320

  const recompute = useCallback(() => {
    const anchor = anchorRef.current
    if (!anchor) return
    const rect = anchor.getBoundingClientRect()
    const top = rect.bottom + 6
    // 默认让 popover 右边对齐 anchor 右边;若超出窗口左边,夹到 8px 边距。
    const desiredLeft = rect.right - POPOVER_WIDTH
    const left = Math.max(8, Math.min(window.innerWidth - POPOVER_WIDTH - 8, desiredLeft))
    setPosition({ top, left })
  }, [anchorRef])

  useLayoutEffect(() => {
    recompute()
  }, [recompute])

  useEffect(() => {
    const handle = () => recompute()
    window.addEventListener('resize', handle)
    window.addEventListener('scroll', handle, true)
    return () => {
      window.removeEventListener('resize', handle)
      window.removeEventListener('scroll', handle, true)
    }
  }, [recompute])

  useEffect(() => {
    const handlePointerDown = (event: PointerEvent) => {
      const target = event.target as Node | null
      if (!target) return
      if (popoverRef.current?.contains(target)) return
      if (anchorRef.current?.contains(target)) return
      onCancel()
    }
    const handleEscape = (event: KeyboardEvent) => {
      if (event.key === 'Escape') onCancel()
    }
    document.addEventListener('pointerdown', handlePointerDown)
    document.addEventListener('keydown', handleEscape)
    return () => {
      document.removeEventListener('pointerdown', handlePointerDown)
      document.removeEventListener('keydown', handleEscape)
    }
  }, [anchorRef, onCancel])

  const handleApply = () => {
    const startDate = localInputValueToDate(startStr)
    const endDate = localInputValueToDate(endStr)
    if (!startDate || !endDate) {
      setError(t('usage.customRangeInvalid'))
      return
    }
    if (endDate.getTime() <= startDate.getTime()) {
      setError(t('usage.customRangeEndBeforeStart'))
      return
    }
    if (endDate.getTime() - startDate.getTime() > CUSTOM_RANGE_MAX_MS) {
      setError(t('usage.customRangeTooLong', { days: CUSTOM_RANGE_MAX_DAYS }))
      return
    }
    setError(null)
    onApply({
      start: dateToLocalRFC3339(startDate),
      end: dateToLocalRFC3339(endDate),
    })
  }

  if (!position) return null

  return createPortal(
    <div
      ref={popoverRef}
      style={{
        position: 'fixed',
        top: position.top,
        left: position.left,
        width: POPOVER_WIDTH,
      }}
      className="z-[1000] rounded-lg border border-border bg-popover p-3 text-popover-foreground shadow-[0_18px_40px_hsl(222_30%_18%/0.18)]"
    >
      <div className="mb-2 text-xs font-semibold text-foreground">
        {t('usage.customRangeTitle')}
      </div>
      <div className="space-y-2">
        <label className="block text-[11px] text-muted-foreground">
          {t('usage.customRangeStart')}
          <input
            type="datetime-local"
            value={startStr}
            onChange={(e) => setStartStr(e.target.value)}
            className="mt-1 block w-full rounded-md border border-border bg-background px-2 py-1 text-xs"
          />
        </label>
        <label className="block text-[11px] text-muted-foreground">
          {t('usage.customRangeEnd')}
          <input
            type="datetime-local"
            value={endStr}
            onChange={(e) => setEndStr(e.target.value)}
            className="mt-1 block w-full rounded-md border border-border bg-background px-2 py-1 text-xs"
          />
        </label>
      </div>
      {error && (
        <div className="mt-2 text-[11px] text-destructive">{error}</div>
      )}
      <div className="mt-3 flex justify-end gap-2">
        <Button variant="ghost" size="sm" onClick={onCancel}>
          {t('common.cancel', { defaultValue: 'Cancel' })}
        </Button>
        <Button size="sm" onClick={handleApply}>
          {t('usage.customRangeApply')}
        </Button>
      </div>
    </div>,
    document.body,
  )
}
