import type { ReactNode } from 'react'
import { useCallback, useEffect } from 'react'
import { api } from '../api'
import PageHeader from '../components/PageHeader'
import StateShell from '../components/StateShell'
import { useDataLoader } from '../hooks/useDataLoader'
import { useToast } from '../hooks/useToast'
import type { UsageLog, UsageStats } from '../types'
import { formatRelativeTime } from '../utils/time'

function formatTokens(value?: number | null): string {
  if (value === undefined || value === null) return '0'
  if (value >= 1_000_000_000) return `${(value / 1_000_000_000).toFixed(1)}B`
  if (value >= 1_000_000) return `${(value / 1_000_000).toFixed(1)}M`
  if (value >= 1_000) return `${(value / 1_000).toFixed(1)}K`
  return value.toLocaleString()
}

const statIcons: Record<string, ReactNode> = {
  requests: <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2"><polyline points="22 12 18 12 15 21 9 3 6 12 2 12" /></svg>,
  tokens: <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2"><path d="M21 16V8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16z" /></svg>,
  rpm: <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2"><circle cx="12" cy="12" r="10" /><polyline points="12 6 12 12 16 14" /></svg>,
  tpm: <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2"><path d="M13 2L3 14h9l-1 8 10-12h-9l1-8z" /></svg>,
  error: <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2"><path d="M10.29 3.86L1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z" /><line x1="12" y1="9" x2="12" y2="13" /><line x1="12" y1="17" x2="12.01" y2="17" /></svg>,
}

export default function Usage() {
  const { toast, showToast } = useToast()

  const loadUsageData = useCallback(async () => {
    const [stats, logsResponse] = await Promise.all([api.getUsageStats(), api.getUsageLogs(100)])
    return {
      stats,
      logs: logsResponse.logs ?? [],
    }
  }, [])

  const { data, loading, error, reload, reloadSilently } = useDataLoader<{
    stats: UsageStats | null
    logs: UsageLog[]
  }>({
    initialData: {
      stats: null,
      logs: [],
    },
    load: loadUsageData,
  })

  useEffect(() => {
    const timer = window.setInterval(() => {
      void reloadSilently()
    }, 30000)

    return () => window.clearInterval(timer)
  }, [reloadSilently])

  const { stats, logs } = data
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

  return (
    <StateShell
      variant="page"
      loading={loading}
      error={error}
      onRetry={() => void reload()}
      loadingTitle="正在加载使用统计"
      loadingDescription="请求日志和性能指标正在同步。"
      errorTitle="统计页加载失败"
    >
      <>
        <PageHeader
          title="使用统计"
          description="请求日志与性能指标"
          onRefresh={() => void reload()}
        />

      <div className="stat-grid usage-grid-two">
        <div className="card stat-card-large">
          <div className="stat-card-header">
            <span className="stat-card-label">总请求数</span>
            <div className="stat-icon purple">{statIcons.requests}</div>
          </div>
          <div className="stat-card-value">{formatTokens(totalRequests)}</div>
          <div className="stat-card-sub">
            <span className="text-success">● 成功请求: {formatTokens(successRequests)}</span>
            <span className="text-muted" style={{ marginLeft: 8 }}>● 今日请求: {formatTokens(todayRequests)}</span>
          </div>
        </div>

        <div className="card stat-card-large">
          <div className="stat-card-header">
            <span className="stat-card-label">总 Token 数</span>
            <div className="stat-icon blue">{statIcons.tokens}</div>
          </div>
          <div className="stat-card-value">{formatTokens(totalTokens)}</div>
          <div className="stat-card-sub">
            <span>输入 Tokens: {formatTokens(totalPromptTokens)}</span>
            <span style={{ marginLeft: 8 }}>输出 Tokens: {formatTokens(totalCompletionTokens)}</span>
          </div>
        </div>
      </div>

      <div className="stat-grid usage-grid-three">
        <div className="card stat-card-large">
          <div className="stat-card-header">
            <span className="stat-card-label">RPM</span>
            <div className="stat-icon green">{statIcons.rpm}</div>
          </div>
          <div className="stat-card-value">{Math.round(rpm)}</div>
          <div className="stat-card-sub">每分钟请求数</div>
        </div>

        <div className="card stat-card-large">
          <div className="stat-card-header">
            <span className="stat-card-label">TPM</span>
            <div className="stat-icon red">{statIcons.tpm}</div>
          </div>
          <div className="stat-card-value">{formatTokens(tpm)}</div>
          <div className="stat-card-sub">每分钟 Token 数</div>
        </div>

        <div className="card stat-card-large">
          <div className="stat-card-header">
            <span className="stat-card-label">错误率</span>
            <div className="stat-icon" style={{ background: 'rgba(245, 158, 11, 0.12)', color: '#f59e0b' }}>
              {statIcons.error}
            </div>
          </div>
          <div className="stat-card-value">{errorRate.toFixed(1)}%</div>
          <div className="stat-card-sub">平均延迟: {Math.round(avgDurationMs)}ms</div>
        </div>
      </div>

      <div className="card">
        <div className="flex-between mb-4">
          <h3 style={{ fontSize: 15, fontWeight: 600, color: 'var(--text-primary)' }}>请求记录</h3>
          <span className="table-meta">最近 {logs.length} 条</span>
        </div>
        <StateShell
          variant="section"
          isEmpty={logs.length === 0}
          emptyTitle="暂无请求记录"
          emptyDescription="请求进入代理后，会在这里展示最近日志和状态码。"
        >
          <div className="table-container">
            <table>
              <thead>
                <tr>
                  <th>状态</th>
                  <th>模型</th>
                  <th>推理强度</th>
                  <th>端点</th>
                  <th>类型</th>
                  <th>TOKEN</th>
                  <th>首字时间</th>
                  <th>总耗时</th>
                  <th>时间</th>
                </tr>
              </thead>
              <tbody>
                {logs.map((log) => (
                  <tr key={log.id}>
                    <td>
                      <span className={`badge ${log.status_code < 400 ? 'badge-success' : log.status_code < 500 ? 'badge-warning' : 'badge-danger'}`} style={{ fontSize: 11 }}>
                        {log.status_code}
                      </span>
                    </td>
                    <td><span className="badge badge-info" style={{ fontSize: 11 }}>{log.model || '-'}</span></td>
                    <td>
                      {log.reasoning_effort ? (
                        <span className="badge" style={{
                          fontSize: 10,
                          background: log.reasoning_effort === 'high' ? 'rgba(239, 68, 68, 0.12)' :
                                     log.reasoning_effort === 'medium' ? 'rgba(245, 158, 11, 0.12)' :
                                     'rgba(34, 197, 94, 0.12)',
                          color: log.reasoning_effort === 'high' ? '#ef4444' :
                                 log.reasoning_effort === 'medium' ? '#f59e0b' :
                                 '#22c55e',
                        }}>
                          {log.reasoning_effort}
                        </span>
                      ) : <span className="text-muted" style={{ fontSize: 11 }}>-</span>}
                    </td>
                    <td>
                      <div style={{ fontSize: 11, lineHeight: 1.5 }}>
                        <span className="text-mono" style={{ color: 'var(--text-secondary)' }}>
                          {log.inbound_endpoint || log.endpoint || '-'}
                        </span>
                        {log.upstream_endpoint && log.upstream_endpoint !== log.inbound_endpoint && (
                          <span className="text-muted"> → {log.upstream_endpoint}</span>
                        )}
                      </div>
                    </td>
                    <td>
                      <span className="badge" style={{
                        fontSize: 10,
                        background: log.stream ? 'rgba(99, 102, 241, 0.12)' : 'rgba(107, 114, 128, 0.12)',
                        color: log.stream ? '#6366f1' : '#6b7280',
                      }}>
                        {log.stream ? 'stream' : 'sync'}
                      </span>
                    </td>
                    <td>
                      {log.status_code < 400 && (log.input_tokens > 0 || log.output_tokens > 0) ? (
                        <div style={{ fontSize: 11, lineHeight: 1.6 }}>
                          <span style={{ color: '#3b82f6' }}>↓{formatTokens(log.input_tokens)}</span>
                          <span style={{ margin: '0 4px', color: 'var(--border-color)' }}>|</span>
                          <span style={{ color: '#22c55e' }}>↑{formatTokens(log.output_tokens)}</span>
                          {log.reasoning_tokens > 0 && (
                            <>
                              <span style={{ margin: '0 4px', color: 'var(--border-color)' }}>|</span>
                              <span style={{ color: '#f59e0b' }}>💡{formatTokens(log.reasoning_tokens)}</span>
                            </>
                          )}
                        </div>
                      ) : (
                        <span className="text-muted" style={{ fontSize: 11 }}>-</span>
                      )}
                    </td>
                    <td>
                      {log.first_token_ms > 0 ? (
                        <span className="text-mono" style={{ fontSize: 11, color: log.first_token_ms > 5000 ? '#ef4444' : log.first_token_ms > 2000 ? '#f59e0b' : '#22c55e' }}>
                          {log.first_token_ms > 1000 ? `${(log.first_token_ms / 1000).toFixed(1)}s` : `${log.first_token_ms}ms`}
                        </span>
                      ) : <span className="text-muted" style={{ fontSize: 11 }}>-</span>}
                    </td>
                    <td>
                      <span className="text-mono" style={{ fontSize: 11, color: log.duration_ms > 30000 ? '#ef4444' : log.duration_ms > 10000 ? '#f59e0b' : 'var(--text-secondary)' }}>
                        {log.duration_ms > 1000 ? `${(log.duration_ms / 1000).toFixed(1)}s` : `${log.duration_ms}ms`}
                      </span>
                    </td>
                    <td className="text-muted" style={{ fontSize: 11, whiteSpace: 'nowrap' }}>
                      {formatRelativeTime(log.created_at, { variant: 'compact', includeSeconds: true })}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </StateShell>
      </div>

        {toast ? <div className={`toast toast-${toast.type}`}>{toast.msg}</div> : null}
      </>
    </StateShell>
  )
}
