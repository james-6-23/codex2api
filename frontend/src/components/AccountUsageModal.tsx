import type { ReactNode } from 'react'
import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { useNavigate } from 'react-router-dom'
import { useTranslation } from 'react-i18next'
import { PieChart, Pie, Cell, ResponsiveContainer, Tooltip } from 'recharts'
import {
  Activity,
  BarChart3,
  Clock3,
  Gauge,
  KeyRound,
  Package,
  RotateCcw,
  Search,
  Zap,
} from 'lucide-react'
import Modal from './Modal'
import { api } from '../api'
import type { AccountKeyStat, AccountModelStat, AccountRow, AccountUsageDayStat, AccountUsageDetail, ResetCreditItem } from '../types'
import { getErrorMessage } from '../utils/error'
import { formatBeijingTime } from '../utils/time'

const COLORS = [
  '#0f766e',
  '#2563eb',
  '#d97706',
  '#7c3aed',
  '#dc2626',
  '#059669',
  '#0891b2',
  '#ea580c',
  '#4f46e5',
  '#db2777',
]

type UsagePage = 'overview' | 'detail' | 'quality'
type UsageRangeKey = '7' | '30' | '90' | 'all'
type ModelMetricKey = 'requests' | 'tokens' | 'cost'
type QualityTone = 'neutral' | 'success' | 'warning' | 'danger'

const USAGE_RANGE_OPTIONS: Array<{ key: UsageRangeKey; days: number; labelKey: string }> = [
  { key: '7', days: 7, labelKey: 'accounts.usageRange7d' },
  { key: '30', days: 30, labelKey: 'accounts.usageRange30d' },
  { key: '90', days: 90, labelKey: 'accounts.usageRange90d' },
  { key: 'all', days: 0, labelKey: 'accounts.usageRangeAll' },
]

const MODEL_METRIC_OPTIONS: Array<{ key: ModelMetricKey; labelKey: string }> = [
  { key: 'requests', labelKey: 'accounts.usageModelMetricRequests' },
  { key: 'tokens', labelKey: 'accounts.usageModelMetricTokens' },
  { key: 'cost', labelKey: 'accounts.usageModelMetricCost' },
]

interface Props {
  account: AccountRow
  onClose: () => void
  onCreditsReset?: () => void
  // Codex 专属的额度券/credit 设置区块;Grok 等非 Codex 账号传 false 隐藏。
  showCreditSettings?: boolean
}

export default function AccountUsageModal({ account, onClose, onCreditsReset, showCreditSettings = true }: Props) {
  const { t } = useTranslation()
  const navigate = useNavigate()
  const [data, setData] = useState<AccountUsageDetail | null>(null)
  const [dataRange, setDataRange] = useState<UsageRangeKey | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [page, setPage] = useState<UsagePage>('overview')
  const [range, setRange] = useState<UsageRangeKey>('30')
  const requestSeq = useRef(0)

  const [creditEnabled, setCreditEnabled] = useState(account.credit_enabled ?? false)
  const [creditSkipWindow, setCreditSkipWindow] = useState(account.credit_skip_usage_window ?? false)
  const [savingCredit, setSavingCredit] = useState(false)
  const [creditError, setCreditError] = useState<string | null>(null)

  const load = useCallback(async () => {
    const seq = requestSeq.current + 1
    requestSeq.current = seq
    setLoading(true)
    setError(null)
    try {
      const result = await api.getAccountUsage(account.id, usageRangeToDays(range))
      if (requestSeq.current !== seq) return
      setData(result)
      setDataRange(range)
    } catch (err) {
      if (requestSeq.current !== seq) return
      setError(getErrorMessage(err))
    } finally {
      if (requestSeq.current === seq) {
        setLoading(false)
      }
    }
  }, [account.id, range])

  useEffect(() => { void load() }, [load])

  const accountLabel = account.openai_responses_api
    ? (account.name?.trim() || `#${account.id}`)
    : (account.email || account.name || `#${account.id}`)
  const title = t('accounts.usageDetailTitle') + ' — ' + accountLabel

  const handleViewLogs = () => {
    const params = new URLSearchParams({ account_id: String(account.id) })
    if (range === '7' || range === '30') {
      params.set('range', `${range}d`)
    } else if (range === '90' || range === 'all') {
      params.set('range', 'custom')
      params.set('days', '90')
    }
    onClose()
    navigate(`/usage?${params.toString()}`)
  }

  const handleCreditToggle = async (field: 'credit_enabled' | 'credit_skip_usage_window', value: boolean) => {
    setCreditError(null)
    const newEnabled = field === 'credit_enabled' ? value : creditEnabled
    const newSkip = field === 'credit_skip_usage_window' ? value : creditSkipWindow
    setSavingCredit(true)
    try {
      await api.updateAccountCredit(account.id, {
        credit_enabled: newEnabled,
        credit_skip_usage_window: newSkip,
      })
      if (field === 'credit_enabled') setCreditEnabled(value)
      if (field === 'credit_skip_usage_window') setCreditSkipWindow(value)
    } catch (err) {
      setCreditError(getErrorMessage(err))
    } finally {
      setSavingCredit(false)
    }
  }

  return (
    <Modal
      show
      title={title}
      onClose={onClose}
      contentClassName="sm:max-w-[960px]"
      bodyClassName="px-5 py-5 sm:px-6"
    >
      {loading && !data ? (
        <div className="flex items-center justify-center py-12 text-sm text-muted-foreground">
          {t('common.loading')}
        </div>
      ) : error && !data ? (
        <div className="py-8 text-center text-sm text-red-500">{error}</div>
      ) : !data ? (
        <div className="py-12 text-center text-sm text-muted-foreground">
          {t('accounts.noUsageData')}
        </div>
      ) : (
        <UsageStatsContent
          account={account}
          accountLabel={accountLabel}
          data={data}
          page={page}
          range={range}
          dataRange={dataRange || range}
          refreshing={loading}
          refreshError={error}
          onPageChange={setPage}
          onRangeChange={setRange}
          onViewLogs={handleViewLogs}
        />
      )}

      {showCreditSettings && (
        <>
          <CreditSettings
            creditEnabled={creditEnabled}
            creditSkipWindow={creditSkipWindow}
            savingCredit={savingCredit}
            creditError={creditError}
            onToggle={handleCreditToggle}
          />

          <ResetCreditsSection account={account} onResetDone={onCreditsReset} />
        </>
      )}
    </Modal>
  )
}

function UsageStatsContent({
  account,
  accountLabel,
  data,
  page,
  range,
  dataRange,
  refreshing,
  refreshError,
  onPageChange,
  onRangeChange,
  onViewLogs,
}: {
  account: AccountRow
  accountLabel: string
  data: AccountUsageDetail
  page: UsagePage
  range: UsageRangeKey
  dataRange: UsageRangeKey
  refreshing: boolean
  refreshError: string | null
  onPageChange: (page: UsagePage) => void
  onRangeChange: (range: UsageRangeKey) => void
  onViewLogs: () => void
}) {
  const { t } = useTranslation()
  const activeDays = Math.max(0, data.active_days || 0)
  const periodDays = data.period_days ?? usageRangeToDays(dataRange)
  const displayDays = Math.max(1, periodDays || usageRangeToDays(dataRange))
  const rangeDescription = dataRange === 'all'
    ? t('accounts.usageAllTimeStats')
    : t('accounts.usageLastDays', { days: displayDays })
  const totalCostLabel = dataRange === 'all'
    ? t('accounts.usageTotalCostAll')
    : t('accounts.usageTotalCostRange', { days: displayDays })
  const today = data.today || emptyDayStat()
  const highestCostDay = data.highest_cost_day || emptyDayStat()
  const highestRequestDay = data.highest_request_day || emptyDayStat()
  const topModel = data.models[0]

  return (
    <div className="space-y-5">
      <div className="rounded-2xl border bg-card shadow-sm">
        <div className="flex flex-col gap-4 border-b px-4 py-4 sm:flex-row sm:items-center sm:justify-between">
          <div className="flex min-w-0 items-center gap-3">
            <div className="flex size-11 shrink-0 items-center justify-center rounded-xl bg-foreground text-background">
              <BarChart3 className="size-5" />
            </div>
            <div className="min-w-0">
              <div className="truncate text-base font-semibold text-foreground">
                {accountLabel}
              </div>
              <div className="text-sm text-muted-foreground">
                {rangeDescription}
                {refreshing && (
                  <span className="ml-2 text-xs">{t('common.loading')}</span>
                )}
              </div>
              {refreshError && (
                <div className="mt-1 text-xs text-red-500">{refreshError}</div>
              )}
            </div>
          </div>

          <div className="flex flex-wrap items-center justify-end gap-2">
            <span className="inline-flex h-8 items-center rounded-full border bg-muted/40 px-3 text-xs font-semibold text-muted-foreground">
              {account.status || t('accounts.unknown')}
            </span>
            <button
              type="button"
              onClick={onViewLogs}
              className="inline-flex h-8 items-center gap-1.5 rounded-lg border bg-background px-3 text-xs font-semibold text-muted-foreground transition-colors hover:text-foreground"
            >
              <Search className="size-3.5" />
              {t('accounts.usageViewLogs')}
            </button>
            <div className="grid h-9 grid-cols-3 rounded-lg border bg-muted/40 p-1">
              <PageButton
                active={page === 'overview'}
                icon={<Gauge className="size-3.5" />}
                label={t('accounts.usageOverviewTab')}
                onClick={() => onPageChange('overview')}
              />
              <PageButton
                active={page === 'detail'}
                icon={<Package className="size-3.5" />}
                label={t('accounts.usageDetailTab')}
                onClick={() => onPageChange('detail')}
              />
              <PageButton
                active={page === 'quality'}
                icon={<Activity className="size-3.5" />}
                label={t('accounts.usageQualityTab')}
                onClick={() => onPageChange('quality')}
              />
            </div>
          </div>
        </div>

        <div className="flex flex-wrap items-center gap-2 border-b px-4 py-3">
          <span className="text-xs font-semibold uppercase text-muted-foreground">
            {t('accounts.usageRange')}
          </span>
          <div className="flex rounded-lg border bg-muted/40 p-1">
            {USAGE_RANGE_OPTIONS.map((option) => (
              <RangeButton
                key={option.key}
                active={range === option.key}
                label={t(option.labelKey)}
                onClick={() => onRangeChange(option.key)}
              />
            ))}
          </div>
        </div>

        {page === 'overview' ? (
          <OverviewPage
            data={data}
            today={today}
            highestCostDay={highestCostDay}
            highestRequestDay={highestRequestDay}
            activeDays={activeDays}
            periodDays={periodDays}
            totalCostLabel={totalCostLabel}
            topModel={topModel}
          />
        ) : page === 'detail' ? (
          <DetailPage
            data={data}
            activeDays={activeDays}
            periodDays={periodDays}
          />
        ) : (
          <QualityPage data={data} />
        )}
      </div>
    </div>
  )
}

function OverviewPage({
  data,
  today,
  highestCostDay,
  highestRequestDay,
  activeDays,
  periodDays,
  totalCostLabel,
  topModel,
}: {
  data: AccountUsageDetail
  today: AccountUsageDayStat
  highestCostDay: AccountUsageDayStat
  highestRequestDay: AccountUsageDayStat
  activeDays: number
  periodDays: number
  totalCostLabel: string
  topModel?: { model: string; requests: number; tokens: number }
}) {
  const { t } = useTranslation()
  const activeDaysText = formatActiveDaysText(activeDays, periodDays, t('accounts.usageDaysUnit'))
  return (
    <div className="p-4 sm:p-5">
      <div className="grid gap-4 lg:grid-cols-[minmax(0,1.2fr)_minmax(300px,0.8fr)]">
        <section className="rounded-2xl border bg-gradient-to-br from-background via-background to-muted/50 p-5">
          <div className="flex flex-col gap-4 sm:flex-row sm:items-start sm:justify-between">
            <div>
              <div className="text-sm font-medium text-muted-foreground">
                {totalCostLabel}
              </div>
              <div className="mt-2 text-5xl font-semibold tracking-normal text-foreground">
                ${formatCost(data.total_account_billed)}
              </div>
            </div>
            <div className="grid grid-cols-2 gap-2 text-sm">
              <MiniPill label={t('accounts.accountBilledLabel')} value={`$${formatCost(data.total_account_billed)}`} />
              <MiniPill label={t('accounts.userBilledLabel')} value={`$${formatCost(data.total_user_billed)}`} />
            </div>
          </div>

          <div className="mt-6 grid gap-3 sm:grid-cols-3">
            <CompactMetric icon={<Zap className="size-4" />} label={t('accounts.totalRequests')} value={formatCompactNumber(data.total_requests)} />
            <CompactMetric icon={<Package className="size-4" />} label={t('accounts.totalTokens')} value={formatTokens(data.total_tokens)} />
            <CompactMetric icon={<Clock3 className="size-4" />} label={t('accounts.usageAvgResponse')} value={formatDuration(data.avg_duration_ms)} />
          </div>

          <UsageTrend history={data.history || []} />
        </section>

        <section className="grid gap-3 sm:grid-cols-2 lg:grid-cols-1">
          <SignalCard
            icon={<Activity className="size-4" />}
            title={t('accounts.usageTodayOverview')}
            rows={[
              [t('accounts.usageRequests'), formatNumber(today.requests)],
              [t('accounts.usageTokens'), formatTokens(today.tokens)],
              [t('accounts.usageTodayCost'), `$${formatCost(today.account_billed)}`],
            ]}
          />
          <SignalCard
            icon={<Gauge className="size-4" />}
            title={t('accounts.usageDailyBaseline')}
            rows={[
              [t('accounts.usageAvgDailyCost'), `$${formatCost(data.avg_daily_account_billed)}`],
              [t('accounts.usageAvgDailyRequests'), formatCompactNumber(Math.round(data.avg_daily_requests))],
              [t('accounts.usageActiveDays'), activeDaysText],
            ]}
          />
        </section>
      </div>
      <div className="mt-4 grid gap-3 md:grid-cols-3">
        <HighlightStrip
          label={t('accounts.usageHighestCostDay')}
          value={highestCostDay.label || '-'}
          detail={`$${formatCost(highestCostDay.account_billed)} · ${formatNumber(highestCostDay.requests)} ${t('accounts.usageReqUnit')}`}
        />
        <HighlightStrip
          label={t('accounts.usageHighestRequestDay')}
          value={highestRequestDay.label || '-'}
          detail={`${formatCompactNumber(highestRequestDay.requests)} ${t('accounts.usageReqUnit')} · $${formatCost(highestRequestDay.account_billed)}`}
        />
        <HighlightStrip
          label={t('accounts.usageTopModel')}
          value={topModel?.model || '-'}
          detail={topModel ? `${formatNumber(topModel.requests)} ${t('accounts.usageReqUnit')} · ${formatTokens(topModel.tokens)} ${t('accounts.usageTokUnit')}` : '-'}
        />
      </div>
    </div>
  )
}

function QualityPage({ data }: { data: AccountUsageDetail }) {
  return (
    <div className="p-4 sm:p-5">
      <QualitySignals data={data} />
    </div>
  )
}

function QualitySignals({ data }: { data: AccountUsageDetail }) {
  const { t } = useTranslation()
  return (
    <section className="rounded-2xl border bg-background p-4">
      <div className="mb-3 flex flex-col gap-1 sm:flex-row sm:items-end sm:justify-between">
        <div>
          <h4 className="text-base font-semibold text-foreground">{t('accounts.usageQualitySignals')}</h4>
          <p className="text-sm text-muted-foreground">{t('accounts.usageQualitySignalsDesc')}</p>
        </div>
      </div>
      <div className="grid gap-3 sm:grid-cols-2 lg:grid-cols-3">
        <QualityMetric
          icon={<Activity className="size-4" />}
          label={t('accounts.usageErrorRate')}
          value={formatPercent(data.error_rate)}
          detail={t('accounts.usageOfRequests', { count: formatNumber(data.error_requests), total: formatNumber(data.total_requests) })}
          tone={data.error_rate >= 10 ? 'danger' : data.error_rate >= 3 ? 'warning' : 'success'}
        />
        <QualityMetric
          icon={<Gauge className="size-4" />}
          label={t('accounts.usageRetryRequests')}
          value={formatNumber(data.retry_requests)}
          detail={t('accounts.usageOfRequests', { count: formatNumber(data.retry_requests), total: formatNumber(data.total_requests) })}
          tone={data.retry_requests > 0 ? 'warning' : 'success'}
        />
        <QualityMetric
          icon={<Clock3 className="size-4" />}
          label={t('accounts.usageAvgFirstToken')}
          value={formatDurationOrDash(data.avg_first_token_ms)}
          detail={t('accounts.usageSamplesCount', { count: formatNumber(data.first_token_samples) })}
          tone={data.avg_first_token_ms >= 5000 ? 'danger' : data.avg_first_token_ms >= 2000 ? 'warning' : 'neutral'}
        />
        <QualityMetric
          icon={<Gauge className="size-4" />}
          label={t('accounts.usageP95Response')}
          value={formatDurationOrDash(data.p95_duration_ms)}
          detail={t('accounts.usageSamplesCount', { count: formatNumber(data.total_requests) })}
          tone={data.p95_duration_ms >= 30000 ? 'danger' : data.p95_duration_ms >= 10000 ? 'warning' : 'neutral'}
        />
        <QualityMetric
          icon={<Zap className="size-4" />}
          label={t('accounts.usageStreamShare')}
          value={formatPercent(data.stream_rate)}
          detail={t('accounts.usageOfRequests', { count: formatNumber(data.stream_requests), total: formatNumber(data.total_requests) })}
        />
        <QualityMetric
          icon={<Package className="size-4" />}
          label={t('accounts.usageCompactShare')}
          value={formatPercent(data.compact_rate)}
          detail={t('accounts.usageOfRequests', { count: formatNumber(data.compact_requests), total: formatNumber(data.total_requests) })}
        />
      </div>
    </section>
  )
}

function DetailPage({
  data,
  activeDays,
  periodDays,
}: {
  data: AccountUsageDetail
  activeDays: number
  periodDays: number
}) {
  const { t } = useTranslation()
  const [modelMetric, setModelMetric] = useState<ModelMetricKey>('requests')
  const activeDaysText = formatActiveDaysText(activeDays, periodDays, t('accounts.usageDaysUnit'))
  const sortedModels = useMemo(() => {
    return [...data.models].sort((a, b) => {
      const valueDiff = modelMetricValue(b, modelMetric) - modelMetricValue(a, modelMetric)
      if (valueDiff !== 0) return valueDiff
      return b.requests - a.requests
    })
  }, [data.models, modelMetric])
  const modelMetricTotal = useMemo(
    () => sortedModels.reduce((sum, item) => sum + modelMetricValue(item, modelMetric), 0),
    [sortedModels, modelMetric],
  )
  const topModel = sortedModels[0]
  const chartModels = useMemo(
    () => sortedModels.map((model) => ({
      ...model,
      metric_value: modelMetricValue(model, modelMetric),
    })),
    [sortedModels, modelMetric],
  )

  return (
    <div className="p-4 sm:p-5">
      <div className="grid gap-4 lg:grid-cols-[minmax(0,1fr)_340px]">
        <section className="rounded-2xl border bg-background p-4">
          <div className="mb-3 flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
            <div>
              <h4 className="text-base font-semibold">{t('accounts.modelDistribution')}</h4>
              <p className="text-sm text-muted-foreground">
                {topModel ? t('accounts.usageTopModelByMetric', { model: topModel.model }) : '-'}
              </p>
            </div>
            <div className="flex flex-wrap items-center justify-end gap-2">
              <span className="rounded-full bg-muted px-3 py-1 text-xs font-semibold text-muted-foreground">
                {formatModelMetricValue(modelMetricTotal, modelMetric)} {t(modelMetricLabelKey(modelMetric))}
              </span>
              <div className="flex rounded-lg border bg-muted/40 p-1">
                {MODEL_METRIC_OPTIONS.map((option) => (
                  <ModelMetricButton
                    key={option.key}
                    active={modelMetric === option.key}
                    label={t(option.labelKey)}
                    onClick={() => setModelMetric(option.key)}
                  />
                ))}
              </div>
            </div>
          </div>
          <div className="grid gap-4 md:grid-cols-[230px_minmax(0,1fr)]">
            <div className="h-[230px]">
              <ResponsiveContainer width="100%" height="100%">
                <PieChart>
                  <Pie
                    data={chartModels}
                    dataKey="metric_value"
                    nameKey="model"
                    cx="50%"
                    cy="50%"
                    innerRadius={62}
                    outerRadius={92}
                    // 与 Usage / Key Usage 门户一致：无扇区间隙，避免环形图出现白缝
                    paddingAngle={0}
                    strokeWidth={0}
                  >
                    {chartModels.map((_, i) => (
                      <Cell key={i} fill={COLORS[i % COLORS.length]} />
                    ))}
                  </Pie>
                  <Tooltip
                    formatter={(value, name) => [formatModelMetricValue(Number(value || 0), modelMetric), String(name ?? '')]}
                    contentStyle={{ fontSize: 12, borderRadius: 8, border: '1px solid hsl(var(--border))' }}
                  />
                </PieChart>
              </ResponsiveContainer>
            </div>
            <div className="min-w-0 space-y-2 self-center">
              {sortedModels.map((m, i) => (
                <ModelRow
                  key={m.model}
                  color={COLORS[i % COLORS.length]}
                  model={m}
                  metric={modelMetric}
                  total={modelMetricTotal}
                />
              ))}
            </div>
          </div>
        </section>

        <section className="rounded-2xl border bg-background p-4">
          <div className="mb-4 flex items-center gap-2">
            <span className="flex size-8 items-center justify-center rounded-lg bg-muted text-muted-foreground">
              <Package className="size-4" />
            </span>
            <h4 className="text-base font-semibold">{t('accounts.usageTokenBreakdown')}</h4>
          </div>
          <div className="space-y-4">
            <TokenBar label={t('accounts.inputTokens')} value={data.input_tokens} total={data.total_tokens} />
            <TokenBar label={t('accounts.outputTokens')} value={data.output_tokens} total={data.total_tokens} />
            <TokenBar label={t('accounts.reasoningTokens')} value={data.reasoning_tokens} total={data.total_tokens} />
            <TokenBar label={t('accounts.cachedTokens')} value={data.cached_tokens} total={data.total_tokens} />
          </div>
        </section>
      </div>

      <KeyDistribution data={data} />

      <div className="mt-4 grid gap-3 md:grid-cols-4">
        <DetailKpi label={t('accounts.usageActiveDays')} value={activeDaysText} />
        <DetailKpi label={t('accounts.usageDailyAvgTokens')} value={formatTokens(Math.round(data.avg_daily_tokens))} />
        <DetailKpi label={t('accounts.usageCacheHitRate')} value={formatPercent(data.cache_hit_rate)} />
        <DetailKpi label={t('accounts.usageAvgResponse')} value={formatDuration(data.avg_duration_ms)} />
      </div>
    </div>
  )
}

function KeyDistribution({ data }: { data: AccountUsageDetail }) {
  const { t } = useTranslation()
  const [metric, setMetric] = useState<ModelMetricKey>('requests')
  const sortedKeys = useMemo(() => {
    return [...(data.by_api_key || [])].sort((a, b) => {
      const diff = keyMetricValue(b, metric) - keyMetricValue(a, metric)
      if (diff !== 0) return diff
      return b.requests - a.requests
    })
  }, [data.by_api_key, metric])
  const metricTotal = useMemo(
    () => sortedKeys.reduce((sum, item) => sum + keyMetricValue(item, metric), 0),
    [sortedKeys, metric],
  )
  const topKey = sortedKeys[0]

  return (
    <section className="mt-4 rounded-2xl border bg-background p-4">
      <div className="mb-3 flex flex-col gap-3 sm:flex-row sm:items-start sm:justify-between">
        <div className="flex items-start gap-2.5">
          <span className="mt-0.5 flex size-8 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-primary">
            <KeyRound className="size-4" />
          </span>
          <div>
            <h4 className="text-base font-semibold">{t('accounts.usageKeyDistribution')}</h4>
            <p className="text-sm text-muted-foreground">
              {topKey
                ? t('accounts.usageTopKeyByMetric', { key: keyDisplayName(topKey, t) })
                : t('accounts.usageKeyDistributionDesc')}
            </p>
          </div>
        </div>
        <div className="flex flex-wrap items-center justify-end gap-2">
          <span className="rounded-full bg-muted px-3 py-1 text-xs font-semibold tabular-nums text-muted-foreground">
            {formatModelMetricValue(metricTotal, metric)} {t(modelMetricLabelKey(metric))}
          </span>
          {sortedKeys.length > 0 && (
            <span className="rounded-full bg-muted/70 px-3 py-1 text-xs font-medium text-muted-foreground">
              {t('accounts.usageKeyCount', { count: sortedKeys.length })}
            </span>
          )}
          <div className="flex rounded-lg border bg-muted/40 p-1">
            {MODEL_METRIC_OPTIONS.map((option) => (
              <ModelMetricButton
                key={option.key}
                active={metric === option.key}
                label={t(option.labelKey)}
                onClick={() => setMetric(option.key)}
              />
            ))}
          </div>
        </div>
      </div>
      {sortedKeys.length === 0 ? (
        <div className="flex h-20 flex-col items-center justify-center gap-1.5 rounded-xl border border-dashed bg-muted/20 text-sm text-muted-foreground">
          <KeyRound className="size-4 opacity-60" />
          {t('accounts.usageKeyEmpty')}
        </div>
      ) : (
        <div className="grid gap-2 sm:grid-cols-2">
          {sortedKeys.map((k, i) => (
            <KeyRow
              key={`${k.api_key_id}-${k.api_key_masked}-${i}`}
              color={COLORS[i % COLORS.length]}
              stat={k}
              metric={metric}
              total={metricTotal}
            />
          ))}
        </div>
      )}
    </section>
  )
}

function KeyRow({
  color,
  stat,
  metric,
  total,
}: {
  color: string
  stat: AccountKeyStat
  metric: ModelMetricKey
  total: number
}) {
  const { t } = useTranslation()
  const value = keyMetricValue(stat, metric)
  const percent = total > 0 ? Math.min(100, Math.max(0, (value / total) * 100)) : 0
  const detail = keyMetricDetail(stat, metric, t)
  return (
    <div className="rounded-xl border border-border/80 bg-muted/15 px-3 py-2.5 transition-colors hover:border-border hover:bg-muted/25">
      <div className="mb-2 grid grid-cols-[auto_minmax(0,1fr)_auto] items-center gap-2 text-sm">
        <span
          className="size-2.5 shrink-0 rounded-full ring-2 ring-background"
          style={{ background: color }}
        />
        <span className="flex min-w-0 items-baseline gap-1.5">
          <span className="truncate font-medium text-foreground">{keyDisplayName(stat, t)}</span>
          {stat.api_key_masked && (
            <span className="shrink-0 rounded-md bg-muted px-1.5 py-0.5 font-mono text-[10px] tracking-wide text-muted-foreground">
              {stat.api_key_masked}
            </span>
          )}
        </span>
        <span className="tabular-nums text-xs font-semibold text-muted-foreground">
          {formatPercent(percent)}
        </span>
      </div>
      <div className="h-1.5 overflow-hidden rounded-full bg-muted">
        <div
          className="h-full rounded-full transition-[width] duration-300"
          style={{ width: `${percent}%`, background: color }}
        />
      </div>
      <div className="mt-2 flex items-center justify-between gap-2 text-xs text-muted-foreground">
        <span className="font-semibold tabular-nums text-foreground">
          {formatModelMetricValue(value, metric)}
        </span>
        <span className="truncate text-right">{detail}</span>
      </div>
    </div>
  )
}

function CreditSettings({
  creditEnabled,
  creditSkipWindow,
  savingCredit,
  creditError,
  onToggle,
}: {
  creditEnabled: boolean
  creditSkipWindow: boolean
  savingCredit: boolean
  creditError: string | null
  onToggle: (field: 'credit_enabled' | 'credit_skip_usage_window', value: boolean) => Promise<void>
}) {
  const { t } = useTranslation()
  return (
    <div className="mt-5 rounded-2xl border bg-card p-4">
      <h4 className="mb-3 text-base font-semibold">{t('accounts.creditSettings')}</h4>
      {creditError && (
        <div className="mb-3 text-xs text-red-500">{creditError}</div>
      )}
      <div className="space-y-3">
        <CreditToggle
          label={t('accounts.creditEnabled')}
          hint={t('accounts.creditEnabledHint')}
          checked={creditEnabled}
          disabled={savingCredit}
          onClick={() => void onToggle('credit_enabled', !creditEnabled)}
        />
        {creditEnabled && (
          <CreditToggle
            label={t('accounts.creditSkipWindow')}
            hint={t('accounts.creditSkipWindowHint')}
            checked={creditSkipWindow}
            disabled={savingCredit}
            onClick={() => void onToggle('credit_skip_usage_window', !creditSkipWindow)}
          />
        )}
      </div>
    </div>
  )
}

function ResetCreditsSection({
  account,
  onResetDone,
}: {
  account: AccountRow
  onResetDone?: () => void
}) {
  const { t } = useTranslation()
  const initial =
    typeof account.rate_limit_reset_credits === 'number'
      ? account.rate_limit_reset_credits
      : null
  const [count, setCount] = useState<number | null>(initial)
  const [credits, setCredits] = useState<ResetCreditItem[] | null>(null)
  const [detailError, setDetailError] = useState<string | null>(null)
  const [confirming, setConfirming] = useState(false)
  const [resetting, setResetting] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [done, setDone] = useState(false)

  // 打开弹窗时拉取重置券明细（每张的有效期，issue #322）。
  // 明细端点同时返回权威可用张数，一并校准本地计数。
  // 仅对已知有重置次数语义的账号发起（count===null 表示非 Codex 账号或未探测）。
  const loadDetail = useCallback(async () => {
    setDetailError(null)
    try {
      const res = await api.getResetCredits(account.id)
      setCredits(res.credits ?? [])
      if (typeof res.available_count === 'number') {
        setCount(res.available_count)
      }
    } catch (err) {
      // 明细拉取失败不影响原有的次数展示与重置按钮,仅提示明细不可用。
      setDetailError(getErrorMessage(err))
    }
  }, [account.id])

  useEffect(() => {
    if (initial === null) return
    void loadDetail()
  }, [initial, loadDetail])

  // 次数未知（非 Codex 账号或尚未探测）时不显示该区块。
  if (count === null) return null

  const handleReset = async () => {
    setResetting(true)
    setError(null)
    try {
      const res = await api.resetCredits(account.id)
      const next =
        typeof res.rate_limit_reset_credits === 'number'
          ? res.rate_limit_reset_credits
          : Math.max(0, count - 1)
      setCount(next)
      setDone(true)
      setConfirming(false)
      onResetDone?.()
      // 重置消耗了一张券,重新拉取明细同步有效期列表。
      void loadDetail()
    } catch (err) {
      setError(getErrorMessage(err))
    } finally {
      setResetting(false)
    }
  }

  const balanceDisplay = account.credits_has_credits
    ? account.credits_unlimited
      ? t('accounts.creditsBalanceUnlimitedShort')
      : (account.credits_balance ?? '').trim() || null
    : null
  const applicable =
    typeof account.applicable_reset_credits === 'number'
      ? account.applicable_reset_credits
      : null

  return (
    <div className="mt-5 rounded-2xl border bg-card p-4">
      <h4 className="mb-3 text-base font-semibold">{t('accounts.resetCreditsTitle')}</h4>
      {balanceDisplay !== null && (
        <div className="mb-3 flex items-center justify-between gap-4 rounded-xl bg-muted/40 px-3 py-2">
          <span className="text-sm font-medium">{t('accounts.creditsBalanceLabel')}</span>
          <span className="flex items-center gap-2">
            <span className="text-base font-semibold tabular-nums text-foreground">{balanceDisplay}</span>
            {account.credits_overage_limit_reached && (
              <span className="rounded-md bg-red-50 px-1.5 py-0.5 text-[10px] font-medium text-red-600 ring-1 ring-inset ring-red-500/20 dark:bg-red-950 dark:text-red-400 dark:ring-red-400/20">
                {t('accounts.creditsOverageReached')}
              </span>
            )}
          </span>
        </div>
      )}
      {error && <div className="mb-3 text-xs text-red-500">{error}</div>}
      {done && !error && (
        <div className="mb-3 text-xs text-emerald-600">{t('accounts.resetCreditsSuccess')}</div>
      )}
      <div className="flex items-center justify-between gap-4">
        <div>
          <p className="text-sm font-medium">{t('accounts.resetCreditsLabel')}</p>
          <p className="text-xs text-muted-foreground">{t('accounts.resetCreditsHint')}</p>
        </div>
        <div className="flex items-center gap-3">
          <div className="flex flex-col items-end">
            <span className="text-2xl font-semibold tabular-nums text-foreground">{count}</span>
            {count > 0 && applicable === 0 && (
              <span className="text-[10px] text-muted-foreground">
                {t('accounts.resetCreditsNotApplicable')}
              </span>
            )}
          </div>
          {count > 0 &&
            (confirming ? (
              <div className="flex items-center gap-2">
                <button
                  type="button"
                  disabled={resetting}
                  onClick={() => void handleReset()}
                  className="inline-flex h-8 items-center gap-1.5 rounded-lg bg-primary px-3 text-xs font-semibold text-primary-foreground transition-opacity hover:opacity-90 disabled:cursor-not-allowed disabled:opacity-60"
                >
                  {resetting ? t('common.loading') : t('accounts.resetCreditsConfirmButton')}
                </button>
                <button
                  type="button"
                  disabled={resetting}
                  onClick={() => setConfirming(false)}
                  className="inline-flex h-8 items-center rounded-lg border bg-background px-3 text-xs font-semibold text-muted-foreground transition-colors hover:text-foreground disabled:opacity-60"
                >
                  {t('common.cancel')}
                </button>
              </div>
            ) : (
              <button
                type="button"
                onClick={() => {
                  setDone(false)
                  setConfirming(true)
                }}
                className="inline-flex h-8 items-center gap-1.5 rounded-lg border bg-background px-3 text-xs font-semibold text-foreground transition-colors hover:bg-muted/60"
              >
                <RotateCcw className="size-3.5" />
                {t('accounts.resetCreditsButton')}
              </button>
            ))}
        </div>
      </div>
      {confirming && (
        <p className="mt-3 text-xs text-amber-600">
          {t('accounts.resetCreditsConfirmMessage')}
        </p>
      )}
      <ResetCreditExpiryList credits={credits} detailError={detailError} />
    </div>
  )
}

// 重置券有效期明细列表：按过期时间升序展示每张券,最快过期的在最前,
// 并对 7 天内到期的券以醒目色提示,方便优先消耗快过期的券(issue #322)。
function ResetCreditExpiryList({
  credits,
  detailError,
}: {
  credits: ResetCreditItem[] | null
  detailError: string | null
}) {
  const { t } = useTranslation()

  const sorted = useMemo(() => {
    if (!credits) return []
    return credits
      .map((credit, index) => {
        const expiresAtMs = new Date(credit.expires_at).getTime()
        return Number.isFinite(expiresAtMs)
          ? { key: credit.id || String(index), expiresAtMs, credit }
          : null
      })
      .filter((item): item is { key: string; expiresAtMs: number; credit: ResetCreditItem } =>
        Boolean(item),
      )
      .sort((a, b) => a.expiresAtMs - b.expiresAtMs || a.key.localeCompare(b.key))
  }, [credits])

  if (detailError) {
    return (
      <p className="mt-3 text-xs text-muted-foreground">
        {t('accounts.resetCreditsDetailUnavailable')}
      </p>
    )
  }
  if (!credits || sorted.length === 0) return null

  const nowMs = Date.now()
  const soonThresholdMs = 7 * 24 * 60 * 60 * 1000

  return (
    <div className="mt-3 border-t pt-3">
      <p className="mb-2 text-xs font-medium text-muted-foreground">
        {t('accounts.resetCreditsExpiryTitle')}
      </p>
      <ul className="space-y-1">
        {sorted.map((item, index) => {
          const expiringSoon = item.expiresAtMs - nowMs <= soonThresholdMs
          return (
            <li
              key={item.key}
              className="flex items-center justify-between gap-3 text-xs"
            >
              <span className="text-muted-foreground">
                {t('accounts.resetCreditsExpiryItem', { index: index + 1 })}
              </span>
              <span
                className={`tabular-nums ${expiringSoon ? 'font-medium text-amber-600' : 'text-foreground'}`}
              >
                {t('accounts.resetCreditsExpiresAt', {
                  time: formatBeijingTime(item.credit.expires_at),
                })}
              </span>
            </li>
          )
        })}
      </ul>
    </div>
  )
}

function PageButton({
  active,
  icon,
  label,
  onClick,
}: {
  active: boolean
  icon: ReactNode
  label: string
  onClick: () => void
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={`inline-flex min-w-20 items-center justify-center gap-1.5 rounded-md px-3 text-sm font-semibold transition-colors ${active ? 'bg-background text-foreground shadow-sm' : 'text-muted-foreground hover:text-foreground'}`}
    >
      {icon}
      {label}
    </button>
  )
}

function RangeButton({
  active,
  label,
  onClick,
}: {
  active: boolean
  label: string
  onClick: () => void
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={`h-7 min-w-12 rounded-md px-2.5 text-xs font-semibold transition-colors ${active ? 'bg-background text-foreground shadow-sm' : 'text-muted-foreground hover:text-foreground'}`}
    >
      {label}
    </button>
  )
}

function ModelMetricButton({
  active,
  label,
  onClick,
}: {
  active: boolean
  label: string
  onClick: () => void
}) {
  return (
    <button
      type="button"
      onClick={onClick}
      className={`h-7 min-w-14 rounded-md px-2.5 text-xs font-semibold transition-colors ${active ? 'bg-background text-foreground shadow-sm' : 'text-muted-foreground hover:text-foreground'}`}
    >
      {label}
    </button>
  )
}

function MiniPill({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded-xl border bg-background/80 px-3 py-2">
      <div className="text-xs text-muted-foreground">{label}</div>
      <div className="mt-0.5 font-semibold tabular-nums text-foreground">{value}</div>
    </div>
  )
}

function CompactMetric({ icon, label, value }: { icon: ReactNode; label: string; value: string }) {
  return (
    <div className="rounded-xl border bg-background px-3 py-3">
      <div className="mb-2 flex items-center gap-2 text-muted-foreground">
        {icon}
        <span className="text-xs font-medium">{label}</span>
      </div>
      <div className="text-xl font-semibold tabular-nums text-foreground">{value}</div>
    </div>
  )
}

function SignalCard({
  icon,
  title,
  rows,
}: {
  icon: ReactNode
  title: string
  rows: Array<[string, string]>
}) {
  return (
    <div className="rounded-2xl border bg-background p-4">
      <div className="mb-3 flex items-center gap-2">
        <span className="flex size-8 items-center justify-center rounded-lg bg-muted text-muted-foreground">
          {icon}
        </span>
        <h4 className="font-semibold text-foreground">{title}</h4>
      </div>
      <div className="space-y-2">
        {rows.map(([label, value]) => (
          <div key={label} className="flex items-center justify-between gap-4">
            <span className="text-sm text-muted-foreground">{label}</span>
            <span className="font-semibold tabular-nums text-foreground">{value}</span>
          </div>
        ))}
      </div>
    </div>
  )
}

function HighlightStrip({ label, value, detail }: { label: string; value: string; detail: string }) {
  return (
    <div className="rounded-xl border bg-background p-4">
      <div className="text-xs font-semibold uppercase text-muted-foreground">{label}</div>
      <div className="mt-2 truncate text-lg font-semibold text-foreground">{value}</div>
      <div className="mt-1 truncate text-sm text-muted-foreground">{detail}</div>
    </div>
  )
}

function QualityMetric({
  icon,
  label,
  value,
  detail,
  tone = 'neutral',
}: {
  icon: ReactNode
  label: string
  value: string
  detail: string
  tone?: QualityTone
}) {
  const toneClass = qualityToneClass(tone)
  return (
    <div className={`rounded-xl border px-3 py-3 ${toneClass.box}`}>
      <div className="mb-3 flex items-center justify-between gap-3">
        <span className={`flex size-8 items-center justify-center rounded-lg ${toneClass.icon}`}>
          {icon}
        </span>
        <span className="text-xs font-medium text-muted-foreground">{label}</span>
      </div>
      <div className={`text-2xl font-semibold tabular-nums ${toneClass.value}`}>{value}</div>
      <div className="mt-1 text-xs text-muted-foreground">{detail}</div>
    </div>
  )
}

function qualityToneClass(tone: QualityTone): { box: string; icon: string; value: string } {
  switch (tone) {
    case 'success':
      return {
        box: 'bg-emerald-500/5 border-emerald-500/20',
        icon: 'bg-emerald-500/10 text-emerald-600',
        value: 'text-emerald-600',
      }
    case 'warning':
      return {
        box: 'bg-amber-500/5 border-amber-500/20',
        icon: 'bg-amber-500/10 text-amber-600',
        value: 'text-amber-600',
      }
    case 'danger':
      return {
        box: 'bg-red-500/5 border-red-500/20',
        icon: 'bg-red-500/10 text-red-600',
        value: 'text-red-600',
      }
    default:
      return {
        box: 'bg-background',
        icon: 'bg-muted text-muted-foreground',
        value: 'text-foreground',
      }
  }
}

function UsageTrend({ history }: { history: AccountUsageDayStat[] }) {
  const { t } = useTranslation()
  const display = history.slice(-60)
  const maxCost = Math.max(...display.map((day) => day.account_billed), 0)
  const maxRequests = Math.max(...display.map((day) => day.requests), 0)

  return (
    <div className="mt-5 border-t pt-4">
      <div className="mb-3 flex items-center justify-between gap-3">
        <div>
          <div className="text-sm font-semibold text-foreground">{t('accounts.usageTrend')}</div>
          <div className="text-xs text-muted-foreground">
            {display.length > 0
              ? t('accounts.usageTrendDays', { count: display.length })
              : t('accounts.usageTrendEmpty')}
          </div>
        </div>
        {display.length > 0 && (
          <div className="text-right text-xs text-muted-foreground">
            <div>{t('accounts.usageHighestCostDay')}: ${formatCost(maxCost)}</div>
            <div>{t('accounts.usageHighestRequestDay')}: {formatCompactNumber(maxRequests)}</div>
          </div>
        )}
      </div>
      {display.length === 0 ? (
        <div className="flex h-20 items-center justify-center rounded-xl border bg-background text-sm text-muted-foreground">
          {t('accounts.usageTrendEmpty')}
        </div>
      ) : (
        <div className="flex h-20 items-end gap-1 rounded-xl border bg-background px-2 py-2">
          {display.map((day) => {
            const height = maxCost > 0 ? Math.max(8, (day.account_billed / maxCost) * 100) : Math.max(8, (day.requests / Math.max(maxRequests, 1)) * 100)
            return (
              <div
                key={day.date}
                title={`${day.label || day.date}: $${formatCost(day.account_billed)} / ${formatNumber(day.requests)} ${t('accounts.usageReqUnit')}`}
                className="min-w-[3px] flex-1 rounded-t bg-foreground/70 hover:bg-foreground"
                style={{ height: `${height}%` }}
              />
            )
          })}
        </div>
      )}
    </div>
  )
}

function ModelRow({
  color,
  model,
  metric,
  total,
}: {
  color: string
  model: AccountModelStat
  metric: ModelMetricKey
  total: number
}) {
  const { t } = useTranslation()
  const value = modelMetricValue(model, metric)
  const percent = total > 0 ? Math.min(100, Math.max(0, (value / total) * 100)) : 0
  const detail = modelMetricDetail(model, metric, t)
  return (
    <div className="rounded-xl border bg-background px-3 py-2.5">
      <div className="mb-2 grid grid-cols-[auto_minmax(0,1fr)_auto] items-center gap-2 text-sm">
        <span className="size-2.5 rounded-full" style={{ background: color }} />
        <span className="truncate font-medium text-foreground">{model.model}</span>
        <span className="tabular-nums text-muted-foreground">{formatPercent(percent)}</span>
      </div>
      <div className="h-1.5 overflow-hidden rounded-full bg-muted">
        <div className="h-full rounded-full" style={{ width: `${percent}%`, background: color }} />
      </div>
      <div className="mt-2 flex items-center justify-between text-xs text-muted-foreground">
        <span className="font-semibold text-foreground">{formatModelMetricValue(value, metric)}</span>
        <span className="truncate text-right">{detail}</span>
      </div>
    </div>
  )
}

function TokenBar({ label, value, total }: { label: string; value: number; total: number }) {
  const percent = total > 0 ? Math.min(100, Math.max(0, (value / total) * 100)) : 0
  return (
    <div>
      <div className="mb-1 flex items-center justify-between gap-3 text-sm">
        <span className="text-muted-foreground">{label}</span>
        <span className="font-semibold tabular-nums text-foreground">{formatTokens(value)}</span>
      </div>
      <div className="h-2 overflow-hidden rounded-full bg-muted">
        <div className="h-full rounded-full bg-foreground" style={{ width: `${percent}%` }} />
      </div>
    </div>
  )
}

function DetailKpi({ label, value }: { label: string; value: string }) {
  return (
    <div className="rounded-xl border bg-background p-4">
      <div className="text-sm text-muted-foreground">{label}</div>
      <div className="mt-1 text-xl font-semibold tabular-nums text-foreground">{value}</div>
    </div>
  )
}

function CreditToggle({
  label,
  hint,
  checked,
  disabled,
  onClick,
}: {
  label: string
  hint: string
  checked: boolean
  disabled: boolean
  onClick: () => void
}) {
  return (
    <div className="flex items-center justify-between gap-4">
      <div>
        <p className="text-sm font-medium">{label}</p>
        <p className="text-xs text-muted-foreground">{hint}</p>
      </div>
      <button
        type="button"
        role="switch"
        aria-label={label}
        aria-checked={checked}
        disabled={disabled}
        onClick={onClick}
        className={`relative inline-flex h-5 w-9 shrink-0 ${disabled ? 'cursor-not-allowed opacity-60' : 'cursor-pointer'} items-center rounded-full border-2 border-transparent transition-colors focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/60 focus-visible:ring-offset-2 ${checked ? 'bg-primary' : 'bg-muted'}`}
      >
        <span className={`pointer-events-none block size-4 rounded-full bg-white shadow transition-transform ${checked ? 'translate-x-4' : 'translate-x-0'}`} />
      </button>
    </div>
  )
}

function emptyDayStat(): AccountUsageDayStat {
  return {
    date: '',
    label: '',
    requests: 0,
    tokens: 0,
    account_billed: 0,
    user_billed: 0,
  }
}

function usageRangeToDays(range: UsageRangeKey): number {
  return USAGE_RANGE_OPTIONS.find((option) => option.key === range)?.days ?? 30
}

function formatActiveDaysText(activeDays: number, periodDays: number, dayUnit: string): string {
  if (periodDays > 0) return `${activeDays} / ${periodDays}`
  return `${activeDays} ${dayUnit}`
}

function modelMetricValue(model: AccountModelStat, metric: ModelMetricKey): number {
  switch (metric) {
    case 'tokens':
      return Number(model.tokens || 0)
    case 'cost':
      return Number(model.account_billed || 0)
    default:
      return Number(model.requests || 0)
  }
}

function modelMetricLabelKey(metric: ModelMetricKey): string {
  switch (metric) {
    case 'tokens':
      return 'accounts.usageModelMetricTokens'
    case 'cost':
      return 'accounts.usageModelMetricCost'
    default:
      return 'accounts.usageModelMetricRequests'
  }
}

function formatModelMetricValue(value: number, metric: ModelMetricKey): string {
  switch (metric) {
    case 'tokens':
      return formatTokens(value)
    case 'cost':
      return `$${formatCost(value)}`
    default:
      return formatNumber(value)
  }
}

function modelMetricDetail(model: AccountModelStat, metric: ModelMetricKey, t: (key: string) => string): string {
  const requests = `${formatNumber(model.requests)} ${t('accounts.usageReqUnit')}`
  const tokens = `${formatTokens(model.tokens)} ${t('accounts.usageTokUnit')}`
  const cost = `$${formatCost(model.account_billed)}`
  switch (metric) {
    case 'tokens':
      return `${requests} · ${cost}`
    case 'cost':
      return `${requests} · ${tokens}`
    default:
      return `${tokens} · ${cost}`
  }
}

function keyDisplayName(stat: AccountKeyStat, t: (key: string) => string): string {
  return stat.api_key_name?.trim() || t('accounts.usageKeyUnnamed')
}

function keyMetricValue(stat: AccountKeyStat, metric: ModelMetricKey): number {
  switch (metric) {
    case 'tokens':
      return Number(stat.tokens || 0)
    case 'cost':
      return Number(stat.account_billed || 0)
    default:
      return Number(stat.requests || 0)
  }
}

function keyMetricDetail(stat: AccountKeyStat, metric: ModelMetricKey, t: (key: string) => string): string {
  const requests = `${formatNumber(stat.requests)} ${t('accounts.usageReqUnit')}`
  const tokens = `${formatTokens(stat.tokens)} ${t('accounts.usageTokUnit')}`
  const cost = `$${formatCost(stat.account_billed)}`
  switch (metric) {
    case 'tokens':
      return `${requests} · ${cost}`
    case 'cost':
      return `${requests} · ${tokens}`
    default:
      return `${tokens} · ${cost}`
  }
}

function formatNumber(value: number): string {
  return Math.round(Number(value || 0)).toLocaleString()
}

function formatCompactNumber(value: number): string {
  const n = Number(value || 0)
  if (n >= 1_000_000) return `${(n / 1_000_000).toFixed(n >= 10_000_000 ? 0 : 1)}M`
  if (n >= 1_000) return `${(n / 1_000).toFixed(n >= 10_000 ? 0 : 1)}K`
  return Math.round(n).toLocaleString()
}

function formatTokens(value: number): string {
  return formatCompactNumber(value)
}

function formatCost(value: number): string {
  const n = Number(value || 0)
  return n >= 1 ? n.toFixed(2) : n.toFixed(4)
}

function formatDuration(value: number): string {
  const n = Number(value || 0)
  if (n <= 0) return '0ms'
  if (n >= 1000) return `${(n / 1000).toFixed(2)}s`
  return `${Math.round(n)}ms`
}

function formatDurationOrDash(value: number): string {
  const n = Number(value || 0)
  return n > 0 ? formatDuration(n) : '-'
}

function formatPercent(value: number): string {
  const n = Number(value || 0)
  return `${n.toFixed(n >= 10 ? 1 : 2)}%`
}
