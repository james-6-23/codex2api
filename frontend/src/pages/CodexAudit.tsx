import { type ReactNode, useCallback, useEffect, useMemo, useState } from 'react'
import { Activity, AlertTriangle, BarChart3, CheckCircle2, Clock3, Gauge, RefreshCw, ShieldAlert, ShieldCheck, ShieldX, Zap } from 'lucide-react'
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
import { useDataLoader } from '../hooks/useDataLoader'
import { formatBeijingTime } from '../utils/time'
import type { CodexAuditReport, HealthResponse, PromptFilterLog, UsageLog } from '../types'
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
}

type Tone = 'ok' | 'warn' | 'bad' | 'neutral'

const rangeOptions = [
  { label: '最近 30 分钟', value: '0.5' },
  { label: '最近 1 小时', value: '1' },
  { label: '最近 3 小时', value: '3' },
  { label: '最近 6 小时', value: '6' },
  { label: '最近 12 小时', value: '12' },
  { label: '最近 24 小时', value: '24' },
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
    const bucketMinutes = rangeHours <= 1 ? 5 : rangeHours <= 6 ? 10 : 30
    const [report, health] = await Promise.all([
      api.getCodexAuditReport({ hours: rangeHours, bucketMinutes, limit: 30 }),
      api.getHealth(),
    ])
    return { report, health }
  }, [rangeHours])

  const { data, loading, error, reload } = useDataLoader<AuditData>({
    initialData: { report: null, health: null },
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
        title="Codex2API 巡检"
        description="集中查看误伤、漏网、cyb、探针、首字延迟和运行健康。"
        actions={
          <>
            <Select value={String(rangeHours)} onValueChange={(value) => setRangeHours(Number(value))} options={rangeOptions} />
            <Select value={String(refreshSeconds)} onValueChange={(value) => setRefreshSeconds(Number(value))} options={refreshOptions} />
            <Button variant="outline" onClick={() => void reload()} disabled={loading}>
              <RefreshCw className={loading ? 'size-3.5 animate-spin' : 'size-3.5'} />
              刷新
            </Button>
          </>
        }
      />

      <StateShell loading={loading && !report} error={error} isEmpty={!loading && !report} onRetry={() => void reload()} emptyTitle="暂无巡检数据">
        {report ? (
          <div className="space-y-4">
            <Card className="overflow-hidden border-border/70 bg-gradient-to-br from-background via-background to-muted/40 shadow-sm">
              <CardContent className="p-0">
                <div className="grid lg:grid-cols-[minmax(0,1fr)_minmax(520px,2fr)]">
                  <div className="border-b border-border/70 p-5 lg:border-b-0 lg:border-r">
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

                  <div className="grid gap-px bg-border/70 sm:grid-cols-2 xl:grid-cols-4">
                    <SignalTile label="请求总量" value={formatNumber(report.usage.requests)} detail={`错误率 ${formatPercent(errorRate)}`} icon={<Activity />} tone={errorTone} />
                    <SignalTile label="拦截命中" value={formatNumber(report.summary.prompt_blocks)} detail={`拦截率 ${formatPercent(blockRate)}`} icon={<ShieldX />} tone={report.summary.prompt_blocks ? 'warn' : 'ok'} />
                    <SignalTile label="疑似漏网" value={formatNumber(report.summary.upstream_cyber_policy)} detail="上游 cyb / policy 信号" icon={<AlertTriangle />} tone={report.summary.upstream_cyber_policy ? 'bad' : 'ok'} />
                    <SignalTile label="审查异常" value={formatNumber(report.summary.review_errors)} detail="审核/语义复核错误" icon={<ShieldAlert />} tone={report.summary.review_errors ? 'bad' : 'ok'} />
                    <SignalTile label="首字 P95" value={formatMS(report.usage.first_token_p95_ms)} detail={`${formatNumber(report.usage.first_token_samples)} 个样本`} icon={<Clock3 />} tone={firstTokenTone} />
                    <SignalTile label="WS 占比" value={formatPercent(report.usage.websocket_ratio || 0)} detail={`${formatNumber(report.usage.websocket_requests)} 个 WS 请求`} icon={<Zap />} tone={(report.usage.websocket_ratio || 0) >= 0.85 ? 'ok' : 'warn'} />
                    <SignalTile label="语义分歧" value={formatNumber(report.summary.semantic_disagreements)} detail={`拦截 ${formatNumber(report.summary.semantic_disagreement_blocks)}`} icon={<Gauge />} tone={report.summary.semantic_disagreement_blocks ? 'warn' : 'neutral'} />
                    <SignalTile label="探针短路" value={formatNumber(report.summary.probe_short_circuits)} detail={`观测 ${formatNumber(report.summary.probe_observed)}`} icon={<CheckCircle2 />} tone={report.summary.probe_short_circuits ? 'neutral' : 'ok'} />
                  </div>
                </div>
              </CardContent>
            </Card>

            <div className="grid gap-4 xl:grid-cols-[minmax(0,1.45fr)_minmax(360px,0.85fr)]">
              <ChartPanel title="请求与风险趋势" description="按时间窗口聚合请求、拦截、上游 cyb 和 5xx。">
                <ResponsiveContainer width="100%" height={286}>
                  <LineChart data={timeline} margin={{ top: 12, right: 18, bottom: 0, left: 0 }}>
                    <CartesianGrid strokeDasharray="4 4" stroke="hsl(var(--border))" vertical={false} />
                    <XAxis dataKey="label" tick={{ fontSize: 11 }} stroke="hsl(var(--muted-foreground))" tickLine={false} axisLine={false} />
                    <YAxis tick={{ fontSize: 11 }} stroke="hsl(var(--muted-foreground))" tickLine={false} axisLine={false} />
                    <RechartsTooltip contentStyle={chartTooltipStyle} />
                    <Line type="monotone" dataKey="requests" name="请求" stroke="hsl(var(--primary))" strokeWidth={2.5} dot={false} activeDot={{ r: 4 }} />
                    <Line type="monotone" dataKey="prompt_blocks" name="拦截" stroke="#ef4444" strokeWidth={2} dot={false} activeDot={{ r: 4 }} />
                    <Line type="monotone" dataKey="upstream_cyber_policy" name="上游 cyb" stroke="#f97316" strokeWidth={2} dot={false} activeDot={{ r: 4 }} />
                    <Line type="monotone" dataKey="errors_5xx" name="5xx" stroke="#6366f1" strokeWidth={2} dot={false} activeDot={{ r: 4 }} />
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
                    <Bar dataKey="requests" name="请求" fill="hsl(var(--primary))" radius={[0, 6, 6, 0]} />
                  </BarChart>
                </ResponsiveContainer>
              </ChartPanel>
            </div>

            <div className="grid gap-4 xl:grid-cols-2">
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

              <Panel title="探针行为" description="观察高频探针和本地缓存短路效果。">
                <SubsectionTitle title="高频探针" />
                <ProbeTable rows={report.probe_observed || []} />
                <div className="mt-4">
                  <SubsectionTitle title="本地短路探针" />
                  <ProbeTable rows={report.probe_short_circuits || []} />
                </div>
              </Panel>
            </div>

            <Panel title="可疑样本" description="高分放行、语义分歧、上游 cyb 等需要人工复核的样本。">
              <PromptSampleTable rows={report.suspicious_samples || []} />
            </Panel>

            <div className="grid gap-4 xl:grid-cols-2">
              <Panel title="Policy-like 错误" description="从请求日志中提取 policy、cyber、violat、safety 相关错误。">
                <UsageSampleTable rows={report.policy_errors || []} empty="暂无 policy-like 错误" />
              </Panel>
              <Panel title="首字慢请求" description="按首字时间倒序列出最慢样本，用于观察 WS 和上游延迟。">
                <UsageSampleTable rows={report.slow_requests || []} empty="暂无慢请求样本" showFirstToken />
              </Panel>
            </div>

            {report.notes?.length ? (
              <Panel title="说明">
                <ul className="space-y-2 text-sm leading-6 text-muted-foreground">
                  {report.notes.map((note, index) => <li key={index}>{note}</li>)}
                </ul>
              </Panel>
            ) : null}
          </div>
        ) : null}
      </StateShell>
    </>
  )
}

function SignalTile({ label, value, detail, icon, tone }: { label: string; value: ReactNode; detail: string; icon: ReactNode; tone: Tone }) {
  return (
    <div className="bg-background/95 p-4">
      <div className="flex items-center justify-between gap-3">
        <div className="min-w-0">
          <div className="text-xs font-medium text-muted-foreground">{label}</div>
          <div className="mt-2 truncate text-2xl font-semibold tracking-tight text-foreground">{value}</div>
        </div>
        <div className={`flex size-9 shrink-0 items-center justify-center rounded-lg ${toneIconClass(tone)} [&>svg]:size-4`}>
          {icon}
        </div>
      </div>
      <div className="mt-3 text-xs text-muted-foreground">{detail}</div>
    </div>
  )
}

function WindowLine({ label, value, tone = 'neutral' }: { label: string; value: ReactNode; tone?: Tone }) {
  return (
    <div className="flex items-center justify-between gap-4 rounded-md border border-border/70 bg-background/70 px-3 py-2">
      <span className="text-xs text-muted-foreground">{label}</span>
      <span className={`truncate text-right text-xs font-medium ${toneTextClass(tone)}`}>{value}</span>
    </div>
  )
}

function ChartPanel({ title, description, children }: { title: string; description: string; children: ReactNode }) {
  return (
    <Card className="border-border/70 shadow-sm">
      <CardContent className="p-5">
        <div className="mb-4 flex items-start justify-between gap-4">
          <div>
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
    <Card className="border-border/70 shadow-sm">
      <CardContent className="p-5">
        <div className="mb-4">
          <h2 className="text-base font-semibold text-foreground">{title}</h2>
          {description ? <p className="mt-1 text-sm text-muted-foreground">{description}</p> : null}
        </div>
        {children}
      </CardContent>
    </Card>
  )
}

function SubsectionTitle({ title }: { title: string }) {
  return <h3 className="mb-2 text-sm font-semibold text-foreground">{title}</h3>
}

function SimpleTable({ columns, rows, empty }: { columns: string[]; rows: string[][]; empty: string }) {
  if (!rows.length) {
    return <EmptyState>{empty}</EmptyState>
  }
  return (
    <div className="overflow-x-auto rounded-lg border border-border/70">
      <Table>
        <TableHeader className="bg-muted/40">
          <TableRow>{columns.map((column) => <TableHead key={column} className="text-xs font-semibold text-muted-foreground">{column}</TableHead>)}</TableRow>
        </TableHeader>
        <TableBody>
          {rows.map((row, rowIndex) => (
            <TableRow key={rowIndex} className="hover:bg-muted/30">
              {row.map((cell, cellIndex) => <TableCell key={cellIndex} className="text-[13px]">{cell}</TableCell>)}
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  )
}

function ProbeTable({ rows }: { rows: CodexAuditReport['probe_observed'] }) {
  if (!rows.length) {
    return <EmptyState compact>暂无探针记录</EmptyState>
  }
  return (
    <SimpleTable
      columns={['API Key', '端点', '模型', '次数', '跨度']}
      rows={rows.map((row) => [
        row.api_key_name || row.api_key_masked || String(row.api_key_id || '-'),
        row.endpoint || '-',
        row.model || '-',
        formatNumber(row.count),
        formatDuration(row.span_seconds || 0),
      ])}
      empty="暂无探针记录"
    />
  )
}

function PromptSampleTable({ rows }: { rows: PromptFilterLog[] }) {
  if (!rows.length) {
    return <EmptyState>暂无可疑样本</EmptyState>
  }
  return (
    <div className="overflow-x-auto rounded-lg border border-border/70">
      <Table>
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
          {rows.map((row) => (
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
  )
}

function UsageSampleTable({ rows, empty, showFirstToken = false }: { rows: UsageLog[]; empty: string; showFirstToken?: boolean }) {
  if (!rows.length) {
    return <EmptyState>{empty}</EmptyState>
  }
  return (
    <div className="overflow-x-auto rounded-lg border border-border/70">
      <Table>
        <TableHeader className="bg-muted/40">
          <TableRow>
            <TableHead className="text-xs font-semibold text-muted-foreground">时间</TableHead>
            <TableHead className="text-xs font-semibold text-muted-foreground">模型</TableHead>
            <TableHead className="text-xs font-semibold text-muted-foreground">状态</TableHead>
            <TableHead className="text-xs font-semibold text-muted-foreground">{showFirstToken ? '首字' : '错误'}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.map((row) => (
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

function formatShortTime(value: string) {
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return date.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit', hour12: false })
}
