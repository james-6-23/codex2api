import { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import type { KeyboardEvent as ReactKeyboardEvent } from 'react'
import { useTranslation } from 'react-i18next'
import { useSearchParams } from 'react-router-dom'
import {
  AlertTriangle,
  ArrowLeft,
  Check,
  ChevronDown,
  Clock,
  Copy,
  Gift,
  History,
  Loader2,
  Mail,
  RefreshCw,
  Send,
  Sparkles,
  UserCircle2,
  Users,
  X,
} from 'lucide-react'
import PageHeader from './PageHeader'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { api } from '../api'
import type {
  AccountRow,
  InviteCacheMeta,
  InviteEligibility,
  InviteGuideAccountPlan,
  InviteRecipientRecord,
  InviteResult,
  InviteTrackingItem,
} from '../types'
import { getErrorMessage } from '../utils/error'
import { useToast } from '../hooks/useToast'
import {
  alreadyInvitedEmails,
  inviteRecipientRecord,
  inviteRecipientCandidates,
  isCodexInviteSenderCandidate,
  mergeInviteRecipientIndex,
  normalizeInviteEmail,
  normalizeInviteEmails,
  type InviteRecipientIndex,
} from '../lib/inviteAccountSelection'

interface Props {
  accounts: AccountRow[]
  onClose: () => void
  // loading 区分「账号还没拉回来」与「确实没有可用账号」。直接深链进本页（/accounts/invite）
  // 时账号列表为空是正常的加载中状态，若按空列表渲染会误报「没有可用于邀请的账号」。
  loading?: boolean
}

const MAX_EMAILS = 10
const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/
const SPLIT_RE = /[,;\r\n\t ]+/

interface ParsedEmails {
  valid: string[]
  invalid: string[]
  duplicates: number
}

// 与后端 collectInviteEmails 保持一致：按分隔符切分、去重（忽略大小写）、正则校验。
function parseEmails(text: string): ParsedEmails {
  const tokens = text.split(SPLIT_RE).map((s) => s.trim()).filter(Boolean)
  const seen = new Set<string>()
  const valid: string[] = []
  const invalid: string[] = []
  let duplicates = 0
  for (const tk of tokens) {
    if (!EMAIL_RE.test(tk)) {
      invalid.push(tk)
      continue
    }
    const key = tk.toLowerCase()
    if (seen.has(key)) {
      duplicates++
      continue
    }
    seen.add(key)
    valid.push(tk)
  }
  return { valid, invalid, duplicates }
}

function accountDisplayName(account: AccountRow): string {
  return account.email || account.name || `#${account.id}`
}

function accountSearchText(account: AccountRow): string {
  return [
    String(account.id),
    `#${account.id}`,
    account.email,
    account.name,
    account.status,
    account.plan_type,
  ]
    .filter(Boolean)
    .join(' ')
    .toLowerCase()
}

// 状态圆点配色，与全局 StatusBadge 保持一致。
const STATUS_DOT_COLOR: Record<string, string> = {
  active: 'bg-emerald-500',
  ready: 'bg-emerald-500',
  cooldown: 'bg-amber-500',
  rate_limited: 'bg-yellow-500',
  usage_exhausted: 'bg-yellow-500',
  quota_paused: 'bg-yellow-500',
  unauthorized: 'bg-red-500',
  error: 'bg-red-400',
  refreshing: 'bg-blue-500 animate-pulse',
  paused: 'bg-blue-500',
}

function statusDotColor(status?: string | null): string {
  return STATUS_DOT_COLOR[(status || '').toLowerCase()] ?? 'bg-gray-400'
}

// 邀请记录状态配色。上游 status 实测有 redeemed / pending，其余走兜底样式。
const INVITE_STATUS_TONE: Record<string, string> = {
  redeemed: 'bg-emerald-500/10 text-emerald-600',
  accepted: 'bg-emerald-500/10 text-emerald-600',
  pending: 'bg-amber-500/10 text-amber-600',
  sent: 'bg-amber-500/10 text-amber-600',
  expired: 'bg-muted text-muted-foreground',
  revoked: 'bg-red-500/10 text-red-600',
}

function inviteStatusTone(status?: string): string {
  return INVITE_STATUS_TONE[(status || '').toLowerCase()] ?? 'bg-muted text-muted-foreground'
}

// 上游返回 ISO 时间串；解析失败时原样显示，不吞掉信息。
function formatInviteTime(value?: string): string {
  if (!value) return '-'
  const date = new Date(value)
  return Number.isNaN(date.getTime()) ? value : date.toLocaleString()
}

// 从 time_frame_rules 里取指定维度的规则（send = 发送次数、reward = 奖励次数）。
function findCapacityRule(eligibility: InviteEligibility | null, capacityType: string) {
  return eligibility?.time_frame_rules?.find((rule) => rule.capacity_type === capacityType) ?? null
}

// 锁定用于保护账号不被自动清理，并不代表邀请凭证失效，保留提示帮助用户辨认。
function accountAbnormalKey(account: AccountRow): 'locked' | null {
  if (account.locked) return 'locked'
  return null
}

function resolveAccountInput(accounts: AccountRow[], input: string): AccountRow | null {
  const normalized = input.trim().toLowerCase()
  if (!normalized) return null
  return accounts.find((account) => {
    const id = String(account.id)
    const name = account.name?.trim().toLowerCase()
    const normalizedNameWithID = normalized.replace(/\s+#(?=\d+$)/, '#')
    return (
      normalized === id ||
      normalized === `#${id}` ||
      normalized === account.email?.trim().toLowerCase() ||
      normalized === name ||
      (Boolean(name) && normalizedNameWithID === `${name}#${id}`)
    )
  }) ?? null
}

// CodexInviteView 是账号管理页内的「Codex 邀请」视图，入口与回收站一致。
export default function CodexInviteView({ accounts, onClose, loading = false }: Props) {
  const { t } = useTranslation()
  const { showToast } = useToast()

  // 引导弹窗跳转过来时会带 ?account=<邮箱>。用邮箱而不是 ID：选择器是服务端分页
  // 搜索，邮箱当查询词能把目标账号捞进候选集；传 ID 的话账号若不在首页 100 条里，
  // 会被下面「不在候选集就清空」的 effect 抹掉。
  const [searchParams] = useSearchParams()
  const presetAccount = (searchParams.get('account') ?? '').trim()

  const [pickerAccounts, setPickerAccounts] = useState<AccountRow[]>(accounts)
  const [pickerLoading, setPickerLoading] = useState(loading)

  // 仅保留状态正常且可用于 referral 的 Codex OAuth 账号；选择器由服务端分页搜索驱动。
  const codexAccounts = useMemo(
    () => pickerAccounts.filter(isCodexInviteSenderCandidate),
    [pickerAccounts],
  )
  const firstAccount = codexAccounts[0] ?? null

  const [accountId, setAccountId] = useState<number | null>(firstAccount?.id ?? null)
  const [accountQuery, setAccountQuery] = useState(() =>
    presetAccount || (firstAccount ? accountDisplayName(firstAccount) : ''))
  const [accountOpen, setAccountOpen] = useState(false)
  // accountTyping 区分「用户正在输入搜索」与「输入框只是回显已选账号」。仅在输入时
  // 才按文本过滤，否则展开下拉应显示全部账号（否则会被已选账号的邮箱过滤成只剩一条）。
  const [accountTyping, setAccountTyping] = useState(Boolean(presetAccount))
  // 下拉键盘导航的高亮项索引（指向 filteredAccounts）。-1 表示未高亮任何项。
  const [activeIndex, setActiveIndex] = useState(-1)
  const [emailsText, setEmailsText] = useState('')
  const [inviteRecipientIndex, setInviteRecipientIndex] = useState<InviteRecipientIndex>({})
  const [checkedRecipientSignature, setCheckedRecipientSignature] = useState('')
  const [recipientCheckPending, setRecipientCheckPending] = useState(false)
  const [recipientCheckError, setRecipientCheckError] = useState<string | null>(null)
  const [showAdvanced, setShowAdvanced] = useState(false)
  const [proxyUrl, setProxyUrl] = useState('')
  const [sending, setSending] = useState(false)
  const [result, setResult] = useState<InviteResult | null>(null)
  const [error, setError] = useState<string | null>(null)
  const [eligibility, setEligibility] = useState<InviteEligibility | null>(null)
  const [tracking, setTracking] = useState<InviteTrackingItem[] | null>(null)
  const [infoLoading, setInfoLoading] = useState(false)
  const [eligibilityError, setEligibilityError] = useState<string | null>(null)
  const [trackingError, setTrackingError] = useState<string | null>(null)
  // 网关下发的数据新鲜度。source=upstream 是刚拉的，其余来自缓存，要把观测时刻
  // 显示出来——否则用户看到的剩余次数可能是十几分钟前的，却毫无提示。
  const [eligibilityMeta, setEligibilityMeta] = useState<InviteCacheMeta | null>(null)
  const [trackingMeta, setTrackingMeta] = useState<InviteCacheMeta | null>(null)
  // 下拉里每个账号的邀请积分。只读网关缓存，绝不在展开时顺手探测——一次下拉最多
  // 100 个账号，自动探测等于开合一次就打 100 个 Cloudflare 防护端点。没有缓存的
  // 账号不显示徽标，由用户点「探测积分」显式触发。
  const [creditsMap, setCreditsMap] = useState<Record<number, InviteGuideAccountPlan>>({})
  const [creditsProbing, setCreditsProbing] = useState(false)
  const accountPickerRef = useRef<HTMLDivElement>(null)
  // 代理走 ref：它只是查询时的可选参数，不该让每次输入都重新拉取上游。
  const proxyUrlRef = useRef(proxyUrl)
  // 递增序号用于丢弃过期响应（快速切换账号时后到的旧响应不能覆盖新账号的数据）。
  const infoSeqRef = useRef(0)

  useEffect(() => {
    const controller = new AbortController()
    const timer = window.setTimeout(() => {
      setPickerLoading(true)
      void api.getAccountsPage({
        channel: 'codex',
        page: 1,
        pageSize: 100,
        search: accountTyping ? accountQuery.trim() : undefined,
      }, controller.signal)
        .then((response) => {
          if (!controller.signal.aborted) setPickerAccounts(response.accounts ?? [])
        })
        .catch(() => undefined)
        .finally(() => {
          if (!controller.signal.aborted) setPickerLoading(false)
        })
    }, accountTyping ? 250 : 0)
    return () => {
      window.clearTimeout(timer)
      controller.abort()
    }
  }, [accountQuery, accountTyping])

  const parsed = useMemo(() => parseEmails(emailsText), [emailsText])
  const inputRecipientEmails = useMemo(() => normalizeInviteEmails(parsed.valid), [parsed.valid])
  const inputRecipientSignature = inputRecipientEmails.join('\n')
  const mergeInviteRecipients = useCallback((recipients: InviteRecipientRecord[]) => {
    setInviteRecipientIndex((current) => mergeInviteRecipientIndex(current, recipients))
  }, [])

  // 手动粘贴的邮箱也必须走持久化记录检查，不能只依赖下拉行上的 disabled。
  // signature 让输入一变化就立即失去「已检查」状态，防止防抖窗口内误点发送。
  useEffect(() => {
    if (!inputRecipientSignature) {
      setCheckedRecipientSignature('')
      setRecipientCheckPending(false)
      setRecipientCheckError(null)
      return
    }

    const controller = new AbortController()
    setRecipientCheckPending(true)
    setRecipientCheckError(null)
    const timer = window.setTimeout(() => {
      void api.checkInviteRecipients(inputRecipientEmails, controller.signal)
        .then((response) => {
          if (controller.signal.aborted) return
          mergeInviteRecipients(response.recipients ?? [])
          setCheckedRecipientSignature(inputRecipientSignature)
        })
        .catch((err) => {
          if (!controller.signal.aborted) setRecipientCheckError(getErrorMessage(err))
        })
        .finally(() => {
          if (!controller.signal.aborted) setRecipientCheckPending(false)
        })
    }, 250)

    return () => {
      window.clearTimeout(timer)
      controller.abort()
    }
  }, [inputRecipientEmails, inputRecipientSignature, mergeInviteRecipients])

  const duplicateInviteEmails = useMemo(
    () => alreadyInvitedEmails(parsed.valid, inviteRecipientIndex),
    [parsed.valid, inviteRecipientIndex],
  )
  const recipientsChecked =
    inputRecipientSignature === '' || checkedRecipientSignature === inputRecipientSignature
  // 收件人选择器的勾选状态直接派生自输入框文本（单一事实来源）：手动输入的邮箱
  // 在下拉里自动呈已选态，勾选/取消即增删输入框内容，两个入口不会各存一份。
  const selectedRecipientEmails = useMemo(() => {
    const set = new Set<string>()
    for (const token of emailsText.split(SPLIT_RE)) {
      const value = token.trim()
      if (value) set.add(value.toLowerCase())
    }
    return set
  }, [emailsText])
  const toggleRecipientEmail = useCallback((email: string) => {
    const key = email.trim().toLowerCase()
    setEmailsText((prev) => {
      const tokens = prev.split(SPLIT_RE).map((token) => token.trim()).filter(Boolean)
      if (tokens.some((token) => token.toLowerCase() === key)) {
        return tokens.filter((token) => token.toLowerCase() !== key).join('\n')
      }
      return tokens.length > 0 ? `${tokens.join('\n')}\n${email.trim()}` : email.trim()
    })
  }, [])
  const selectedAccount = useMemo(
    () => codexAccounts.find((a) => a.id === accountId) ?? null,
    [codexAccounts, accountId],
  )
  const filteredAccounts = useMemo(() => {
    // 未在输入搜索时显示全部；正在输入才按文本过滤。
    if (!accountTyping) return codexAccounts
    const query = accountQuery.trim().toLowerCase()
    if (!query) return codexAccounts
    return codexAccounts.filter((account) => accountSearchText(account).includes(query))
  }, [accountTyping, accountQuery, codexAccounts])
  const overLimit = parsed.valid.length > MAX_EMAILS
  // 上游明确回 0 才算配额用尽；字段缺失（undefined）时不做任何拦截。
  const sendCapacity = eligibility?.remaining_send_capacity
  const rewardCapacity = eligibility?.remaining_reward_capacity
  const sendCapacityExhausted = sendCapacity === 0
  const rewardCapacityExhausted = rewardCapacity === 0
  const overCapacity = typeof sendCapacity === 'number' && sendCapacity > 0 && parsed.valid.length > sendCapacity
  // should_show=false 是上游给的硬性无资格结论，直接拦住，省一次注定 4xx 的请求。
  const ineligible = eligibility != null && eligibility.ok && !eligibility.should_show
  const canSend =
    !sending &&
    accountQuery.trim() !== '' &&
    parsed.valid.length > 0 &&
    parsed.invalid.length === 0 &&
    !overLimit &&
    recipientsChecked &&
    !recipientCheckError &&
    duplicateInviteEmails.length === 0 &&
    !sendCapacityExhausted &&
    !ineligible
  // 锁定账号仍可邀请，但保留其保护状态提示。
  const selectedAbnormal = useMemo(
    () => (selectedAccount ? accountAbnormalKey(selectedAccount) : null),
    [selectedAccount],
  )

  // 统一的选中逻辑：下拉点击、键盘回车共用。
  const selectAccount = (account: AccountRow) => {
    setAccountId(account.id)
    setAccountQuery(accountDisplayName(account))
    setAccountOpen(false)
    setAccountTyping(false)
    setActiveIndex(-1)
    setError(null)
  }

  // 打开下拉或过滤结果变化时，重置高亮到当前选中项（没有则不高亮）。
  useEffect(() => {
    if (!accountOpen) {
      setActiveIndex(-1)
      return
    }
    setActiveIndex((prev) => {
      if (prev >= 0 && prev < filteredAccounts.length) return prev
      const selectedIdx = filteredAccounts.findIndex((a) => a.id === accountId)
      return selectedIdx >= 0 ? selectedIdx : filteredAccounts.length > 0 ? 0 : -1
    })
  }, [accountOpen, filteredAccounts, accountId])

  // 下拉键盘导航：↑↓ 移动高亮、回车确认、Esc 关闭。
  const handlePickerKeyDown = (event: ReactKeyboardEvent<HTMLInputElement>) => {
    if (event.key === 'Escape') {
      if (accountOpen) {
        event.preventDefault()
        setAccountOpen(false)
      }
      return
    }
    if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
      event.preventDefault()
      if (!accountOpen) {
        setAccountOpen(true)
        setAccountTyping(false)
        return
      }
      if (filteredAccounts.length === 0) return
      setActiveIndex((prev) => {
        const delta = event.key === 'ArrowDown' ? 1 : -1
        const base = prev < 0 ? (delta === 1 ? -1 : 0) : prev
        return (base + delta + filteredAccounts.length) % filteredAccounts.length
      })
      return
    }
    if (event.key === 'Enter') {
      if (accountOpen && activeIndex >= 0 && activeIndex < filteredAccounts.length) {
        event.preventDefault()
        selectAccount(filteredAccounts[activeIndex])
      }
    }
  }

  // URL 预设账号只解析一次：等服务端搜索把它带回候选集后按精确匹配选中。
  // 用 ref 上锁，避免用户随后手动改选时被这个 effect 拽回预设值。
  const presetAppliedRef = useRef(false)
  useEffect(() => {
    if (presetAppliedRef.current || !presetAccount || accountId != null) return
    const matched = resolveAccountInput(codexAccounts, presetAccount)
    if (!matched) return
    presetAppliedRef.current = true
    setAccountId(matched.id)
    setAccountQuery(accountDisplayName(matched))
    setAccountTyping(false)
  }, [presetAccount, codexAccounts, accountId])

  useEffect(() => {
    if (accountId == null) return
    if (codexAccounts.some((a) => a.id === accountId)) return
    setAccountId(null)
    setAccountQuery('')
  }, [accountId, codexAccounts])

  useEffect(() => {
    proxyUrlRef.current = proxyUrl
  }, [proxyUrl])

  // 拉取资格与已发邀请。两个请求串行发出而非并发：上游端点在 Cloudflare bot 管理后面，
  // 后端按账号复用 cookie，第一个请求拿到的 __cf_bm 能让第二个请求少被挑战。
  // 一个失败不影响另一个展示。
  // force=true 让网关绕过资格/记录缓存直连上游：手动刷新与发送邀请后的重拉都要带，
  // 否则刚被消耗掉的配额会继续按缓存值显示。
  const loadInviteInfo = useCallback(async (id: number, force = false) => {
    const seq = ++infoSeqRef.current
    setInfoLoading(true)
    setEligibilityError(null)
    setTrackingError(null)
    const proxy = proxyUrlRef.current.trim() || undefined

    try {
      const res = await api.getInviteEligibility(id, { proxy_url: proxy, refresh: force })
      if (seq !== infoSeqRef.current) return
      setEligibility(res.result)
      setEligibilityMeta(res.cache ?? null)
    } catch (err) {
      if (seq !== infoSeqRef.current) return
      setEligibility(null)
      setEligibilityMeta(null)
      setEligibilityError(getErrorMessage(err))
    }

    try {
      const res = await api.getInviteTracking(id, { proxy_url: proxy, refresh: force })
      if (seq !== infoSeqRef.current) return
      setTracking(res.result.challenged ? null : res.result.items ?? [])
      setTrackingMeta(res.result.challenged ? null : res.cache ?? null)
      setTrackingError(res.result.challenged ? t('invite.challengedRetry') : null)
    } catch (err) {
      if (seq !== infoSeqRef.current) return
      setTracking(null)
      setTrackingMeta(null)
      setTrackingError(getErrorMessage(err))
    }

    if (seq !== infoSeqRef.current) return
    setInfoLoading(false)
  }, [t])

  // 切换账号时重新拉取。清空必须发生在发起请求之前：序号守卫只防「后到的旧响应
  // 覆盖新数据」，防不住「新账号还在路上时页面仍渲染着上一个账号的配额和记录」——
  // 那一两秒里账号名已经变了，剩余次数却还是上一个号的，用户会照着错的数字发。
  useEffect(() => {
    infoSeqRef.current++
    setEligibility(null)
    setTracking(null)
    setEligibilityMeta(null)
    setTrackingMeta(null)
    setEligibilityError(null)
    setTrackingError(null)
    if (accountId == null) {
      setInfoLoading(false)
      return
    }
    void loadInviteInfo(accountId)
  }, [accountId, loadInviteInfo])

  // 合并而不是替换：翻页/改搜索词时保留已取到的值，避免徽标闪烁消失。
  const loadVisibleCredits = useCallback(async (ids: number[]) => {
    if (ids.length === 0) return
    try {
      const plan = await api.getInviteGuidePlan(ids)
      setCreditsMap((prev) => {
        const next = { ...prev }
        for (const item of plan.accounts) next[item.id] = item
        return next
      })
    } catch {
      /* 积分是附加信息，取不到就不显示，不打扰主流程 */
    }
  }, [])

  useEffect(() => {
    if (!accountOpen || filteredAccounts.length === 0) return
    const ids = filteredAccounts.map((a) => a.id)
    const timer = window.setTimeout(() => void loadVisibleCredits(ids), 300)
    return () => window.clearTimeout(timer)
  }, [accountOpen, filteredAccounts, loadVisibleCredits])

  // 只探当前可见且还没有结果的账号。后端按 50 封顶并走导入闸门排队，
  // 这里探完后轮询几次回读，不做无限等待。
  const probeCreditsByIds = async (candidateIds: number[]) => {
    const ids = candidateIds.filter((id) => !creditsMap[id] || creditsMap[id].state === 'pending')
    if (ids.length === 0 || creditsProbing) return
    setCreditsProbing(true)
    try {
      await api.probeInviteGuidePlan(ids)
      for (let i = 0; i < 5; i++) {
        await new Promise((resolve) => window.setTimeout(resolve, 2500))
        await loadVisibleCredits(ids)
      }
    } catch (err) {
      showToast(getErrorMessage(err), 'error')
    } finally {
      setCreditsProbing(false)
    }
  }
  const probeVisibleCredits = () => probeCreditsByIds(filteredAccounts.map((a) => a.id))

  useEffect(() => {
    const handlePointerDown = (event: PointerEvent) => {
      const target = event.target
      if (target instanceof Node && accountPickerRef.current?.contains(target)) return
      setAccountOpen(false)
    }
    document.addEventListener('pointerdown', handlePointerDown)
    return () => document.removeEventListener('pointerdown', handlePointerDown)
  }, [])

  const handleSend = async () => {
    const accountInput = accountQuery.trim()
    if (!accountInput) {
      setError(t('invite.noAccountSelected'))
      return
    }
    const account = selectedAccount ?? resolveAccountInput(codexAccounts, accountInput)
    if (!account) {
      setError(t('invite.accountNotFound'))
      showToast(t('invite.accountNotFound'), 'error')
      return
    }
    if (parsed.valid.length === 0) {
      setError(t('invite.noValidEmails'))
      return
    }
    setAccountId(account.id)
    setAccountQuery(accountDisplayName(account))
    setSending(true)
    setError(null)
    setResult(null)
    try {
      const res = await api.sendInvite(account.id, {
        emails: parsed.valid,
        proxy_url: proxyUrl.trim() || undefined,
      })
      setResult(res.result)
      if (res.ok) {
        const invitedAt = new Date().toISOString()
        mergeInviteRecipients((res.recorded_emails ?? []).map((email) => ({
          email,
          state: 'sent',
          sender_account_id: account.id,
          invited_at: invitedAt,
        })))
        setEmailsText('')
        showToast(t('invite.sendSuccess'), 'success')
      } else {
        showToast(t('invite.sendUpstreamFailed', { code: res.result.status_code }), 'error')
      }
      // 无论成败都刷新配额与记录：成功要扣减剩余次数，失败也可能是配额已被别处用掉。
      // 强制回上游——网关侧虽然在发送后已清缓存，但这里再兜一次，避免任何一层残留。
      void loadInviteInfo(account.id, true)
    } catch (err) {
      setError(getErrorMessage(err))
      showToast(t('invite.sendFailed', { error: getErrorMessage(err) }), 'error')
    } finally {
      setSending(false)
    }
  }

  return (
    <div>
      <PageHeader
        title={t('invite.title')}
        description={t('invite.description')}
        actions={
          <div className="flex flex-wrap items-center justify-end gap-1.5">
            <Button variant="outline" onClick={onClose} className="max-sm:w-full">
              <ArrowLeft className="size-3.5" />
              {t('invite.back')}
            </Button>
          </div>
        }
      />

      {/* 宽屏水平三列：账号/资格 | 发送表单 | 邀请记录；窄屏纵向堆叠 */}
      <div className="mt-4 space-y-5">
        {codexAccounts.length === 0 ? (
          <div className="mx-auto max-w-2xl">
            <EmptyState message={pickerLoading ? t('invite.accountsLoading') : t('invite.noCodexAccounts')} spinning={pickerLoading} />
          </div>
        ) : (
          <div className="grid grid-cols-1 items-stretch gap-5 lg:grid-cols-2 xl:min-h-[calc(100dvh-12rem)] xl:grid-cols-12">
            {/* 左列：账号选择 + 资格配额；与中/右列拉齐等高 */}
            <section className="flex min-h-0 min-w-0 flex-col gap-5 xl:col-span-4">
              <div className="shrink-0 rounded-2xl border bg-card shadow-sm">
                <div className="border-b px-5 py-4">
                  <div className="flex items-center gap-2">
                    <div className="flex size-8 items-center justify-center rounded-lg bg-muted text-muted-foreground">
                      <UserCircle2 className="size-4" />
                    </div>
                    <div className="min-w-0">
                      <h3 className="text-sm font-semibold leading-tight">{t('invite.accountLabel')}</h3>
                      <p className="text-xs text-muted-foreground">{t('invite.accountHint')}</p>
                    </div>
                  </div>
                </div>
                <div className="p-5">
                  <div ref={accountPickerRef} className="relative">
                    <div className="relative">
                      <Input
                        value={accountQuery}
                        onFocus={() => { setAccountOpen(true); setAccountTyping(false) }}
                        onClick={() => { setAccountOpen(true); setAccountTyping(false) }}
                        onKeyDown={handlePickerKeyDown}
                        onChange={(e) => {
                          const next = e.target.value
                          setAccountQuery(next)
                          setAccountOpen(true)
                          setAccountTyping(true)
                          setAccountId(resolveAccountInput(codexAccounts, next)?.id ?? null)
                          if (error === t('invite.accountNotFound')) setError(null)
                        }}
                        placeholder={t('invite.accountPlaceholder')}
                        role="combobox"
                        autoComplete="off"
                        aria-expanded={accountOpen}
                        aria-controls="codex-invite-account-list"
                        aria-activedescendant={
                          accountOpen && activeIndex >= 0 && activeIndex < filteredAccounts.length
                            ? `codex-invite-option-${filteredAccounts[activeIndex].id}`
                            : undefined
                        }
                        className="h-10 pr-9"
                      />
                      <button
                        type="button"
                        onClick={() => { setAccountOpen((open) => !open); setAccountTyping(false) }}
                        className="absolute inset-y-0 right-0 inline-flex w-9 items-center justify-center text-muted-foreground transition-colors hover:text-foreground"
                        aria-label={t('invite.accountToggle')}
                      >
                        <ChevronDown className={`size-4 transition-transform ${accountOpen ? 'rotate-180' : ''}`} />
                      </button>
                    </div>
                    {accountOpen && (
                      <div className="absolute z-30 mt-1.5 w-full overflow-hidden rounded-lg border bg-popover text-popover-foreground shadow-lg">
                      <div
                        id="codex-invite-account-list"
                        role="listbox"
                        className="max-h-[min(60dvh,30rem)] overflow-auto p-1"
                      >
                        {filteredAccounts.length > 0 ? (
                          filteredAccounts.map((account, index) => {
                            const active = account.id === accountId
                            const highlighted = index === activeIndex
                            const abnormal = accountAbnormalKey(account)
                            return (
                              <button
                                key={account.id}
                                id={`codex-invite-option-${account.id}`}
                                type="button"
                                role="option"
                                aria-selected={active}
                                ref={highlighted ? (el) => el?.scrollIntoView({ block: 'nearest' }) : undefined}
                                onMouseDown={(event) => event.preventDefault()}
                                onMouseEnter={() => setActiveIndex(index)}
                                onClick={() => selectAccount(account)}
                                className={`flex w-full items-center gap-2 rounded-md px-2.5 py-2 text-left text-sm transition-colors ${
                                  highlighted ? 'bg-accent text-accent-foreground' : 'hover:bg-accent/70 hover:text-accent-foreground'
                                }`}
                              >
                                <span className="flex size-7 shrink-0 items-center justify-center rounded-md bg-muted text-[11px] font-semibold text-muted-foreground">
                                  #{account.id}
                                </span>
                                <span className="min-w-0 flex-1">
                                  <span className="flex items-center gap-1.5">
                                    <span className={`size-1.5 shrink-0 rounded-full ${statusDotColor(account.status)}`} />
                                    <span className="truncate font-medium">{accountDisplayName(account)}</span>
                                    {abnormal && (
                                      <span className="shrink-0 rounded-full bg-muted px-1.5 py-0.5 text-[10px] font-medium text-muted-foreground">
                                        {t(`invite.state.${abnormal}`)}
                                      </span>
                                    )}
                                  </span>
                                  <span className="flex items-center gap-1.5 text-xs text-muted-foreground">
                                    <span className="truncate">
                                      {[account.name && account.name !== account.email ? account.name : '', account.plan_type, account.status]
                                        .filter(Boolean)
                                        .join(' · ') || '-'}
                                    </span>
                                    <InviteStatsInline plan={creditsMap[account.id]} />
                                  </span>
                                </span>
                                <InviteCreditsBadge plan={creditsMap[account.id]} />
                                {active && <Check className="size-4 shrink-0 text-primary" />}
                              </button>
                            )
                          })
                        ) : (
                          <div className="px-3 py-6 text-center text-sm text-muted-foreground">
                            {t('invite.noAccountMatches')}
                          </div>
                        )}
                      </div>
                      {filteredAccounts.length > 0 && (
                        <div className="flex items-center justify-between gap-2 border-t bg-muted/30 px-2.5 py-1.5 text-[11px] text-muted-foreground">
                          <span>{t('invite.creditsHint')}</span>
                          <button
                            type="button"
                            // preventDefault 保持输入框焦点，否则点一下就把下拉关了。
                            onMouseDown={(event) => event.preventDefault()}
                            onClick={() => void probeVisibleCredits()}
                            disabled={creditsProbing}
                            className="inline-flex shrink-0 items-center gap-1 rounded-md border bg-background px-2 py-0.5 font-medium transition-colors hover:text-foreground disabled:opacity-50"
                          >
                            {creditsProbing && <Loader2 className="size-3 animate-spin" />}
                            {creditsProbing ? t('invite.creditsProbing') : t('invite.creditsProbe')}
                          </button>
                        </div>
                      )}
                      </div>
                    )}
                  </div>
                  {selectedAccount && (
                    <div className="mt-3 flex flex-wrap items-center gap-1.5">
                      {selectedAccount.plan_type && (
                        <InfoPill label={t('invite.planLabel')} value={selectedAccount.plan_type} />
                      )}
                      <InfoPill label={t('invite.statusLabel')} value={selectedAccount.status || '-'} />
                    </div>
                  )}
                  {selectedAbnormal && (
                    <p className="mt-2 flex items-start gap-1.5 text-xs text-amber-600">
                      <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
                      <span>{t('invite.abnormalHint', { state: t(`invite.state.${selectedAbnormal}`) })}</span>
                    </p>
                  )}
                </div>
              </div>

              {accountId != null ? (
                <EligibilityPanel
                  eligibility={eligibility}
                  loading={infoLoading}
                  error={eligibilityError}
                  meta={eligibilityMeta}
                  onRefresh={() => void loadInviteInfo(accountId, true)}
                  className="min-h-0 flex-1"
                />
              ) : (
                <div className="min-h-0 flex-1 rounded-2xl border border-dashed bg-card/60 shadow-sm" aria-hidden />
              )}
            </section>

            {/* 中列：邮箱输入与发送 */}
            <section className="flex min-h-0 min-w-0 flex-col xl:col-span-4">
              <div className="flex h-full min-h-[22rem] flex-col rounded-2xl border bg-card shadow-sm">
                <div className="shrink-0 border-b px-5 py-4">
                  <div className="flex items-center gap-2">
                    <div className="flex size-8 items-center justify-center rounded-lg bg-muted text-muted-foreground">
                      <Mail className="size-4" />
                    </div>
                    <div className="min-w-0">
                      <h3 className="text-sm font-semibold leading-tight">{t('invite.emailsLabel')}</h3>
                      <p className="text-xs text-muted-foreground">{t('invite.emailsHint')}</p>
                    </div>
                  </div>
                </div>
                <div className="flex min-h-0 flex-1 flex-col p-5">
                  <div className="mb-2 flex items-start gap-2">
                    <div className="min-w-0 flex-1">
                      <RecipientAccountPicker
                        selectedEmails={selectedRecipientEmails}
                        onToggle={toggleRecipientEmail}
                        excludeEmail={selectedAccount?.email}
                        inviteRecipientIndex={inviteRecipientIndex}
                        onRecipientsChecked={mergeInviteRecipients}
                        creditsMap={creditsMap}
                        onLoadCredits={loadVisibleCredits}
                        onProbeCredits={probeCreditsByIds}
                        probing={creditsProbing}
                      />
                    </div>
                    {emailsText.trim() !== '' && (
                      <button
                        type="button"
                        onClick={() => setEmailsText('')}
                        title={t('invite.clearEmails')}
                        className="inline-flex shrink-0 items-center gap-1.5 rounded-lg border bg-background px-2.5 py-1.5 text-xs font-medium text-muted-foreground transition-colors hover:text-foreground"
                      >
                        <X className="size-3.5" />
                        {t('invite.clearEmails')}
                      </button>
                    )}
                  </div>
                  <textarea
                    value={emailsText}
                    onChange={(e) => setEmailsText(e.target.value)}
                    rows={6}
                    placeholder={t('invite.emailsPlaceholder')}
                    className="min-h-[8rem] w-full flex-1 resize-none rounded-lg border bg-background px-3 py-2 font-mono text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/40"
                  />

                  {(parsed.valid.length > 0 || parsed.invalid.length > 0 || parsed.duplicates > 0) && (
                    <div className="mt-3 flex flex-wrap items-center gap-1.5">
                      <CountPill tone="success" text={t('invite.parsedValid', { count: parsed.valid.length })} />
                      {parsed.duplicates > 0 && (
                        <CountPill tone="muted" text={t('invite.parsedDuplicate', { count: parsed.duplicates })} />
                      )}
                      {parsed.invalid.length > 0 && (
                        <CountPill tone="danger" text={t('invite.parsedInvalid', { count: parsed.invalid.length })} />
                      )}
                    </div>
                  )}
                  {parsed.invalid.length > 0 && (
                    <p className="mt-1.5 break-all text-xs text-red-500">
                      {t('invite.invalidList')} {parsed.invalid.join(', ')}
                    </p>
                  )}
                  {recipientCheckPending && parsed.valid.length > 0 && (
                    <p className="mt-1.5 flex items-center gap-1.5 text-xs text-muted-foreground">
                      <Loader2 className="size-3.5 animate-spin" />
                      <span>{t('invite.recipientChecking')}</span>
                    </p>
                  )}
                  {recipientCheckError && parsed.valid.length > 0 && (
                    <p className="mt-1.5 flex items-start gap-1.5 text-xs text-red-500">
                      <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
                      <span>{t('invite.recipientCheckFailed', { error: recipientCheckError })}</span>
                    </p>
                  )}
                  {duplicateInviteEmails.length > 0 && (
                    <p className="mt-1.5 flex items-start gap-1.5 text-xs text-red-500">
                      <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
                      <span className="break-all">
                        {t('invite.alreadyInvitedEmails', { emails: duplicateInviteEmails.join(', ') })}
                      </span>
                    </p>
                  )}
                  {overLimit && (
                    <p className="mt-1.5 flex items-center gap-1 text-xs text-amber-600">
                      <AlertTriangle className="size-3.5" />
                      {t('invite.overLimit', { max: MAX_EMAILS })}
                    </p>
                  )}

                  {/* 配额提示：0 是硬拦截，超出剩余次数只警告（配额是月度累计，本地数据可能已过时）。 */}
                  {ineligible && (
                    <p className="mt-1.5 flex items-start gap-1.5 text-xs text-red-500">
                      <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
                      <span>{t('invite.blockedIneligible')}</span>
                    </p>
                  )}
                  {sendCapacityExhausted && (
                    <p className="mt-1.5 flex items-start gap-1.5 text-xs text-red-500">
                      <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
                      <span>{t('invite.blockedSendCapacity')}</span>
                    </p>
                  )}
                  {!sendCapacityExhausted && overCapacity && (
                    <p className="mt-1.5 flex items-start gap-1.5 text-xs text-amber-600">
                      <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
                      <span>{t('invite.warnOverCapacity', { remaining: sendCapacity, count: parsed.valid.length })}</span>
                    </p>
                  )}
                  {!sendCapacityExhausted && rewardCapacityExhausted && (
                    <p className="mt-1.5 flex items-start gap-1.5 text-xs text-amber-600">
                      <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
                      <span>{t('invite.warnRewardExhausted')}</span>
                    </p>
                  )}

                  {showAdvanced && (
                    <div className="mt-4 rounded-xl border bg-muted/30 p-3">
                      <label className="mb-1 block text-xs font-medium text-muted-foreground">
                        {t('invite.proxyLabel')}
                      </label>
                      <input
                        value={proxyUrl}
                        onChange={(e) => setProxyUrl(e.target.value)}
                        placeholder={t('invite.proxyPlaceholder')}
                        className="h-9 w-full rounded-lg border bg-background px-3 text-sm focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-primary/40"
                      />
                    </div>
                  )}

                  {error && <div className="mt-3 text-sm text-red-500">{error}</div>}

                  {/* 次级操作(高级选项)与主 CTA 同一行:左 toggle、右发送,消除底部两行堆叠 */}
                  <div className="mt-auto flex items-center justify-between gap-3 pt-4">
                    <button
                      type="button"
                      onClick={() => setShowAdvanced((v) => !v)}
                      className="inline-flex items-center gap-1 text-xs font-medium text-muted-foreground transition-colors hover:text-foreground"
                    >
                      <ChevronDown className={`size-3.5 transition-transform ${showAdvanced ? 'rotate-180' : ''}`} />
                      {t('invite.advanced')}
                    </button>
                    <Button disabled={!canSend} onClick={() => void handleSend()} className="min-w-[8.5rem]">
                      <Send className="size-3.5" />
                      {sending
                        ? t('invite.sending')
                        : parsed.valid.length > 0
                          ? t('invite.sendCount', { count: parsed.valid.length })
                          : t('invite.send')}
                    </Button>
                  </div>
                </div>
              </div>
            </section>

            {/* 右列：邀请记录；中屏整行铺开，宽屏与左右列并排等高 */}
            <section className="flex min-h-0 min-w-0 flex-col lg:col-span-2 xl:col-span-4">
              {accountId != null ? (
                <TrackingCard
                  items={tracking}
                  loading={infoLoading}
                  error={trackingError}
                  meta={trackingMeta}
                  onRefresh={() => void loadInviteInfo(accountId, true)}
                  className="h-full min-h-[22rem]"
                />
              ) : (
                <div className="flex h-full min-h-[22rem] flex-col items-center justify-center rounded-2xl border border-dashed bg-card/60 px-6 py-12 text-center shadow-sm">
                  <div className="mb-3 flex size-10 items-center justify-center rounded-xl bg-muted text-muted-foreground">
                    <History className="size-5" />
                  </div>
                  <p className="text-sm font-medium text-foreground">{t('invite.trackingTitle')}</p>
                  <p className="mt-1 text-xs text-muted-foreground">{t('invite.trackingSelectAccount')}</p>
                </div>
              )}
            </section>
          </div>
        )}

        {result && <InviteResultCard result={result} />}
      </div>
    </div>
  )
}

// EligibilityPanel 展示上游给的资格结论、剩余配额与官方文案。
// 文案由上游按 Oai-Language 下发（目前上游 i18n 不完整，会中英混排），原样透传不做二次翻译。
function EligibilityPanel({
  eligibility,
  loading,
  error,
  meta,
  onRefresh,
  className,
}: {
  eligibility: InviteEligibility | null
  loading: boolean
  error: string | null
  meta: InviteCacheMeta | null
  onRefresh: () => void
  className?: string
}) {
  const { t } = useTranslation()
  const sendRule = findCapacityRule(eligibility, 'send')
  const rewardRule = findCapacityRule(eligibility, 'reward')
  const shell = `flex flex-col rounded-2xl border bg-card shadow-sm ${className ?? ''}`

  if (loading && !eligibility) {
    return (
      <div className={`${shell} items-center justify-center p-5 text-sm text-muted-foreground`}>
        <Loader2 className="size-4 animate-spin" />
        <span className="mt-2">{t('invite.eligibilityLoading')}</span>
      </div>
    )
  }

  if (error) {
    return (
      <div className={`${shell} p-5 text-sm text-muted-foreground`}>
        <div className="flex items-start gap-2">
          <AlertTriangle className="mt-0.5 size-4 shrink-0 text-amber-600" />
          <span className="min-w-0 flex-1 break-all">{t('invite.eligibilityLoadFailed', { error })}</span>
          <RefreshButton onClick={onRefresh} />
        </div>
      </div>
    )
  }

  if (!eligibility) return null

  // 上游非 2xx：把状态码摊开，不假装有资格。被 Cloudflare 挑战时状态码同为 403，
  // 但含义是「没问到」而非「没资格」，必须分开说，否则会把正常账号误报成无权限。
  if (!eligibility.ok) {
    return (
      <div className={`flex flex-col rounded-2xl border border-amber-500/30 bg-amber-500/5 p-5 text-sm text-amber-700 shadow-sm dark:text-amber-300 ${className ?? ''}`}>
        <div className="flex items-start gap-2">
          <AlertTriangle className="mt-0.5 size-4 shrink-0" />
          <span className="min-w-0 flex-1">
            {eligibility.challenged
              ? t('invite.challengedRetry')
              : eligibility.upstream_message ||
                t('invite.eligibilityUpstreamFailed', { code: eligibility.status_code })}
          </span>
          <RefreshButton onClick={onRefresh} />
        </div>
      </div>
    )
  }

  return (
    <div className={shell}>
      <div className="flex shrink-0 items-start gap-3 border-b px-5 py-4">
        <div className="flex size-8 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-primary">
          <Gift className="size-4" />
        </div>
        <div className="min-w-0 flex-1">
          <div className="flex flex-wrap items-center gap-1.5">
            <span className="text-sm font-semibold">{eligibility.title || t('invite.eligibilityTitle')}</span>
            {eligibility.offer_id && (
              <span className="rounded-full bg-muted px-1.5 py-0.5 font-mono text-[10px] text-muted-foreground">
                {eligibility.offer_id}
              </span>
            )}
          </div>
          {eligibility.description && (
            <p className="mt-1 text-xs leading-relaxed text-muted-foreground">{eligibility.description}</p>
          )}
        </div>
        {loading ? (
          <Loader2 className="mt-0.5 size-3.5 shrink-0 animate-spin text-muted-foreground" />
        ) : (
          <div className="flex shrink-0 items-center gap-1.5">
            <FreshnessHint meta={meta} />
            <RefreshButton onClick={onRefresh} />
          </div>
        )}
      </div>

      <div className="flex min-h-0 flex-1 flex-col gap-3 p-5">
        {!eligibility.should_show && (
          <p className="flex items-start gap-1.5 text-xs text-amber-600">
            <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
            <span>
              {eligibility.ineligible_reason ||
                t('invite.ineligibleNoReason', { code: eligibility.ineligible_reason_code || '-' })}
            </span>
          </p>
        )}

        {/* 两种配额分开展示：能发几封 ≠ 能拿几次奖励，后者才决定还有没有收益。 */}
        <div className="flex flex-wrap items-center gap-1.5">
          <CapacityPill
            label={t('invite.remainingSend')}
            remaining={eligibility.remaining_send_capacity}
            used={sendRule?.invites_sent}
            total={sendRule?.invites_total}
          />
          <CapacityPill
            label={t('invite.remainingReward')}
            remaining={eligibility.remaining_reward_capacity}
            used={rewardRule?.invites_sent}
            total={rewardRule?.invites_total}
            highlight
          />
        </div>

        {eligibility.rules && eligibility.rules.length > 0 && (
          <ul className="space-y-1.5 rounded-xl border bg-muted/20 px-3 py-2.5 text-xs text-muted-foreground">
            {eligibility.rules.map((rule, i) => (
              <li key={i} className="flex items-start gap-1.5">
                <span className="mt-1.5 size-1 shrink-0 rounded-full bg-muted-foreground/50" />
                <span className="leading-relaxed">{rule}</span>
              </li>
            ))}
          </ul>
        )}
      </div>
    </div>
  )
}

// CapacityPill 展示单个维度的剩余配额。remaining 为 undefined 表示上游没给这个字段，
// 显示为「未知」而不是 0 —— 两者的处理方式完全不同。
function CapacityPill({
  label,
  remaining,
  used,
  total,
  highlight,
}: {
  label: string
  remaining?: number
  used?: number
  total?: number
  highlight?: boolean
}) {
  const { t } = useTranslation()
  const unknown = typeof remaining !== 'number'
  const exhausted = remaining === 0
  const cls = unknown
    ? 'bg-muted text-muted-foreground'
    : exhausted
      ? 'bg-red-500/10 text-red-600'
      : highlight
        ? 'bg-primary/10 text-primary'
        : 'bg-emerald-500/10 text-emerald-600'
  const detail =
    typeof used === 'number' && typeof total === 'number' ? ` (${used}/${total})` : ''
  return (
    <span className={`inline-flex items-center gap-1 rounded-full px-2.5 py-1 text-xs font-semibold ${cls}`}>
      <span className="font-normal opacity-70">{label}</span>
      <span>{unknown ? t('invite.capacityUnknown') : `${remaining}${detail}`}</span>
    </span>
  )
}

// TrackingCard 展示该账号已发出的邀请及兑换状态（上游 90 天窗口）。
function TrackingCard({
  items,
  loading,
  error,
  meta,
  onRefresh,
  className,
}: {
  items: InviteTrackingItem[] | null
  loading: boolean
  error: string | null
  meta: InviteCacheMeta | null
  onRefresh: () => void
  className?: string
}) {
  const { t } = useTranslation()

  return (
    <div className={`flex flex-col rounded-2xl border bg-card shadow-sm ${className ?? ''}`}>
      <div className="flex items-center gap-2 border-b px-5 py-4">
        <div className="flex size-8 items-center justify-center rounded-lg bg-muted text-muted-foreground">
          <History className="size-4" />
        </div>
        <div className="min-w-0 flex-1">
          <h4 className="text-sm font-semibold leading-tight">{t('invite.trackingTitle')}</h4>
          <p className="text-xs text-muted-foreground">{t('invite.trackingDescription')}</p>
        </div>
        <div className="flex items-center gap-1.5">
          {loading && <Loader2 className="size-3.5 animate-spin text-muted-foreground" />}
          {!loading && <FreshnessHint meta={meta} />}
          <RefreshButton onClick={onRefresh} />
        </div>
      </div>

      <div className="flex min-h-0 flex-1 flex-col overflow-auto p-5">
        {error ? (
          <p className="flex items-start gap-1.5 break-all text-sm text-amber-600">
            <AlertTriangle className="mt-0.5 size-3.5 shrink-0" />
            <span>{t('invite.trackingLoadFailed', { error })}</span>
          </p>
        ) : items == null ? (
          <div className="flex flex-1 flex-col items-center justify-center py-10 text-center">
            <Loader2 className="size-5 animate-spin text-muted-foreground" />
            <p className="mt-3 text-sm text-muted-foreground">{t('invite.trackingLoading')}</p>
          </div>
        ) : items.length === 0 ? (
          <div className="flex flex-1 flex-col items-center justify-center py-10 text-center">
            <div className="mb-3 flex size-10 items-center justify-center rounded-xl bg-muted text-muted-foreground">
              <History className="size-5" />
            </div>
            <p className="text-sm font-medium text-foreground">{t('invite.trackingEmpty')}</p>
            <p className="mt-1 max-w-56 text-xs leading-relaxed text-muted-foreground">
              {t('invite.trackingEmptyHint')}
            </p>
          </div>
        ) : (
          <div className="space-y-2">
            {items.map((item, i) => (
              <div
                key={item.referral_id || item.email || i}
                className="rounded-xl border bg-background px-3 py-2.5"
              >
                <div className="flex items-center gap-2">
                  <span className="min-w-0 flex-1 truncate text-sm font-medium text-foreground">
                    {item.email || '-'}
                  </span>
                  <span className={`shrink-0 rounded-full px-2 py-0.5 text-[11px] font-semibold ${inviteStatusTone(item.status)}`}>
                    {item.status || '-'}
                  </span>
                  {item.invite_url && <CopyButton text={item.invite_url} />}
                </div>
                <div className="mt-1 flex flex-wrap items-center gap-x-3 gap-y-0.5 text-xs text-muted-foreground">
                  <span>{t('invite.trackingCreatedAt', { time: formatInviteTime(item.created_at) })}</span>
                  {item.expires_at && (
                    <span>{t('invite.trackingExpiresAt', { time: formatInviteTime(item.expires_at) })}</span>
                  )}
                </div>
              </div>
            ))}
          </div>
        )}
      </div>
    </div>
  )
}

// FreshnessHint 只在数据来自缓存时出现。刚打过上游（source=upstream）不提示——
// 那是默认预期，多挂一个「刚刚」标签只是噪音。
function FreshnessHint({ meta }: { meta: InviteCacheMeta | null }) {
  const { t } = useTranslation()
  if (!meta || meta.source === 'upstream' || !meta.observed_at) return null
  const observed = new Date(meta.observed_at)
  if (Number.isNaN(observed.getTime())) return null
  return (
    <span
      title={t('invite.cachedHint')}
      className="inline-flex shrink-0 items-center gap-1 rounded-full bg-muted px-2 py-0.5 text-[10px] font-medium text-muted-foreground"
    >
      <Clock className="size-3" />
      {observed.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })}
    </span>
  )
}

// InviteStatsInline 在下拉副行追加邀请维度的小信息：单次能拿多少积分、发了多少封、
// 被接受了几封。没有数据的字段直接不渲染——用 0 占位会被读成「一封都没发过」。
//
// 「已发」优先取跟踪记录（近 90 天），此时和「已接受」同属一个窗口，两个数字可比；
// 没有跟踪数据才退回资格接口的本月发送用量，并明确标注「本月」，避免让人以为
// 这两个数来自同一个统计口径。
function InviteStatsInline({ plan }: { plan?: InviteGuideAccountPlan }) {
  const { t } = useTranslation()
  if (!plan || plan.state === 'pending') return null

  const parts: string[] = []
  if (typeof plan.grant_amount === 'number' && plan.grant_amount > 0) {
    parts.push(t('invite.statPerInvite', { amount: Math.round(plan.grant_amount).toLocaleString() }))
  }
  // 无资格账号且从没发过邀请时不显示「已发 0 · 已接受 0」——行上已经有「无资格」
  // 徽标，再挂两个 0 只是噪音。发过的仍然显示：那是有用的历史。
  const hideEmptyStats = plan.state === 'ineligible' && !plan.invites_sent
  if (typeof plan.invites_sent === 'number' && !hideEmptyStats) {
    parts.push(t('invite.statSent', { count: plan.invites_sent }))
    if (typeof plan.invites_accepted === 'number') {
      parts.push(t('invite.statAccepted', { count: plan.invites_accepted }))
    }
  } else if (typeof plan.monthly_sent === 'number' && !hideEmptyStats) {
    parts.push(
      typeof plan.monthly_send_total === 'number'
        ? t('invite.statMonthlySentOf', { sent: plan.monthly_sent, total: plan.monthly_send_total })
        : t('invite.statMonthlySent', { sent: plan.monthly_sent }),
    )
  }
  if (parts.length === 0) return null

  return <span className="shrink-0 whitespace-nowrap text-muted-foreground/80">· {parts.join(' · ')}</span>
}

// InviteCreditsBadge 显示该账号还能拿到多少邀请积分（单次奖励额度 × 剩余奖励次数）。
// 没探测过（pending / 无数据）时不渲染任何东西——留白比显示一个「0」诚实，
// 后者会被读成「这个号没积分了」。
function InviteCreditsBadge({ plan }: { plan?: InviteGuideAccountPlan }) {
  const { t } = useTranslation()
  if (!plan || plan.state === 'pending') return null

  if (plan.state === 'ineligible') {
    return (
      <span className="shrink-0 rounded-full bg-muted px-1.5 py-0.5 text-[10px] font-medium text-muted-foreground">
        {t('invite.creditsIneligible')}
      </span>
    )
  }
  // 还能发但本月奖励次数已用尽：发了也拿不到积分，与「无资格」是两回事。
  if (plan.state === 'exhausted') {
    return (
      <span className="shrink-0 rounded-full bg-amber-500/10 px-1.5 py-0.5 text-[10px] font-medium text-amber-600">
        {t('invite.creditsExhausted')}
      </span>
    )
  }
  return (
    <span
      title={t('invite.creditsTooltip', {
        amount: Math.round(plan.grant_amount ?? 0).toLocaleString(),
        count: plan.remaining_reward_capacity ?? 0,
      })}
      className="shrink-0 rounded-full bg-emerald-500/10 px-1.5 py-0.5 text-[10px] font-semibold tabular-nums text-emerald-600"
    >
      +{Math.round(plan.potential_credits).toLocaleString()}
    </span>
  )
}

// RecipientAccountPicker 从账号列表挑选受邀邮箱，支持单选与连续多选（勾选不关闭
// 下拉）。候选由服务端分页搜索驱动，与左列账号选择器同一条数据通道；排除禁用、
// 错误和封禁账号，只展示有邮箱的账号，按邮箱去重（team 空间会让同一登录邮箱出现
// 在多个账号行里），并排除当前发起账号自己——给自己发邀请必然被上游拒绝。
function RecipientAccountPicker({
  selectedEmails,
  onToggle,
  excludeEmail,
  inviteRecipientIndex,
  onRecipientsChecked,
  creditsMap,
  onLoadCredits,
  onProbeCredits,
  probing,
}: {
  selectedEmails: Set<string>
  onToggle: (email: string) => void
  excludeEmail?: string
  inviteRecipientIndex: InviteRecipientIndex
  onRecipientsChecked: (recipients: InviteRecipientRecord[]) => void
  // 与发起方下拉共享同一份积分数据：任一侧探测过的账号，另一侧立即可见。
  creditsMap: Record<number, InviteGuideAccountPlan>
  onLoadCredits: (ids: number[]) => void
  onProbeCredits: (ids: number[]) => void
  probing: boolean
}) {
  const { t } = useTranslation()
  const [open, setOpen] = useState(false)
  const [query, setQuery] = useState('')
  const [rows, setRows] = useState<AccountRow[]>([])
  const [loading, setLoading] = useState(false)
  const [checkedCandidateEmails, setCheckedCandidateEmails] = useState<Set<string>>(new Set())
  const [recipientCheckError, setRecipientCheckError] = useState<string | null>(null)
  const containerRef = useRef<HTMLDivElement>(null)

  // 只在展开时拉取，收起后不做任何后台请求。输入搜索词走 250ms 防抖，
  // 首次展开立即取；AbortController 取消在途请求，防止慢响应覆盖新词的结果。
  useEffect(() => {
    if (!open) return
    const controller = new AbortController()
    const timer = window.setTimeout(() => {
      setLoading(true)
      void api.getAccountsPage({
        channel: 'codex',
        page: 1,
        pageSize: 100,
        search: query.trim() || undefined,
      }, controller.signal)
        .then((response) => {
          if (!controller.signal.aborted) setRows(response.accounts ?? [])
        })
        .catch(() => undefined)
        .finally(() => {
          if (!controller.signal.aborted) setLoading(false)
        })
    }, query ? 250 : 0)
    return () => {
      window.clearTimeout(timer)
      controller.abort()
    }
  }, [open, query])

  useEffect(() => {
    if (!open) return
    const handlePointerDown = (event: PointerEvent) => {
      const target = event.target
      if (target instanceof Node && containerRef.current?.contains(target)) return
      setOpen(false)
    }
    document.addEventListener('pointerdown', handlePointerDown)
    return () => document.removeEventListener('pointerdown', handlePointerDown)
  }, [open])

  const candidates = useMemo(
    () => inviteRecipientCandidates(rows, excludeEmail),
    [rows, excludeEmail],
  )
  const candidateEmails = useMemo(
    () => normalizeInviteEmails(candidates.map((row) => row.email ?? '')),
    [candidates],
  )

  // 下拉每一页候选一次批量查询邀请记录。返回列表只包含已经存在的邮箱；请求中的
  // 其他邮箱在本轮完成后才允许选择，避免状态还没回来时短暂放行重复邀请。
  useEffect(() => {
    setCheckedCandidateEmails(new Set())
    setRecipientCheckError(null)
    if (!open || candidateEmails.length === 0) return

    const controller = new AbortController()
    void api.checkInviteRecipients(candidateEmails, controller.signal)
      .then((response) => {
        if (controller.signal.aborted) return
        onRecipientsChecked(response.recipients ?? [])
        setCheckedCandidateEmails(new Set(candidateEmails))
      })
      .catch((err) => {
        if (!controller.signal.aborted) setRecipientCheckError(getErrorMessage(err))
      })
    return () => controller.abort()
  }, [open, candidateEmails, onRecipientsChecked])

  const creditCandidates = useMemo(
    () => candidates.filter((row) => {
      const email = row.email ?? ''
      const key = normalizeInviteEmail(email)
      return checkedCandidateEmails.has(key) && !inviteRecipientRecord(inviteRecipientIndex, email)
    }),
    [candidates, checkedCandidateEmails, inviteRecipientIndex],
  )

  // 候选可见时回读网关缓存里的积分（纯缓存读，零上游），让「单次 250」这类
  // 信息直接标在行上——单次收益低的号更适合当受邀方，一眼可辨。
  useEffect(() => {
    if (!open || creditCandidates.length === 0) return
    const ids = creditCandidates.map((row) => row.id)
    const timer = window.setTimeout(() => onLoadCredits(ids), 300)
    return () => window.clearTimeout(timer)
  }, [open, creditCandidates, onLoadCredits])

  return (
    <div ref={containerRef} className="relative">
      <button
        type="button"
        onClick={() => setOpen((value) => !value)}
        aria-expanded={open}
        className="inline-flex items-center gap-1.5 rounded-lg border bg-background px-2.5 py-1.5 text-xs font-medium text-muted-foreground transition-colors hover:text-foreground"
      >
        <Users className="size-3.5" />
        {t('invite.recipientPicker')}
        <ChevronDown className={`size-3.5 transition-transform ${open ? 'rotate-180' : ''}`} />
      </button>
      {open && (
        <div className="absolute left-0 right-0 z-30 mt-1.5 overflow-hidden rounded-lg border bg-popover text-popover-foreground shadow-lg">
          <div className="flex items-center gap-2 border-b p-1.5">
            <Input
              autoFocus
              value={query}
              onChange={(e) => setQuery(e.target.value)}
              onKeyDown={(e) => {
                if (e.key === 'Escape') {
                  e.preventDefault()
                  setOpen(false)
                }
              }}
              placeholder={t('invite.recipientSearch')}
              className="h-8 text-sm"
            />
            {loading && <Loader2 className="mr-1 size-3.5 shrink-0 animate-spin text-muted-foreground" />}
          </div>
          <div role="listbox" aria-multiselectable="true" className="max-h-[min(52dvh,26rem)] overflow-auto p-1">
            {candidates.length > 0 ? (
              candidates.map((row) => {
                const email = row.email?.trim() ?? ''
                const key = normalizeInviteEmail(email)
                const checked = selectedEmails.has(key)
                const invited = inviteRecipientRecord(inviteRecipientIndex, email)
                const statusChecked = checkedCandidateEmails.has(key) || Boolean(invited)
                const disabled = Boolean(invited) || !statusChecked
                return (
                  <button
                    key={row.id}
                    type="button"
                    role="option"
                    aria-selected={checked}
                    aria-disabled={disabled}
                    disabled={disabled}
                    // preventDefault 保住输入框焦点，连续勾选时不触发下拉外的失焦。
                    onMouseDown={(event) => event.preventDefault()}
                    onClick={() => onToggle(email)}
                    className="flex w-full items-center gap-2 rounded-md px-2.5 py-1.5 text-left text-sm transition-colors hover:bg-accent/70 hover:text-accent-foreground disabled:cursor-not-allowed disabled:opacity-60 disabled:hover:bg-transparent"
                  >
                    <span
                      className={`flex size-4 shrink-0 items-center justify-center rounded border transition-colors ${
                        checked ? 'border-primary bg-primary text-primary-foreground' : 'border-input'
                      }`}
                    >
                      {checked && <Check className="size-3" />}
                    </span>
                    <span className="min-w-0 flex-1 truncate">{email}</span>
                    {invited ? (
                      <span className="shrink-0 rounded-full bg-amber-500/10 px-1.5 py-0.5 text-[10px] font-medium text-amber-700 dark:text-amber-300">
                        {t('invite.recipientInvited')}
                      </span>
                    ) : (
                      <RecipientCreditsHint plan={creditsMap[row.id]} />
                    )}
                    {row.plan_type && (
                      <span className="shrink-0 rounded-full bg-muted px-1.5 py-0.5 text-[10px] text-muted-foreground">
                        {row.plan_type}
                      </span>
                    )}
                  </button>
                )
              })
            ) : (
              <div className="px-3 py-6 text-center text-sm text-muted-foreground">
                {loading ? t('invite.recipientLoading') : t('invite.noAccountMatches')}
              </div>
            )}
          </div>
          <div className="flex items-center justify-between gap-2 border-t bg-muted/30 px-2.5 py-1.5 text-[11px] text-muted-foreground">
            <span className={`min-w-0 truncate ${recipientCheckError ? 'text-red-500' : ''}`}>
              {recipientCheckError
                ? t('invite.recipientCheckFailed', { error: recipientCheckError })
                : t('invite.recipientToggleHint')}
            </span>
            <button
              type="button"
              onMouseDown={(event) => event.preventDefault()}
              onClick={() => onProbeCredits(creditCandidates.map((row) => row.id))}
              disabled={probing || creditCandidates.length === 0}
              className="inline-flex shrink-0 items-center gap-1 rounded-md border bg-background px-2 py-0.5 font-medium transition-colors hover:text-foreground disabled:opacity-50"
            >
              {probing && <Loader2 className="size-3 animate-spin" />}
              {probing ? t('invite.creditsProbing') : t('invite.creditsProbe')}
            </button>
          </div>
        </div>
      )}
    </div>
  )
}

// RecipientCreditsHint 在收件人候选行上标注该账号「作为发起方」的单次邀请收益。
// 这是挑受邀方的核心依据：单次只有 250 的号当发起方不划算，牺牲它当受邀方最优；
// 「无资格」的号自己发不了邀请，当受邀方毫无机会成本。没探测过则留白，不编 0。
function RecipientCreditsHint({ plan }: { plan?: InviteGuideAccountPlan }) {
  const { t } = useTranslation()
  if (!plan || plan.state === 'pending') return null
  if (plan.state === 'ineligible') {
    return (
      <span className="shrink-0 rounded-full bg-muted px-1.5 py-0.5 text-[10px] font-medium text-muted-foreground">
        {t('invite.creditsIneligible')}
      </span>
    )
  }
  if (plan.state === 'exhausted') {
    return (
      <span className="shrink-0 rounded-full bg-amber-500/10 px-1.5 py-0.5 text-[10px] font-medium text-amber-600">
        {t('invite.creditsExhausted')}
      </span>
    )
  }
  if (typeof plan.grant_amount === 'number' && plan.grant_amount > 0) {
    return (
      <span className="shrink-0 rounded-full bg-muted px-1.5 py-0.5 text-[10px] font-medium tabular-nums text-muted-foreground">
        {t('invite.statPerInvite', { amount: Math.round(plan.grant_amount).toLocaleString() })}
      </span>
    )
  }
  return null
}

function RefreshButton({ onClick }: { onClick: () => void }) {
  const { t } = useTranslation()
  return (
    <button
      type="button"
      onClick={onClick}
      title={t('invite.refresh')}
      aria-label={t('invite.refresh')}
      className="inline-flex size-7 shrink-0 items-center justify-center rounded-lg border bg-background text-muted-foreground transition-colors hover:text-foreground"
    >
      <RefreshCw className="size-3.5" />
    </button>
  )
}

function InviteResultCard({ result }: { result: InviteResult }) {
  const { t } = useTranslation()
  const [showRaw, setShowRaw] = useState(false)
  const rawText =
    result.upstream != null
      ? JSON.stringify(result.upstream, null, 2)
      : result.upstream_raw || ''

  return (
    <div className="rounded-2xl border bg-card shadow-sm">
      <div className="flex items-center gap-2 border-b px-5 py-4">
        <div className={`flex size-8 items-center justify-center rounded-lg ${result.ok ? 'bg-emerald-500/10 text-emerald-600' : 'bg-red-500/10 text-red-600'}`}>
          {result.ok ? <Check className="size-4" /> : <AlertTriangle className="size-4" />}
        </div>
        <div className="min-w-0 flex-1">
          <h4 className="text-sm font-semibold leading-tight">{t('invite.resultTitle')}</h4>
          <p className="text-xs text-muted-foreground">
            {result.ok
              ? t('invite.resultOkDesc', { count: result.emails.length })
              : t('invite.resultFailed', { code: result.status_code })}
          </p>
        </div>
        {result.request_id && (
          <span className="hidden rounded-full bg-muted px-2.5 py-1 font-mono text-[11px] text-muted-foreground sm:inline">
            {result.request_id}
          </span>
        )}
      </div>

      <div className="space-y-3 p-5">
        {/* 被 Cloudflare 挑战：状态码也是 403，但邀请并未发出，且与资格无关，提示重试。 */}
        {!result.ok && result.challenged && (
          <div className="flex items-start gap-2 rounded-xl border border-amber-500/30 bg-amber-500/5 p-3 text-sm text-amber-700 dark:text-amber-300">
            <AlertTriangle className="mt-0.5 size-4 shrink-0" />
            <span>{t('invite.challengedRetry')}</span>
          </div>
        )}

        {/* 上游给了具体原因就直接显示它（如「此人已收到推荐邀请」），并列出被拒邮箱。
            这类失败是收件人级的，账号资格完好，不能套用下面那条无资格提示。 */}
        {!result.ok && !result.challenged && result.upstream_message && (
          <div className="rounded-xl border border-amber-500/30 bg-amber-500/5 p-3 text-sm text-amber-700 dark:text-amber-300">
            <div className="flex items-start gap-2">
              <AlertTriangle className="mt-0.5 size-4 shrink-0" />
              <span className="min-w-0 flex-1">{result.upstream_message}</span>
            </div>
            {result.failed_emails && result.failed_emails.length > 0 && (
              <p className="mt-1.5 break-all pl-6 text-xs">
                {t('invite.failedEmails')} {result.failed_emails.join(', ')}
              </p>
            )}
          </div>
        )}

        {/* 无资格的兜底提示：仅在既不是挑战、上游又没给具体原因时才用这句推测。 */}
        {!result.ok && !result.challenged && !result.upstream_message && result.status_code === 403 && (
          <div className="flex items-start gap-2 rounded-xl border border-amber-500/30 bg-amber-500/5 p-3 text-sm text-amber-700 dark:text-amber-300">
            <Sparkles className="mt-0.5 size-4 shrink-0" />
            <span>{t('invite.eligibilityHint')}</span>
          </div>
        )}

        {/* 邀请明细 */}
        {result.invites && result.invites.length > 0 && (
          <div className="space-y-2">
            {result.invites.map((inv, i) => (
              <div
                key={inv.referral_id || inv.email || i}
                className="flex items-center justify-between gap-3 rounded-xl border bg-background px-3 py-2.5"
              >
                <div className="min-w-0">
                  <div className="truncate text-sm font-medium text-foreground">{inv.email || '-'}</div>
                  {inv.invite_url && (
                    <a
                      href={inv.invite_url}
                      target="_blank"
                      rel="noreferrer"
                      className="block truncate text-xs text-primary hover:underline"
                    >
                      {inv.invite_url}
                    </a>
                  )}
                </div>
                {inv.invite_url && <CopyButton text={inv.invite_url} />}
              </div>
            ))}
          </div>
        )}

        {/* 原始响应（折叠） */}
        {rawText && (
          <div>
            <button
              type="button"
              onClick={() => setShowRaw((v) => !v)}
              className="inline-flex items-center gap-1 text-xs font-medium text-muted-foreground transition-colors hover:text-foreground"
            >
              <ChevronDown className={`size-3.5 transition-transform ${showRaw ? 'rotate-180' : ''}`} />
              {t('invite.rawResponse')}
            </button>
            {showRaw && (
              <pre className="mt-2 max-h-64 overflow-auto rounded-lg border bg-muted/40 p-3 text-xs">
                {rawText}
              </pre>
            )}
          </div>
        )}
      </div>
    </div>
  )
}

function CopyButton({ text }: { text: string }) {
  const { t } = useTranslation()
  const [copied, setCopied] = useState(false)
  const handleCopy = async () => {
    try {
      await navigator.clipboard.writeText(text)
      setCopied(true)
      window.setTimeout(() => setCopied(false), 1500)
    } catch {
      /* 忽略剪贴板权限错误 */
    }
  }
  return (
    <button
      type="button"
      onClick={() => void handleCopy()}
      title={copied ? t('invite.copied') : t('invite.copy')}
      className="inline-flex size-8 shrink-0 items-center justify-center rounded-lg border bg-background text-muted-foreground transition-colors hover:text-foreground"
    >
      {copied ? <Check className="size-3.5 text-emerald-600" /> : <Copy className="size-3.5" />}
    </button>
  )
}

function EmptyState({ message, spinning }: { message: string; spinning?: boolean }) {
  return (
    <div className="flex flex-col items-center justify-center rounded-2xl border border-dashed bg-card py-16 text-center">
      <div className="mb-3 flex size-12 items-center justify-center rounded-2xl bg-muted text-muted-foreground">
        {spinning ? <Loader2 className="size-6 animate-spin" /> : <Mail className="size-6" />}
      </div>
      <p className="text-sm text-muted-foreground">{message}</p>
    </div>
  )
}

function InfoPill({ label, value }: { label: string; value: string }) {
  return (
    <span className="inline-flex items-center gap-1 rounded-full bg-muted/60 px-2.5 py-1 text-xs text-muted-foreground">
      <span className="text-muted-foreground/70">{label}</span>
      <span className="font-medium text-foreground">{value}</span>
    </span>
  )
}

function CountPill({ tone, text }: { tone: 'success' | 'danger' | 'muted'; text: string }) {
  const cls =
    tone === 'success'
      ? 'bg-emerald-500/10 text-emerald-600'
      : tone === 'danger'
        ? 'bg-red-500/10 text-red-600'
        : 'bg-muted text-muted-foreground'
  return <span className={`inline-flex items-center rounded-full px-2.5 py-1 text-xs font-semibold ${cls}`}>{text}</span>
}
