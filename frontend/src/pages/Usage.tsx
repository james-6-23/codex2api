import { useCallback, useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { api } from '../api'
import { getTimeRangeISO } from '../components/DashboardUsageCharts'
import type { TimeRangeKey } from '../components/DashboardUsageCharts'
import PageHeader from '../components/PageHeader'
import Pagination from '../components/Pagination'
import StateShell from '../components/StateShell'
import ToastNotice from '../components/ToastNotice'
import { useDataLoader } from '../hooks/useDataLoader'
import { useConfirmDialog } from '../hooks/useConfirmDialog'
import { useToast } from '../hooks/useToast'
import type { APIKeyRow, UsageLog, UsageStats } from '../types'
import { DEFAULT_CLAUDE_MODEL_MAP } from '../lib/modelMapping'
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
import { Activity, Box, Clock, Zap, AlertTriangle, Search, Brain, DatabaseZap, X, Image as ImageIcon } from 'lucide-react'
import { Input } from '@/components/ui/input'
import { Select } from '@/components/ui/select'

function formatTokens(value?: number | null): string {
  if (value === undefined || value === null) return '0'
  return value.toLocaleString()
}

function parseClaudeModelMap(value: string): Record<string, string> {
  try {
    const parsed = JSON.parse(value || '{}')
    if (!parsed || typeof parsed !== 'object' || Array.isArray(parsed)) {
      return DEFAULT_CLAUDE_MODEL_MAP
    }

    const customMap: Record<string, string> = {}
    for (const [key, mappedModel] of Object.entries(parsed)) {
      const trimmedKey = key.trim()
      const trimmedModel = typeof mappedModel === 'string' ? mappedModel.trim() : ''
      if (trimmedKey && trimmedModel) {
        customMap[trimmedKey] = trimmedModel
      }
    }
    return { ...DEFAULT_CLAUDE_MODEL_MAP, ...customMap }
  } catch {
    return DEFAULT_CLAUDE_MODEL_MAP
  }
}

function getCodexModel(model: string, modelMap: Record<string, string>): string {
  return modelMap[model] || ''
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

const usageTableHeadClass = 'text-[12px] font-semibold'
const usageTableTextClass = 'text-[14px]'
const usageTableMonoClass = 'font-geist-mono text-[13px] tabular-nums'
const usageTableBadgeClass = 'text-[13px]'

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
  const [claudeModelMap, setClaudeModelMap] = useState<Record<string, string>>(DEFAULT_CLAUDE_MODEL_MAP)
  const [apiKeyLoadFailed, setAPIKeyLoadFailed] = useState(false)
  const showFastFilter = false
  const pageSizeOptions = [10, 20, 50, 100]
  const searchTimer = useRef<ReturnType<typeof setTimeout>>(null)

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
    const loadModelMapping = async () => {
      try {
        const settings = await api.getSettings()
        if (active) setClaudeModelMap(parseClaudeModelMap(settings.model_mapping))
      } catch {
        if (active) setClaudeModelMap(DEFAULT_CLAUDE_MODEL_MAP)
      }
    }
    void loadModelMapping()
    return () => {
      active = false
    }
  }, [])

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
  const todayRequests = stats?.today_requests ?? 0
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

        {/* Top stats: 2 columns */}
        <div className="grid grid-cols-2 gap-3 mb-3 max-sm:grid-cols-1">
          <Card className="py-0">
            <CardContent className="flex flex-col gap-2 p-4">
              <div className="flex items-center justify-between gap-3">
                <span className="text-[11px] font-bold uppercase text-muted-foreground">{t('usage.totalRequestsCard')}</span>
                <div className="size-10 flex items-center justify-center rounded-xl bg-primary/12 text-primary">
                  <Activity className="size-[18px]" />
                </div>
              </div>
              <div className="text-[26px] font-bold leading-none">
                {formatTokens(totalRequests)}
              </div>
              <div className="text-[12px] text-muted-foreground leading-relaxed">
                <span className="text-[hsl(var(--success))]">● {t('usage.success')}: {formatTokens(successRequests)}</span>
                <span className="ml-2 text-muted-foreground">● {t('usage.today')}: {formatTokens(todayRequests)}</span>
              </div>
            </CardContent>
          </Card>

          <Card className="py-0">
            <CardContent className="flex flex-col gap-2 p-4">
              <div className="flex items-center justify-between gap-3">
                <span className="text-[11px] font-bold uppercase text-muted-foreground">{t('usage.totalTokensCard')}</span>
                <div className="size-10 flex items-center justify-center rounded-xl bg-[hsl(var(--info-bg))] text-[hsl(var(--info))]">
                  <Box className="size-[18px]" />
                </div>
              </div>
              <div className="text-[26px] font-bold leading-none">
                {formatTokens(totalTokens)}
              </div>
              <div className="text-[12px] text-muted-foreground leading-relaxed">
                <span>{t('usage.inputTokens')}: {formatTokens(totalPromptTokens)}</span>
                <span className="ml-2">{t('usage.outputTokens')}: {formatTokens(totalCompletionTokens)}</span>
              </div>
            </CardContent>
          </Card>
        </div>

        {/* Bottom stats: 3 columns */}
        <div className="grid grid-cols-3 gap-3 mb-6 max-sm:grid-cols-1">
          <Card className="py-0">
            <CardContent className="flex flex-col gap-2 p-4">
              <div className="flex items-center justify-between gap-3">
                <span className="text-[11px] font-bold uppercase text-muted-foreground">RPM</span>
                <div className="size-10 flex items-center justify-center rounded-xl bg-[hsl(var(--success-bg))] text-[hsl(var(--success))]">
                  <Clock className="size-[18px]" />
                </div>
              </div>
              <div className="text-[26px] font-bold leading-none">
                {Math.round(rpm)}
              </div>
              <div className="text-[12px] text-muted-foreground">{t('usage.rpmDesc')}</div>
            </CardContent>
          </Card>

          <Card className="py-0">
            <CardContent className="flex flex-col gap-2 p-4">
              <div className="flex items-center justify-between gap-3">
                <span className="text-[11px] font-bold uppercase text-muted-foreground">TPM</span>
                <div className="size-10 flex items-center justify-center rounded-xl bg-destructive/12 text-destructive">
                  <Zap className="size-[18px]" />
                </div>
              </div>
              <div className="text-[26px] font-bold leading-none">
                {formatTokens(tpm)}
              </div>
              <div className="text-[12px] text-muted-foreground">{t('usage.tpmDesc')}</div>
            </CardContent>
          </Card>

          <Card className="py-0">
            <CardContent className="flex flex-col gap-2 p-4">
              <div className="flex items-center justify-between gap-3">
                <span className="text-[11px] font-bold uppercase text-muted-foreground">{t('usage.errorRateCard')}</span>
                <div className="size-10 flex items-center justify-center rounded-xl bg-[hsl(36_72%_40%/0.12)] text-[hsl(36,72%,40%)]">
                  <AlertTriangle className="size-[18px]" />
                </div>
              </div>
              <div className="text-[26px] font-bold leading-none">
                {errorRate.toFixed(1)}%
              </div>
              <div className="text-[12px] text-muted-foreground">{t('usage.avgLatencyInline', { value: Math.round(avgDurationMs) })}</div>
            </CardContent>
          </Card>
        </div>

        {/* Logs table */}
        <Card>
          <CardContent className="p-4">
            <div className="flex items-center justify-between gap-4 mb-4 flex-wrap">
              <div className="flex items-center gap-3">
                <h3 className="text-base font-semibold text-foreground">{t('usage.requestLogs')}</h3>
                <div className="inline-flex rounded-lg border border-border bg-muted/50 p-0.5">
                  {TIME_RANGE_OPTIONS.map((key) => (
                    <button
                      key={key}
                      type="button"
                      onClick={() => { setTimeRange(key); setPage(1) }}
                      className={`px-2.5 py-1 text-xs font-medium rounded-md transition-all duration-200 ${
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
              <div className="flex items-center gap-3">
                <span className="text-xs text-muted-foreground">{logsLoading ? t('common.loading') : t('usage.recordsCount', { count: logsTotal })}</span>
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
            <div className="toolbar-surface mb-4 flex flex-wrap items-center gap-2">
              {/* 搜索框 */}
              <div className="relative w-72 max-sm:w-full">
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
                className="w-44"
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
                className="w-52"
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
                  className="w-60"
                  compact
                  value={filterApiKeyId}
                  onValueChange={(v) => { setFilterApiKeyId(v); setPage(1) }}
                  placeholder={t('usage.allApiKeys')}
                  options={apiKeyOptions}
                />
              )}

              {/* 类型下拉 */}
              <Select
                className="w-32"
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
                  className={`h-8 px-2.5 rounded-lg border text-[13px] font-medium transition-colors inline-flex items-center gap-1 ${
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
                  className="h-8 px-2.5 rounded-lg border border-border bg-background text-[13px] text-muted-foreground hover:text-foreground hover:bg-muted/50 transition-colors inline-flex items-center gap-1"
                >
                  <X className="size-3.5" />
                  {t('usage.clearFilters')}
                </button>
              )}
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
                      <TableHead className={usageTableHeadClass}>{t('usage.tableStatus')}</TableHead>
                      <TableHead className={usageTableHeadClass}>{t('usage.tableModel')}</TableHead>
                      <TableHead className={usageTableHeadClass}>{t('usage.tableAccount')}</TableHead>
                      <TableHead className={usageTableHeadClass}>{t('usage.tableApiKey')}</TableHead>
                      <TableHead className={usageTableHeadClass}>{t('usage.tableEndpoint')}</TableHead>
                      <TableHead className={usageTableHeadClass}>{t('usage.tableType')}</TableHead>
                      <TableHead className={usageTableHeadClass}>{t('usage.tableToken')}</TableHead>
                      <TableHead className={usageTableHeadClass}>{t('usage.tableCached')}</TableHead>
                      <TableHead className={usageTableHeadClass}>{t('usage.tableFirstToken')}</TableHead>
                      <TableHead className={usageTableHeadClass}>{t('usage.tableDuration')}</TableHead>
                      <TableHead className={usageTableHeadClass}>{t('usage.tableTime')}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {logs.map((log: UsageLog) => {
                      const mappedModel = log.model?.startsWith('claude') ? getCodexModel(log.model, claudeModelMap) : ''
                      return (
                      <TableRow key={log.id}>
                        <TableCell>
                          <Badge
                            variant="outline"
                            className={`${usageTableBadgeClass} ${getStatusBadgeClassName(log.status_code)}`}
                          >
                            {log.status_code}
                          </Badge>
                        </TableCell>
                        <TableCell>
                          <div className="flex items-center gap-1.5 flex-wrap">
                            <Badge variant="outline" className={usageTableBadgeClass}>
                              {log.model || '-'}
                            </Badge>
                            {mappedModel && (
                              <Badge variant="outline" className="text-[11px] font-medium border-transparent bg-blue-500/10 text-blue-600 dark:bg-blue-500/20 dark:text-blue-400">
                                → {mappedModel}
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
                            {log.service_tier === 'fast' && (
                              <Badge
                                variant="outline"
                                className="text-[11px] font-semibold gap-0.5 border-transparent bg-blue-500/12 text-blue-600 dark:bg-blue-500/20 dark:text-blue-400"
                              >
                                <Zap className="size-3" />
                                Fast
                              </Badge>
                            )}
                          </div>
                        </TableCell>
                        <TableCell className={`${usageTableTextClass} text-muted-foreground`}>
                          {formatCompactEmail(log.account_email)}
                        </TableCell>
                        <TableCell className={`${usageTableTextClass} text-muted-foreground`}>
                          <span className="block max-w-[180px] truncate whitespace-nowrap" title={formatUsageAPIKeyLabel(log.api_key_name, log.api_key_masked) || t('usage.unknownApiKey')}>
                            {formatUsageAPIKeyLabel(log.api_key_name, log.api_key_masked) || t('usage.unknownApiKey')}
                          </span>
                        </TableCell>
                        <TableCell>
                          <div className={`${usageTableMonoClass} leading-relaxed`}>
                            <span className="text-muted-foreground">
                              {log.inbound_endpoint || log.endpoint || '-'}
                            </span>
                            {log.upstream_endpoint && log.upstream_endpoint !== log.inbound_endpoint && (
                              <span className="text-muted-foreground"> → {log.upstream_endpoint}</span>
                            )}
                          </div>
                        </TableCell>
                        <TableCell>
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
                        </TableCell>
                        <TableCell>
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
                        </TableCell>
                        <TableCell>
                          {log.cached_tokens > 0 ? (
                            <Badge variant="outline" className={`${usageTableBadgeClass} gap-1 border-transparent bg-indigo-500/10 text-indigo-600 dark:bg-indigo-500/20 dark:text-indigo-400`}>
                              <DatabaseZap className="size-3.5" />
                              {formatTokens(log.cached_tokens)}
                            </Badge>
                          ) : (
                            <span className={`${usageTableMonoClass} text-muted-foreground`}>-</span>
                          )}
                        </TableCell>
                        <TableCell>
                          {log.first_token_ms > 0 ? (
                            <span className={`${usageTableMonoClass} ${log.first_token_ms > 5000 ? 'text-red-500' : log.first_token_ms > 2000 ? 'text-amber-500' : 'text-emerald-500'}`}>
                              {log.first_token_ms > 1000 ? `${(log.first_token_ms / 1000).toFixed(1)}s` : `${log.first_token_ms}ms`}
                            </span>
                          ) : <span className={`${usageTableMonoClass} text-muted-foreground`}>-</span>}
                        </TableCell>
                        <TableCell>
                          <span className={`${usageTableMonoClass} ${log.duration_ms > 30000 ? 'text-red-500' : log.duration_ms > 10000 ? 'text-amber-500' : 'text-muted-foreground'}`}>
                            {log.duration_ms > 1000 ? `${(log.duration_ms / 1000).toFixed(1)}s` : `${log.duration_ms}ms`}
                          </span>
                        </TableCell>
                        <TableCell className={`${usageTableMonoClass} text-muted-foreground whitespace-nowrap`}>
                          {formatBeijingTime(log.created_at)}
                        </TableCell>
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

        <ToastNotice toast={toast} />
        {confirmDialog}
      </>
    </StateShell>
  )
}
