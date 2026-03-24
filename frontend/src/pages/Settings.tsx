import type { ChangeEvent, KeyboardEvent } from 'react'
import { useCallback, useState } from 'react'
import { api } from '../api'
import PageHeader from '../components/PageHeader'
import StateShell from '../components/StateShell'
import { useDataLoader } from '../hooks/useDataLoader'
import { useToast } from '../hooks/useToast'
import type { APIKeyRow, HealthResponse } from '../types'
import { getErrorMessage } from '../utils/error'
import { formatRelativeTime } from '../utils/time'

function maskKey(key: string): string {
  if (!key || key.length < 12) return key
  return key.slice(0, 7) + '???????' + key.slice(-4)
}

export default function Settings() {
  const [newKeyName, setNewKeyName] = useState('')
  const [createdKey, setCreatedKey] = useState<string | null>(null)
  const { toast, showToast } = useToast()

  const loadSettingsData = useCallback(async () => {
    const [health, keysResponse] = await Promise.all([api.getHealth(), api.getAPIKeys()])
    return {
      health,
      keys: keysResponse.keys ?? [],
    }
  }, [])

  const { data, loading, error, reload } = useDataLoader<{
    health: HealthResponse | null
    keys: APIKeyRow[]
  }>({
    initialData: {
      health: null,
      keys: [],
    },
    load: loadSettingsData,
  })

  const handleCreateKey = async () => {
    try {
      const result = await api.createAPIKey(newKeyName.trim() || 'default')
      setCreatedKey(result.key)
      setNewKeyName('')
      showToast('密钥创建成功')
      void reload()
    } catch (error) {
      showToast(`创建失败: ${getErrorMessage(error)}`, 'error')
    }
  }

  const handleDeleteKey = async (id: number) => {
    if (!confirm('确定删除此密钥？使用该密钥的客户端将无法访问。')) {
      return
    }

    try {
      await api.deleteAPIKey(id)
      showToast('密钥已删除')
      void reload()
    } catch (error) {
      showToast(`删除失败: ${getErrorMessage(error)}`, 'error')
    }
  }

  const handleCopy = (text: string) => {
    void navigator.clipboard.writeText(text)
    showToast('已复制到剪贴板')
  }

  const { health, keys } = data
  return (
    <StateShell
      variant="page"
      loading={loading}
      error={error}
      onRetry={() => void reload()}
      loadingTitle="正在加载系统设置"
      loadingDescription="密钥列表和系统状态正在同步。"
      errorTitle="设置页加载失败"
    >
      <>
        <PageHeader
          title="系统设置"
          description="密钥管理与系统状态"
        />

      <div className="card" style={{ marginBottom: 16 }}>
        <div className="flex-between mb-4">
          <h3 style={{ fontSize: 16, fontWeight: 600, color: 'var(--text-primary)' }}>API 密钥</h3>
        </div>

        <div style={{ display: 'flex', gap: 8, marginBottom: 16 }}>
          <input
            className="form-input"
            style={{ flex: 1, minHeight: 40 }}
            placeholder="密钥名称（可选）"
            value={newKeyName}
            onChange={(event: ChangeEvent<HTMLInputElement>) => setNewKeyName(event.target.value)}
            onKeyDown={(event: KeyboardEvent<HTMLInputElement>) => {
              if (event.key === 'Enter') {
                void handleCreateKey()
              }
            }}
          />
          <button className="btn btn-primary" onClick={() => void handleCreateKey()}>
            生成密钥
          </button>
        </div>

        {createdKey ? (
          <div
            style={{
              padding: '12px 16px',
              marginBottom: 16,
              borderRadius: 12,
              background: 'var(--success-bg)',
              border: '1px solid rgba(47, 125, 87, 0.2)',
              fontSize: 13,
            }}
          >
            <div style={{ fontWeight: 600, marginBottom: 4, color: 'var(--success)' }}>新密钥已生成（仅显示一次）</div>
            <div style={{ display: 'flex', alignItems: 'center', gap: 8 }}>
              <code className="text-mono" style={{ flex: 1, wordBreak: 'break-all' }}>{createdKey}</code>
              <button className="btn btn-secondary btn-sm" onClick={() => handleCopy(createdKey)}>复制</button>
            </div>
          </div>
        ) : null}

        <StateShell
          variant="section"
          isEmpty={keys.length === 0}
          emptyTitle="暂无 API 密钥"
          emptyDescription="未设置密钥时接口无需鉴权，生成后会显示在这里。"
        >
          <div className="table-container">
            <table>
              <thead>
                <tr>
                  <th>名称</th>
                  <th>密钥</th>
                  <th>创建时间</th>
                  <th>操作</th>
                </tr>
              </thead>
              <tbody>
                {keys.map((keyRow) => (
                  <tr key={keyRow.id}>
                    <td style={{ fontWeight: 500 }}>{keyRow.name}</td>
                    <td>
                      <span className="text-mono" style={{ fontSize: 12 }}>{maskKey(keyRow.key)}</span>
                    </td>
                    <td className="text-muted">{formatRelativeTime(keyRow.created_at, { variant: 'compact' })}</td>
                    <td>
                      <button className="btn btn-danger btn-sm" onClick={() => void handleDeleteKey(keyRow.id)}>删除</button>
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        </StateShell>

        <div className="text-muted" style={{ fontSize: 12, marginTop: 12 }}>
          未设置密钥时 API 无需鉴权。添加第一个密钥后，所有 /v1/* 请求需携带 Authorization: Bearer sk-xxx
        </div>
      </div>

      <div className="card" style={{ marginBottom: 16 }}>
        <h3 style={{ fontSize: 16, fontWeight: 600, marginBottom: 16, color: 'var(--text-primary)' }}>系统状态</h3>
        <div className="info-grid">
          <div className="info-item">
            <label>服务</label>
            <div className="info-value">
              <span className={`badge ${health?.status === 'ok' ? 'badge-success' : 'badge-danger'}`}>
                <span className="badge-dot" />
                {health?.status === 'ok' ? '运行中' : '异常'}
              </span>
            </div>
          </div>
          <div className="info-item">
            <label>账号</label>
            <div className="info-value">{health?.available ?? 0} / {health?.total ?? 0}</div>
          </div>
          <div className="info-item">
            <label>PostgreSQL</label>
            <div className="info-value">
              <span className="badge badge-success"><span className="badge-dot" />已连接</span>
            </div>
          </div>
          <div className="info-item">
            <label>Redis</label>
            <div className="info-value">
              <span className="badge badge-success"><span className="badge-dot" />已连接</span>
            </div>
          </div>
        </div>
      </div>

      <div className="card">
        <h3 style={{ fontSize: 16, fontWeight: 600, marginBottom: 16, color: 'var(--text-primary)' }}>API 端点</h3>
        <div className="table-container">
          <table>
            <thead>
              <tr>
                <th>方法</th>
                <th>路径</th>
                <th>说明</th>
              </tr>
            </thead>
            <tbody>
              <tr>
                <td><span className="badge badge-success">POST</span></td>
                <td className="text-mono">/v1/chat/completions</td>
                <td className="text-secondary">OpenAI 兼容</td>
              </tr>
              <tr>
                <td><span className="badge badge-info">POST</span></td>
                <td className="text-mono">/v1/responses</td>
                <td className="text-secondary">Responses API</td>
              </tr>
              <tr>
                <td><span className="badge badge-warning">GET</span></td>
                <td className="text-mono">/v1/models</td>
                <td className="text-secondary">模型列表</td>
              </tr>
            </tbody>
          </table>
        </div>
      </div>

        {toast ? <div className={`toast toast-${toast.type}`}>{toast.msg}</div> : null}
      </>
    </StateShell>
  )
}
