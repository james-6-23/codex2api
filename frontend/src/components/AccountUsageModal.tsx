import { useCallback, useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { PieChart, Pie, Cell, ResponsiveContainer, Tooltip } from 'recharts'
import Modal from './Modal'
import { api } from '../api'
import type { AccountRow, AccountUsageDetail } from '../types'
import { getErrorMessage } from '../utils/error'

const COLORS = ['#7c3aed', '#3b82f6', '#10b981', '#f59e0b', '#ef4444', '#ec4899', '#8b5cf6', '#06b6d4', '#84cc16', '#f97316']

interface Props {
  account: AccountRow
  onClose: () => void
}

export default function AccountUsageModal({ account, onClose }: Props) {
  const { t } = useTranslation()
  const [data, setData] = useState<AccountUsageDetail | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  const load = useCallback(async () => {
    setLoading(true)
    setError(null)
    try {
      const result = await api.getAccountUsage(account.id)
      setData(result)
    } catch (err) {
      setError(getErrorMessage(err))
    } finally {
      setLoading(false)
    }
  }, [account.id])

  useEffect(() => { void load() }, [load])

  const title = t('accounts.usageDetailTitle') + ' — ' + (account.email || account.name || `#${account.id}`)

  return (
    <Modal show title={title} onClose={onClose} contentClassName="sm:max-w-[720px]">
      {loading ? (
        <div className="flex items-center justify-center py-12 text-muted-foreground text-sm">{t('common.loading')}</div>
      ) : error ? (
        <div className="py-8 text-center text-sm text-red-500">{error}</div>
      ) : !data || data.total_requests === 0 ? (
        <div className="py-12 text-center text-sm text-muted-foreground">{t('accounts.noUsageData')}</div>
      ) : (
        <div className="space-y-6">
          <div className="flex gap-6">
            {/* 左侧：饼图 */}
            <div className="shrink-0">
              <h4 className="text-sm font-semibold mb-2">{t('accounts.modelDistribution')}</h4>
              <div className="w-[200px] h-[200px]">
                <ResponsiveContainer width="100%" height="100%">
                  <PieChart>
                    <Pie
                      data={data.models}
                      dataKey="requests"
                      nameKey="model"
                      cx="50%"
                      cy="50%"
                      innerRadius={45}
                      outerRadius={85}
                      paddingAngle={2}
                      strokeWidth={0}
                    >
                      {data.models.map((_, i) => (
                        <Cell key={i} fill={COLORS[i % COLORS.length]} />
                      ))}
                    </Pie>
                    <Tooltip
                      formatter={(value, name) => [`${Number(value ?? 0)} 次`, String(name ?? '')]}
                      contentStyle={{ fontSize: 12, borderRadius: 8, border: '1px solid hsl(var(--border))' }}
                    />
                  </PieChart>
                </ResponsiveContainer>
              </div>
              {/* 图例 */}
              <div className="mt-2 space-y-1">
                {data.models.map((m, i) => (
                  <div key={m.model} className="flex items-center gap-2 text-[12px]">
                    <span className="size-2.5 rounded-full shrink-0" style={{ background: COLORS[i % COLORS.length] }} />
                    <span className="truncate text-foreground font-medium">{m.model}</span>
                    <span className="ml-auto shrink-0 text-muted-foreground tabular-nums">{m.requests.toLocaleString()}</span>
                  </div>
                ))}
              </div>
            </div>

            {/* 右侧：Token 统计 */}
            <div className="flex-1 space-y-2.5">
              <StatRow label={t('accounts.totalRequests')} value={data.total_requests.toLocaleString()} highlight />
              <StatRow label={t('accounts.totalTokens')} value={data.total_tokens.toLocaleString()} highlight />
              <div className="h-px bg-border" />
              <StatRow label={t('accounts.inputTokens')} value={data.input_tokens.toLocaleString()} />
              <StatRow label={t('accounts.outputTokens')} value={data.output_tokens.toLocaleString()} />
              <StatRow label={t('accounts.reasoningTokens')} value={data.reasoning_tokens.toLocaleString()} />
              <StatRow label={t('accounts.cachedTokens')} value={data.cached_tokens.toLocaleString()} />
            </div>
          </div>

          {(data.usage_5h || data.usage_7d) && (
            <div className="space-y-4">
              <h4 className="text-sm font-semibold text-foreground">时间范围用量统计</h4>
              <div className="grid grid-cols-1 md:grid-cols-2 gap-4">
                {data.usage_5h && (
                  <TimeRangeCard title="5小时用量" data={data.usage_5h} />
                )}
                {data.usage_7d && (
                  <TimeRangeCard title="7天用量" data={data.usage_7d} />
                )}
              </div>
            </div>
          )}
        </div>
      )}
    </Modal>
  )
}

function StatRow({ label, value, highlight }: { label: string; value: string; highlight?: boolean }) {
  return (
    <div className="flex items-center justify-between rounded-lg border border-border px-3.5 py-2">
      <span className="text-[13px] text-muted-foreground">{label}</span>
      <span className={`tabular-nums font-semibold ${highlight ? 'text-[15px] text-foreground' : 'text-[14px] text-foreground/80'}`}>{value}</span>
    </div>
  )
}

function TimeRangeCard({ title, data }: { title: string; data: import('../types').TimeRangeUsage }) {
  return (
    <div className="rounded-lg border border-border bg-card p-4">
      <h5 className="text-sm font-semibold text-foreground mb-3">{title}</h5>
      <div className="space-y-2">
        <div className="flex items-center justify-between">
          <span className="text-[12px] text-muted-foreground">请求数 (Requests)</span>
          <span className="text-[13px] font-semibold text-foreground tabular-nums">{data.requests.toLocaleString()}</span>
        </div>
        <div className="flex items-center justify-between">
          <span className="text-[12px] text-muted-foreground">Token 数 (Tokens)</span>
          <span className="text-[13px] font-semibold text-foreground tabular-nums">{data.tokens.toLocaleString()}</span>
        </div>
        <div className="h-px bg-border my-1" />
        <div className="flex items-center justify-between">
          <span className="text-[12px] text-muted-foreground">账号计费 (Account Billed)</span>
          <span className="text-[13px] font-semibold text-emerald-600 dark:text-emerald-400 tabular-nums">
            ${data.account_billed.toFixed(4)}
          </span>
        </div>
        <div className="flex items-center justify-between">
          <span className="text-[12px] text-muted-foreground">用户扣费 (User Billed)</span>
          <span className="text-[13px] font-semibold text-blue-600 dark:text-blue-400 tabular-nums">
            ${data.user_billed.toFixed(4)}
          </span>
        </div>
      </div>
    </div>
  )
}
