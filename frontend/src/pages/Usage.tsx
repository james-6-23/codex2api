import { useCallback, useEffect, useLayoutEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'
import { createPortal } from 'react-dom'
import { Cell, Pie, PieChart, ResponsiveContainer, Tooltip as RechartsTooltip } from 'recharts'
import { api } from '../api'
import { getTimeRangeISO, type TimeRangeKey } from '../lib/timeRange'
import PageHeader from '../components/PageHeader'
import Pagination from '../components/Pagination'
import ChannelFilter, { useUsageChannel } from '../components/ChannelFilter'
import ChannelLogo from '../components/ChannelLogo'
import CompactionBadges from '../components/CompactionBadges'
import ModelLogo from '../components/ModelLogo'
import Modal from '../components/Modal'
import ColumnSettingsMenu from '../components/ColumnSettingsMenu'
import StateShell from '../components/StateShell'
import { useDataLoader } from '../hooks/useDataLoader'
import { useConfirmDialog } from '../hooks/useConfirmDialog'
import { useToast } from '../hooks/useToast'
import { DEFAULT_PAGE_SIZE_OPTIONS, usePersistedPageSize } from '../hooks/usePersistedPageSize'
import type { APIKeyRow, OpsErrorSummary, SystemSettings, UsageAPIKeyStat, UsageEndpointStat, UsageFeatureStats, UsageLog, UsageModelStat, UsageStats, PromptFilterLog, PromptPolicyIncidentDetailResponse } from '../types'
import { cn, formatCompactEmail } from '../lib/utils'
import { formatUsageNumber as formatTokens } from '../lib/usageFormat'
import { getUsageTokenBreakdown } from '../lib/usageTokenDisplay'
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
import { Activity, Box, Clock, Zap, AlertTriangle, Search, Brain, DatabaseZap, DatabaseBackup, X, Image as ImageIcon, Info, CircleDollarSign, BarChart3, KeyRound, Route, SlidersHorizontal, ShieldAlert, RefreshCw, ChevronDown, RotateCcw } from 'lucide-react'
import { Input } from '@/components/ui/input'
import { Select } from '@/components/ui/select'

/** Color ramp for reasoning effort: cool/muted → hot/intense. */
function getReasoningEffortBadgeClassName(effort: string): string {
  switch (effort.trim().toLowerCase()) {
    case 'none':
    case 'off':
      return 'border-transparent bg-slate-500/12 text-slate-500 dark:bg-slate-500/20 dark:text-slate-400'
    case 'minimal':
    case 'min':
      return 'border-transparent bg-sky-500/12 text-sky-600 dark:bg-sky-500/20 dark:text-sky-400'
    case 'low':
      return 'border-transparent bg-emerald-500/12 text-emerald-600 dark:bg-emerald-500/20 dark:text-emerald-400'
    case 'medium':
    case 'med':
      return 'border-transparent bg-amber-500/12 text-amber-600 dark:bg-amber-500/20 dark:text-amber-400'
    case 'high':
      return 'border-transparent bg-orange-500/14 text-orange-600 dark:bg-orange-500/22 dark:text-orange-400'
    case 'xhigh':
    case 'max':
      return 'border-transparent bg-rose-500/14 text-rose-600 dark:bg-rose-500/22 dark:text-rose-400'
    case 'ultra':
      return 'border-transparent bg-violet-500/14 text-violet-600 dark:bg-violet-500/22 dark:text-violet-300'
    default:
      return 'border-transparent bg-muted text-muted-foreground'
  }
}

function ReasoningEffortBadge({ effort }: { effort: string }) {
  const label = effort.trim()
  if (!label) return null
  return (
    <Badge
      variant="outline"
      title={`reasoning: ${label}`}
      className={`text-[11px] font-semibold lowercase tracking-wide ${getReasoningEffortBadgeClassName(label)}`}
    >
      {label}
    </Badge>
  )
}

function InternalRequestBadge({ log }: { log: UsageLog }) {
  const { t } = useTranslation()
  const reason = log.internal_reason?.trim()
  if (!reason) return null

  const label = reason === 'overflow_compact_summary'
    ? t('usage.internalOverflowSummary')
    : t('usage.internalRequest')
  const title = log.parent_request_id?.trim()
    ? t('usage.internalRequestParentTooltip', { parentRequestId: log.parent_request_id.trim() })
    : t('usage.internalRequestTooltip')

  return (
    <Badge
      variant="outline"
      className="gap-0.5 whitespace-nowrap border-transparent bg-fuchsia-500/12 text-[11px] font-semibold text-fuchsia-700 dark:bg-fuchsia-500/20 dark:text-fuchsia-300"
      title={title}
    >
      <Brain className="size-3" />
      {label}
    </Badge>
  )
}

function getStatusBadgeClassName(statusCode: number): string {
  if (statusCode === 200) {
    return 'border-emerald-500/20 bg-emerald-500/12 text-emerald-700 dark:text-emerald-300'
  }
  if (statusCode === 401) {
    return 'border-rose-500/20 bg-rose-500/12 text-rose-700 dark:text-rose-300'
  }
  if (statusCode === 429) {
    return 'border-amber-500/20 bg-amber-500/12 text-amber-700 dark:text-amber-300'
  }
  if (statusCode >= 500) {
    return 'border-rose-500/20 bg-rose-500/12 text-rose-700 dark:text-rose-300'
  }
  if (statusCode >= 400) {
    return 'border-amber-500/20 bg-amber-500/12 text-amber-700 dark:text-amber-300'
  }
  return 'border-slate-500/20 bg-slate-500/12 text-slate-700 dark:text-slate-300'
}

type UsagePresetRangeKey = 'today' | TimeRangeKey
const USAGE_TIME_RANGE_OPTIONS: UsagePresetRangeKey[] = ['today', '1h', '6h', '24h', '7d', '30d']
type UsageTypeFilter = '' | 'stream' | 'sync' | 'compact' | 'history'
type UsageStatusFilter = '' | '2xx' | 'error' | '4xx' | '5xx' | `${number}`
type UsageRetryFilter = '' | 'false' | 'true'
type UsageTransportFilter = '' | 'http' | 'ws'

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
  return normalized === 'fast' || normalized === 'priority' || normalized === 'ultrafast'
}

function formatServiceTierLabel(t: ReturnType<typeof useTranslation>['t'], tier?: string | null): string {
  const normalized = (tier || '').trim().toLowerCase()
  if (!normalized) return '-'
  if (normalized === 'ultrafast') return 'Ultrafast'
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
  // legacy 行（三字段拆分前）只有 service_tier 可用；新行 actual 为空表示上游未回传，
  // 不能回退到偏好请求意图的 legacy 列冒充“上游回传 Tier”。
  const actualTier = log.actual_service_tier || (requestedTier ? '' : log.service_tier || '')
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
    return <span className={`${usageTableMonoClass} text-muted-foreground/50`}>-</span>
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
          {(log.cache_write_5m_tokens ?? 0) > 0 && (
            <CostTooltipRow label={t('usage.cacheWrite5mCost')} value={formatUSD(log.cache_write_5m_cost)} />
          )}
          {(log.cache_write_1h_tokens ?? 0) > 0 && (
            <CostTooltipRow label={t('usage.cacheWrite1hCost')} value={formatUSD(log.cache_write_1h_cost)} />
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
          {(log.cache_write_5m_tokens ?? 0) > 0 && (log.cache_write_5m_price_per_mtoken ?? 0) > 0 && (
            <CostTooltipRow label={t('usage.cacheWrite5mUnitPrice')} value={formatTokenPricePerMillion(log.cache_write_5m_price_per_mtoken)} valueClassName="text-amber-300" />
          )}
          {(log.cache_write_1h_tokens ?? 0) > 0 && (log.cache_write_1h_price_per_mtoken ?? 0) > 0 && (
            <CostTooltipRow label={t('usage.cacheWrite1hUnitPrice')} value={formatTokenPricePerMillion(log.cache_write_1h_price_per_mtoken)} valueClassName="text-amber-300" />
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
                      <div className="flex min-w-0 items-center gap-2">
                        <ModelLogo model={item.model} variant="soft" size={22} className="shrink-0" />
                        <div className="truncate text-sm font-semibold leading-tight tracking-tight text-foreground" title={item.model}>
                          {item.model}
                        </div>
                      </div>
                      <div className="mt-1 flex flex-wrap items-center gap-x-2.5 gap-y-0.5 pl-[30px] text-xs text-muted-foreground">
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
        // 端点是 URL 路径，用等宽字体更清晰
        mono: true,
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
  items: Array<{ key: string; label: string; mono?: boolean; requests: number; tokens: number; errors: number }>
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
                    <div
                      className={cn(
                        "truncate text-sm font-semibold leading-tight tracking-tight text-foreground",
                        // 端点是路径（代码性质）保留等宽；密钥名等宽显糙，用常规字体
                        item.mono && "font-geist-mono text-[13px]",
                      )}
                      title={item.label}
                    >
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
  const dotColor = log.status_code === 200
    ? 'bg-emerald-500'
    : log.status_code === 429
      ? 'bg-amber-500 animate-pulse'
      : 'bg-rose-500 animate-pulse'

  const badge = (
    <Badge
      variant="outline"
      className={cn(
        usageTableBadgeClass,
        'gap-1.5',
        getStatusBadgeClassName(log.status_code),
        log.status_code !== 200 ? 'cursor-help ring-1 ring-inset ring-current/10' : ''
      )}
    >
      <span className={cn('size-1.5 rounded-full', dotColor)} />
      <span>{log.status_code}</span>
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
      <TooltipContent side="right" sideOffset={8} className="max-w-[360px] rounded-xl border border-slate-700 bg-slate-950 px-3.5 py-3 text-xs text-slate-50 shadow-xl backdrop-blur-md">
        <div className="space-y-1.5">
          <div className="font-semibold text-slate-300">{title}</div>
          <div className="font-mono text-[11px] tabular-nums text-slate-400">HTTP {log.status_code}</div>
          <div className="whitespace-pre-wrap break-words leading-relaxed text-slate-50">{message}</div>
        </div>
      </TooltipContent>
    </Tooltip>
  )
}

function UsageErrorSummaryCell({ log, mobile = false }: { log: UsageLog; mobile?: boolean }) {
  const { t } = useTranslation()
  const errorKind = log.upstream_error_kind?.trim() || ''
  const message = log.error_message?.trim() || ''
  const hasError = log.status_code >= 400 || Boolean(errorKind || message)

  if (!hasError) {
    return mobile ? null : <span className="text-muted-foreground/50">-</span>
  }

  return (
    <div
      className={cn(
        'min-w-0',
        mobile
          ? 'mt-2.5 rounded-lg border border-red-500/15 bg-red-500/5 px-3 py-2.5'
          : 'w-[260px] max-w-[24vw]',
      )}
    >
      <div className="flex min-w-0 items-center gap-1.5">
        <AlertTriangle className="size-3.5 shrink-0 text-amber-500" />
        <span className="truncate text-[11px] font-semibold text-foreground" title={errorKind || t('usage.unknownErrorKind')}>
          {errorKind || t('usage.unknownErrorKind')}
        </span>
        {log.is_retry_attempt ? (
          <Badge variant="outline" className="ml-auto shrink-0 gap-0.5 border-transparent bg-blue-500/12 px-1.5 py-0 text-[10px] text-blue-600 dark:bg-blue-500/20 dark:text-blue-300">
            <RotateCcw className="size-2.5" />
            #{Math.max(1, log.attempt_index)}
          </Badge>
        ) : null}
      </div>
      <div
        className={cn(
          'mt-1 break-words text-[11px] leading-relaxed text-muted-foreground',
          mobile ? 'line-clamp-3' : 'line-clamp-2',
        )}
        title={message || t('usage.statusErrorEmpty')}
      >
        {message || t('usage.statusErrorEmpty')}
      </div>
    </div>
  )
}

function UserAgentCell({ log, mobile = false }: { log: UsageLog; mobile?: boolean }) {
  const { t } = useTranslation()
  const clientUserAgent = log.client_user_agent?.trim() || ''
  const upstreamUserAgent = log.upstream_user_agent?.trim() || ''
  const hasAudit = Boolean(clientUserAgent || upstreamUserAgent || log.user_agent_overridden)
  const upstreamLabel = upstreamUserAgent || (hasAudit ? t('usage.userAgentNotSent') : '-')
  const statusLabel = !hasAudit
    ? t('usage.userAgentNotRecorded')
    : log.user_agent_overridden
      ? t('usage.userAgentOverridden')
      : t('usage.userAgentPreserved')

  if (!hasAudit && !log.request_id && !log.upstream_request_id) {
    return (
      <div className="font-mono text-[11px] text-muted-foreground" title={t('usage.userAgentNotRecorded')}>
        UA: -
      </div>
    )
  }

  const statusChip = (
    <Badge
      variant="outline"
      className={`ml-auto shrink-0 border-transparent px-1.5 py-0 text-[10px] font-semibold ${
        log.user_agent_overridden
          ? 'bg-amber-500/12 text-amber-700 dark:bg-amber-500/20 dark:text-amber-300'
          : 'bg-emerald-500/12 text-emerald-700 dark:bg-emerald-500/20 dark:text-emerald-300'
      }`}
    >
      {statusLabel}
    </Badge>
  )
  // 客户端与上游 UA 完全一致且未改写:合成一行(C=U),两行会重复同一串字符串白占行高。
  const sameUA = !log.user_agent_overridden && Boolean(clientUserAgent) && clientUserAgent === upstreamUserAgent

  const content = sameUA ? (
    <div className={`${mobile ? 'w-full' : 'w-[260px] max-w-[28vw]'} font-mono text-[11px] leading-relaxed`}>
      <div className="flex min-w-0 items-center gap-1.5" title={`${t('usage.clientUserAgent')} = ${t('usage.upstreamUserAgent')}`}>
        <span className="shrink-0 font-sans font-semibold text-muted-foreground">C=U</span>
        <span className="min-w-0 truncate text-foreground/80">{clientUserAgent}</span>
        {statusChip}
      </div>
    </div>
  ) : (
    <div className={`${mobile ? 'w-full' : 'w-[260px] max-w-[28vw]'} space-y-1 font-mono text-[11px] leading-relaxed`}>
      {log.request_id ? <div className="truncate text-muted-foreground" title={`Request ID: ${log.request_id}`}>ID: {log.request_id}</div> : null}
      <div className="flex min-w-0 items-center gap-1.5" title={t('usage.clientUserAgent')}>
        <span className="w-4 shrink-0 font-sans font-semibold text-muted-foreground">C</span>
        <span className="min-w-0 truncate text-foreground/80">{clientUserAgent || '-'}</span>
      </div>
      <div className="flex min-w-0 items-center gap-1.5" title={t('usage.upstreamUserAgent')}>
        <span className="w-4 shrink-0 font-sans font-semibold text-muted-foreground">U</span>
        <span className="min-w-0 truncate text-foreground/80">{upstreamLabel}</span>
        {statusChip}
      </div>
    </div>
  )

  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <div
          tabIndex={0}
          aria-label={`${t('usage.clientUserAgent')}: ${clientUserAgent || '-'}; ${t('usage.upstreamUserAgent')}: ${upstreamLabel}; ${statusLabel}`}
          className="cursor-help focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
        >
          {content}
        </div>
      </TooltipTrigger>
      <TooltipContent side="top" sideOffset={6} className="max-w-[440px] p-3">
        <div className="space-y-2 text-xs">
          <div>
            <div className="font-semibold text-background/70">{t('usage.clientUserAgent')}</div>
            <div className="mt-0.5 break-all font-mono leading-relaxed">{clientUserAgent || '-'}</div>
          </div>
          <div>
            <div className="font-semibold text-background/70">{t('usage.upstreamUserAgent')}</div>
            <div className="mt-0.5 break-all font-mono leading-relaxed">{upstreamLabel}</div>
          </div>
          {log.request_id ? <div className="break-all font-mono">Request ID: {log.request_id}</div> : null}
          {log.upstream_request_id ? <div className="break-all font-mono">Upstream ID: {log.upstream_request_id}</div> : null}
          {log.upstream_proxy_name ? <div className="break-all">Proxy: {log.upstream_proxy_name}{log.upstream_proxy_id ? ` (#${log.upstream_proxy_id})` : ''}</div> : null}
          <div className="font-semibold">{statusLabel}</div>
          {log.via_websocket ? (
            <div className="leading-relaxed text-background/70">{t('usage.userAgentWebSocketHint')}</div>
          ) : null}
        </div>
      </TooltipContent>
    </Tooltip>
  )
}

// New usage rows resolve by immutable incident ID. Only historical rows without
// an ID fall back to the legacy nearest-timestamp inference endpoint.
function CyberPolicyDetailButton({ log }: { log: UsageLog }) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  const [loading, setLoading] = useState(false)
  const [detail, setDetail] = useState<PromptPolicyIncidentDetailResponse | null>(null)
  const [legacyDetail, setLegacyDetail] = useState<PromptFilterLog | null>(null)
  const [legacyInferred, setLegacyInferred] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [loaded, setLoaded] = useState(false)

  const handleOpen = async () => {
    setOpen(true)
    if (loaded || loading) return
    setLoading(true)
    setError(null)
    try {
      if (log.prompt_policy_incident_id) {
        setDetail(await api.getPromptPolicyIncident(log.prompt_policy_incident_id))
      } else {
        const res = await api.matchPromptFilterLog({
          at: log.created_at,
          endpoint: log.endpoint,
          apiKeyId: log.api_key_id || undefined,
        })
        setLegacyDetail(res.log)
        setLegacyInferred(res.legacy_inferred)
      }
      setLoaded(true)
    } catch (err) {
      setError(err instanceof Error ? err.message : String(err))
    } finally {
      setLoading(false)
    }
  }

  const incident = detail?.incident
  const content = (incident?.prompt_text || incident?.prompt_preview || legacyDetail?.full_text || legacyDetail?.text_preview || '').trim()
  const score = (value: number | null | undefined) => value === null || value === undefined ? t('usage.cyberPolicyUnscored') : String(value)

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
          ) : incident ? (
            <div className="space-y-3">
              <div className="grid gap-2 text-xs sm:grid-cols-2 lg:grid-cols-3">
                <CyberPolicyField label={t('usage.cyberPolicyUpstreamResult')} value={`${incident.status_code || '-'} · ${incident.upstream_error_code || 'cyber_policy'}`} />
                <CyberPolicyField label={t('usage.cyberPolicyLocalResult')} value={`${t(`usage.cyberPolicyState.${incident.local_evaluation_state}`)} · ${t(`usage.cyberPolicyOutcome.${incident.local_outcome}`)}`} />
                <CyberPolicyField label={t('usage.cyberPolicyLocalMiss')} value={incident.local_miss ? t('promptFilter.testResultYes') : t('promptFilter.testResultNo')} danger={incident.local_miss} />
                <CyberPolicyField label={t('usage.cyberPolicyExecutionScore')} value={score(incident.local_score)} />
                <CyberPolicyField label={t('usage.cyberPolicyAuditScore')} value={score(incident.local_audit_score)} />
                <CyberPolicyField label={t('usage.cyberPolicyTransport')} value={`${incident.protocol || '-'} · ${incident.transport || '-'}`} />
                <CyberPolicyField label={t('usage.cyberPolicyAccountAttempt')} value={`${incident.account_id || '-'} · #${incident.attempt_index || '-'}`} />
                <CyberPolicyField label={t('usage.cyberPolicyReview')} value={incident.local_review_model ? `${incident.local_review_model} · ${incident.local_review_flagged ? t('promptFilter.testReviewFlagged') : t('promptFilter.testReviewCleared')}` : t('promptFilter.testReviewSkipped')} />
                <CyberPolicyField label={t('usage.cyberPolicyCandidate')} value={detail.candidate ? `${detail.candidate.status} · #${detail.candidate.id}` : '-'} />
              </div>
              {incident.local_evaluation_state === 'legacy_unknown' ? (
                <div className="rounded-md border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-xs text-amber-700 dark:text-amber-300">{t('usage.cyberPolicyLegacyUnknown')}</div>
              ) : null}
              {(incident.local_reason || incident.local_reason_code) ? (
                <div className="rounded-md border border-border bg-muted/20 px-3 py-2 text-xs">
                  <div className="font-semibold text-muted-foreground">{t('usage.cyberPolicyReason')}</div>
                  <div className="mt-1 break-words">{incident.local_reason || incident.local_reason_code}</div>
                </div>
              ) : null}
              {detail.matches.length > 0 ? (
                <div>
                  <div className="mb-1.5 text-xs font-semibold text-muted-foreground">{t('usage.cyberPolicyMatches')}</div>
                  <div className="flex flex-wrap gap-1.5">
                    {detail.matches.map((match, index) => <Badge key={`${match.name}-${index}`} variant="secondary">{match.name} · {match.weight}</Badge>)}
                  </div>
                </div>
              ) : null}
              {content ? (
                <div>
                  <div className="mb-1.5 flex items-center justify-between">
                    <span className="text-xs font-semibold text-muted-foreground">{t('usage.cyberPolicyRequestContent')}</span>
                    <button type="button" onClick={() => void navigator.clipboard?.writeText(content)} className="text-xs font-medium text-primary hover:underline">{t('common.copy')}</button>
                  </div>
                  <pre className="max-h-[50vh] overflow-auto whitespace-pre-wrap break-words rounded-md border border-border bg-muted/30 p-3 text-xs leading-relaxed text-foreground">{content}</pre>
                </div>
              ) : null}
            </div>
          ) : content ? (
            <div className="space-y-2">
              {legacyInferred ? <div className="rounded-md border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-xs text-amber-700 dark:text-amber-300">{t('usage.cyberPolicyLegacyInferred')}</div> : null}
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

function CyberPolicyField({ label, value, danger = false }: { label: string; value: string; danger?: boolean }) {
  return (
    <div className="rounded-md border border-border bg-muted/20 px-2.5 py-2">
      <div className="font-semibold text-muted-foreground">{label}</div>
      <div className={cn('mt-1 break-words font-mono text-foreground', danger && 'text-red-600 dark:text-red-300')}>{value}</div>
    </div>
  )
}

const usageTableHeadClass = 'text-[13px] font-semibold'
const usageTableTextClass = 'text-[14px]'
const usageTableMonoClass = 'font-mono text-[13px] tabular-nums'
const usageTableBadgeClass = 'text-[13px]'

function UsageInputTokenCount({ log }: { log: UsageLog }) {
  const { t } = useTranslation()
  const tokens = getUsageTokenBreakdown(log)
  const title = tokens.isClaude
    ? t('usage.claudeInputTooltip', {
      input: formatTokens(tokens.inputTokens, true),
      total: formatTokens(tokens.totalInputTokens, true),
      read: formatTokens(tokens.cacheReadTokens, true),
      write: formatTokens(tokens.cacheWriteTokens, true),
    })
    : `${t('usage.inputTokens')}: ${formatTokens(tokens.inputTokens, true)}`

  return <span className="text-blue-500" title={title}>↓{formatTokens(tokens.inputTokens, true)}</span>
}

function UsageCacheBadges({ log, align = 'end' }: { log: UsageLog; align?: 'start' | 'end' }) {
  const { t } = useTranslation()
  const tokens = getUsageTokenBreakdown(log)
  if (tokens.cacheReadTokens === 0 && tokens.cacheWriteTokens === 0) {
    return <span className={`${usageTableMonoClass} text-muted-foreground/50`}>-</span>
  }
  const readTitle = t('usage.cacheReadTooltip', { tokens: formatTokens(tokens.cacheReadTokens, true) })
  const writeTitle = t('usage.cacheCreateTooltip', {
    tokens: formatTokens(tokens.cacheWriteTokens, true),
    m5: formatTokens(tokens.cacheWrite5mTokens, true),
    h1: formatTokens(tokens.cacheWrite1hTokens, true),
  })

  // 读/写两枚徽章横排(空间不够再换行),不再竖着堆叠把整行撑高
  return (
    <div className={cn('flex flex-wrap items-center gap-1', align === 'start' ? 'justify-start' : 'justify-end')}>
      {tokens.cacheReadTokens > 0 && (
        <Badge variant="outline" title={readTitle} aria-label={readTitle} className={`${usageTableBadgeClass} gap-1 border-transparent bg-indigo-500/10 text-indigo-600 dark:bg-indigo-500/20 dark:text-indigo-400`}>
          <DatabaseZap className="size-3.5" aria-hidden="true" />
          {formatTokens(tokens.cacheReadTokens, true)}
        </Badge>
      )}
      {tokens.cacheWriteTokens > 0 && (
        <Badge variant="outline" title={writeTitle} aria-label={writeTitle} className={`${usageTableBadgeClass} gap-1 border-transparent bg-amber-500/10 text-amber-600 dark:bg-amber-500/20 dark:text-amber-400`}>
          <DatabaseBackup className="size-3.5" aria-hidden="true" />
          {formatTokens(tokens.cacheWriteTokens, true)}
        </Badge>
      )}
    </div>
  )
}

// UsageTimeCell 时间列双色:日期弱化、时分秒突出,扫表时先看到时间;整串仍在 title。
function UsageTimeCell({ value }: { value?: string | null }) {
  const full = formatBeijingTime(value)
  const m = /^(\d{4}-\d{2}-\d{2}) (\d{2}:\d{2}:\d{2})$/.exec(full)
  if (!m) return <span>{full}</span>
  return (
    <span title={full}>
      <span className="text-[11px] text-muted-foreground/60">{m[1]}</span>
      <span className="ml-1.5 text-foreground/80">{m[2]}</span>
    </span>
  )
}

// UsageEndpointText 端点列双色:公共前缀 /v1/ 弱化,真正区分请求类型的尾段突出。
function UsageEndpointText({ value }: { value: string }) {
  const m = /^(\/v\d+\/)(.+)$/.exec(value)
  if (!m) return <span className="text-muted-foreground">{value}</span>
  return (
    <>
      <span className="text-muted-foreground/50">{m[1]}</span>
      <span className="text-foreground/80">{m[2]}</span>
    </>
  )
}

function StreamBadge({ stream }: { stream: boolean }) {
  return (
    <Badge
      variant="outline"
      className={cn(
        usageTableBadgeClass,
        'border-transparent',
        stream
          ? 'bg-indigo-500/12 text-indigo-600 dark:bg-indigo-500/20 dark:text-indigo-400'
          // 非流式是少数派,给琥珀色让它在一列 stream 里能被一眼挑出来
          : 'bg-amber-500/12 text-amber-700 dark:bg-amber-500/20 dark:text-amber-300',
      )}
    >
      {stream ? 'stream' : 'sync'}
    </Badge>
  )
}
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

type UsageTableColumn = 'status' | 'error' | 'model' | 'account' | 'apiKey' | 'clientIp' | 'userAgent' | 'endpoint' | 'type' | 'token' | 'cost' | 'cached' | 'wsAcquire' | 'tokensPerSec' | 'timing' | 'time'

const USAGE_COLUMN_DEFINITIONS: Array<{ key: UsageTableColumn; labelKey: string }> = [
  { key: 'status', labelKey: 'usage.tableStatus' },
  { key: 'model', labelKey: 'usage.tableModel' },
  { key: 'account', labelKey: 'usage.tableAccount' },
  { key: 'apiKey', labelKey: 'usage.tableApiKey' },
  { key: 'clientIp', labelKey: 'usage.tableClientIP' },
  { key: 'userAgent', labelKey: 'usage.tableUserAgent' },
  { key: 'endpoint', labelKey: 'usage.tableEndpoint' },
  { key: 'type', labelKey: 'usage.tableType' },
  { key: 'token', labelKey: 'usage.tableToken' },
  { key: 'cached', labelKey: 'usage.tableCached' },
  { key: 'wsAcquire', labelKey: 'usage.tableWsAcquire' },
  { key: 'timing', labelKey: 'usage.tableTiming' },
  { key: 'tokensPerSec', labelKey: 'usage.tableTokensPerSec' },
  { key: 'cost', labelKey: 'usage.tableCost' },
  // 错误摘要宽度随内容波动，放倒数第二列避免撑开中段（issue #522）
  { key: 'error', labelKey: 'usage.tableError' },
  { key: 'time', labelKey: 'usage.tableTime' },
]

// 列设置菜单按这个顺序列出可选列，与表头顺序一致。
const USAGE_TABLE_COLUMN_ORDER: readonly UsageTableColumn[] = USAGE_COLUMN_DEFINITIONS.map((column) => column.key)

const USAGE_VISIBLE_COLUMNS_KEY = 'codex2api:usage:visible-columns'
const DEFAULT_USAGE_VISIBLE_COLUMNS: Record<UsageTableColumn, boolean> = {
  status: true,
  error: true,
  model: true,
  account: true,
  apiKey: true,
  clientIp: true,
  userAgent: true,
  endpoint: true,
  type: true,
  token: true,
  cost: true,
  cached: true,
  // 取得连接耗时属于深挖排障信息，默认隐藏，需在列设置中手动开启
  wsAcquire: false,
  tokensPerSec: true,
  // 首字 + 总耗时聚合为「用时」一列
  timing: true,
  time: true,
}

/**
 * Output tokens/sec from existing log fields — no backend change needed.
 * Prefers generation window (duration − first_token) so TTFT does not drag the rate down.
 */
function computeOutputTokensPerSec(log: UsageLog): number | null {
  if (log.status_code >= 400) return null
  const outputTokens = Math.max(0, log.output_tokens || log.completion_tokens || 0)
  if (outputTokens <= 0 || log.duration_ms <= 0) return null

  let generationMs = log.duration_ms
  if (log.first_token_ms > 0 && log.first_token_ms < log.duration_ms) {
    generationMs = log.duration_ms - log.first_token_ms
  }
  // Guard tiny windows (e.g. first token almost equals end) that explode the rate.
  if (generationMs < 20) generationMs = log.duration_ms
  if (generationMs <= 0) return null

  return outputTokens / (generationMs / 1000)
}

function formatTokensPerSec(value: number): string {
  if (!Number.isFinite(value) || value <= 0) return '-'
  if (value >= 1000) return `${(value / 1000).toFixed(1)}k`
  if (value >= 100) return value.toFixed(0)
  if (value >= 10) return value.toFixed(1)
  return value.toFixed(2)
}

function tokensPerSecClassName(value: number): string {
  // Rough bands for visual scan; absolute numbers vary by model.
  if (value >= 80) return 'text-emerald-600 dark:text-emerald-400'
  if (value >= 30) return 'text-foreground'
  if (value >= 10) return 'text-amber-600 dark:text-amber-400'
  return 'text-red-500 dark:text-red-400'
}

function TokensPerSecCell({ log }: { log: UsageLog }) {
  const value = computeOutputTokensPerSec(log)
  if (value == null) {
    return <span className={`${usageTableMonoClass} text-muted-foreground/50`}>-</span>
  }
  return (
    <span className={`${usageTableMonoClass} ${tokensPerSecClassName(value)}`} title={`${value.toFixed(2)} tok/s`}>
      {formatTokensPerSec(value)}
      <span className="ml-0.5 text-[11px] font-medium opacity-70">tok/s</span>
    </span>
  )
}

function formatLatencyMs(ms: number): string {
  if (!Number.isFinite(ms) || ms <= 0) return '-'
  if (ms >= 1000) return `${(ms / 1000).toFixed(1)}s`
  return `${Math.round(ms)}ms`
}

type TimingTone = 'good' | 'warn' | 'bad' | 'muted'

function firstTokenTone(ms: number): TimingTone {
  if (ms <= 0) return 'muted'
  if (ms > 5000) return 'bad'
  if (ms > 2000) return 'warn'
  return 'good'
}

function durationTone(ms: number): TimingTone {
  if (ms <= 0) return 'muted'
  if (ms > 30000) return 'bad'
  if (ms > 10000) return 'warn'
  return 'good'
}

/** 数值色阶：在各自胶囊内单独表意快慢。 */
const TIMING_VALUE_TONE: Record<TimingTone, string> = {
  good: 'text-emerald-700 dark:text-emerald-300',
  warn: 'text-amber-700 dark:text-amber-300',
  bad: 'text-red-600 dark:text-red-400',
  muted: 'text-muted-foreground',
}

/**
 * 首字 / 耗时 用固定身份色胶囊区分（蓝 vs 紫），
 * 扫表时即使数值色相同也能立刻分辨是哪一项。
 */
const TIMING_PILL_SHELL = {
  first:
    'bg-sky-500/12 ring-1 ring-inset ring-sky-500/25 dark:bg-sky-500/16 dark:ring-sky-400/30',
  duration:
    'bg-violet-500/12 ring-1 ring-inset ring-violet-500/25 dark:bg-violet-500/16 dark:ring-violet-400/30',
  muted: 'bg-muted/60 ring-1 ring-inset ring-border',
} as const

const TIMING_PILL_LABEL = {
  first: 'text-sky-700/85 dark:text-sky-300/90',
  duration: 'text-violet-700/85 dark:text-violet-300/90',
  muted: 'text-muted-foreground',
} as const

function TimingPill({
  kind,
  label,
  valueMs,
  tone,
}: {
  kind: 'first' | 'duration'
  label: string
  valueMs: number
  tone: TimingTone
}) {
  const display = valueMs > 0 ? formatLatencyMs(valueMs) : '-'
  const shell = tone === 'muted' ? TIMING_PILL_SHELL.muted : TIMING_PILL_SHELL[kind]
  const labelClass = tone === 'muted' ? TIMING_PILL_LABEL.muted : TIMING_PILL_LABEL[kind]

  return (
    <span
      className={cn(
        'inline-flex h-5 items-center gap-0.5 rounded-full px-1.5 whitespace-nowrap',
        shell,
      )}
    >
      <span className={cn('text-[10px] font-semibold leading-none tracking-tight', labelClass)}>
        {label}
      </span>
      <span
        className={cn(
          'font-mono text-[11px] font-bold tabular-nums leading-none',
          TIMING_VALUE_TONE[tone],
        )}
      >
        {display}
      </span>
    </span>
  )
}

/** 首字 + 总耗时聚合列：横向双色胶囊，避免把整行撑高。 */
function TimingCell({ log }: { log: UsageLog }) {
  const { t } = useTranslation()
  const firstMs = log.first_token_ms || 0
  const durationMs = log.duration_ms || 0
  if (firstMs <= 0 && durationMs <= 0) {
    return <span className={`${usageTableMonoClass} text-muted-foreground/50`}>-</span>
  }

  const firstLabel = firstMs > 0 ? formatLatencyMs(firstMs) : '-'
  const durationLabel = durationMs > 0 ? formatLatencyMs(durationMs) : '-'
  const title = [
    firstMs > 0 ? `${t('usage.timingFirst')}: ${firstLabel}` : null,
    durationMs > 0 ? `${t('usage.timingDuration')}: ${durationLabel}` : null,
  ]
    .filter(Boolean)
    .join(' · ')

  return (
    <div className="inline-flex items-center gap-1" title={title || undefined}>
      <TimingPill
        kind="first"
        label={t('usage.timingFirst')}
        valueMs={firstMs}
        tone={firstTokenTone(firstMs)}
      />
      <TimingPill
        kind="duration"
        label={t('usage.timingDuration')}
        valueMs={durationMs}
        tone={durationTone(durationMs)}
      />
    </div>
  )
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
        // 兼容旧版独立「首字 / 总耗时」列：任一开启则聚合列开启
        if (!('timing' in parsed)) {
          const oldFirst = 'firstToken' in parsed ? Boolean(parsed.firstToken) : true
          const oldDuration = 'duration' in parsed ? Boolean(parsed.duration) : true
          defaults.timing = oldFirst || oldDuration
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
  const [errorSummary, setErrorSummary] = useState<OpsErrorSummary | null>(null)
  const [searchInput, setSearchInput] = useState('')
  const [searchQuery, setSearchQuery] = useState('')
  const [filterStatus, setFilterStatus] = useState<UsageStatusFilter>('')
  const [filterModel, setFilterModel] = useState('')
  const [filterEndpoint, setFilterEndpoint] = useState('')
  const [filterApiKeyId, setFilterApiKeyId] = useState('')
  const [filterAccountId, setFilterAccountId] = useState(getInitialUsageAccountID)
  const [filterFast, setFilterFast] = useState('')
  const [filterType, setFilterType] = useState<UsageTypeFilter>('')
  const [filterErrorKind, setFilterErrorKind] = useState('')
  const [filterRetry, setFilterRetry] = useState<UsageRetryFilter>('')
  const [filterTransport, setFilterTransport] = useState<UsageTransportFilter>('')
  const [showAdvancedFilters, setShowAdvancedFilters] = useState(false)
  const [apiKeys, setAPIKeys] = useState<APIKeyRow[]>([])
  const [modelOptions, setModelOptions] = useState<string[]>([])
  const [grokModelOptions, setGrokModelOptions] = useState<string[]>([])
  const [claudeModelOptions, setClaudeModelOptions] = useState<string[]>([])
  const [apiKeyLoadFailed, setAPIKeyLoadFailed] = useState(false)
  const showFastFilter = true
  const pageSizeOptions = DEFAULT_PAGE_SIZE_OPTIONS
  const searchTimer = useRef<ReturnType<typeof setTimeout>>(null)
  const [visibleColumns, setVisibleColumns] = useState<Record<UsageTableColumn, boolean>>(getInitialUsageVisibleColumns)
  // 列设置菜单与账号管理页共用同一个组件，标签按当前语言从列定义里取。
  const usageColumnLabels = useMemo(
    () => Object.fromEntries(USAGE_COLUMN_DEFINITIONS.map((column) => [column.key, t(column.labelKey)])) as Record<UsageTableColumn, string>,
    [t],
  )
  const [showAnalysis, setShowAnalysis] = useState(getInitialAnalysisVisibility)
  const [channel, setChannel] = useUsageChannel()

  // 搜索防抖：输入停止 400ms 后触发查询
  const handleSearchChange = useCallback((value: string) => {
    setSearchInput(value)
    if (searchTimer.current) clearTimeout(searchTimer.current)
    searchTimer.current = setTimeout(() => {
      setSearchQuery(value.trim())
      setPage(1)
    }, 400)
  }, [])

  useEffect(() => () => {
    if (searchTimer.current) clearTimeout(searchTimer.current)
  }, [])

  // 仅加载轻量统计（秒级）—— 联动同页 timeRange,与下方请求记录的范围保持一致
  const loadStats = useCallback(async () => {
    const { start, end } = resolveRangeISO(timeRange, customRange)
    const [stats, settings] = await Promise.all([
      api.getUsageStats({ start, end, channel: channel || undefined }),
      api.getSettings().catch((): SystemSettings | null => null),
    ])
    return { stats, settings }
  }, [timeRange, customRange, channel])

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

  const buildLogFilterParams = useCallback(() => {
    const { start, end } = resolveRangeISO(timeRange, customRange)
    return {
      start,
      end,
      q: searchQuery || undefined,
      model: filterModel || undefined,
      endpoint: filterEndpoint || undefined,
      apiKeyId: filterApiKeyId || undefined,
      accountId: filterAccountId || undefined,
      fast: filterFast || undefined,
      stream: filterType === 'stream' ? 'true' : filterType === 'sync' ? 'false' : undefined,
      compact: filterType === 'compact' ? 'true' : undefined,
      hasCompactionHistory: filterType === 'history' ? 'true' : undefined,
      channel: channel || undefined,
      status: filterStatus && filterStatus !== 'error' ? filterStatus : undefined,
      errorOnly: filterStatus === 'error' ? 'true' : undefined,
      errorKind: filterErrorKind || undefined,
      retry: filterRetry || undefined,
      viaWebsocket: filterTransport === 'ws' ? 'true' : filterTransport === 'http' ? 'false' : undefined,
    }
  }, [timeRange, customRange, searchQuery, filterModel, filterEndpoint, filterApiKeyId, filterAccountId, filterFast, filterType, channel, filterStatus, filterErrorKind, filterRetry, filterTransport])

  // 服务端分页加载日志
  const loadLogs = useCallback(async () => {
    setLogsLoading(true)
    try {
      const res = await api.getUsageLogsPaged({
        ...buildLogFilterParams(),
        page,
        pageSize,
      })
      setLogs(res.logs ?? [])
      setLogsTotal(res.total ?? 0)
    } catch {
      // 静默容错
    } finally {
      setLogsLoading(false)
    }
  }, [buildLogFilterParams, page, pageSize])

  const loadErrorSummary = useCallback(async () => {
    try {
      const params = buildLogFilterParams()
      const summary = await api.getUsageLogsErrorSummary({
        ...params,
        status: undefined,
        errorOnly: undefined,
      })
      setErrorSummary(summary)
    } catch {
      setErrorSummary(null)
    }
  }, [buildLogFilterParams])

  // 首次加载 + timeRange/page 变更时重新拉取日志
  useEffect(() => {
    void loadLogs()
  }, [loadLogs])

  useEffect(() => {
    void loadErrorSummary()
  }, [loadErrorSummary])

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
        setGrokModelOptions(response.grok_models ?? [])
        setClaudeModelOptions(response.claude_models ?? [])
      } catch {
        if (active) {
          setModelOptions([])
          setGrokModelOptions([])
          setClaudeModelOptions([])
        }
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
  // 下拉选项跟随渠道过滤：codex 只列 Codex manifest 目录，grok 只列 Grok 账号声明模型，
  // 全部渠道两者都列；再并上当前范围实际用过的模型（统计已按渠道过滤），去重后目录顺序优先。
  const modelFilterOptions = useMemo(() => {
    const seen = new Set<string>()
    const merged: string[] = []
    const catalog = channel === 'grok'
      ? grokModelOptions
      : channel === 'codex'
        ? modelOptions
        : channel === 'claude'
          ? claudeModelOptions
          : [...modelOptions, ...grokModelOptions, ...claudeModelOptions]
    for (const m of catalog) {
      const key = m.trim()
      if (key && !seen.has(key)) { seen.add(key); merged.push(key) }
    }
    for (const item of modelStats) {
      const key = (item.model || '').trim()
      if (key && key !== 'unknown' && !seen.has(key)) { seen.add(key); merged.push(key) }
    }
    return merged
  }, [modelOptions, grokModelOptions, claudeModelOptions, modelStats, channel])
  const featureStats = stats?.feature_stats
  const endpointStats = stats?.endpoint_stats ?? []
  const apiKeyStats = stats?.api_key_stats ?? []
  const rpm = stats?.rpm ?? 0
  const tpm = stats?.tpm ?? 0
  const errorRate = stats?.error_rate ?? 0
  const avgDurationMs = stats?.avg_duration_ms ?? 0
  const successRequests = rangeRequests - Math.round(rangeRequests * errorRate / 100)
  const showAPIKeyFilter = !apiKeyLoadFailed && apiKeys.length > 0
  const advancedFilterCount = [
    filterEndpoint,
    filterType,
    filterFast,
    filterErrorKind,
    filterRetry,
    filterTransport,
  ].filter(Boolean).length
  const hasActiveFilters = Boolean(
    searchInput
    || filterStatus
    || filterModel
    || filterEndpoint
    || filterApiKeyId
    || filterAccountId
    || filterType
    || filterFast
    || filterErrorKind
    || filterRetry
    || filterTransport,
  )
  const statusFilterOptions: Array<{ value: UsageStatusFilter; label: string; tone?: string }> = [
    { value: '', label: t('usage.statusAll') },
    { value: '2xx', label: t('usage.statusSuccess'), tone: 'text-emerald-600 dark:text-emerald-300' },
    { value: 'error', label: t('usage.statusErrors'), tone: 'text-red-600 dark:text-red-300' },
    { value: '4xx', label: '4xx', tone: 'text-amber-600 dark:text-amber-300' },
    { value: '5xx', label: '5xx', tone: 'text-red-600 dark:text-red-300' },
    { value: '401', label: '401', tone: 'text-red-600 dark:text-red-300' },
    { value: '429', label: '429', tone: 'text-amber-600 dark:text-amber-300' },
    { value: '499', label: '499', tone: 'text-slate-600 dark:text-slate-300' },
  ]
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
  const resetLogFilters = () => {
    setSearchInput('')
    setSearchQuery('')
    setFilterStatus('')
    setFilterModel('')
    setFilterEndpoint('')
    setFilterApiKeyId('')
    setFilterAccountId('')
    setFilterType('')
    setFilterFast('')
    setFilterErrorKind('')
    setFilterRetry('')
    setFilterTransport('')
    setPage(1)
  }

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
          titleAdornment={<ChannelFilter value={channel} onChange={setChannel} />}
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

        <div key={channel || 'all'} className="space-y-6 animate-channel-switch-in">
        {/* Stat overview: 6 metrics in a single row */}
        <div className="grid grid-cols-2 gap-2.5 sm:gap-3 md:grid-cols-3 xl:grid-cols-6">
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
                <span>{t('usage.totalInputTokens')}: {formatTokens(rangePromptTokens, showFullUsageNumbers)}</span>
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
                <div className="flex size-9 items-center justify-center rounded-lg bg-[hsl(var(--warning-bg))] text-[hsl(var(--warning))]">
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
            <div className="grid grid-cols-1 gap-3 lg:grid-cols-2">
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
                <div className="inline-flex max-w-full flex-wrap rounded-xl border border-border/70 bg-muted/40 p-1 shadow-2xs">
                  {USAGE_TIME_RANGE_OPTIONS.map((key) => {
                    const active = timeRange === key
                    return (
                      <button
                        key={key}
                        type="button"
                        onClick={() => {
                          setTimeRange(key)
                          setPage(1)
                          setShowCustomPopover(false)
                        }}
                        className={cn(
                          'whitespace-nowrap px-3 py-1.5 text-xs font-semibold rounded-lg transition-all duration-150',
                          active
                            ? 'bg-background text-foreground shadow-xs ring-1 ring-border/50 font-bold'
                            : 'text-muted-foreground hover:bg-background/40 hover:text-foreground'
                        )}
                      >
                        {key === 'today' ? t('usage.today') : t(`dashboard.timeRange${key.toUpperCase()}`)}
                      </button>
                    )
                  })}
                  <button
                    ref={customChipRef}
                    type="button"
                    onClick={() => setShowCustomPopover((v) => !v)}
                    className={cn(
                      'whitespace-nowrap px-3 py-1.5 text-xs font-semibold rounded-lg transition-all duration-150',
                      timeRange === 'custom'
                        ? 'bg-background text-foreground shadow-xs ring-1 ring-border/50 font-bold'
                        : 'text-muted-foreground hover:bg-background/40 hover:text-foreground'
                    )}
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
              <div className="flex shrink-0 items-center gap-2">
                <span className="whitespace-nowrap text-xs text-muted-foreground">{logsLoading ? t('common.loading') : t('usage.recordsCount', { count: logsTotal })}</span>
                <Button
                  variant="outline"
                  size="sm"
                  disabled={logsLoading}
                  title={t('common.refresh')}
                  aria-label={t('common.refresh')}
                  onClick={() => {
                    void reloadSilently()
                    void loadLogs()
                  }}
                >
                  <RefreshCw className={cn('size-3.5', logsLoading && 'animate-spin')} />
                  {t('common.refresh')}
                </Button>
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

            {/* 状态快速筛选 */}
            <div className="mb-3 flex flex-wrap items-center gap-1.5">
              {statusFilterOptions.map((option) => (
                <button
                  key={option.value || 'all'}
                  type="button"
                  onClick={() => { setFilterStatus(option.value); setPage(1) }}
                  className={cn(
                    'h-8 rounded-lg border px-3 text-[13px] font-medium transition-colors',
                    filterStatus === option.value
                      ? 'border-primary/35 bg-primary/10 text-primary shadow-sm'
                      : 'border-border bg-background text-muted-foreground hover:bg-muted/60 hover:text-foreground',
                    filterStatus !== option.value && option.tone,
                  )}
                >
                  {option.label}
                </button>
              ))}
              {filterStatus && !statusFilterOptions.some((option) => option.value === filterStatus) ? (
                <button
                  type="button"
                  onClick={() => { setFilterStatus(''); setPage(1) }}
                  className="inline-flex h-8 items-center gap-1 rounded-lg border border-primary/35 bg-primary/10 px-3 text-[13px] font-medium text-primary shadow-sm"
                  title={t('usage.clearStatusFilter')}
                >
                  HTTP {filterStatus}
                  <X className="size-3.5" />
                </button>
              ) : null}
            </div>

            {/* 错误摘要：不受上方状态按钮影响，便于在各错误类别之间快速切换 */}
            {errorSummary && errorSummary.total_errors > 0 ? (
              <div className="mb-3 grid grid-cols-2 gap-2 sm:grid-cols-4 xl:grid-cols-7">
                {[
                  { label: t('usage.errorTotal'), value: errorSummary.total_errors, status: 'error' as UsageStatusFilter },
                  { label: '4xx', value: errorSummary.status_4xx, status: '4xx' as UsageStatusFilter },
                  { label: '5xx', value: errorSummary.status_5xx, status: '5xx' as UsageStatusFilter },
                  { label: '401', value: errorSummary.unauthorized, status: '401' as UsageStatusFilter },
                  { label: '429', value: errorSummary.rate_limited, status: '429' as UsageStatusFilter },
                  { label: '499', value: errorSummary.canceled, status: '499' as UsageStatusFilter },
                  { label: t('usage.retryRequests'), value: errorSummary.retry_attempts, retry: 'true' as UsageRetryFilter },
                ].map((item) => (
                  <button
                    key={item.label}
                    type="button"
                    onClick={() => {
                      if (item.status) setFilterStatus(item.status)
                      if (item.retry) {
                        setFilterRetry(item.retry)
                        setFilterStatus('error')
                      }
                      setPage(1)
                    }}
                    className="flex min-w-0 items-center justify-between gap-2 rounded-lg border border-border/80 bg-muted/25 px-3 py-2 text-left transition-colors hover:border-primary/30 hover:bg-primary/5"
                  >
                    <span className="truncate text-[11px] font-medium text-muted-foreground">{item.label}</span>
                    <span className="font-geist-mono text-[13px] font-semibold tabular-nums text-foreground">
                      {item.value.toLocaleString()}
                    </span>
                  </button>
                ))}
              </div>
            ) : null}

            {/* 主筛选栏 */}
            <div className="toolbar-surface mb-4 overflow-visible">
              <div className="flex flex-wrap items-center gap-2 max-lg:gap-1.5">
                <div className="relative min-w-60 flex-1 max-sm:w-full">
                  <Search className="pointer-events-none absolute left-2.5 top-1/2 size-3.5 -translate-y-1/2 text-muted-foreground" />
                  <Input
                    className="h-8 rounded-lg pl-8 text-[13px]"
                    placeholder={t('usage.searchLogs')}
                    value={searchInput}
                    onChange={(e: React.ChangeEvent<HTMLInputElement>) => handleSearchChange(e.target.value)}
                  />
                </div>

                <Select
                  className="w-full min-w-0 sm:w-40 shrink-0"
                  compact
                  value={filterModel}
                  onValueChange={(value) => { setFilterModel(value); setPage(1) }}
                  placeholder={t('usage.allModels')}
                  options={[
                    { label: t('usage.allModels'), value: '' },
                    ...modelFilterOptions.map((model) => ({ label: model, value: model })),
                  ]}
                />

                {showAPIKeyFilter ? (
                  <Select
                    className="w-full min-w-0 sm:w-52 shrink-0"
                    compact
                    value={filterApiKeyId}
                    onValueChange={(value) => { setFilterApiKeyId(value); setPage(1) }}
                    placeholder={t('usage.allApiKeys')}
                    options={apiKeyOptions}
                  />
                ) : null}

                <button
                  type="button"
                  onClick={() => setShowAdvancedFilters((current) => !current)}
                  aria-expanded={showAdvancedFilters}
                  className={cn(
                    'inline-flex h-8 shrink-0 items-center justify-center gap-1.5 rounded-lg border px-2.5 text-[13px] font-medium transition-colors max-sm:w-full',
                    showAdvancedFilters || advancedFilterCount > 0
                      ? 'border-primary/30 bg-primary/8 text-primary'
                      : 'border-border bg-background text-muted-foreground hover:bg-muted/50 hover:text-foreground',
                  )}
                >
                  <SlidersHorizontal className="size-3.5" />
                  {t('usage.moreFilters')}
                  {advancedFilterCount > 0 ? (
                    <span className="rounded-full bg-primary px-1.5 text-[10px] font-semibold leading-4 text-primary-foreground">
                      {advancedFilterCount}
                    </span>
                  ) : null}
                  <ChevronDown className={cn('size-3.5 transition-transform', showAdvancedFilters && 'rotate-180')} />
                </button>

                {hasActiveFilters ? (
                  <button
                    type="button"
                    onClick={resetLogFilters}
                    className="inline-flex h-8 shrink-0 items-center gap-1 rounded-lg border border-border bg-background px-2.5 text-[13px] text-muted-foreground transition-colors hover:bg-muted/50 hover:text-foreground"
                  >
                    <X className="size-3.5" />
                    {t('usage.clearFilters')}
                  </button>
                ) : null}

                <div className="ml-auto shrink-0">
                  <ColumnSettingsMenu
                    columnOrder={USAGE_TABLE_COLUMN_ORDER}
                    columns={visibleColumns}
                    labels={usageColumnLabels}
                    onToggle={(key) => setVisibleColumns((current) => ({ ...current, [key]: !current[key] }))}
                    onReset={() => setVisibleColumns({ ...DEFAULT_USAGE_VISIBLE_COLUMNS })}
                    title={t('accounts.columnSettings')}
                    resetTitle={t('accounts.columnReset')}
                  />
                </div>
              </div>

              {filterAccountId ? (
                <button
                  type="button"
                  onClick={() => { setFilterAccountId(''); setPage(1) }}
                  className="mt-2 inline-flex h-8 items-center gap-1 rounded-lg border border-primary/30 bg-primary/10 px-2.5 text-[13px] font-medium text-primary transition-colors hover:bg-primary/15"
                  title={t('usage.accountIdFilterTitle', { id: filterAccountId })}
                >
                  {t('usage.accountIdFilter', { id: filterAccountId })}
                  <X className="size-3.5" />
                </button>
              ) : null}

              {showAdvancedFilters ? (
                <div className="mt-3 grid grid-cols-1 gap-2 border-t border-border/70 pt-3 sm:grid-cols-2 lg:grid-cols-3 xl:grid-cols-6">
                  <Select
                    compact
                    value={filterEndpoint}
                    onValueChange={(value) => { setFilterEndpoint(value); setPage(1) }}
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
                  <Select
                    compact
                    value={filterType}
                    onValueChange={(value) => { setFilterType(value as UsageTypeFilter); setPage(1) }}
                    placeholder={t('usage.allTypes')}
                    options={[
                      { label: t('usage.allTypes'), value: '' },
                      { label: 'Stream', value: 'stream' },
                      { label: 'Sync', value: 'sync' },
                      { label: t('usage.compactionTrigger'), value: 'compact' },
                      { label: t('usage.compactionHistory'), value: 'history' },
                    ]}
                  />
                  <Select
                    compact
                    value={filterErrorKind}
                    onValueChange={(value) => { setFilterErrorKind(value); setPage(1) }}
                    placeholder={t('usage.allErrorKinds')}
                    options={[
                      { label: t('usage.allErrorKinds'), value: '' },
                      { label: 'rate_limited_model', value: 'rate_limited_model' },
                      { label: 'client', value: 'client' },
                      { label: 'server', value: 'server' },
                      { label: 'unauthorized', value: 'unauthorized' },
                      { label: 'transport', value: 'transport' },
                      { label: 'usage_limit', value: 'usage_limit' },
                      { label: 'payment_required', value: 'payment_required' },
                      { label: 'cyber_policy', value: 'cyber_policy' },
                      { label: 'upstream_error', value: 'upstream_error' },
                      { label: 'upstream_timeout', value: 'upstream_timeout' },
                    ]}
                  />
                  <Select
                    compact
                    value={filterRetry}
                    onValueChange={(value) => { setFilterRetry(value as UsageRetryFilter); setPage(1) }}
                    placeholder={t('usage.allAttempts')}
                    options={[
                      { label: t('usage.allAttempts'), value: '' },
                      { label: t('usage.initialRequests'), value: 'false' },
                      { label: t('usage.retryRequests'), value: 'true' },
                    ]}
                  />
                  <Select
                    compact
                    value={filterTransport}
                    onValueChange={(value) => { setFilterTransport(value as UsageTransportFilter); setPage(1) }}
                    placeholder={t('usage.allTransports')}
                    options={[
                      { label: t('usage.allTransports'), value: '' },
                      { label: 'HTTP', value: 'http' },
                      { label: 'WebSocket', value: 'ws' },
                    ]}
                  />
                  {showFastFilter ? (
                    <button
                      type="button"
                      onClick={() => { setFilterFast(filterFast === 'true' ? '' : 'true'); setPage(1) }}
                      className={cn(
                        'inline-flex h-8 items-center justify-center gap-1 rounded-lg border px-2.5 text-[13px] font-medium transition-colors',
                        filterFast === 'true'
                          ? 'border-blue-500/40 bg-blue-500/12 text-blue-600 dark:bg-blue-500/20 dark:text-blue-400'
                          : 'border-border bg-background text-muted-foreground hover:bg-muted/50 hover:text-foreground',
                      )}
                    >
                      <Zap className="size-3.5" />
                      Fast
                    </button>
                  ) : null}
                </div>
              ) : null}
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
                  const hasDetails = visibleColumns.account || visibleColumns.apiKey || visibleColumns.clientIp || visibleColumns.endpoint || visibleColumns.userAgent
                  const hasMetrics = visibleColumns.token || visibleColumns.cached || visibleColumns.timing || visibleColumns.tokensPerSec || visibleColumns.cost
                  return (
                    <div
                      key={log.id}
                      className="rounded-xl border border-border bg-background/70 p-3.5 shadow-sm"
                    >
                      {/* flex-wrap + nowrap 时间：徽标再多也只会把时间挤到下一行，不会挤出视口（issue #522） */}
                      <div className="flex flex-wrap items-start justify-between gap-2">
                        <div className="flex min-w-0 flex-wrap items-center gap-1.5">
                          {visibleColumns.status && (
                            <button
                              type="button"
                              onClick={() => { setFilterStatus(String(log.status_code) as UsageStatusFilter); setPage(1) }}
                              className="rounded focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                              title={t('usage.filterByStatus', { status: log.status_code })}
                            >
                              <StatusCodeBadge log={log} />
                            </button>
                          )}
                          {log.upstream_error_kind === 'cyber_policy' ? <CyberPolicyDetailButton log={log} /> : null}
                          {visibleColumns.type && log.via_websocket ? (
                            <Badge
                              variant="outline"
                              className="border-transparent bg-cyan-500/12 text-[11px] font-semibold uppercase text-cyan-600 dark:bg-cyan-500/20 dark:text-cyan-400"
                            >
                              ws
                            </Badge>
                          ) : null}
                          {visibleColumns.model && (
                            <Badge variant="outline" className={usageTableBadgeClass}>
                              {(log.channel === 'codex' || log.channel === 'grok' || log.channel === 'antigravity' || log.channel === 'claude') && (
                                <ChannelLogo
                                  channel={log.channel}
                                  size={13}
                                  className="mr-1"
                                  title={log.channel === 'grok' ? 'Grok' : log.channel === 'antigravity' ? 'Antigravity' : log.channel === 'claude' ? 'Claude' : 'Codex'}
                                />
                              )}
                              {log.model || '-'}
                            </Badge>
                          )}
                          {log.reasoning_effort ? (
                            <ReasoningEffortBadge effort={log.reasoning_effort} />
                          ) : null}
                          {visibleColumns.type && isFastTier(log.billing_service_tier || log.service_tier) ? (
                            <Badge
                              variant="outline"
                              className="gap-0.5 border-transparent bg-blue-500/12 text-[11px] font-semibold text-blue-600 dark:bg-blue-500/20 dark:text-blue-400"
                            >
                              <Zap className="size-3" />
                              {formatServiceTierLabel(t, log.billing_service_tier || log.service_tier)}
                            </Badge>
                          ) : null}
                          {visibleColumns.type && <StreamBadge stream={log.stream} />}
                          <CompactionBadges
                            compact={log.compact}
                            hasCompactionHistory={log.has_compaction_history}
                          />
                          <InternalRequestBadge log={log} />
                        </div>
                        {visibleColumns.time && (
                          <div className="shrink-0 whitespace-nowrap text-right text-[11px] tabular-nums text-muted-foreground">
                            {formatBeijingTime(log.created_at)}
                          </div>
                        )}
                      </div>

                      {visibleColumns.error && <UsageErrorSummaryCell log={log} mobile />}

                      {hasDetails && (
                        <div className="mt-2.5 space-y-1 text-xs text-muted-foreground">
                          {visibleColumns.account && (
                            <div className="truncate" title={formatUsageAccountTitle(log)}>
                              <span className="font-semibold text-foreground/80">{t('usage.tableAccount')}: </span>
                              {formatUsageAccountLabel(log)}
                            </div>
                          )}
                          {visibleColumns.apiKey && (
                            <div className="truncate font-mono" title={formatUsageAPIKeyLabel(log.api_key_name, log.api_key_masked) || t('usage.unknownApiKey')}>
                              <span className="font-sans font-semibold text-foreground/80">{t('usage.tableApiKey')}: </span>
                              {formatUsageAPIKeyLabel(log.api_key_name, log.api_key_masked) || t('usage.unknownApiKey')}
                            </div>
                          )}
                          {visibleColumns.clientIp && (
                            <div className="truncate font-mono" title={log.client_ip || '-'}>
                              <span className="font-sans font-semibold text-foreground/80">{t('usage.tableClientIP')}: </span>
                              {log.client_ip || '-'}
                            </div>
                          )}
                          {visibleColumns.endpoint && (
                            <div className="truncate font-mono">
                              <span className="font-sans font-semibold text-foreground/80">{t('usage.tableEndpoint')}: </span>
                              {log.inbound_endpoint || log.endpoint || '-'}
                            </div>
                          )}
                          {visibleColumns.userAgent && (
                            <div className="border-t border-border/60 pt-2">
                              <UserAgentCell log={log} mobile />
                            </div>
                          )}
                        </div>
                      )}

                      {hasMetrics && (
                        <div className="mt-3 grid grid-cols-2 gap-2 text-xs sm:grid-cols-4">
                          {visibleColumns.token && (
                            <div className="rounded-lg border border-border/70 bg-card/60 px-2.5 py-2">
                              <div className="text-[11px] font-semibold text-muted-foreground">{t('usage.tableToken')}</div>
                              <div className="mt-1 font-mono tabular-nums">
                                {log.status_code < 400 && (log.input_tokens > 0 || log.output_tokens > 0) ? (
                                  <>
                                    <UsageInputTokenCount log={log} />
                                    <span className="mx-0.5 text-border">/</span>
                                    <span className="text-emerald-500">↑{formatTokens(log.output_tokens, true)}</span>
                                  </>
                                ) : (
                                  <span className="text-muted-foreground">-</span>
                                )}
                              </div>
                            </div>
                          )}
                          {visibleColumns.cached && (
                            <div className="rounded-lg border border-border/70 bg-card/60 px-2.5 py-2">
                              <div className="text-[11px] font-semibold text-muted-foreground">{t('usage.tableCached')}</div>
                              <div className="mt-1.5">
                                <UsageCacheBadges log={log} align="start" />
                              </div>
                            </div>
                          )}
                          {visibleColumns.timing && (
                            <div className="rounded-lg border border-border/70 bg-card/60 px-2.5 py-2">
                              <div
                                className="text-[11px] font-semibold text-muted-foreground"
                                title={t('usage.tableTimingHint')}
                              >
                                {t('usage.tableTiming')}
                              </div>
                              <div className="mt-1.5">
                                <TimingCell log={log} />
                              </div>
                            </div>
                          )}
                          {visibleColumns.tokensPerSec && (
                            <div className="rounded-lg border border-border/70 bg-card/60 px-2.5 py-2">
                              <div
                                className="text-[11px] font-semibold text-muted-foreground"
                                title={t('usage.tableTokensPerSecHint')}
                              >
                                {t('usage.tableTokensPerSec')}
                              </div>
                              <div className="mt-1">
                                <TokensPerSecCell log={log} />
                              </div>
                            </div>
                          )}
                          {visibleColumns.cost && (
                            <div className="rounded-lg border border-border/70 bg-card/60 px-2.5 py-2">
                              <div className="text-[11px] font-semibold text-muted-foreground">{t('usage.tableCost')}</div>
                              <div className="mt-1">
                                <UsageCostCell log={log} />
                              </div>
                            </div>
                          )}
                        </div>
                      )}
                    </div>
                  )
                })}
              </div>
              </TooltipProvider>

              {/* Desktop table — denser py so multi-badge columns don't leave empty vertical air */}
              <div className="data-table-shell usage-logs-table hidden lg:block">
                <TooltipProvider>
                <Table>
                  <TableHeader>
                    <TableRow>
                      {visibleColumns.status && <TableHead className={usageTableHeadClass}>{t('usage.tableStatus')}</TableHead>}
                      {visibleColumns.model && <TableHead className={usageTableHeadClass}>{t('usage.tableModel')}</TableHead>}
                      {visibleColumns.account && <TableHead className={usageTableHeadClass}>{t('usage.tableAccount')}</TableHead>}
                      {visibleColumns.apiKey && <TableHead className={usageTableHeadClass}>{t('usage.tableApiKey')}</TableHead>}
                      {visibleColumns.clientIp && <TableHead className={usageTableHeadClass}>{t('usage.tableClientIP')}</TableHead>}
                      {visibleColumns.userAgent && <TableHead className={usageTableHeadClass}>{t('usage.tableUserAgent')}</TableHead>}
                      {visibleColumns.endpoint && <TableHead className={usageTableHeadClass}>{t('usage.tableEndpoint')}</TableHead>}
                      {visibleColumns.type && <TableHead className={usageTableHeadClass}>{t('usage.tableType')}</TableHead>}
                      {visibleColumns.token && <TableHead className={`${usageTableHeadClass} text-right`}>{t('usage.tableToken')}</TableHead>}
                      {visibleColumns.cached && <TableHead className={`${usageTableHeadClass} text-right`}>{t('usage.tableCached')}</TableHead>}
                      {visibleColumns.wsAcquire && <TableHead className={`${usageTableHeadClass} text-right`}><span title={t('usage.wsAcquireTooltip')} className="cursor-help underline decoration-dotted underline-offset-2">{t('usage.tableWsAcquire')}</span></TableHead>}
                      {visibleColumns.timing && (
                        <TableHead className={`${usageTableHeadClass} text-right`}>
                          <span
                            title={t('usage.tableTimingHint')}
                            className="cursor-help underline decoration-dotted underline-offset-2"
                          >
                            {t('usage.tableTiming')}
                          </span>
                        </TableHead>
                      )}
                      {visibleColumns.tokensPerSec && (
                        <TableHead className={`${usageTableHeadClass} text-right`}>
                          <span
                            title={t('usage.tableTokensPerSecHint')}
                            className="cursor-help underline decoration-dotted underline-offset-2"
                          >
                            {t('usage.tableTokensPerSec')}
                          </span>
                        </TableHead>
                      )}
                      {visibleColumns.cost && <TableHead className={`${usageTableHeadClass} text-right`}>{t('usage.tableCost')}</TableHead>}
                      {visibleColumns.error && <TableHead className={usageTableHeadClass}>{t('usage.tableError')}</TableHead>}
                      {visibleColumns.time && <TableHead className={`${usageTableHeadClass} text-right`}>{t('usage.tableTime')}</TableHead>}
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {logs.map((log: UsageLog) => {
                      return (
                      <TableRow key={log.id}>
                        {visibleColumns.status && <TableCell>
                          <div className="flex items-center gap-1.5">
                            <button
                              type="button"
                              onClick={() => { setFilterStatus(String(log.status_code) as UsageStatusFilter); setPage(1) }}
                              className="rounded focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
                              title={t('usage.filterByStatus', { status: log.status_code })}
                            >
                              <StatusCodeBadge log={log} />
                            </button>
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
                              {(log.channel === 'codex' || log.channel === 'grok' || log.channel === 'antigravity' || log.channel === 'claude') && (
                                <ChannelLogo
                                  channel={log.channel}
                                  size={13}
                                  className="mr-1"
                                  title={log.channel === 'grok' ? 'Grok' : log.channel === 'antigravity' ? 'Antigravity' : log.channel === 'claude' ? 'Claude' : 'Codex'}
                                />
                              )}
                              {log.model || '-'}
                            </Badge>
                            {log.effective_model && log.effective_model !== log.model && (
                              <Badge variant="outline" className="text-[11px] font-medium border-transparent bg-blue-500/10 text-blue-600 dark:bg-blue-500/20 dark:text-blue-400">
                                → {log.effective_model}
                              </Badge>
                            )}
                            {log.reasoning_effort ? (
                              <ReasoningEffortBadge effort={log.reasoning_effort} />
                            ) : null}
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
                                {formatServiceTierLabel(t, log.billing_service_tier || log.service_tier)}
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
                        {visibleColumns.userAgent && <TableCell>
                          <UserAgentCell log={log} />
                        </TableCell>}
                        {visibleColumns.endpoint && <TableCell>
                          <div
                            className={`${usageTableMonoClass} leading-relaxed`}
                            // 中转/Grok 账号的完整上游 URL 收进 tooltip，列内只显示入站端点
                            title={
                              log.upstream_endpoint && log.upstream_endpoint !== log.inbound_endpoint
                                ? `→ ${log.upstream_endpoint}`
                                : undefined
                            }
                          >
                            <UsageEndpointText value={log.inbound_endpoint || log.endpoint || '-'} />
                          </div>
                        </TableCell>}
                        {visibleColumns.type && <TableCell>
                          <div className="flex flex-wrap items-center gap-1.5">
                            <StreamBadge stream={log.stream} />
                            <CompactionBadges
                              compact={log.compact}
                              hasCompactionHistory={log.has_compaction_history}
                            />
                            <InternalRequestBadge log={log} />
                          </div>
                        </TableCell>}
                        {visibleColumns.token && <TableCell className="text-right">
                          {log.status_code < 400 && (log.input_tokens > 0 || log.output_tokens > 0) ? (
                            <div className={`${usageTableMonoClass} whitespace-nowrap leading-relaxed`}>
                              <UsageInputTokenCount log={log} />
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
                            <span className={`${usageTableMonoClass} text-muted-foreground/50`}>-</span>
                          )}
                        </TableCell>}
                        {visibleColumns.cached && <TableCell className="text-right">
                          <UsageCacheBadges log={log} />
                        </TableCell>}
                        {visibleColumns.wsAcquire && <TableCell className="text-right">
                          {(log.ws_acquire_ms ?? 0) > 0 ? (
                            <span
                              className={`${usageTableMonoClass} ${(log.ws_acquire_ms as number) > 5000 ? 'text-red-500' : (log.ws_acquire_ms as number) > 1000 ? 'text-amber-500' : 'text-muted-foreground'}`}
                              title={t('usage.wsAcquireTooltip')}
                            >
                              {(log.ws_acquire_ms as number) > 1000 ? `${((log.ws_acquire_ms as number) / 1000).toFixed(1)}s` : `${log.ws_acquire_ms}ms`}
                            </span>
                          ) : <span className={`${usageTableMonoClass} text-muted-foreground/50`}>-</span>}
                        </TableCell>}
                        {visibleColumns.timing && (
                          <TableCell className="text-right">
                            <TimingCell log={log} />
                          </TableCell>
                        )}
                        {visibleColumns.tokensPerSec && (
                          <TableCell className="text-right">
                            <TokensPerSecCell log={log} />
                          </TableCell>
                        )}
                        {visibleColumns.cost && <TableCell className="text-right">
                          <UsageCostCell log={log} />
                        </TableCell>}
                        {visibleColumns.error && <TableCell>
                          <UsageErrorSummaryCell log={log} />
                        </TableCell>}
                        {visibleColumns.time && <TableCell className={`${usageTableMonoClass} text-right whitespace-nowrap`}>
                          <UsageTimeCell value={log.created_at} />
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
