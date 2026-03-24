import type { ChangeEvent } from 'react'
import { useCallback, useState } from 'react'
import { api } from '../api'
import Modal from '../components/Modal'
import PageHeader from '../components/PageHeader'
import StateShell from '../components/StateShell'
import StatusBadge from '../components/StatusBadge'
import { useDataLoader } from '../hooks/useDataLoader'
import { useToast } from '../hooks/useToast'
import type { AccountRow, AddAccountRequest } from '../types'
import { getErrorMessage } from '../utils/error'
import { formatRelativeTime } from '../utils/time'

export default function Accounts() {
  const [showAdd, setShowAdd] = useState(false)
  const [addForm, setAddForm] = useState<AddAccountRequest>({
    name: '',
    refresh_token: '',
    proxy_url: '',
  })
  const [submitting, setSubmitting] = useState(false)
  const { toast, showToast } = useToast()

  const loadAccounts = useCallback(async () => {
    const data = await api.getAccounts()
    return data.accounts ?? []
  }, [])

  const { data: accounts, loading, error, reload } = useDataLoader<AccountRow[]>({
    initialData: [],
    load: loadAccounts,
  })

  const handleAdd = async () => {
    if (!addForm.refresh_token.trim()) {
      return
    }

    setSubmitting(true)
    try {
      await api.addAccount(addForm)
      showToast('账号添加成功')
      setShowAdd(false)
      setAddForm({ name: '', refresh_token: '', proxy_url: '' })
      void reload()
    } catch (error) {
      showToast(`添加失败: ${getErrorMessage(error)}`, 'error')
    } finally {
      setSubmitting(false)
    }
  }

  const handleDelete = async (account: AccountRow) => {
    if (!confirm(`确定删除账号 "${account.name || account.id}" 吗？`)) {
      return
    }

    try {
      await api.deleteAccount(account.id)
      showToast('已删除')
      void reload()
    } catch (error) {
      showToast(`删除失败: ${getErrorMessage(error)}`, 'error')
    }
  }

  const handleRefresh = async (account: AccountRow) => {
    try {
      await api.refreshAccount(account.id)
      showToast('刷新成功')
      void reload()
    } catch (error) {
      showToast(`刷新失败: ${getErrorMessage(error)}`, 'error')
    }
  }

  return (
    <StateShell
      variant="page"
      loading={loading}
      error={error}
      onRetry={() => void reload()}
      loadingTitle="正在加载账号列表"
      loadingDescription="账号池和实时状态正在同步。"
      errorTitle="账号页加载失败"
    >
      <>
        <PageHeader
          title="账号管理"
          description="管理 Codex 反代账号（Refresh Token）"
          onRefresh={() => void reload()}
          actions={(
            <button className="btn btn-primary" onClick={() => setShowAdd(true)}>
              <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2"><line x1="12" y1="5" x2="12" y2="19" /><line x1="5" y1="12" x2="19" y2="12" /></svg>
              添加账号
            </button>
          )}
        />

      <div className="card">
        <StateShell
          variant="section"
          isEmpty={accounts.length === 0}
          emptyTitle="还没有账号"
          emptyDescription="导入 Refresh Token 后，账号会立即加入代理池并显示在这里。"
          action={<button className="btn btn-primary" onClick={() => setShowAdd(true)}>添加账号</button>}
        >
          <div className="table-container">
            <table>
              <thead>
                <tr>
                  <th>ID</th>
                  <th>名称</th>
                  <th>邮箱</th>
                  <th>套餐</th>
                  <th>状态</th>
                  <th>更新时间</th>
                  <th style={{ textAlign: 'right' }}>操作</th>
                </tr>
              </thead>
              <tbody>
                {accounts.map((account) => (
                  <tr key={account.id}>
                    <td className="text-mono text-muted">{account.id}</td>
                    <td style={{ fontWeight: 500 }}>{account.name || '-'}</td>
                    <td className="text-secondary">{account.email || '-'}</td>
                    <td><span className="text-mono">{account.plan_type || '-'}</span></td>
                    <td><StatusBadge status={account.status} /></td>
                    <td className="text-muted">{formatRelativeTime(account.updated_at)}</td>
                    <td style={{ textAlign: 'right' }}>
                      <div className="btn-group" style={{ justifyContent: 'flex-end' }}>
                        <button className="btn btn-secondary btn-sm" onClick={() => void handleRefresh(account)} title="刷新 AT">
                          <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2"><path d="M23 4v6h-6" /><path d="M1 20v-6h6" /><path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0 0 20.49 15" /></svg>
                          刷新
                        </button>
                        <button className="btn btn-danger btn-sm" onClick={() => void handleDelete(account)}>
                          <svg width="12" height="12" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2"><polyline points="3 6 5 6 21 6" /><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2" /></svg>
                          删除
                        </button>
                      </div>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </StateShell>
      </div>

      <Modal
        show={showAdd}
        title="添加账号"
        onClose={() => setShowAdd(false)}
        footer={(
          <>
            <button className="btn btn-secondary" onClick={() => setShowAdd(false)}>取消</button>
            <button className="btn btn-primary" onClick={() => void handleAdd()} disabled={submitting || !addForm.refresh_token.trim()}>
              {submitting ? '添加中...' : '添加'}
            </button>
          </>
        )}
      >
        <div className="form-group">
          <label>名称（可选）</label>
          <input
            className="form-input"
            placeholder="例如：account-1"
            value={addForm.name}
            onChange={(event: ChangeEvent<HTMLInputElement>) =>
              setAddForm((form) => ({ ...form, name: event.target.value }))
            }
          />
        </div>
        <div className="form-group">
          <label>Refresh Token *</label>
          <textarea
            className="form-input"
            placeholder="粘贴 Refresh Token"
            value={addForm.refresh_token}
            onChange={(event: ChangeEvent<HTMLTextAreaElement>) =>
              setAddForm((form) => ({ ...form, refresh_token: event.target.value }))
            }
            rows={3}
          />
        </div>
        <div className="form-group">
          <label>代理地址（可选）</label>
          <input
            className="form-input"
            placeholder="例如：http://127.0.0.1:7890"
            value={addForm.proxy_url}
            onChange={(event: ChangeEvent<HTMLInputElement>) =>
              setAddForm((form) => ({ ...form, proxy_url: event.target.value }))
            }
          />
        </div>
      </Modal>

        {toast ? <div className={`toast toast-${toast.type}`}>{toast.msg}</div> : null}
      </>
    </StateShell>
  )
}
