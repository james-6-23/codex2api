import { type ReactNode, useCallback, useEffect, useMemo, useState } from 'react'
import { Activity, AlertTriangle, BarChart3, Clock, RefreshCw, ShieldCheck, ShieldX, Zap } from 'lucide-react'
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

const verdictMeta: Record<string, { label: string; tone: 'ok' | 'warn' | 'bad' }> = {
  normal: { label: '正常', tone: 'ok' },
  blocked_activity: { label: '存在拦截', tone: 'warn' },
  suspected_miss: { label: '疑似漏网', tone: 'bad' },
  review_error_risk: { label: '审查异常', tone: 'bad' },
  operational_issue: { label: '运行异常', tone: 'bad' },
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
  const meta = verdictMeta[report?.verdict || 'normal'] || { label: report?.verdict || '-', tone: 'warn' as const }

  const timeline = useMemo(() => (report?.timeline || []).map((point) => ({
    ...point,
    label: formatShortTime(point.bucket),
  })), [report?.timeline])

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
            <div className="grid grid-cols-[repeat(auto-fit,minmax(180px,1fr))] gap-3">
              <MetricCard title="整体结论" icon={meta.tone === 'ok' ? <ShieldCheck /> : <ShieldX />}>
                <Badge className={verdictClass(meta.tone)}>{meta.label}</Badge>
              </MetricCard>
              <MetricCard title="请求数" icon={<Activity />}>{formatNumber(report.usage.requests)}</MetricCard>
              <MetricCard title="拦截数" icon={<ShieldX />}>{formatNumber(report.summary.prompt_blocks)}</MetricCard>
              <MetricCard title="疑似漏网" icon={<AlertTriangle />}>{formatNumber(report.summary.upstream_cyber_policy)}</MetricCard>
              <MetricCard title="审查异常" icon={<AlertTriangle />}>{formatNumber(report.summary.review_errors)}</MetricCard>
              <MetricCard title="首字 P95" icon={<Clock />}>{formatMS(report.usage.first_token_p95_ms)}</MetricCard>
              <MetricCard title="WS 占比" icon={<Zap />}>{Math.round((report.usage.websocket_ratio || 0) * 100)}%</MetricCard>
              <MetricCard title="运行健康" icon={<Activity />}>{health?.status || '-'}</MetricCard>
            </div>

            <Card>
              <CardContent>
                <div className="mb-4 flex flex-wrap items-center justify-between gap-3">
                  <div>
                    <h2 className="text-lg font-semibold">巡检窗口</h2>
                    <p className="mt-1 text-sm text-muted-foreground">
                      {formatBeijingTime(report.window_start)} 至 {formatBeijingTime(report.window_end)}
                    </p>
                  </div>
                  <Badge variant="outline">生成于 {formatBeijingTime(report.generated_at)}</Badge>
                </div>
                <div className="grid gap-4 xl:grid-cols-[minmax(0,1.4fr)_minmax(320px,0.8fr)]">
                  <ChartCard title="请求与风险趋势">
                    <ResponsiveContainer width="100%" height={260}>
                      <LineChart data={timeline} margin={{ top: 8, right: 12, bottom: 0, left: 0 }}>
                        <CartesianGrid strokeDasharray="3 3" stroke="hsl(var(--border))" />
                        <XAxis dataKey="label" tick={{ fontSize: 11 }} stroke="hsl(var(--muted-foreground))" />
                        <YAxis tick={{ fontSize: 11 }} stroke="hsl(var(--muted-foreground))" />
                        <RechartsTooltip />
                        <Line type="monotone" dataKey="requests" name="请求" stroke="hsl(var(--primary))" strokeWidth={2} dot={false} />
                        <Line type="monotone" dataKey="prompt_blocks" name="拦截" stroke="#ef4444" strokeWidth={2} dot={false} />
                        <Line type="monotone" dataKey="upstream_cyber_policy" name="上游 cyb" stroke="#f97316" strokeWidth={2} dot={false} />
                        <Line type="monotone" dataKey="errors_5xx" name="5xx" stroke="#a855f7" strokeWidth={2} dot={false} />
                      </LineChart>
                    </ResponsiveContainer>
                  </ChartCard>
                  <ChartCard title="模型请求分布">
                    <ResponsiveContainer width="100%" height={260}>
                      <BarChart data={(report.models || []).slice(0, 10)} layout="vertical" margin={{ top: 8, right: 12, bottom: 0, left: 0 }}>
                        <CartesianGrid strokeDasharray="3 3" stroke="hsl(var(--border))" />
                        <XAxis type="number" tick={{ fontSize: 11 }} stroke="hsl(var(--muted-foreground))" />
                        <YAxis type="category" dataKey="model" width={120} tick={{ fontSize: 11 }} stroke="hsl(var(--muted-foreground))" />
                        <RechartsTooltip />
                        <Bar dataKey="requests" name="请求" fill="hsl(var(--primary))" radius={[0, 5, 5, 0]} />
                      </BarChart>
                    </ResponsiveContainer>
                  </ChartCard>
                </div>
              </CardContent>
            </Card>

            <div className="grid gap-4 xl:grid-cols-2">
              <Card>
                <CardContent>
                  <SectionTitle title="Prompt Filter 聚合" />
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
                </CardContent>
              </Card>
              <Card>
                <CardContent>
                  <SectionTitle title="高频探针" />
                  <ProbeTable rows={report.probe_observed || []} />
                  {(report.probe_short_circuits || []).length > 0 ? (
                    <div className="mt-4">
                      <SectionTitle title="本地短路探针" />
                      <ProbeTable rows={report.probe_short_circuits || []} />
                    </div>
                  ) : null}
                </CardContent>
              </Card>
            </div>

            <Card>
              <CardContent>
                <SectionTitle title="可疑样本" />
                <PromptSampleTable rows={report.suspicious_samples || []} />
              </CardContent>
            </Card>

            <div className="grid gap-4 xl:grid-cols-2">
              <Card>
                <CardContent>
                  <SectionTitle title="Policy-like 错误" />
                  <UsageSampleTable rows={report.policy_errors || []} empty="暂无 policy-like 错误" />
                </CardContent>
              </Card>
              <Card>
                <CardContent>
                  <SectionTitle title="首字慢请求" />
                  <UsageSampleTable rows={report.slow_requests || []} empty="暂无慢请求样本" showFirstToken />
                </CardContent>
              </Card>
            </div>

            {report.notes?.length ? (
              <Card>
                <CardContent>
                  <SectionTitle title="说明" />
                  <ul className="space-y-1 text-sm text-muted-foreground">
                    {report.notes.map((note, index) => <li key={index}>{note}</li>)}
                  </ul>
                </CardContent>
              </Card>
            ) : null}
          </div>
        ) : null}
      </StateShell>
    </>
  )
}

function MetricCard({ title, icon, children }: { title: string; icon: ReactNode; children: ReactNode }) {
  return (
    <Card>
      <CardContent className="flex items-center justify-between gap-3 py-4">
        <div>
          <div className="text-xs font-semibold text-muted-foreground">{title}</div>
          <div className="mt-2 text-2xl font-semibold">{children}</div>
        </div>
        <div className="flex size-10 items-center justify-center rounded-lg bg-primary/10 text-primary [&>svg]:size-5">
          {icon}
        </div>
      </CardContent>
    </Card>
  )
}

function ChartCard({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div className="rounded-lg border border-border p-4">
      <div className="mb-3 flex items-center gap-2 text-sm font-semibold">
        <BarChart3 className="size-4 text-primary" />
        {title}
      </div>
      {children}
    </div>
  )
}

function SectionTitle({ title }: { title: string }) {
  return <h2 className="mb-3 text-base font-semibold text-foreground">{title}</h2>
}

function SimpleTable({ columns, rows, empty }: { columns: string[]; rows: string[][]; empty: string }) {
  if (!rows.length) {
    return <div className="rounded-lg border border-dashed border-border p-6 text-center text-sm text-muted-foreground">{empty}</div>
  }
  return (
    <div className="overflow-x-auto rounded-lg border border-border">
      <Table>
        <TableHeader>
          <TableRow>{columns.map((column) => <TableHead key={column}>{column}</TableHead>)}</TableRow>
        </TableHeader>
        <TableBody>
          {rows.map((row, rowIndex) => (
            <TableRow key={rowIndex}>
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
    return <div className="rounded-lg border border-dashed border-border p-6 text-center text-sm text-muted-foreground">暂无探针记录</div>
  }
  return (
    <SimpleTable
      columns={['API Key', '端点', '模型', '次数', '跨度']}
      rows={rows.map((row) => [
        row.api_key_name || row.api_key_masked || String(row.api_key_id || '-'),
        row.endpoint || '-',
        row.model || '-',
        formatNumber(row.count),
        `${Math.round(row.span_seconds || 0)}s`,
      ])}
      empty="暂无探针记录"
    />
  )
}

function PromptSampleTable({ rows }: { rows: PromptFilterLog[] }) {
  if (!rows.length) {
    return <div className="rounded-lg border border-dashed border-border p-6 text-center text-sm text-muted-foreground">暂无可疑样本</div>
  }
  return (
    <div className="overflow-x-auto rounded-lg border border-border">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>时间</TableHead>
            <TableHead>来源</TableHead>
            <TableHead>动作</TableHead>
            <TableHead>分数</TableHead>
            <TableHead>审查</TableHead>
            <TableHead>预览</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.map((row) => (
            <TableRow key={row.id}>
              <TableCell className="whitespace-nowrap text-[12px]">{formatBeijingTime(row.created_at)}</TableCell>
              <TableCell className="text-[12px]">{row.source || '-'}</TableCell>
              <TableCell><Badge variant="outline">{row.action || '-'}</Badge></TableCell>
              <TableCell>{row.score}</TableCell>
              <TableCell className="text-[12px]">{row.review_model ? `${row.review_model} / ${row.review_flagged ? 'flagged' : 'clear'}` : '-'}</TableCell>
              <TableCell className="min-w-[360px] max-w-[640px] text-[12px] text-muted-foreground">
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
    return <div className="rounded-lg border border-dashed border-border p-6 text-center text-sm text-muted-foreground">{empty}</div>
  }
  return (
    <div className="overflow-x-auto rounded-lg border border-border">
      <Table>
        <TableHeader>
          <TableRow>
            <TableHead>时间</TableHead>
            <TableHead>模型</TableHead>
            <TableHead>状态</TableHead>
            <TableHead>{showFirstToken ? '首字' : '错误'}</TableHead>
          </TableRow>
        </TableHeader>
        <TableBody>
          {rows.map((row) => (
            <TableRow key={row.id}>
              <TableCell className="whitespace-nowrap text-[12px]">{formatBeijingTime(row.created_at)}</TableCell>
              <TableCell className="text-[12px]">{row.effective_model || row.model || '-'}</TableCell>
              <TableCell><Badge variant="outline">{row.status_code}</Badge></TableCell>
              <TableCell className="max-w-[520px] text-[12px] text-muted-foreground">
                {showFirstToken ? formatMS(row.first_token_ms) : <span className="line-clamp-2">{row.upstream_error_kind || row.error_message || '-'}</span>}
              </TableCell>
            </TableRow>
          ))}
        </TableBody>
      </Table>
    </div>
  )
}

function verdictClass(tone: 'ok' | 'warn' | 'bad') {
  if (tone === 'ok') return 'bg-emerald-500/12 text-emerald-700 hover:bg-emerald-500/12 dark:text-emerald-300'
  if (tone === 'bad') return 'bg-destructive/12 text-destructive hover:bg-destructive/12'
  return 'bg-amber-500/12 text-amber-700 hover:bg-amber-500/12 dark:text-amber-300'
}

function formatNumber(value?: number) {
  return new Intl.NumberFormat('zh-CN').format(value || 0)
}

function formatMS(value?: number) {
  if (!value) return '-'
  if (value >= 1000) return `${(value / 1000).toFixed(1)}s`
  return `${value}ms`
}

function formatShortTime(value: string) {
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return date.toLocaleTimeString('zh-CN', { hour: '2-digit', minute: '2-digit', hour12: false })
}
