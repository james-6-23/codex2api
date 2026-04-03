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
import { formatBeijingTime } from '../utils/time'
import { Card, CardContent } from '@/components/ui/card'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { Activity, Box, Clock, Zap, AlertTriangle, Search, Brain, DatabaseZap, X } from 'lucide-react'
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

export default function Usage() {
  const { t } = useTranslation()
  const { toast, showToast } = useToast()
  const { confirm, confirmDialog } = useConfirmDialog()
  const [page, setPage] = useState(1)
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
  const [apiKeyLoadFailed, setAPIKeyLoadFailed] = useState(false)
  const showFastFilter = false
  const PAGE_SIZE = 20
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

  // 服务端分页加载日志（每页仅传输 20 行）
  const loadLogs = useCallback(async () => {
    setLogsLoading(true)
    try {
      const { start, end } = getTimeRangeISO(timeRange)
      const res = await api.getUsageLogsPaged({
        start, end, page, pageSize: PAGE_SIZE,
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
  }, [timeRange, page, searchEmail, filterModel, filterEndpoint, filterApiKeyId, filterFast, filterStream])

  // 首次加载 + timeRange/page 变更时重新拉取日志
  useEffect(() => {
    void loadLogs()
  }, [loadLogs])

  useEffect(() => {
    void loadAPIKeys()
  }, [loadAPIKeys])

  useEffect(() => {
    const timer = window.setInterval(() => {
      void reloadSilently()
    }, 30000)
    return () => window.clearInterval(timer)
  }, [reloadSilently])

  const { stats } = data
  const totalPages = Math.max(1, Math.ceil(logsTotal / PAGE_SIZE))
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
                <span className="text-[11px] font-bold tracking-[0.12em] uppercase text-muted-foreground">{t('usage.totalRequestsCard')}</span>
                <div className="size-10 flex items-center justify-center rounded-xl bg-primary/12 text-primary">
                  <Activity className="size-[18px]" />
                </div>
              </div>
              <div className="text-[28px] font-bold leading-none tracking-tighter">
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
                <span className="text-[11px] font-bold tracking-[0.12em] uppercase text-muted-foreground">{t('usage.totalTokensCard')}</span>
                <div className="size-10 flex items-center justify-center rounded-xl bg-[hsl(var(--info-bg))] text-[hsl(var(--info))]">
                  <Box className="size-[18px]" />
                </div>
              </div>
              <div className="text-[28px] font-bold leading-none tracking-tighter">
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
                <span className="text-[11px] font-bold tracking-[0.12em] uppercase text-muted-foreground">RPM</span>
                <div className="size-10 flex items-center justify-center rounded-xl bg-[hsl(var(--success-bg))] text-[hsl(var(--success))]">
                  <Clock className="size-[18px]" />
                </div>
              </div>
              <div className="text-[28px] font-bold leading-none tracking-tighter">
                {Math.round(rpm)}
              </div>
              <div className="text-[12px] text-muted-foreground">{t('usage.rpmDesc')}</div>
            </CardContent>
          </Card>

          <Card className="py-0">
            <CardContent className="flex flex-col gap-2 p-4">
              <div className="flex items-center justify-between gap-3">
                <span className="text-[11px] font-bold tracking-[0.12em] uppercase text-muted-foreground">TPM</span>
                <div className="size-10 flex items-center justify-center rounded-xl bg-destructive/12 text-destructive">
                  <Zap className="size-[18px]" />
                </div>
              </div>
              <div className="text-[28px] font-bold leading-none tracking-tighter">
                {formatTokens(tpm)}
              </div>
              <div className="text-[12px] text-muted-foreground">{t('usage.tpmDesc')}</div>
            </CardContent>
          </Card>

          <Card className="py-0">
            <CardContent className="flex flex-col gap-2 p-4">
              <div className="flex items-center justify-between gap-3">
                <span className="text-[11px] font-bold tracking-[0.12em] uppercase text-muted-foreground">{t('usage.errorRateCard')}</span>
                <div className="size-10 flex items-center justify-center rounded-xl bg-[hsl(36_72%_40%/0.12)] text-[hsl(36,72%,40%)]">
                  <AlertTriangle className="size-[18px]" />
                </div>
              </div>
              <div className="text-[28px] font-bold leading-none tracking-tighter">
                {errorRate.toFixed(1)}%
              </div>
              <div className="text-[12px] text-muted-foreground">{t('usage.avgLatencyInline', { value: Math.round(avgDurationMs) })}</div>
            </CardContent>
          </Card>
        </div>

        {/* Logs table */}
        <Card>
          <CardContent className="p-6">
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
            <div className="flex items-center gap-2 mb-4 flex-wrap">
              {/* 搜索框 */}
              <div className="relative w-80">
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
                  ...['gpt-5.4', 'gpt-5.4-mini', 'gpt-5', 'gpt-5-codex', 'gpt-5-codex-mini', 'gpt-5.1', 'gpt-5.1-codex', 'gpt-5.1-codex-mini', 'gpt-5.1-codex-max', 'gpt-5.2', 'gpt-5.2-codex', 'gpt-5.3-codex'].map((m) => ({ label: m, value: m })),
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
              <div className="overflow-auto border border-border rounded-xl">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead className="text-[14px] font-semibold">{t('usage.tableStatus')}</TableHead>
                      <TableHead className="text-[14px] font-semibold">{t('usage.tableModel')}</TableHead>
                      <TableHead className="text-[14px] font-semibold">{t('usage.tableAccount')}</TableHead>
                      <TableHead className="text-[14px] font-semibold">{t('usage.tableApiKey')}</TableHead>
                      <TableHead className="text-[16px] font-semibold" style={{ fontFamily: "'Geist Mono', monospace" }}>{t('usage.tableEndpoint')}</TableHead>
                      <TableHead className="text-[14px] font-semibold">{t('usage.tableType')}</TableHead>
                      <TableHead className="text-[14px] font-semibold">{t('usage.tableToken')}</TableHead>
                      <TableHead className="text-[14px] font-semibold">{t('usage.tableCached')}</TableHead>
                      <TableHead className="text-[16px] font-semibold" style={{ fontFamily: "'Geist Mono', monospace" }}>{t('usage.tableFirstToken')}</TableHead>
                      <TableHead className="text-[16px] font-semibold" style={{ fontFamily: "'Geist Mono', monospace" }}>{t('usage.tableDuration')}</TableHead>
                      <TableHead className="text-[13px] font-semibold" style={{ fontFamily: "'Geist Mono', monospace" }}>{t('usage.tableTime')}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {logs.map((log: UsageLog) => {
                      return (
                      <TableRow key={log.id}>
                        <TableCell>
                          <Badge
                            variant="outline"
                            className={`text-[14px] ${getStatusBadgeClassName(log.status_code)}`}
                          >
                            {log.status_code}
                          </Badge>
                        </TableCell>
                        <TableCell>
                          <div className="flex items-center gap-1.5 flex-wrap">
                            <Badge variant="outline" className="text-[14px]">
                              {log.model || '-'}
                            </Badge>
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
                        <TableCell className="text-[14px] text-muted-foreground">
                          {log.account_email || '-'}
                        </TableCell>
                        <TableCell className="text-[14px] text-muted-foreground">
                          <span className="block max-w-[180px] truncate whitespace-nowrap" title={formatUsageAPIKeyLabel(log.api_key_name, log.api_key_masked) || t('usage.unknownApiKey')}>
                            {formatUsageAPIKeyLabel(log.api_key_name, log.api_key_masked) || t('usage.unknownApiKey')}
                          </span>
                        </TableCell>
                        <TableCell>
                          <div className="text-[16px] leading-relaxed" style={{ fontFamily: "'Geist Mono', monospace" }}>
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
                            className="text-[13px]"
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
                            <div className="text-[14px] leading-relaxed">
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
                            <span className="text-[14px] text-muted-foreground">-</span>
                          )}
                        </TableCell>
                        <TableCell>
                          {log.cached_tokens > 0 ? (
                            <Badge variant="outline" className="text-[13px] gap-1 border-transparent bg-indigo-500/10 text-indigo-600 dark:bg-indigo-500/20 dark:text-indigo-400">
                              <DatabaseZap className="size-3.5" />
                              {formatTokens(log.cached_tokens)}
                            </Badge>
                          ) : (
                            <span className="text-[14px] text-muted-foreground">-</span>
                          )}
                        </TableCell>
                        <TableCell>
                          {log.first_token_ms > 0 ? (
                            <span className={`text-[16px] ${log.first_token_ms > 5000 ? 'text-red-500' : log.first_token_ms > 2000 ? 'text-amber-500' : 'text-emerald-500'}`} style={{ fontFamily: "'Geist Mono', monospace" }}>
                              {log.first_token_ms > 1000 ? `${(log.first_token_ms / 1000).toFixed(1)}s` : `${log.first_token_ms}ms`}
                            </span>
                          ) : <span className="text-[16px] text-muted-foreground" style={{ fontFamily: "'Geist Mono', monospace" }}>-</span>}
                        </TableCell>
                        <TableCell>
                          <span className={`text-[16px] ${log.duration_ms > 30000 ? 'text-red-500' : log.duration_ms > 10000 ? 'text-amber-500' : 'text-muted-foreground'}`} style={{ fontFamily: "'Geist Mono', monospace" }}>
                            {log.duration_ms > 1000 ? `${(log.duration_ms / 1000).toFixed(1)}s` : `${log.duration_ms}ms`}
                          </span>
                        </TableCell>
                        <TableCell className="text-[12px] text-muted-foreground whitespace-nowrap" style={{ fontFamily: "'Geist Mono', monospace" }}>
                          {formatBeijingTime(log.created_at)}
                        </TableCell>
                      </TableRow>
                      )
                    })}
                  </TableBody>
                </Table>
              </div>
              <Pagination
                page={page}
                totalPages={totalPages}
                onPageChange={setPage}
                totalItems={logsTotal}
                pageSize={PAGE_SIZE}
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
