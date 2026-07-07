import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react'
import { useTranslation } from 'react-i18next'
import {
  AlertCircle,
  Clock3,
  Copy,
  Download,
  RefreshCw,
  RotateCcw,
  Search,
  ServerCrash,
  ShieldAlert,
  TimerReset,
  X,
} from 'lucide-react'
import { api } from '../api'
import OpsTabs from '../components/OpsTabs'
import PageHeader from '../components/PageHeader'
import Pagination from '../components/Pagination'
import StateShell from '../components/StateShell'
import { useDataLoader } from '../hooks/useDataLoader'
import { useToast } from '../hooks/useToast'
import { DEFAULT_PAGE_SIZE_OPTIONS, usePersistedPageSize } from '../hooks/usePersistedPageSize'
import { getTimeRangeISO, type TimeRangeKey } from '../lib/timeRange'
import { formatCompactEmail } from '../lib/utils'
import { formatBeijingTime } from '../utils/time'
import type { APIKeyRow, OpsErrorSummary, UsageLog } from '../types'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
} from '@/components/ui/dialog'
import { Input } from '@/components/ui/input'
import { Select } from '@/components/ui/select'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'

const ERROR_TIME_RANGES: TimeRangeKey[] = ['1h', '6h', '24h', '7d', '30d']
const pageSizeOptions = DEFAULT_PAGE_SIZE_OPTIONS

const errorTableHeadClass = 'text-[12px] font-semibold'
const errorTableTextClass = 'text-[14px]'
const errorTableMonoClass = 'font-geist-mono text-[13px] tabular-nums'

export default function OperationsErrors() {
  const { t } = useTranslation()
  const { toast, showToast } = useToast()
  const [timeRange, setTimeRange] = useState<TimeRangeKey>('1h')
  const [page, setPage] = useState(1)
  const [pageSize, setPageSize] = usePersistedPageSize('ops_errors', 20, pageSizeOptions)
  const [statusFilter, setStatusFilter] = useState('')
  const [errorKindFilter, setErrorKindFilter] = useState('')
  const [endpointFilter, setEndpointFilter] = useState('')
  const [apiKeyFilter, setApiKeyFilter] = useState('')
  const [streamFilter, setStreamFilter] = useState<'' | 'true' | 'false'>('')
  const [searchInput, setSearchInput] = useState('')
  const [searchQuery, setSearchQuery] = useState('')
  const [exportDedupe, setExportDedupe] = useState(true)
  const [exportExcludeAuthRateLimit, setExportExcludeAuthRateLimit] = useState(true)
  const [exporting, setExporting] = useState(false)
  const [selectedLog, setSelectedLog] = useState<UsageLog | null>(null)
  const searchTimer = useRef<ReturnType<typeof setTimeout>>(null)

  const handleSearchChange = useCallback((value: string) => {
    setSearchInput(value)
    if (searchTimer.current) {
      clearTimeout(searchTimer.current)
    }
    searchTimer.current = setTimeout(() => {
      setSearchQuery(value.trim())
      setPage(1)
    }, 400)
  }, [])

  useEffect(() => () => {
    if (searchTimer.current) {
      clearTimeout(searchTimer.current)
    }
  }, [])

  const buildBaseParams = useCallback(() => {
    const range = getTimeRangeISO(timeRange)
    return {
      start: range.start,
      end: range.end,
      status: statusFilter,
      errorKind: errorKindFilter,
      endpoint: endpointFilter,
      apiKeyId: apiKeyFilter,
      stream: streamFilter,
      q: searchQuery,
    }
  }, [apiKeyFilter, endpointFilter, errorKindFilter, searchQuery, statusFilter, streamFilter, timeRange])

  const loadErrorData = useCallback(async () => {
    const baseParams = buildBaseParams()
    const [summary, pageResult, apiKeysResult] = await Promise.all([
      api.getOpsErrorSummary(baseParams),
      api.getOpsErrors({
        ...baseParams,
        page,
        pageSize,
      }),
      api.getAPIKeys().catch(() => ({ keys: [] as APIKeyRow[] })),
    ])

    return {
      summary,
      logs: pageResult.logs ?? [],
      total: pageResult.total ?? 0,
      apiKeys: apiKeysResult.keys ?? [],
    }
  }, [buildBaseParams, page, pageSize])

  const { data, loading, error, reload, reloadSilently } = useDataLoader<{
    summary: OpsErrorSummary | null
    logs: UsageLog[]
    total: number
    apiKeys: APIKeyRow[]
  }>({
    initialData: {
      summary: null,
      logs: [],
      total: 0,
      apiKeys: [],
    },
    load: loadErrorData,
  })

  useEffect(() => {
    const timer = window.setInterval(() => {
      void reloadSilently()
    }, 15000)

    return () => window.clearInterval(timer)
  }, [reloadSilently])

  const hasActiveFilters = Boolean(statusFilter || errorKindFilter || endpointFilter || apiKeyFilter || streamFilter || searchQuery)
  const totalPages = Math.max(1, Math.ceil(data.total / pageSize))
  const currentPage = Math.min(page, totalPages)
  const apiKeyOptions = useMemo(() => [
    { label: t('opsErrors.allApiKeys'), value: '' },
    ...data.apiKeys.map((apiKey) => ({
      label: apiKey.name ? `${apiKey.name} · ${apiKey.key}` : apiKey.key,
      value: String(apiKey.id),
    })),
  ], [data.apiKeys, t])

  useEffect(() => {
    if (page > totalPages) {
      setPage(totalPages)
    }
  }, [page, totalPages])

  const resetFilters = () => {
    setStatusFilter('')
    setErrorKindFilter('')
    setEndpointFilter('')
    setApiKeyFilter('')
    setStreamFilter('')
    setSearchInput('')
    setSearchQuery('')
    setPage(1)
  }

  const handleExport = async () => {
    setExporting(true)
    try {
      const blob = await api.downloadOpsErrors({
        ...buildBaseParams(),
        dedupe: exportDedupe,
        excludeStatus: exportExcludeAuthRateLimit ? '401,429' : '',
      })
      downloadBlob(blob, buildOpsErrorExportFilename(timeRange, exportDedupe, exportExcludeAuthRateLimit))
      showToast(t('opsErrors.exportSuccess'))
    } catch (err) {
      showToast(err instanceof Error ? err.message : t('opsErrors.exportFailed'), 'error')
    } finally {
      setExporting(false)
    }
  }

  const copyLog = async (log: UsageLog) => {
    const text = JSON.stringify({
      id: log.id,
      created_at: log.created_at,
      status_code: log.status_code,
      error_kind: log.upstream_error_kind,
      error_message: log.error_message,
      account_id: log.account_id,
      account_name: log.account_name,
      account_email: log.account_email,
      api_key_id: log.api_key_id,
      api_key_name: log.api_key_name,
      endpoint: log.inbound_endpoint || log.endpoint,
      upstream_endpoint: log.upstream_endpoint,
      model: log.model,
      effective_model: log.effective_model,
      stream: log.stream,
      duration_ms: log.duration_ms,
      first_token_ms: log.first_token_ms,
      is_retry_attempt: log.is_retry_attempt,
      attempt_index: log.attempt_index,
    }, null, 2)
    try {
      await copyTextToClipboard(text)
      showToast(t('opsErrors.copySuccess'))
    } catch {
      showToast(t('opsErrors.copyFailed'), 'error')
    }
  }

  return (
    <StateShell
      variant="page"
      loading={loading}
      error={error}
      onRetry={() => void reload()}
      loadingTitle={t('opsErrors.loadingTitle')}
      loadingDescription={t('opsErrors.loadingDesc')}
      errorTitle={t('opsErrors.errorTitle')}
    >
      <>
        <PageHeader
          title={t('opsErrors.title')}
          description={t('opsErrors.description')}
          actions={
            <Button variant="outline" onClick={() => void reload()}>
              <RefreshCw className="size-3.5" />
              {t('common.refresh')}
            </Button>
          }
        />
        <OpsTabs />

        <div className="grid grid-cols-[repeat(auto-fit,minmax(180px,1fr))] gap-4 mb-6">
          <SummaryPill
            label={t('opsErrors.totalErrors')}
            value={formatNumber(data.summary?.total_errors ?? 0)}
            icon={<AlertCircle className="size-4" />}
            tone="danger"
          />
          <SummaryPill
            label="5xx"
            value={formatNumber(data.summary?.status_5xx ?? 0)}
            icon={<ServerCrash className="size-4" />}
            tone="danger"
          />
          <SummaryPill
            label="401"
            value={formatNumber(data.summary?.unauthorized ?? 0)}
            icon={<ShieldAlert className="size-4" />}
            tone="danger"
          />
          <SummaryPill
            label="429"
            value={formatNumber(data.summary?.rate_limited ?? 0)}
            icon={<TimerReset className="size-4" />}
            tone="warning"
          />
          <SummaryPill
            label={t('opsErrors.timeouts')}
            value={formatNumber(data.summary?.timeouts ?? 0)}
            icon={<Clock3 className="size-4" />}
            tone="warning"
          />
          <SummaryPill
            label={t('opsErrors.retryAttempts')}
            value={formatNumber(data.summary?.retry_attempts ?? 0)}
            icon={<RotateCcw className="size-4" />}
            tone="info"
          />
        </div>

        <Card>
          <CardContent className="p-6">
            <div className="mb-4 flex flex-wrap items-center justify-between gap-3">
              <div>
                <h3 className="text-base font-semibold text-foreground">{t('opsErrors.tableTitle')}</h3>
                <p className="mt-1 text-sm text-muted-foreground">{t('opsErrors.tableDesc')}</p>
              </div>
              <div className="inline-flex rounded-lg border border-border bg-muted/50 p-0.5">
                {ERROR_TIME_RANGES.map((key) => (
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

            <div className="toolbar-surface mb-4 flex flex-wrap items-center gap-2">
              <div className="relative w-80 max-sm:w-full">
                <Search className="absolute left-2.5 top-1/2 -translate-y-1/2 size-3.5 text-muted-foreground pointer-events-none" />
                <Input
                  className="pl-8 h-8 rounded-lg text-[13px]"
                  placeholder={t('opsErrors.searchPlaceholder')}
                  value={searchInput}
                  onChange={(e: React.ChangeEvent<HTMLInputElement>) => handleSearchChange(e.target.value)}
                />
              </div>
              <Select
                className="w-36"
                compact
                value={statusFilter}
                onValueChange={(value) => { setStatusFilter(value); setPage(1) }}
                placeholder={t('opsErrors.allStatus')}
                options={[
                  { label: t('opsErrors.allStatus'), value: '' },
                  { label: '4xx', value: '4xx' },
                  { label: '5xx', value: '5xx' },
                  { label: '401', value: '401' },
                  { label: '403', value: '403' },
                  { label: '429', value: '429' },
                  { label: '499', value: '499' },
                ]}
              />
              <Select
                className="w-52"
                compact
                value={errorKindFilter}
                onValueChange={(value) => { setErrorKindFilter(value); setPage(1) }}
                placeholder={t('opsErrors.allErrorKinds')}
                options={[
                  { label: t('opsErrors.allErrorKinds'), value: '' },
                  { label: 'upstream_error', value: 'upstream_error' },
                  { label: 'upstream_timeout', value: 'upstream_timeout' },
                  { label: 'server_error', value: 'server_error' },
                  { label: 'rate_limit', value: 'rate_limit' },
                  { label: 'transport_error', value: 'transport_error' },
                ]}
              />
              <Select
                className="w-52"
                compact
                value={endpointFilter}
                onValueChange={(value) => { setEndpointFilter(value); setPage(1) }}
                placeholder={t('opsErrors.allEndpoints')}
                options={[
                  { label: t('opsErrors.allEndpoints'), value: '' },
                  { label: '/v1/responses', value: '/v1/responses' },
                  { label: '/v1/chat/completions', value: '/v1/chat/completions' },
                  { label: '/v1/messages', value: '/v1/messages' },
                  { label: '/v1/images/generations', value: '/v1/images/generations' },
                  { label: '/v1/images/edits', value: '/v1/images/edits' },
                ]}
              />
              <Select
                className="w-60"
                compact
                value={apiKeyFilter}
                onValueChange={(value) => { setApiKeyFilter(value); setPage(1) }}
                placeholder={t('opsErrors.allApiKeys')}
                options={apiKeyOptions}
              />
              <Select
                className="w-32"
                compact
                value={streamFilter}
                onValueChange={(value) => { setStreamFilter(value as '' | 'true' | 'false'); setPage(1) }}
                placeholder={t('usage.allTypes')}
                options={[
                  { label: t('usage.allTypes'), value: '' },
                  { label: 'Stream', value: 'true' },
                  { label: 'Sync', value: 'false' },
                ]}
              />
              <label className="inline-flex h-8 items-center gap-1.5 rounded-lg border border-border bg-background px-2.5 text-[13px] text-muted-foreground">
                <input
                  type="checkbox"
                  checked={exportDedupe}
                  onChange={(event) => setExportDedupe(event.target.checked)}
                  className="size-3.5 rounded border-border"
                />
                {t('opsErrors.exportDedupe')}
              </label>
              <label className="inline-flex h-8 items-center gap-1.5 rounded-lg border border-border bg-background px-2.5 text-[13px] text-muted-foreground">
                <input
                  type="checkbox"
                  checked={exportExcludeAuthRateLimit}
                  onChange={(event) => setExportExcludeAuthRateLimit(event.target.checked)}
                  className="size-3.5 rounded border-border"
                />
                {t('opsErrors.exclude401429')}
              </label>
              <Button variant="outline" size="sm" onClick={() => void handleExport()} disabled={exporting}>
                <Download className="size-3.5" />
                {exporting ? t('opsErrors.exporting') : t('opsErrors.exportJson')}
              </Button>
              {hasActiveFilters && (
                <button
                  type="button"
                  onClick={resetFilters}
                  className="h-8 px-2.5 rounded-lg border border-border bg-background text-[13px] text-muted-foreground hover:text-foreground hover:bg-muted/50 transition-colors inline-flex items-center gap-1"
                >
                  <X className="size-3.5" />
                  {t('usage.clearFilters')}
                </button>
              )}
              <span className="ml-auto text-xs text-muted-foreground max-sm:ml-0">
                {t('usage.recordsCount', { count: data.total })}
              </span>
            </div>

            <StateShell
              variant="section"
              isEmpty={data.logs.length === 0}
              emptyTitle={t('opsErrors.emptyTitle')}
              emptyDescription={hasActiveFilters ? t('opsErrors.emptyFilteredDesc') : t('opsErrors.emptyDesc')}
            >
              <div className="data-table-shell">
                <Table>
                  <TableHeader>
                    <TableRow>
                      <TableHead className={errorTableHeadClass}>{t('usage.tableTime')}</TableHead>
                      <TableHead className={errorTableHeadClass}>{t('usage.tableStatus')}</TableHead>
                      <TableHead className={errorTableHeadClass}>{t('opsErrors.errorKind')}</TableHead>
                      <TableHead className={errorTableHeadClass}>{t('usage.tableModel')}</TableHead>
                      <TableHead className={errorTableHeadClass}>{t('usage.tableEndpoint')}</TableHead>
                      <TableHead className={errorTableHeadClass}>{t('usage.tableAccount')}</TableHead>
                      <TableHead className={errorTableHeadClass}>{t('usage.tableApiKey')}</TableHead>
                      <TableHead className={errorTableHeadClass}>{t('usage.tableDuration')}</TableHead>
                      <TableHead className={errorTableHeadClass}>{t('opsErrors.errorSummary')}</TableHead>
                      <TableHead className={errorTableHeadClass}>{t('common.actions')}</TableHead>
                    </TableRow>
                  </TableHeader>
                  <TableBody>
                    {data.logs.map((log) => (
                      <TableRow key={log.id}>
                        <TableCell className={`${errorTableMonoClass} text-muted-foreground`}>{formatBeijingTime(log.created_at)}</TableCell>
                        <TableCell>
                          <Badge variant="outline" className={`text-[13px] ${getStatusBadgeClassName(log.status_code)}`}>
                            {log.status_code}
                          </Badge>
                        </TableCell>
                        <TableCell>
                          <Badge variant="outline" className="border-transparent bg-slate-500/10 text-slate-600 dark:bg-slate-500/20 dark:text-slate-300">
                            {log.upstream_error_kind || classifyStatus(log.status_code)}
                          </Badge>
                        </TableCell>
                        <TableCell>
                          <div className="flex flex-wrap items-center gap-1.5">
                            <Badge variant="outline" className="text-[13px]">{log.model || '-'}</Badge>
                            {log.effective_model && log.effective_model !== log.model && (
                              <Badge variant="outline" className="border-transparent bg-blue-500/10 text-blue-600 dark:bg-blue-500/20 dark:text-blue-400">
                                {log.effective_model}
                              </Badge>
                            )}
                          </div>
                        </TableCell>
                        <TableCell>
                          <div className={`${errorTableMonoClass} max-w-[240px] truncate text-muted-foreground`} title={formatEndpoint(log)}>
                            {formatEndpoint(log)}
                          </div>
                        </TableCell>
                        <TableCell className={`${errorTableTextClass} text-muted-foreground`}>
                          <span className="block max-w-[180px] truncate whitespace-nowrap" title={formatAccountTitle(log)}>
                            {formatAccountLabel(log)}
                          </span>
                        </TableCell>
                        <TableCell className={`${errorTableTextClass} text-muted-foreground`}>
                          <span className="block max-w-[160px] truncate" title={formatAPIKeyLabel(log)}>
                            {formatAPIKeyLabel(log) || t('usage.unknownApiKey')}
                          </span>
                        </TableCell>
                        <TableCell>
                          <span className={`${errorTableMonoClass} ${log.duration_ms > 30000 ? 'text-red-500' : log.duration_ms > 10000 ? 'text-amber-500' : 'text-muted-foreground'}`}>
                            {formatDuration(log.duration_ms)}
                          </span>
                        </TableCell>
                        <TableCell className="max-w-[360px] whitespace-normal">
                          <div className="line-clamp-2 text-[13px] leading-relaxed text-muted-foreground" title={log.error_message || ''}>
                            {log.error_message || t('opsErrors.noErrorMessage')}
                          </div>
                        </TableCell>
                        <TableCell>
                          <Button variant="outline" size="xs" onClick={() => setSelectedLog(log)}>
                            {t('opsErrors.details')}
                          </Button>
                        </TableCell>
                      </TableRow>
                    ))}
                  </TableBody>
                </Table>
              </div>
              <Pagination
                page={currentPage}
                totalPages={totalPages}
                onPageChange={setPage}
                totalItems={data.total}
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

        <Dialog open={Boolean(selectedLog)} onOpenChange={(open) => { if (!open) setSelectedLog(null) }}>
          {selectedLog ? (
            <DialogContent className="max-h-[86vh] overflow-y-auto sm:max-w-4xl">
              <DialogHeader>
                <DialogTitle>{t('opsErrors.detailTitle', { id: selectedLog.id })}</DialogTitle>
                <DialogDescription>{formatBeijingTime(selectedLog.created_at)}</DialogDescription>
              </DialogHeader>
              <div className="flex flex-wrap items-center gap-2">
                <Badge variant="outline" className={`text-[13px] ${getStatusBadgeClassName(selectedLog.status_code)}`}>
                  HTTP {selectedLog.status_code}
                </Badge>
                <Badge variant="outline">{selectedLog.upstream_error_kind || classifyStatus(selectedLog.status_code)}</Badge>
                {selectedLog.is_retry_attempt && (
                  <Badge variant="outline" className="border-transparent bg-blue-500/10 text-blue-600 dark:bg-blue-500/20 dark:text-blue-400">
                    {t('opsErrors.retryAttempt', { index: selectedLog.attempt_index })}
                  </Badge>
                )}
              </div>

              <div className="rounded-lg border border-border bg-muted/30 p-4">
                <div className="mb-2 text-[12px] font-semibold uppercase text-muted-foreground">{t('opsErrors.fullError')}</div>
                <pre className="max-h-56 overflow-auto whitespace-pre-wrap break-words font-geist-mono text-[12px] leading-relaxed text-foreground">
                  {selectedLog.error_message || t('opsErrors.noErrorMessage')}
                </pre>
              </div>

              <div className="grid gap-4 md:grid-cols-2">
                <DetailPanel title={t('opsErrors.requestContext')}>
                  <DetailRow label={t('usage.tableEndpoint')} value={formatEndpoint(selectedLog)} mono />
                  <DetailRow label={t('usage.tableModel')} value={selectedLog.effective_model && selectedLog.effective_model !== selectedLog.model ? `${selectedLog.model} → ${selectedLog.effective_model}` : selectedLog.model || '-'} />
                  <DetailRow label={t('usage.tableType')} value={selectedLog.stream ? 'stream' : 'sync'} />
                  <DetailRow label="Service Tier" value={selectedLog.service_tier || '-'} />
                  <DetailRow label="Reasoning" value={selectedLog.reasoning_effort || '-'} />
                </DetailPanel>
                <DetailPanel title={t('opsErrors.runtimeContext')}>
                  <DetailRow label={t('usage.tableAccount')} value={formatAccountDetail(selectedLog)} />
                  <DetailRow label={t('usage.tableApiKey')} value={formatAPIKeyLabel(selectedLog) || t('usage.unknownApiKey')} />
                  <DetailRow label={t('usage.tableDuration')} value={formatDuration(selectedLog.duration_ms)} mono />
                  <DetailRow label={t('usage.tableFirstToken')} value={selectedLog.first_token_ms > 0 ? formatDuration(selectedLog.first_token_ms) : '-'} mono />
                  <DetailRow label="Tokens" value={`${selectedLog.input_tokens} / ${selectedLog.output_tokens} / ${selectedLog.reasoning_tokens}`} mono />
                </DetailPanel>
              </div>

              <div className="flex justify-end gap-2">
                <Button variant="outline" onClick={() => void copyLog(selectedLog)}>
                  <Copy className="size-3.5" />
                  {t('opsErrors.copyJson')}
                </Button>
              </div>
            </DialogContent>
          ) : null}
        </Dialog>
      </>
    </StateShell>
  )
}

function SummaryPill({
  label,
  value,
  icon,
  tone,
}: {
  label: string
  value: string
  icon: ReactNode
  tone: 'danger' | 'warning' | 'info'
}) {
  const toneStyle = {
    danger: 'bg-red-500/10 text-red-600 dark:bg-red-500/20 dark:text-red-300',
    warning: 'bg-amber-500/10 text-amber-600 dark:bg-amber-500/20 dark:text-amber-300',
    info: 'bg-blue-500/10 text-blue-600 dark:bg-blue-500/20 dark:text-blue-300',
  }[tone]

  return (
    <div className="rounded-lg border border-border bg-card/85 px-3 py-3 shadow-sm">
      <div className="flex items-center justify-between gap-3">
        <div className="text-[12px] font-bold uppercase text-muted-foreground">{label}</div>
        <div className={`flex size-8 items-center justify-center rounded-lg ${toneStyle}`}>
          {icon}
        </div>
      </div>
      <div className="mt-3 text-[24px] font-bold leading-none text-foreground">{value}</div>
    </div>
  )
}

function DetailPanel({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div className="rounded-lg border border-border bg-card/75 p-4">
      <div className="mb-3 text-[12px] font-semibold uppercase text-muted-foreground">{title}</div>
      <div className="space-y-2">{children}</div>
    </div>
  )
}

function DetailRow({ label, value, mono = false }: { label: string; value: string; mono?: boolean }) {
  return (
    <div className="grid grid-cols-[120px_minmax(0,1fr)] gap-3 text-sm">
      <span className="text-muted-foreground">{label}</span>
      <span className={`min-w-0 break-words text-foreground ${mono ? 'font-geist-mono text-[13px] tabular-nums' : ''}`}>{value}</span>
    </div>
  )
}

function formatNumber(value: number): string {
  return value.toLocaleString()
}

function formatDuration(value: number): string {
  if (!value || value <= 0) return '-'
  if (value >= 1000) return `${(value / 1000).toFixed(1)}s`
  return `${value}ms`
}

function formatAPIKeyLabel(log: UsageLog): string {
  const name = log.api_key_name?.trim()
  if (name) return name
  const masked = log.api_key_masked?.trim()
  if (!masked) return ''
  if (masked.length <= 8) return masked
  return `${masked.slice(0, 4)}...${masked.slice(-4)}`
}

function formatAccountLabel(log: UsageLog): string {
  // 邮箱优先：身份账号一律显示邮箱，账号名仅作为无邮箱账号（如 relay API-key 账号）的兜底。
  // 避免 AT 导入未命名时的占位名（at-account-N 等）盖过真实邮箱身份。
  const accountEmail = log.account_email?.trim()
  if (accountEmail) return formatCompactEmail(accountEmail)
  const accountName = log.account_name?.trim()
  if (accountName) return accountName
  return log.account_id > 0 ? `ID ${log.account_id}` : '-'
}

function formatAccountTitle(log: UsageLog): string {
  const accountEmail = log.account_email?.trim()
  const accountName = log.account_name?.trim()
  if (accountEmail && accountName && accountEmail !== accountName) {
    return `${accountEmail} · ${accountName}`
  }
  return accountEmail || accountName || (log.account_id > 0 ? `ID ${log.account_id}` : '-')
}

function formatAccountDetail(log: UsageLog): string {
  const title = formatAccountTitle(log)
  if (log.account_id <= 0) return title
  return title === `ID ${log.account_id}` ? title : `${title} · ID ${log.account_id}`
}

function formatEndpoint(log: UsageLog): string {
  const inbound = log.inbound_endpoint || log.endpoint || '-'
  if (log.upstream_endpoint && log.upstream_endpoint !== inbound) {
    return `${inbound} → ${log.upstream_endpoint}`
  }
  return inbound
}

async function copyTextToClipboard(text: string) {
  if (navigator.clipboard?.writeText) {
    try {
      await navigator.clipboard.writeText(text)
      return
    } catch {
      // Fall back for non-secure contexts or browsers that block clipboard writes.
    }
  }

  const textarea = document.createElement('textarea')
  textarea.value = text
  textarea.setAttribute('readonly', 'true')
  textarea.style.position = 'fixed'
  textarea.style.top = '-1000px'
  textarea.style.opacity = '0'
  textarea.style.pointerEvents = 'none'
  document.body.appendChild(textarea)
  textarea.select()
  textarea.setSelectionRange(0, text.length)
  const copied = document.execCommand('copy')
  document.body.removeChild(textarea)
  if (!copied) {
    throw new Error('copy failed')
  }
}

function downloadBlob(blob: Blob, filename: string) {
  const url = URL.createObjectURL(blob)
  const a = document.createElement('a')
  a.href = url
  a.download = filename
  document.body.appendChild(a)
  a.click()
  document.body.removeChild(a)
  URL.revokeObjectURL(url)
}

function buildOpsErrorExportFilename(timeRange: TimeRangeKey, dedupe: boolean, excludeAuthRateLimit: boolean) {
  const timestamp = new Date().toISOString().replace(/[:.]/g, '-')
  const options = [
    dedupe ? 'deduped' : 'raw',
    excludeAuthRateLimit ? 'no-401-429' : 'all-status',
  ].join('-')
  return `ops-errors-${timeRange}-${options}-${timestamp}.json`
}

function classifyStatus(statusCode: number): string {
  if (statusCode === 401) return 'unauthorized'
  if (statusCode === 403) return 'forbidden'
  if (statusCode === 429) return 'rate_limit'
  if (statusCode === 499) return 'client_closed'
  if (statusCode >= 500) return 'server_error'
  if (statusCode >= 400) return 'client_error'
  return 'error'
}

function getStatusBadgeClassName(statusCode: number): string {
  if (statusCode === 401 || statusCode >= 500) {
    return 'border-transparent bg-red-500/14 text-red-600 dark:bg-red-500/20 dark:text-red-300'
  }
  if (statusCode === 429) {
    return 'border-transparent bg-amber-500/14 text-amber-600 dark:bg-amber-500/20 dark:text-amber-300'
  }
  if (statusCode === 499) {
    return 'border-transparent bg-slate-500/14 text-slate-600 dark:bg-slate-500/20 dark:text-slate-300'
  }
  return 'border-transparent bg-amber-500/14 text-amber-600 dark:bg-amber-500/20 dark:text-amber-300'
}
