import { type ReactNode, useCallback, useEffect, useMemo, useState } from 'react'
import { Activity, AlertTriangle, BarChart3, CheckCircle2, ChevronDown, Clock3, Gauge, RefreshCw, ShieldAlert, ShieldCheck, ShieldX, Zap } from 'lucide-react'
import {
  Bar,
  BarChart,
  CartesianGrid,
  Line,
  LineChart,
  ResponsiveContainer,
  Tooltip as RechartsTooltip,
  XAxis,
  YAxis,
} from 'recharts'
import { api } from '../api'
import PageHeader from '../components/PageHeader'
import StateShell from '../components/StateShell'
import Pagination from '../components/Pagination'
import { useDataLoader } from '../hooks/useDataLoader'
import { formatBeijingTime } from '../utils/time'
import { getErrorMessage } from '../utils/error'
import type { AccountRow, CodexAuditReport, HealthResponse, PromptFilterLog, UsageLog } from '../types'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Select } from '@/components/ui/select'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'

type AuditData = {
  report: CodexAuditReport | null
  health: HealthResponse | null
  sessionBleedTotal: number
  accounts: AccountRow[]
}

type Tone = 'ok' | 'warn' | 'bad' | 'neutral'

const chartColors = {
  request: '#2563eb',
  block: '#ef4444',
  cyber: '#f97316',
  error: '#6366f1',
}

const rangeOptions = [
  { label: '最近 30 分钟', value: '0.5' },
  { label: '最近 1 小时', value: '1' },
  { label: '最近 3 小时', value: '3' },
  { label: '最近 6 小时', value: '6' },
  { label: '最近 12 小时', value: '12' },
  { label: '最近 24 小时', value: '24' },
  { label: '最近 3 天', value: '72' },
  { label: '最近 7 天', value: '168' },
]

const refreshOptions = [
  { label: '手动刷新', value: '0' },
  { label: '每 30 秒', value: '30' },
  { label: '每 1 分钟', value: '60' },
  { label: '每 5 分钟', value: '300' },
  { label: '每 15 分钟', value: '900' },
]

const verdictMeta: Record<string, { label: string; title: string; description: string; tone: Tone }> = {
  normal: {
    label: '正常',
    title: '巡检态势稳定',
    description: '当前窗口内未发现明确漏网、审查异常或运行故障。',
    tone: 'ok',
  },
  blocked_activity: {
    label: '存在拦截',
    title: '已拦截风险请求',
    description: '当前窗口内存在命中的安全拦截记录，请关注样本是否符合预期。',
    tone: 'warn',
  },
  suspected_miss: {
    label: '疑似漏网',
    title: '发现疑似漏网信号',
    description: '上游 policy/cyb 错误或高风险样本需要复核，建议优先查看可疑样本。',
    tone: 'bad',
  },
  review_error_risk: {
    label: '审查异常',
    title: '审查链路存在异常',
    description: '模型审核或语义复核出现错误，需检查配置、额度和网络状态。',
    tone: 'bad',
  },
  operational_issue: {
    label: '运行异常',
    title: '服务运行存在异常',
    description: '请求错误率或运行健康状态异常，需优先排查服务与账号池。',
    tone: 'bad',
  },
}

const chartTooltipStyle = {
  background: 'hsl(var(--popover))',
  border: '1px solid hsl(var(--border))',
  borderRadius: 8,
  boxShadow: '0 14px 40px rgba(15, 23, 42, 0.12)',
  color: 'hsl(var(--popover-foreground))',
}

function loadStoredNumber(key: string, fallback: number) {
  if (typeof window === 'undefined') return fallback
  const raw = window.localStorage.getItem(key)
  const parsed = raw ? Number(raw) : NaN
  return Number.isFinite(parsed) && parsed >= 0 ? parsed : fallback
}

export default function CodexAudit() {
  const [rangeHours, setRangeHours] = useState(() => loadStoredNumber('codex_audit_range_hours', 0.5))
  const [refreshSeconds, setRefreshSeconds] = useState(() => loadStoredNumber('codex_audit_refresh_seconds', 60))

  const loadData = useCallback(async (): Promise<AuditData> => {
    const bucketMinutes = rangeHours <= 1 ? 5 : rangeHours <= 6 ? 10 : rangeHours <= 24 ? 30 : 120
    const [report, health, bleed, accountsResp] = await Promise.all([
      api.getCodexAuditReport({ hours: rangeHours, bucketMinutes, limit: 30 }),
      api.getHealth(),
      api.getPromptFilterLogs({ source: 'session_bleed', pageSize: 1 }),
      api.getAccounts(),
    ])
    return { report, health, sessionBleedTotal: bleed.total ?? 0, accounts: accountsResp.accounts ?? [] }
  }, [rangeHours])

  const { data, loading, error, reload } = useDataLoader<AuditData>({
    initialData: { report: null, health: null, sessionBleedTotal: 0, accounts: [] },
    load: loadData,
  })

  useEffect(() => {
    window.localStorage.setItem('codex_audit_range_hours', String(rangeHours))
  }, [rangeHours])

  useEffect(() => {
    window.localStorage.setItem('codex_audit_refresh_seconds', String(refreshSeconds))
    if (!refreshSeconds) return
    const timer = window.setInterval(() => void reload(), refreshSeconds * 1000)
    return () => window.clearInterval(timer)
  }, [refreshSeconds, reload])

  const report = data.report
  const health = data.health
  const sessionBleedTotal = data.sessionBleedTotal ?? 0
  const accounts = data.accounts ?? []
  const meta = verdictMeta[report?.verdict || 'normal'] || {
    label: report?.verdict || '-',
    title: '巡检状态待确认',
    description: '当前结论来自后台聚合结果，请结合样本和趋势判断。',
    tone: 'warn' as Tone,
  }

  const timeline = useMemo(() => (report?.timeline || []).map((point) => ({
    ...point,
    label: formatShortTime(point.bucket),
  })), [report?.timeline])

  const errorRate = report?.usage.requests ? (report.usage.errors_4xx + report.usage.errors_5xx) / report.usage.requests : 0
  const blockRate = report?.usage.requests ? report.summary.prompt_blocks / report.usage.requests : 0
  const firstTokenTone: Tone = (report?.usage.first_token_p95_ms || 0) >= 3000 ? 'bad' : (report?.usage.first_token_p95_ms || 0) >= 1500 ? 'warn' : 'ok'
  const errorTone: Tone = errorRate >= 0.05 ? 'bad' : errorRate > 0 ? 'warn' : 'ok'

  return (
    <>
      <PageHeader
        title="审计"
        description="集中查看误伤、漏网、cyb、探针、首字延迟和运行健康。"
        actions={
          <div className="grid w-full min-w-0 gap-2 sm:w-auto sm:grid-cols-[164px_164px_auto] sm:items-end">
            <HeaderControl label="巡检范围">
              <Select value={String(rangeHours)} onValueChange={(value) => setRangeHours(Number(value))} options={rangeOptions} triggerClassName="h-10 rounded-lg text-sm" />
            </HeaderControl>
            <HeaderControl label="自动刷新">
              <Select value={String(refreshSeconds)} onValueChange={(value) => setRefreshSeconds(Number(value))} options={refreshOptions} triggerClassName="h-10 rounded-lg text-sm" />
            </HeaderControl>
            <Button variant="outline" className="h-10 w-full sm:w-auto" onClick={() => void reload()} disabled={loading}>
              <RefreshCw className={loading ? 'size-3.5 animate-spin' : 'size-3.5'} />
              刷新
            </Button>
          </div>
        }
      />

      <StateShell loading={loading && !report} error={error} isEmpty={!loading && !report} onRetry={() => void reload()} emptyTitle="暂无巡检数据">
        {report ? (
          <div className="w-full min-w-0 max-w-full space-y-4">
            <Card className="w-full min-w-0 overflow-hidden border-border/70 bg-gradient-to-br from-background via-background to-muted/40 shadow-sm">
              <CardContent className="min-w-0 p-0">
                <div className="grid min-w-0 grid-cols-1 lg:grid-cols-[minmax(0,1fr)_minmax(520px,2fr)]">
                  <div className="min-w-0 border-b border-border/70 p-4 sm:p-5 lg:border-b-0 lg:border-r">
                    <div className="flex items-start justify-between gap-4">
                      <div className={`flex size-12 items-center justify-center rounded-lg ${toneIconClass(meta.tone)} [&>svg]:size-6`}>
                        {meta.tone === 'ok' ? <ShieldCheck /> : meta.tone === 'warn' ? <ShieldAlert /> : <ShieldX />}
                      </div>
                      <Badge className={verdictClass(meta.tone)}>{meta.label}</Badge>
                    </div>
                    <div className="mt-5">
                      <h2 className="text-xl font-semibold tracking-tight text-foreground">{meta.title}</h2>
                      <p className="mt-2 max-w-xl text-sm leading-6 text-muted-foreground">{meta.description}</p>
                    </div>
                    <div className="mt-5 grid gap-2 text-sm">
                      <WindowLine label="巡检窗口" value={`${formatBeijingTime(report.window_start)} 至 ${formatBeijingTime(report.window_end)}`} />
                      <WindowLine label="生成时间" value={formatBeijingTime(report.generated_at)} />
                      <WindowLine label="运行健康" value={health?.status || '-'} tone={health?.status === 'ok' ? 'ok' : 'warn'} />
                    </div>
                  </div>

                  <div className="grid min-w-0 grid-cols-1 gap-px bg-border/70 sm:grid-cols-2 xl:grid-cols-4">
                    <SignalTile label="请求总量" value={formatNumber(report.usage.requests)} detail={`错误率 ${formatPercent(errorRate)}`} icon={<Activity />} tone={errorTone} />
                    <SignalTile label="拦截命中" value={formatNumber(report.summary.prompt_blocks)} detail={`拦截率 ${formatPercent(blockRate)}`} icon={<ShieldX />} tone={report.summary.prompt_blocks ? 'warn' : 'ok'} />
                    <SignalTile label="疑似漏网" value={formatNumber(report.summary.upstream_cyber_policy)} detail={report.summary.upstream_cyber_policy ? '上游 cyb / policy 信号' : report.last_cyber_policy_at ? `最近一次 cyb ${formatBeijingTime(report.last_cyber_policy_at)}` : '上游 cyb / policy 信号'} icon={<AlertTriangle />} tone={report.summary.upstream_cyber_policy ? 'bad' : report.last_cyber_policy_at ? 'warn' : 'ok'} />
                    <SignalTile label="会话串扰" value={formatNumber(sessionBleedTotal)} detail={sessionBleedTotal ? '⚠️ 立即排查！' : '被动监测中·真实流量'} icon={<ShieldAlert />} tone={sessionBleedTotal ? 'bad' : 'ok'} />
                    <SignalTile label="审查异常" value={formatNumber(report.summary.review_errors)} detail="审核/语义复核错误" icon={<ShieldAlert />} tone={report.summary.review_errors ? 'bad' : 'ok'} />
                    <SignalTile label="首字 P95" value={formatMS(report.usage.first_token_p95_ms)} detail={`${formatNumber(report.usage.first_token_samples)} 个样本`} icon={<Clock3 />} tone={firstTokenTone} />
                    <SignalTile label="WS 占比" value={formatPercent(report.usage.websocket_ratio || 0)} detail={`${formatNumber(report.usage.websocket_requests)} 个 WS 请求`} icon={<Zap />} tone={(report.usage.websocket_ratio || 0) >= 0.85 ? 'ok' : 'warn'} />
                    <SignalTile label="语义分歧" value={formatNumber(report.summary.semantic_disagreements)} detail={`拦截 ${formatNumber(report.summary.semantic_disagreement_blocks)}`} icon={<Gauge />} tone={report.summary.semantic_disagreement_blocks ? 'warn' : 'neutral'} />
                    <SignalTile label="高频探针" value={formatNumber(report.summary.probe_high_frequency || 0)} detail={`已短路 ${formatNumber(report.summary.probe_short_circuits)} / 观测 ${formatNumber(report.summary.probe_observed)}`} icon={<CheckCircle2 />} tone={report.summary.probe_high_frequency ? 'warn' : 'ok'} className="sm:col-span-2 xl:col-span-2" />
                    <AccountPoolTile accounts={accounts} />
                  </div>
                </div>
              </CardContent>
            </Card>

            <CyberMissPanel />
            <SessionBleedPanel />

            <div className="grid min-w-0 gap-4 xl:grid-cols-[minmax(0,1.45fr)_minmax(360px,0.85fr)]">
              <ChartPanel title="请求与风险趋势" description="按时间窗口聚合请求、拦截、上游 cyb 和 5xx。">
                <ResponsiveContainer width="100%" height={286}>
                  <LineChart data={timeline} margin={{ top: 12, right: 18, bottom: 0, left: 0 }}>
                    <CartesianGrid strokeDasharray="4 4" stroke="hsl(var(--border))" vertical={false} />
                    <XAxis dataKey="label" tick={{ fontSize: 11 }} stroke="hsl(var(--muted-foreground))" tickLine={false} axisLine={false} />
                    <YAxis tick={{ fontSize: 11 }} stroke="hsl(var(--muted-foreground))" tickLine={false} axisLine={false} />
                    <RechartsTooltip contentStyle={chartTooltipStyle} />
                    <Line type="monotone" dataKey="requests" name="请求" stroke={chartColors.request} strokeWidth={2.5} dot={false} activeDot={{ r: 4 }} />
                    <Line type="monotone" dataKey="prompt_blocks" name="拦截" stroke={chartColors.block} strokeWidth={2} dot={false} activeDot={{ r: 4 }} />
                    <Line type="monotone" dataKey="upstream_cyber_policy" name="上游 cyb" stroke={chartColors.cyber} strokeWidth={2} dot={false} activeDot={{ r: 4 }} />
                    <Line type="monotone" dataKey="errors_5xx" name="5xx" stroke={chartColors.error} strokeWidth={2} dot={false} activeDot={{ r: 4 }} />
                  </LineChart>
                </ResponsiveContainer>
              </ChartPanel>

              <ChartPanel title="模型请求分布" description="按有效模型统计请求量，辅助定位风险集中点。">
                <ResponsiveContainer width="100%" height={286}>
                  <BarChart data={(report.models || []).slice(0, 10)} layout="vertical" margin={{ top: 12, right: 18, bottom: 0, left: 4 }}>
                    <CartesianGrid strokeDasharray="4 4" stroke="hsl(var(--border))" horizontal={false} />
                    <XAxis type="number" tick={{ fontSize: 11 }} stroke="hsl(var(--muted-foreground))" tickLine={false} axisLine={false} />
                    <YAxis type="category" dataKey="model" width={128} tick={{ fontSize: 11 }} stroke="hsl(var(--muted-foreground))" tickLine={false} axisLine={false} />
                    <RechartsTooltip contentStyle={chartTooltipStyle} />
                    <Bar dataKey="requests" name="请求" fill={chartColors.request} radius={[0, 6, 6, 0]} />
                  </BarChart>
                </ResponsiveContainer>
              </ChartPanel>
            </div>

            <div className="grid min-w-0 gap-4 xl:grid-cols-2">
              <Panel title="Prompt Filter 聚合" description="按来源、动作、审查模型和分数区间聚合。">
                <SimpleTable
                  columns={['来源', '动作', '审查模型', '数量', '分数', '异常']}
                  rows={(report.prompt_filter || []).map((row) => [
                    row.source || '-',
                    row.action || '-',
                    row.review_model || '-',
                    formatNumber(row.count),
                    `${row.min_score}-${row.max_score}`,
                    formatNumber(row.review_errors),
                  ])}
                  empty="暂无 Prompt Filter 日志"
                />
              </Panel>

              <ProbePanel report={report} />
            </div>

            <Panel title="可疑样本" description="高分放行、语义分歧、上游 cyb 等需要人工复核的样本。">
              <PromptSampleTable rows={report.suspicious_samples || []} />
            </Panel>

            <div className="grid min-w-0 gap-4 xl:grid-cols-2">
              <Panel title="Policy-like 错误" description="从请求日志中提取 policy、cyber、violat、safety 相关错误。">
                <UsageSampleTable rows={report.policy_errors || []} empty="暂无 policy-like 错误" />
              </Panel>
              <Panel title="首字慢请求" description="按首字时间倒序列出最慢样本，用于观察 WS 和上游延迟。">
                <UsageSampleTable rows={report.slow_requests || []} empty="暂无慢请求样本" showFirstToken />
              </Panel>
            </div>

          </div>
        ) : null}
      </StateShell>
    </>
  )
}

function CyberMissPanel() {
  const [logs, setLogs] = useState<PromptFilterLog[]>([])
  const [total, setTotal] = useState(0)
  const [page, setPage] = useState(1)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(async () => {
    setLoading(true)
    setError(null)
    try {
      const res = await api.getPromptFilterLogs({ source: 'upstream_cyber_policy', page, pageSize: AUDIT_PAGE_SIZE })
      setLogs(res.logs ?? [])
      setTotal(res.total ?? 0)
    } catch (err) {
      setError(getErrorMessage(err))
    } finally {
      setLoading(false)
    }
  }, [page])

  useEffect(() => {
    void load()
  }, [load])

  const totalPages = Math.max(1, Math.ceil(total / AUDIT_PAGE_SIZE))

  return (
    <Card className="w-full min-w-0 overflow-hidden border-amber-500/30 bg-amber-500/[0.05] shadow-sm">
      <CardContent className="min-w-0 p-4 sm:p-5">
        <div className="mb-4 flex items-start justify-between gap-3 max-sm:flex-col">
          <div className="flex items-start gap-3">
            <div className="mt-0.5 flex size-9 shrink-0 items-center justify-center rounded-xl bg-amber-500/15 text-amber-600 dark:text-amber-400">
              <ShieldAlert className="size-5" />
            </div>
            <div className="min-w-0">
              <h3 className="text-sm font-semibold text-foreground">漏放案卷 · 放行却被上游拦下</h3>
              <p className="mt-1 max-w-2xl text-xs leading-relaxed text-muted-foreground">
                本地过滤与语义判官都放行、却被上游 cyber_policy 拦截的请求。每一条都是账号池的一次违规风险，也是复盘检测盲区、积累知识库的素材。已记录脱敏原始请求与上游原因，点开看完整案卷。
              </p>
            </div>
          </div>
          <div className="flex shrink-0 items-center gap-3 max-sm:w-full max-sm:justify-between">
            <div className="text-right">
              <div className="text-2xl font-bold tabular-nums text-amber-600 dark:text-amber-400">{total}</div>
              <div className="text-[11px] leading-tight text-muted-foreground">累计漏放</div>
            </div>
            <Button variant="outline" onClick={() => void load()} disabled={loading}>
              <RefreshCw className={loading ? 'size-3.5 animate-spin' : 'size-3.5'} />
              刷新
            </Button>
          </div>
        </div>

        {error ? (
          <div className="rounded-lg border border-destructive/40 bg-destructive/10 p-3 text-xs text-destructive">{error}</div>
        ) : total === 0 ? (
          <div className="rounded-lg border border-border/60 bg-background/60 p-6 text-center text-xs text-muted-foreground">
            {loading ? '加载中…' : '暂无漏放记录 —— 本地过滤与语义判官拦住了全部触发上游 cyber 策略的请求'}
          </div>
        ) : (
          <>
            <div className="space-y-2">
              {logs.map((log) => (
                <CyberMissRow key={log.id} log={log} />
              ))}
            </div>
            {total > AUDIT_PAGE_SIZE ? (
              <Pagination page={page} totalPages={totalPages} onPageChange={setPage} totalItems={total} pageSize={AUDIT_PAGE_SIZE} />
            ) : null}
          </>
        )}
      </CardContent>
    </Card>
  )
}

function SessionBleedPanel() {
  const [logs, setLogs] = useState<PromptFilterLog[]>([])
  const [total, setTotal] = useState(0)
  const [page, setPage] = useState(1)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(async () => {
    setLoading(true)
    setError(null)
    try {
      const res = await api.getPromptFilterLogs({ source: 'session_bleed', page, pageSize: AUDIT_PAGE_SIZE })
      setLogs(res.logs ?? [])
      setTotal(res.total ?? 0)
    } catch (err) {
      setError(getErrorMessage(err))
    } finally {
      setLoading(false)
    }
  }, [page])

  useEffect(() => {
    void load()
  }, [load])

  const totalPages = Math.max(1, Math.ceil(total / AUDIT_PAGE_SIZE))
  const clean = total === 0

  return (
    <Card className={clean ? 'w-full min-w-0 overflow-hidden border-emerald-500/30 bg-emerald-500/[0.05] shadow-sm' : 'w-full min-w-0 overflow-hidden border-red-500/50 bg-red-500/[0.08] shadow-sm'}>
      <CardContent className="min-w-0 p-4 sm:p-5">
        <div className="mb-4 flex items-start justify-between gap-3 max-sm:flex-col">
          <div className="flex items-start gap-3">
            <div className={clean ? 'mt-0.5 flex size-9 shrink-0 items-center justify-center rounded-xl bg-emerald-500/15 text-emerald-600 dark:text-emerald-400' : 'mt-0.5 flex size-9 shrink-0 items-center justify-center rounded-xl bg-red-500/15 text-red-600 dark:text-red-400'}>
              <ShieldAlert className="size-5" />
            </div>
            <div className="min-w-0">
              <h3 className="text-sm font-semibold text-foreground">会话串扰监测 · 被动实时检测</h3>
              <p className="mt-1 max-w-2xl text-xs leading-relaxed text-muted-foreground">
                在真实流量的上游 WS 读流上校验 response_id 一致性：一个请求流本应自始至终同一个 response_id，出现第二个不同的即别的请求的帧串入本流（跨用户串扰）。零探针、零误报、实时上报，无需人工发检测。
              </p>
            </div>
          </div>
          <div className="flex shrink-0 items-center gap-3 max-sm:w-full max-sm:justify-between">
            <div className="text-right">
              <div className={clean ? 'text-2xl font-bold tabular-nums text-emerald-600 dark:text-emerald-400' : 'text-2xl font-bold tabular-nums text-red-600 dark:text-red-400'}>{total}</div>
              <div className="text-[11px] leading-tight text-muted-foreground">检测到的串扰</div>
            </div>
            <Button variant="outline" onClick={() => void load()} disabled={loading}>
              <RefreshCw className={loading ? 'size-3.5 animate-spin' : 'size-3.5'} />
              刷新
            </Button>
          </div>
        </div>

        {error ? (
          <div className="rounded-lg border border-destructive/40 bg-destructive/10 p-3 text-xs text-destructive">{error}</div>
        ) : clean ? (
          <div className="rounded-lg border border-emerald-500/30 bg-emerald-500/[0.06] p-6 text-center text-xs text-emerald-700 dark:text-emerald-400">
            {loading ? '加载中…' : '✓ 被动监测运行中，未检测到任何会话串扰'}
          </div>
        ) : (
          <>
            <div className="mb-3 rounded-lg border border-red-500/40 bg-red-500/10 p-3 text-xs font-medium text-red-700 dark:text-red-400">
              ⚠️ 检测到 {total} 起会话串扰！请立即排查（多半是 stateless 会话身份被重新引入，或连接复用异常）。
            </div>
            <div className="space-y-2">
              {logs.map((log) => (
                <CyberMissRow key={log.id} log={log} />
              ))}
            </div>
            {total > AUDIT_PAGE_SIZE ? (
              <Pagination page={page} totalPages={totalPages} onPageChange={setPage} totalItems={total} pageSize={AUDIT_PAGE_SIZE} />
            ) : null}
          </>
        )}
      </CardContent>
    </Card>
  )
}

function CyberMissRow({ log }: { log: PromptFilterLog }) {
  const [open, setOpen] = useState(false)
  const full = (log.full_text || '').trim()
  const preview = (log.text_preview || '').trim()
  return (
    <div className="min-w-0 rounded-lg border border-border/60 bg-background/70">
      <button
        type="button"
        onClick={() => setOpen((v) => !v)}
        className="flex w-full min-w-0 items-center gap-2 px-3 py-2.5 text-left sm:gap-3"
      >
        <Badge className="shrink-0 border-amber-500/30 bg-amber-500/10 text-amber-700 dark:text-amber-300">cyber_policy</Badge>
        <span className="hidden shrink-0 whitespace-nowrap text-[11px] text-muted-foreground sm:inline">{formatBeijingTime(log.created_at)}</span>
        <span className="hidden shrink-0 whitespace-nowrap text-[11px] text-muted-foreground md:inline">{log.endpoint}</span>
        <span className="hidden shrink-0 whitespace-nowrap text-[11px] text-muted-foreground md:inline">{log.model || '-'}</span>
        <span className="min-w-0 flex-1 truncate text-xs text-foreground">{preview || '（改动前的旧记录，未留原始请求）'}</span>
        <ChevronDown className={`size-4 shrink-0 text-muted-foreground transition-transform ${open ? 'rotate-180' : ''}`} />
      </button>
      {open ? (
        <div className="border-t border-border/60 px-3 py-3">
          <div className="mb-2 flex flex-wrap gap-x-4 gap-y-1 text-[11px] text-muted-foreground sm:hidden">
            <span>{formatBeijingTime(log.created_at)}</span>
            <span>{log.endpoint}</span>
            <span>{log.model || '-'}</span>
          </div>
          <pre className="max-h-96 overflow-auto whitespace-pre-wrap break-words rounded-md bg-muted/40 p-3 text-[12px] leading-5 text-foreground">{full || '（无详情）'}</pre>
        </div>
      ) : null}
    </div>
  )
}

function HeaderControl({ label, children }: { label: string; children: ReactNode }) {
  return (
    <label className="grid w-full min-w-0 gap-1.5 sm:w-[164px]">
      <span className="text-xs font-medium text-muted-foreground">{label}</span>
      {children}
    </label>
  )
}

function AccountPoolTile({ accounts }: { accounts: AccountRow[] }) {
  const active = accounts.filter((a) => a.status === 'active' && a.enabled !== false && !a.locked)
  const healthy = active.length > 0 && active.length === accounts.length
  return (
    <div className="min-w-0 bg-background/95 p-4 sm:col-span-2 xl:col-span-2">
      <div className="mb-3 flex items-center justify-between gap-3">
        <div className="flex items-center gap-2">
          <div className="flex size-7 items-center justify-center rounded-lg bg-primary/10 text-primary [&>svg]:size-4"><BarChart3 /></div>
          <span className="text-xs font-medium text-muted-foreground">账号池 · 上游 Pro 号 · 7d 配额 / 并发</span>
        </div>
        <div className={`inline-flex items-center gap-1.5 rounded-full px-2 py-0.5 text-[11px] font-medium ${healthy ? 'bg-emerald-500/10 text-emerald-600 dark:text-emerald-400' : 'bg-amber-500/10 text-amber-600 dark:text-amber-400'}`}>
          <span className={`size-1.5 rounded-full ${healthy ? 'bg-emerald-500' : 'bg-amber-500'}`} />
          {active.length}/{accounts.length} 活跃
        </div>
      </div>
      <div className="space-y-1.5">
        {accounts.slice(0, 6).map((a) => {
          const pct = Math.round(a.usage_percent_7d ?? 0)
          const busy = a.active_requests ?? 0
          const cap = a.base_concurrency_effective ?? 5
          const isActive = a.status === 'active' && a.enabled !== false && !a.locked
          const barColor = pct >= 90 ? 'bg-red-500' : pct >= 70 ? 'bg-amber-500' : 'bg-emerald-500'
          return (
            <div key={a.id} className="flex items-center gap-2 text-xs">
              <span className={`size-1.5 shrink-0 rounded-full ${isActive ? 'bg-emerald-500' : 'bg-muted-foreground/40'}`} />
              <span className="min-w-0 flex-1 truncate text-foreground">{a.email || a.name || `#${a.id}`}</span>
              <span className="hidden shrink-0 rounded bg-muted px-1.5 py-0.5 text-[10px] uppercase text-muted-foreground sm:inline">{a.plan_type || '-'}</span>
              <div className="hidden h-1.5 w-16 shrink-0 overflow-hidden rounded-full bg-muted sm:block">
                <div className={`h-full ${barColor}`} style={{ width: `${Math.min(100, Math.max(0, pct))}%` }} />
              </div>
              <span className="w-14 shrink-0 text-right tabular-nums text-muted-foreground">7d {pct}%</span>
              <span className="w-9 shrink-0 text-right tabular-nums text-muted-foreground">{busy}/{cap}</span>
            </div>
          )
        })}
        {accounts.length === 0 ? <div className="py-3 text-center text-xs text-muted-foreground">加载中…</div> : null}
      </div>
    </div>
  )
}

function SignalTile({ label, value, detail, icon, tone, className }: { label: string; value: ReactNode; detail: string; icon: ReactNode; tone: Tone; className?: string }) {
  return (
    <div className={`min-w-0 bg-background/95 p-4 ${className ?? ''}`}>
      <div className="flex items-center justify-between gap-3">
        <div className="min-w-0">
          <div className="text-xs font-medium text-muted-foreground">{label}</div>
          <div className="mt-2 truncate text-2xl font-semibold tracking-tight text-foreground">{value}</div>
        </div>
        <div className={`flex size-9 shrink-0 items-center justify-center rounded-lg ${toneIconClass(tone)} [&>svg]:size-4`}>
          {icon}
        </div>
      </div>
      <div className="mt-3 truncate text-xs text-muted-foreground">{detail}</div>
    </div>
  )
}

function WindowLine({ label, value, tone = 'neutral' }: { label: string; value: ReactNode; tone?: Tone }) {
  return (
    <div className="flex min-w-0 flex-col gap-1 rounded-md border border-border/70 bg-background/70 px-3 py-2 sm:flex-row sm:items-center sm:justify-between sm:gap-4">
      <span className="shrink-0 text-xs text-muted-foreground">{label}</span>
      <span className={`min-w-0 truncate text-xs font-medium sm:text-right ${toneTextClass(tone)}`}>{value}</span>
    </div>
  )
}

function ChartPanel({ title, description, children }: { title: string; description: string; children: ReactNode }) {
  return (
    <Card className="min-w-0 overflow-hidden border-border/70 shadow-sm">
      <CardContent className="min-w-0 p-4 sm:p-5">
        <div className="mb-4 flex min-w-0 items-start justify-between gap-4">
          <div className="min-w-0">
            <div className="flex items-center gap-2 text-base font-semibold text-foreground">
              <BarChart3 className="size-4 text-primary" />
              {title}
            </div>
            <p className="mt-1 text-sm text-muted-foreground">{description}</p>
          </div>
        </div>
        {children}
      </CardContent>
    </Card>
  )
}

function Panel({ title, description, children }: { title: string; description?: string; children: ReactNode }) {
  return (
    <Card className="min-w-0 overflow-hidden border-border/70 shadow-sm">
      <CardContent className="min-w-0 p-4 sm:p-5">
        <div className="mb-4 min-w-0">
          <h2 className="text-base font-semibold text-foreground">{title}</h2>
          {description ? <p className="mt-1 text-sm text-muted-foreground">{description}</p> : null}
        </div>
        {children}
      </CardContent>
    </Card>
  )
}

function ProbePanel({ report }: { report: CodexAuditReport }) {
  const highFrequency = (report.probe_high_frequency?.length ? report.probe_high_frequency : report.probe_short_circuits) || []
  const observed = report.probe_observed || []
  const [tab, setTab] = useState<'high_frequency' | 'observed'>('high_frequency')
  const tabs = [
    { key: 'high_frequency', label: '高频（已短路）', count: highFrequency.length },
    { key: 'observed', label: '只观测未短路', count: observed.length },
  ] as const
  return (
    <Panel title="探针行为" description="观察高频探针是否已被本地短路，以及是否仍有只观测未短路的探针。">
      <div className="mb-3 inline-flex items-center gap-0.5 rounded-lg border border-border bg-muted/30 p-0.5">
        {tabs.map(({ key, label, count }) => (
          <button
            key={key}
            type="button"
            onClick={() => setTab(key)}
            className={`inline-flex items-center gap-1.5 rounded-md px-3 py-1.5 text-[13px] font-semibold transition-colors ${
              tab === key
                ? 'bg-background text-foreground shadow-sm'
                : 'text-muted-foreground hover:text-foreground'
            }`}
          >
            {label}
            <span className={`rounded-full px-1.5 text-[11px] font-medium leading-4 ${tab === key ? 'bg-muted text-foreground' : 'bg-muted/60 text-muted-foreground'}`}>
              {formatNumber(count)}
            </span>
          </button>
        ))}
      </div>
      {/* key 用于在切换 tab 时重置 usePaged 的页码 */}
      <ProbeTable key={tab} rows={tab === 'high_frequency' ? highFrequency : observed} />
    </Panel>
  )
}

const AUDIT_PAGE_SIZE = 10

// usePaged 对已加载的记录做客户端分页（巡检报表每类样本上限 30，分页即可，无需改后端）。
function usePaged<T>(rows: T[], pageSize = AUDIT_PAGE_SIZE) {
  const [page, setPage] = useState(1)
  const total = rows.length
  const totalPages = Math.max(1, Math.ceil(total / pageSize))
  const currentPage = Math.min(Math.max(page, 1), totalPages)
  const pageRows = useMemo(
    () => rows.slice((currentPage - 1) * pageSize, currentPage * pageSize),
    [rows, currentPage, pageSize],
  )
  return { pageRows, page: currentPage, totalPages, setPage, total, pageSize }
}

function SimpleTable({ columns, rows, empty }: { columns: string[]; rows: string[][]; empty: string }) {
  const { pageRows, page, totalPages, setPage, total } = usePaged(rows)
  if (!rows.length) {
    return <EmptyState>{empty}</EmptyState>
  }
  return (
    <>
      <div className="grid gap-2 sm:hidden">
        {pageRows.map((row, rowIndex) => (
          <MobileTableCard key={rowIndex} columns={columns} row={row} />
        ))}
      </div>
      <div className="hidden w-full max-w-full overflow-x-auto rounded-lg border border-border/70 sm:block">
        <Table className="min-w-[560px]">
          <TableHeader className="bg-muted/40">
            <TableRow>{columns.map((column) => <TableHead key={column} className="text-xs font-semibold text-muted-foreground">{column}</TableHead>)}</TableRow>
          </TableHeader>
          <TableBody>
            {pageRows.map((row, rowIndex) => (
              <TableRow key={rowIndex} className="hover:bg-muted/30">
                {row.map((cell, cellIndex) => <TableCell key={cellIndex} className="text-[13px]">{cell}</TableCell>)}
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </div>
      {total > AUDIT_PAGE_SIZE ? (
        <Pagination page={page} totalPages={totalPages} onPageChange={setPage} totalItems={total} pageSize={AUDIT_PAGE_SIZE} />
      ) : null}
    </>
  )
}

function MobileTableCard({ columns, row }: { columns: string[]; row: string[] }) {
  return (
    <div className="min-w-0 rounded-lg border border-border/60 bg-muted/20 p-3">
      <div className="grid grid-cols-2 gap-2">
        {columns.map((column, index) => (
          <MobileField key={column} label={column} value={row[index] || '-'} />
        ))}
      </div>
    </div>
  )
}

function MobileField({ label, value, wide = false, wrap = false }: { label: string; value: ReactNode; wide?: boolean; wrap?: boolean }) {
  const title = typeof value === 'string' ? value : undefined
  return (
    <div className={`min-w-0 ${wide ? 'col-span-2' : ''}`}>
      <div className="text-[11px] leading-none text-muted-foreground">{label}</div>
      <div className={`mt-1 text-xs font-medium text-foreground ${wrap ? 'line-clamp-3 break-words' : 'truncate'}`} title={title}>
        {value}
      </div>
    </div>
  )
}

const PROBE_PAGE_SIZE = 5

function ProbeTable({ rows }: { rows: CodexAuditReport['probe_observed'] }) {
  const { pageRows, page, totalPages, setPage, total, pageSize } = usePaged(rows, PROBE_PAGE_SIZE)
  if (!rows.length) {
    return <EmptyState compact>暂无探针记录</EmptyState>
  }
  return (
    <>
      <div className="grid gap-2">
      {pageRows.map((row, index) => {
        const apiKeyName = row.api_key_name || row.api_key_masked || String(row.api_key_id || '-')
        return (
          <div
            key={`${row.api_key_id}-${row.endpoint}-${row.model}-${row.signature}-${row.stream}-${index}`}
            className="min-w-0 rounded-lg border border-border/60 bg-muted/20 p-3"
          >
            <div className="flex min-w-0 items-start justify-between gap-3">
              <div className="min-w-0">
                <div className="truncate text-sm font-medium text-foreground" title={apiKeyName}>
                  {apiKeyName}
                </div>
                <div className="mt-1 truncate text-xs text-muted-foreground" title={`${row.model || '-'} · ${row.endpoint || '-'}`}>
                  {row.model || '-'} · {row.endpoint || '-'}
                </div>
              </div>
              <Badge variant="outline" className="shrink-0 bg-background/70 text-xs">
                {formatNumber(row.count)} 次
              </Badge>
            </div>

            <div className="mt-3 grid grid-cols-2 gap-2 sm:grid-cols-4">
              <ProbeMeta label="签名" value={row.signature || '-'} wide wrap />
              <ProbeMeta label="流式" value={row.stream ? '是' : '否'} />
              <ProbeMeta label="频率" value={formatRate(row.rate_per_minute || 0)} />
              <ProbeMeta label="平均间隔" value={formatDuration(row.average_interval_seconds || 0)} />
              <ProbeMeta label="跨度" value={formatDuration(row.span_seconds || 0)} />
            </div>
          </div>
        )
      })}
      </div>
      {total > pageSize ? (
        <Pagination page={page} totalPages={totalPages} onPageChange={setPage} totalItems={total} pageSize={pageSize} />
      ) : null}
    </>
  )
}

function ProbeMeta({ label, value, wide = false, wrap = false }: { label: string; value: string; wide?: boolean; wrap?: boolean }) {
  return (
    <div className={`min-w-0 rounded-md bg-background/70 px-2.5 py-2 ring-1 ring-border/45 ${wide ? 'col-span-2' : ''}`}>
      <div className="text-[11px] leading-none text-muted-foreground">{label}</div>
      <div
        className={`mt-1 text-xs font-medium text-foreground ${wrap ? 'line-clamp-2 break-all' : 'truncate'}`}
        title={value}
      >
        {value}
      </div>
    </div>
  )
}

function PromptSampleTable({ rows }: { rows: PromptFilterLog[] }) {
  const { pageRows, page, totalPages, setPage, total } = usePaged(rows)
  if (!rows.length) {
    return <EmptyState>暂无可疑样本</EmptyState>
  }
  return (
    <>
      <div className="grid gap-2 sm:hidden">
        {pageRows.map((row) => (
          <div key={row.id} className="min-w-0 rounded-lg border border-border/60 bg-muted/20 p-3">
            <div className="flex items-start justify-between gap-3">
              <div className="min-w-0">
                <div className="text-xs font-medium text-foreground">{formatBeijingTime(row.created_at)}</div>
                <div className="mt-1 truncate text-xs text-muted-foreground">{row.source || '-'}</div>
              </div>
              <Badge className={`${actionClass(row.action)} shrink-0`}>{row.action || '-'}</Badge>
            </div>
            <div className="mt-3 grid grid-cols-2 gap-2">
              <MobileField label="分数" value={String(row.score)} />
              <MobileField label="审查" value={row.review_model ? `${row.review_model} / ${row.review_flagged ? 'flagged' : 'clear'}` : '-'} />
              <MobileField label="预览" value={row.text_preview || row.review_error || '-'} wide wrap />
            </div>
          </div>
        ))}
      </div>
      <div className="hidden w-full max-w-full overflow-x-auto rounded-lg border border-border/70 sm:block">
        <Table className="min-w-[760px]">
          <TableHeader className="bg-muted/40">
            <TableRow>
              <TableHead className="text-xs font-semibold text-muted-foreground">时间</TableHead>
              <TableHead className="text-xs font-semibold text-muted-foreground">来源</TableHead>
              <TableHead className="text-xs font-semibold text-muted-foreground">动作</TableHead>
              <TableHead className="text-xs font-semibold text-muted-foreground">分数</TableHead>
              <TableHead className="text-xs font-semibold text-muted-foreground">审查</TableHead>
              <TableHead className="text-xs font-semibold text-muted-foreground">预览</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {pageRows.map((row) => (
              <TableRow key={row.id} className="hover:bg-muted/30">
                <TableCell className="whitespace-nowrap text-[12px]">{formatBeijingTime(row.created_at)}</TableCell>
                <TableCell className="text-[12px]">{row.source || '-'}</TableCell>
                <TableCell><Badge className={actionClass(row.action)}>{row.action || '-'}</Badge></TableCell>
                <TableCell className="font-medium">{row.score}</TableCell>
                <TableCell className="text-[12px]">{row.review_model ? `${row.review_model} / ${row.review_flagged ? 'flagged' : 'clear'}` : '-'}</TableCell>
                <TableCell className="min-w-[360px] max-w-[640px] text-[12px] leading-5 text-muted-foreground">
                  <span className="line-clamp-3">{row.text_preview || row.review_error || '-'}</span>
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </div>
      {total > AUDIT_PAGE_SIZE ? (
        <Pagination page={page} totalPages={totalPages} onPageChange={setPage} totalItems={total} pageSize={AUDIT_PAGE_SIZE} />
      ) : null}
    </>
  )
}

function UsageSampleTable({ rows, empty, showFirstToken = false }: { rows: UsageLog[]; empty: string; showFirstToken?: boolean }) {
  const { pageRows, page, totalPages, setPage, total } = usePaged(rows)
  if (!rows.length) {
    return <EmptyState>{empty}</EmptyState>
  }
  return (
    <>
      <div className="grid gap-2 sm:hidden">
        {pageRows.map((row) => (
          <div key={row.id} className="min-w-0 rounded-lg border border-border/60 bg-muted/20 p-3">
            <div className="flex items-start justify-between gap-3">
              <div className="min-w-0">
                <div className="text-xs font-medium text-foreground">{formatBeijingTime(row.created_at)}</div>
                <div className="mt-1 truncate text-xs text-muted-foreground">{row.effective_model || row.model || '-'}</div>
              </div>
              <Badge variant="outline" className="shrink-0 bg-background/70">{row.status_code}</Badge>
            </div>
            <div className="mt-3">
              <MobileField
                label={showFirstToken ? '首字' : '错误'}
                value={showFirstToken ? formatMS(row.first_token_ms) : (row.upstream_error_kind || row.error_message || '-')}
                wide
                wrap={!showFirstToken}
              />
            </div>
          </div>
        ))}
      </div>
      <div className="hidden w-full max-w-full overflow-x-auto rounded-lg border border-border/70 sm:block">
        <Table className="min-w-[620px]">
          <TableHeader className="bg-muted/40">
            <TableRow>
              <TableHead className="text-xs font-semibold text-muted-foreground">时间</TableHead>
              <TableHead className="text-xs font-semibold text-muted-foreground">模型</TableHead>
              <TableHead className="text-xs font-semibold text-muted-foreground">状态</TableHead>
              <TableHead className="text-xs font-semibold text-muted-foreground">{showFirstToken ? '首字' : '错误'}</TableHead>
            </TableRow>
          </TableHeader>
          <TableBody>
            {pageRows.map((row) => (
              <TableRow key={row.id} className="hover:bg-muted/30">
                <TableCell className="whitespace-nowrap text-[12px]">{formatBeijingTime(row.created_at)}</TableCell>
                <TableCell className="text-[12px]">{row.effective_model || row.model || '-'}</TableCell>
                <TableCell><Badge variant="outline">{row.status_code}</Badge></TableCell>
                <TableCell className="max-w-[520px] text-[12px] leading-5 text-muted-foreground">
                  {showFirstToken ? formatMS(row.first_token_ms) : <span className="line-clamp-2">{row.upstream_error_kind || row.error_message || '-'}</span>}
                </TableCell>
              </TableRow>
            ))}
          </TableBody>
        </Table>
      </div>
      {total > AUDIT_PAGE_SIZE ? (
        <Pagination page={page} totalPages={totalPages} onPageChange={setPage} totalItems={total} pageSize={AUDIT_PAGE_SIZE} />
      ) : null}
    </>
  )
}

function EmptyState({ children, compact = false }: { children: ReactNode; compact?: boolean }) {
  return (
    <div className={`rounded-lg border border-dashed border-border/80 bg-muted/20 text-center text-sm text-muted-foreground ${compact ? 'px-4 py-5' : 'px-6 py-8'}`}>
      {children}
    </div>
  )
}

function verdictClass(tone: Tone) {
  if (tone === 'ok') return 'border-emerald-500/20 bg-emerald-500/12 text-emerald-700 hover:bg-emerald-500/12 dark:text-emerald-300'
  if (tone === 'bad') return 'border-destructive/20 bg-destructive/12 text-destructive hover:bg-destructive/12'
  if (tone === 'warn') return 'border-amber-500/20 bg-amber-500/12 text-amber-700 hover:bg-amber-500/12 dark:text-amber-300'
  return 'border-border bg-muted text-muted-foreground hover:bg-muted'
}

function toneIconClass(tone: Tone) {
  if (tone === 'ok') return 'bg-emerald-500/12 text-emerald-700 dark:text-emerald-300'
  if (tone === 'bad') return 'bg-destructive/12 text-destructive'
  if (tone === 'warn') return 'bg-amber-500/12 text-amber-700 dark:text-amber-300'
  return 'bg-muted text-muted-foreground'
}

function toneTextClass(tone: Tone) {
  if (tone === 'ok') return 'text-emerald-700 dark:text-emerald-300'
  if (tone === 'bad') return 'text-destructive'
  if (tone === 'warn') return 'text-amber-700 dark:text-amber-300'
  return 'text-foreground'
}

function actionClass(action?: string) {
  if (action === 'block') return 'border-destructive/20 bg-destructive/12 text-destructive hover:bg-destructive/12'
  if (action === 'allow') return 'border-emerald-500/20 bg-emerald-500/12 text-emerald-700 hover:bg-emerald-500/12 dark:text-emerald-300'
  return 'border-border bg-muted text-muted-foreground hover:bg-muted'
}

function formatNumber(value?: number) {
  return new Intl.NumberFormat('zh-CN').format(value || 0)
}

function formatPercent(value?: number) {
  return `${Math.round((value || 0) * 100)}%`
}

function formatMS(value?: number) {
  if (!value) return '-'
  if (value >= 1000) return `${(value / 1000).toFixed(1)}s`
  return `${value}ms`
}

function formatDuration(seconds: number) {
  if (seconds >= 3600) return `${Math.round(seconds / 3600)}h`
  if (seconds >= 60) return `${Math.round(seconds / 60)}m`
  return `${Math.round(seconds)}s`
}

function formatRate(value?: number) {
  const rate = value || 0
  if (!rate) return '-'
  if (rate >= 10) return `${Math.round(rate)}/min`
  return `${rate.toFixed(1)}/min`
}

function formatShortTime(value: string) {
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return date.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit', hour12: false })
}
