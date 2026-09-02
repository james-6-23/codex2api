import { useCallback, useEffect, useRef, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { AlertTriangle, Gift, Loader2, Sparkles } from 'lucide-react'
import Modal from './Modal'
import { Button } from '@/components/ui/button'
import { api } from '../api'
import type { InviteGuideAccountPlan, InviteGuidePlan } from '../types'
import { getErrorMessage } from '../utils/error'

interface Props {
  show: boolean
  // 本次导入新建的账号 ID。后端会过滤掉不能发邀请的（中转 / AT-only / 非 Codex）。
  accountIds: number[]
  onClose: () => void
  // 传邮箱而不是 ID:邀请页的账号选择器是服务端搜索,邮箱既能当查询词把目标
  // 账号捞进候选集,又能被精确匹配选中。
  onGoInvite: (accountEmail?: string) => void
}

// 探测在后台按导入闸门排队，结果陆续落库，这里轮询直到没有 pending。
const POLL_INTERVAL_MS = 2500
// 轮询上限：50 个账号 × 串行探测最坏情况约一分多钟，给到两分钟后停手，
// 避免弹窗一直转圈——剩下的用户可以手动点「继续探测」。
const POLL_TIMEOUT_MS = 120_000

function stateTone(state: InviteGuideAccountPlan['state']): string {
  switch (state) {
    case 'eligible':
      return 'bg-emerald-500/10 text-emerald-600'
    case 'exhausted':
      return 'bg-amber-500/10 text-amber-600'
    case 'ineligible':
      return 'bg-muted text-muted-foreground'
    default:
      return 'bg-blue-500/10 text-blue-600'
  }
}

function formatCredits(value: number): string {
  return Math.round(value).toLocaleString()
}

export default function InviteGuideModal({ show, accountIds, onClose, onGoInvite }: Props) {
  const { t } = useTranslation()
  const [plan, setPlan] = useState<InviteGuidePlan | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [loading, setLoading] = useState(false)
  const [dontShowAgain, setDontShowAgain] = useState(false)
  const [probing, setProbing] = useState(false)
  // 受邀邮箱预算。空串表示不限，此时建议次数等于各账号剩余奖励次数。
  const [emailBudget, setEmailBudget] = useState('')

  const startedAtRef = useRef(0)
  const timerRef = useRef<number | null>(null)
  // 递增序号丢弃过期响应：改预算会立刻重新拉取，慢的旧响应不能覆盖新结果。
  const seqRef = useRef(0)

  const clearTimer = useCallback(() => {
    if (timerRef.current !== null) {
      window.clearTimeout(timerRef.current)
      timerRef.current = null
    }
  }, [])

  const fetchPlan = useCallback(async (budget: number) => {
    if (accountIds.length === 0) return
    const seq = ++seqRef.current
    setLoading(true)
    try {
      const data = await api.getInviteGuidePlan(accountIds, budget)
      if (seq !== seqRef.current) return
      setPlan(data)
      setError(null)
      // 还有账号没探测出来就继续轮询，超时后停手交给手动按钮。
      if (data.pending > 0 && Date.now() - startedAtRef.current < POLL_TIMEOUT_MS) {
        clearTimer()
        timerRef.current = window.setTimeout(() => void fetchPlan(budget), POLL_INTERVAL_MS)
      }
    } catch (err) {
      if (seq !== seqRef.current) return
      setError(getErrorMessage(err))
    } finally {
      if (seq === seqRef.current) setLoading(false)
    }
  }, [accountIds, clearTimer])

  useEffect(() => {
    if (!show) {
      clearTimer()
      return
    }
    startedAtRef.current = Date.now()
    setPlan(null)
    setError(null)
    setDontShowAgain(false)
    setEmailBudget('')
    void fetchPlan(0)
    return clearTimer
  }, [show, fetchPlan, clearTimer])

  // 预算变化时按新值重算。后端做贪心分配，前端不自己算，避免两边算法漂移。
  useEffect(() => {
    if (!show || plan == null) return
    const parsed = Number.parseInt(emailBudget, 10)
    const budget = Number.isFinite(parsed) && parsed > 0 ? parsed : 0
    if (budget === plan.email_budget) return
    const timer = window.setTimeout(() => void fetchPlan(budget), 350)
    return () => window.clearTimeout(timer)
    // plan 故意不进依赖：它每次轮询都会换引用，进依赖会变成自触发循环。
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [emailBudget, show, fetchPlan])

  const handleClose = useCallback(() => {
    clearTimer()
    if (dontShowAgain) {
      // 失败也照常关闭：用户的意图是关掉这个弹窗，设置没存上不该把人堵在这里。
      void api.updateInviteGuideSettings(false).catch(() => undefined)
    }
    onClose()
  }, [clearTimer, dontShowAgain, onClose])

  const handleProbeMore = async () => {
    if (!plan) return
    const pendingIDs = plan.accounts.filter((a) => a.state === 'pending').map((a) => a.id)
    if (pendingIDs.length === 0) return
    setProbing(true)
    try {
      await api.probeInviteGuidePlan(pendingIDs)
      startedAtRef.current = Date.now()
      const parsed = Number.parseInt(emailBudget, 10)
      void fetchPlan(Number.isFinite(parsed) && parsed > 0 ? parsed : 0)
    } catch (err) {
      setError(getErrorMessage(err))
    } finally {
      setProbing(false)
    }
  }

  const topAccount = plan?.accounts.find((a) => a.state === 'eligible')

  return (
    <Modal
      show={show}
      onClose={handleClose}
      contentClassName="sm:max-w-[620px]"
      title={
        <span className="flex items-center gap-2">
          <span className="flex size-8 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-primary">
            <Gift className="size-4" />
          </span>
          {t('inviteGuide.title')}
        </span>
      }
      footer={
        <div className="flex w-full flex-wrap items-center justify-between gap-3">
          <label className="flex items-center gap-2 text-xs text-muted-foreground">
            <input
              type="checkbox"
              checked={dontShowAgain}
              onChange={(e) => setDontShowAgain(e.target.checked)}
              className="size-3.5 rounded border-input"
            />
            <span title={t('inviteGuide.dontShowAgainHint')}>{t('inviteGuide.dontShowAgain')}</span>
          </label>
          <div className="flex items-center gap-2">
            <Button variant="outline" onClick={handleClose}>
              {t('inviteGuide.later')}
            </Button>
            <Button disabled={!topAccount} onClick={() => { clearTimer(); onGoInvite(topAccount?.email) }}>
              <Sparkles className="size-3.5" />
              {t('inviteGuide.goInvite')}
            </Button>
          </div>
        </div>
      }
    >
      {error ? (
        <p className="flex items-start gap-2 text-sm text-amber-600">
          <AlertTriangle className="mt-0.5 size-4 shrink-0" />
          <span className="break-all">{t('inviteGuide.loadFailed', { error })}</span>
        </p>
      ) : plan == null ? (
        <div className="flex flex-col items-center justify-center py-10 text-center">
          <Loader2 className="size-5 animate-spin text-muted-foreground" />
          <p className="mt-3 text-sm text-muted-foreground">{t('inviteGuide.loading')}</p>
        </div>
      ) : plan.total === 0 ? (
        <p className="py-8 text-center text-sm text-muted-foreground">{t('inviteGuide.noCandidates')}</p>
      ) : (
        <div className="space-y-4">
          {/* 收益汇总 */}
          <div className="rounded-2xl border bg-muted/30 px-4 py-3.5">
            <div className="flex items-end justify-between gap-3">
              <div className="min-w-0">
                <p className="text-xs text-muted-foreground">{t('inviteGuide.totalCredits')}</p>
                <p className="mt-0.5 text-2xl font-semibold tabular-nums text-foreground">
                  {formatCredits(plan.total_potential_credits)}
                </p>
              </div>
              <p className="shrink-0 pb-1 text-xs text-muted-foreground">
                {t('inviteGuide.summary', { eligible: plan.eligible, slots: plan.total_reward_slots })}
              </p>
            </div>
          </div>

          {/* 受邀邮箱预算：邮箱有限时，把名额分给单次收益最高的号才是最优解。 */}
          <div className="flex flex-wrap items-center gap-2">
            <label className="text-xs text-muted-foreground" htmlFor="invite-guide-budget">
              {t('inviteGuide.emailBudgetLabel')}
            </label>
            <input
              id="invite-guide-budget"
              value={emailBudget}
              onChange={(e) => setEmailBudget(e.target.value.replace(/[^0-9]/g, ''))}
              inputMode="numeric"
              placeholder={t('inviteGuide.emailBudgetPlaceholder')}
              className="h-8 w-24 rounded-lg border bg-background px-2.5 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/40"
            />
            {loading && <Loader2 className="size-3.5 animate-spin text-muted-foreground" />}
          </div>

          {/* 账号列表，已按建议顺序排好 */}
          <div className="space-y-2">
            {plan.accounts.map((account, index) => (
              <div key={account.id} className="rounded-xl border bg-background px-3 py-2.5">
                <div className="flex items-center gap-2">
                  <span className="flex size-6 shrink-0 items-center justify-center rounded-md bg-muted text-[11px] font-semibold text-muted-foreground">
                    {account.state === 'eligible' ? index + 1 : '·'}
                  </span>
                  <span className="min-w-0 flex-1 truncate text-sm font-medium text-foreground">
                    {account.email || `#${account.id}`}
                  </span>
                  {account.plan_type && (
                    <span className="shrink-0 rounded-full bg-muted px-2 py-0.5 text-[10px] text-muted-foreground">
                      {account.plan_type}
                    </span>
                  )}
                  <span className={`shrink-0 rounded-full px-2 py-0.5 text-[11px] font-semibold ${stateTone(account.state)}`}>
                    {t(`inviteGuide.state.${account.state}`)}
                  </span>
                </div>

                <div className="mt-1 flex flex-wrap items-center gap-x-3 gap-y-0.5 pl-8 text-xs text-muted-foreground">
                  {account.state === 'eligible' && (
                    <>
                      {typeof account.grant_amount === 'number' && account.grant_amount > 0 && (
                        <span>{t('inviteGuide.perInvite', { amount: formatCredits(account.grant_amount) })}</span>
                      )}
                      {typeof account.remaining_reward_capacity === 'number' && (
                        <span>{t('inviteGuide.remainingReward', { count: account.remaining_reward_capacity })}</span>
                      )}
                      <span className="font-medium text-emerald-600">
                        {t('inviteGuide.suggested', {
                          count: account.suggested_invites,
                          credits: formatCredits(account.potential_credits),
                        })}
                      </span>
                    </>
                  )}
                  {account.state === 'exhausted' && <span>{t('inviteGuide.exhaustedHint')}</span>}
                  {account.state === 'ineligible' && (
                    <span className="break-all">{account.ineligible_reason || t('inviteGuide.ineligibleNoReason')}</span>
                  )}
                  {account.state === 'pending' && <span>{t('inviteGuide.pendingHint')}</span>}
                </div>
              </div>
            ))}
          </div>

          {plan.pending > 0 && (
            <div className="flex flex-wrap items-center justify-between gap-2 rounded-xl border border-dashed px-3 py-2.5 text-xs text-muted-foreground">
              <span className="flex items-center gap-1.5">
                <Loader2 className="size-3.5 animate-spin" />
                {t('inviteGuide.probing', { count: plan.pending })}
              </span>
              <button
                type="button"
                onClick={() => void handleProbeMore()}
                disabled={probing}
                className="rounded-lg border bg-background px-2.5 py-1 font-medium transition-colors hover:text-foreground disabled:opacity-50"
              >
                {t('inviteGuide.probeMore')}
              </button>
            </div>
          )}

          {plan.unprobed > 0 && (
            <p className="text-xs text-muted-foreground">
              {t('inviteGuide.sampled', { probed: plan.probe_cap, total: plan.total })}
            </p>
          )}
        </div>
      )}
    </Modal>
  )
}
