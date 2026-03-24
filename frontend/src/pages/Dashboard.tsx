import type { ReactNode } from 'react'
import { useCallback } from 'react'
import { api } from '../api'
import PageHeader from '../components/PageHeader'
import StateShell from '../components/StateShell'
import StatCard from '../components/StatCard'
import StatusBadge from '../components/StatusBadge'
import type { AccountRow, StatsResponse } from '../types'
import { useDataLoader } from '../hooks/useDataLoader'
import { formatRelativeTime } from '../utils/time'

const icons: Record<string, ReactNode> = {
  total: <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2"><path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2" /><circle cx="9" cy="7" r="4" /><path d="M22 21v-2a4 4 0 0 0-3-3.87" /><path d="M16 3.13a4 4 0 0 1 0 7.75" /></svg>,
  available: <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2"><path d="M22 11.08V12a10 10 0 1 1-5.93-9.14" /><polyline points="22 4 12 14.01 9 11.01" /></svg>,
  error: <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2"><circle cx="12" cy="12" r="10" /><line x1="15" y1="9" x2="9" y2="15" /><line x1="9" y1="9" x2="15" y2="15" /></svg>,
  requests: <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2"><polyline points="22 12 18 12 15 21 9 3 6 12 2 12" /></svg>,
}

export default function Dashboard() {
  const loadDashboardData = useCallback(async () => {
    const [stats, accountsResponse] = await Promise.all([api.getStats(), api.getAccounts()])
    return {
      stats,
      accounts: accountsResponse.accounts ?? [],
    }
  }, [])

  const { data, loading, error, reload } = useDataLoader<{
    stats: StatsResponse | null
    accounts: AccountRow[]
  }>({
    initialData: {
      stats: null,
      accounts: [],
    },
    load: loadDashboardData,
  })

  const { stats, accounts } = data
  const total = stats?.total ?? 0
  const available = stats?.available ?? 0
  const errorCount = stats?.error ?? 0
  const todayRequests = stats?.today_requests ?? 0

  return (
    <StateShell
      variant="page"
      loading={loading}
      error={error}
      onRetry={() => void reload()}
      loadingTitle="正在加载仪表盘"
      loadingDescription="系统统计和账号状态正在同步。"
      errorTitle="仪表盘加载失败"
    >
      <>
        <PageHeader
          title="仪表盘"
          description="系统概览"
          onRefresh={() => void reload()}
        />

      <div className="stat-grid">
        <StatCard icon={icons.total} iconClass="blue" label="总账号" value={total} />
        <StatCard
          icon={icons.available}
          iconClass="green"
          label="可用"
          value={available}
          sub={`${total ? Math.round((available / total) * 100) : 0}% 可用率`}
        />
        <StatCard icon={icons.error} iconClass="red" label="异常" value={errorCount} />
        <StatCard icon={icons.requests} iconClass="purple" label="今日请求" value={todayRequests} />
      </div>

      <div className="card">
        <div className="flex-between mb-4">
          <h3 style={{ fontSize: 16, fontWeight: 600, color: 'var(--text-primary)' }}>账号状态</h3>
          <span className="table-meta">{accounts.length} 个账号</span>
        </div>
        <StateShell
          variant="section"
          isEmpty={accounts.length === 0}
          emptyTitle="暂无账号数据"
          emptyDescription="账号加入代理池后，会在这里展示状态和最近更新时间。"
        >
          <div className="table-container">
            <table>
              <thead>
                <tr>
                  <th>名称</th>
                  <th>邮箱</th>
                  <th>套餐</th>
                  <th>状态</th>
                  <th>更新时间</th>
                </tr>
              </thead>
              <tbody>
                {accounts.map((account) => (
                  <tr key={account.id}>
                    <td style={{ fontWeight: 500 }}>{account.name || `账号 #${account.id}`}</td>
                    <td className="text-secondary">{account.email || '-'}</td>
                    <td><span className="text-mono">{account.plan_type || '-'}</span></td>
                    <td><StatusBadge status={account.status} /></td>
                    <td className="text-muted">{formatRelativeTime(account.updated_at)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </StateShell>
      </div>
      </>
    </StateShell>
  )
}
