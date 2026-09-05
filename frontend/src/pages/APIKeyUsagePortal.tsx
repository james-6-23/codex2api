import { type FormEvent, type ReactNode, useEffect, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { NavLink, useNavigate, useParams } from 'react-router-dom'
import { Cell, Pie, PieChart, ResponsiveContainer, Tooltip as RechartsTooltip } from 'recharts'
import {
  Activity,
  AlertTriangle,
  BarChart3,
  Box,
  Brain,
  Clock,
  Clock3,
  CircleDollarSign,
  DatabaseZap,
  Eye,
  EyeOff,
  Info,
  KeyRound,
  Languages,
  Loader2,
  LogIn,
  LogOut,
  Moon,
  RefreshCw,
  Route,
  ShieldCheck,
  Sun,
  Zap,
} from 'lucide-react'
import { api } from '../api'
import { DEFAULT_SITE_LOGO, useBranding } from '../branding'
import Pagination from '../components/Pagination'
import CompactionBadges from '../components/CompactionBadges'
import APIKeyModelRequestUsageCard from '../components/APIKeyModelRequestUsage'
import { useTheme } from '../hooks/useTheme'
import { usePersistedPageSize } from '../hooks/usePersistedPageSize'
import type {
  APIKeyLimits,
  PublicAPIKeyUsageBreakdown,
  PublicAPIKeyUsageLog,
  PublicAPIKeyUsageResponse,
  PublicAPIKeyWindowUsage,
} from '../types'
import { getErrorMessage } from '../utils/error'
import { formatBeijingTime, formatRelativeTime } from '../utils/time'
import { Badge } from '@/components/ui/badge'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import {
  Table,
  TableBody,
  TableCell,
  TableHead,
  TableHeader,
  TableRow,
} from '@/components/ui/table'
import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from '@/components/ui/tooltip'

type UsageRange = 'today' | '7d' | '30d' | 'all'

const STORAGE_KEY = 'codex2api_key_usage_api_key'
const RANGE_OPTIONS: UsageRange[] = ['today', '7d', '30d', 'all']
const LOG_PAGE_SIZE_OPTIONS = [10, 20, 25, 50, 100]

const USAGE_VIEWS = ['overview', 'quota', 'logs'] as const
type UsageView = typeof USAGE_VIEWS[number]

function normalizeUsageView(value?: string): UsageView {
  return USAGE_VIEWS.includes(value as UsageView) ? (value as UsageView) : 'overview'
}

export default function APIKeyUsagePortal() {
  const { t, i18n } = useTranslation()
  const { siteName, siteLogo } = useBranding()
  const { theme, toggle } = useTheme()
  const { view } = useParams()
  const navigate = useNavigate()
  const activeView = normalizeUsageView(view)
  const [apiKeyInput, setAPIKeyInput] = useState(() => readStoredAPIKey().key)
  const [remember, setRemember] = useState(() => {
    const stored = readStoredAPIKey()
    return stored.key ? stored.remember : true
  })
  const [activeAPIKey, setActiveAPIKey] = useState(() => readStoredAPIKey().key)
  const [showKey, setShowKey] = useState(false)
  const [range, setRange] = useState<UsageRange>('30d')
  const [logPage, setLogPage] = useState(1)
  const [logPageSize, setLogPageSize] = usePersistedPageSize('key_usage_logs', 25, LOG_PAGE_SIZE_OPTIONS)
  const [loading, setLoading] = useState(false)
  const [bootstrapping, setBootstrapping] = useState(() => Boolean(readStoredAPIKey().key))
  const [error, setError] = useState('')
  const [data, setData] = useState<PublicAPIKeyUsageResponse | null>(null)
  const logoSrc = siteLogo || DEFAULT_SITE_LOGO

  // 已登录后的刷新/切换时间范围（沿用当前会话的 key）
  const reload = async (nextRange = range, nextLogPage = logPage, nextLogPageSize = logPageSize) => {
    if (!activeAPIKey) return
    setLoading(true)
    setError('')
    try {
      const result = await api.getPublicAPIKeyUsage(activeAPIKey, nextRange, {
        page: nextLogPage,
        pageSize: nextLogPageSize,
      })
      setData(result)
      const responsePage = result.usage.recent_logs_page || nextLogPage
      if (responsePage !== nextLogPage) setLogPage(responsePage)
    } catch (err) {
      setError(getErrorMessage(err))
    } finally {
      setLoading(false)
    }
  }

  // 挂载时若浏览器记住了 key，则自动登录；失败则回到登录窗口
  useEffect(() => {
    if (!activeAPIKey) {
      setBootstrapping(false)
      return
    }
    let cancelled = false
    void (async () => {
      setLoading(true)
      setError('')
      try {
        const result = await api.getPublicAPIKeyUsage(activeAPIKey, range, {
          page: logPage,
          pageSize: logPageSize,
        })
        if (!cancelled) setData(result)
      } catch (err) {
        if (!cancelled) {
          setError(getErrorMessage(err))
          setActiveAPIKey('')
          clearStoredAPIKey()
          setData(null)
        }
      } finally {
        if (!cancelled) {
          setLoading(false)
          setBootstrapping(false)
        }
      }
    })()
    return () => {
      cancelled = true
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  // 登录窗口提交：校验 key 成功后才进入仪表盘并记住会话
  const handleLogin = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    const trimmed = apiKeyInput.trim()
    if (!trimmed) {
      setError(t('keyUsage.keyRequired'))
      return
    }
    setLoading(true)
    setError('')
    try {
      const result = await api.getPublicAPIKeyUsage(trimmed, range, {
        page: 1,
        pageSize: logPageSize,
      })
      setData(result)
      setLogPage(1)
      setActiveAPIKey(trimmed)
      writeStoredAPIKey(trimmed, remember)
    } catch (err) {
      setError(getErrorMessage(err))
      setData(null)
    } finally {
      setLoading(false)
    }
  }

  const handleRangeChange = (next: UsageRange) => {
    setRange(next)
    setLogPage(1)
    void reload(next, 1, logPageSize)
  }

  const handleLogPageChange = (nextPage: number) => {
    setLogPage(nextPage)
    void reload(range, nextPage, logPageSize)
  }

  const handleLogPageSizeChange = (nextPageSize: number) => {
    setLogPageSize(nextPageSize)
    setLogPage(1)
    void reload(range, 1, nextPageSize)
  }

  // 非法的子路由回退到概览
  useEffect(() => {
    if (view && !USAGE_VIEWS.includes(view as UsageView)) {
      navigate('/key-usage/overview', { replace: true })
    }
  }, [navigate, view])

  const handleLogout = () => {
    clearStoredAPIKey()
    setActiveAPIKey('')
    setAPIKeyInput('')
    setData(null)
    setError('')
    setLogPage(1)
  }

  const summary = data?.usage.summary
  const errorRate = summary && summary.requests > 0 ? (summary.error_count / summary.requests) * 100 : 0
  const quotaPercent = useMemo(() => {
    const limit = data?.key.quota_limit ?? 0
    const used = data?.key.quota_used ?? 0
    if (limit <= 0) return 0
    return clampPercent((used / limit) * 100)
  }, [data])
  const recentLogsTotal = data?.usage.recent_logs_total ?? data?.usage.recent_logs.length ?? 0
  const recentLogsPage = data?.usage.recent_logs_page ?? logPage
  const recentLogsPageSize = data?.usage.recent_logs_page_size ?? logPageSize
  const recentLogsTotalPages = Math.max(1, Math.ceil(recentLogsTotal / Math.max(1, recentLogsPageSize)))

  const toolbar = (
    <div className="flex items-center gap-2">
      <Button variant="outline" size="icon-sm" onClick={() => i18n.changeLanguage(i18n.language === 'zh' ? 'en' : 'zh')} title={t('common.themeStyle')}>
        <Languages className="size-4" />
      </Button>
      <Button variant="outline" size="icon-sm" onClick={toggle} title={theme === 'dark' ? t('common.switchToLight') : t('common.switchToDark')}>
        {theme === 'dark' ? <Sun className="size-4" /> : <Moon className="size-4" />}
      </Button>
    </div>
  )

  // 启动时自动登录校验中：显示加载占位，避免登录窗口闪现
  if (bootstrapping) {
    return (
      <div className="flex min-h-dvh items-center justify-center bg-background text-foreground">
        <div className="flex flex-col items-center gap-3 text-muted-foreground">
          <Loader2 className="size-7 animate-spin" />
          <div className="text-sm">{t('common.loading')}</div>
        </div>
      </div>
    )
  }

  // 未登录：渲染登录窗口
  if (!activeAPIKey) {
    return (
      <div className="relative flex min-h-dvh items-center justify-center overflow-hidden bg-background px-4 py-10 text-foreground">
        <div
          aria-hidden="true"
          className="pointer-events-none absolute inset-0 opacity-70 [background:radial-gradient(60%_50%_at_50%_-10%,color-mix(in_oklab,var(--color-primary)_16%,transparent),transparent_70%)]"
        />
        <div className="absolute right-4 top-4">{toolbar}</div>
        <Card className="relative w-full max-w-md border-border/70 shadow-xl">
          <CardContent className="p-7 sm:p-8">
            <div className="mb-6 flex flex-col items-center text-center">
              <img src={logoSrc} alt={siteName} className="size-14 rounded-2xl object-cover shadow-sm ring-1 ring-border/60" />
              <h1 className="mt-4 text-xl font-semibold tracking-tight text-foreground">{t('keyUsage.loginTitle')}</h1>
              <p className="mt-1 text-sm text-muted-foreground">{t('keyUsage.loginSubtitle')}</p>
            </div>
            <form onSubmit={handleLogin} className="space-y-4">
              <div className="relative">
                <KeyRound className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
                <Input
                  type={showKey ? 'text' : 'password'}
                  value={apiKeyInput}
                  onChange={(event) => setAPIKeyInput(event.target.value)}
                  placeholder={t('keyUsage.keyPlaceholder')}
                  className="h-11 pl-9 pr-10 font-mono"
                  autoComplete="off"
                  autoFocus
                />
                <button
                  type="button"
                  className="absolute right-2 top-1/2 inline-flex size-7 -translate-y-1/2 items-center justify-center rounded-md text-muted-foreground hover:bg-muted hover:text-foreground"
                  onClick={() => setShowKey((value) => !value)}
                  title={showKey ? t('apiKeys.hideKey') : t('apiKeys.showKey')}
                >
                  {showKey ? <EyeOff className="size-4" /> : <Eye className="size-4" />}
                </button>
              </div>
              <label className="flex cursor-pointer items-start gap-2.5 select-none">
                <input
                  type="checkbox"
                  checked={remember}
                  onChange={(event) => setRemember(event.target.checked)}
                  className="mt-0.5 size-4 shrink-0 cursor-pointer rounded border-border accent-primary"
                />
                <span className="text-sm">
                  <span className="font-medium text-foreground">{t('keyUsage.rememberMe')}</span>
                  <span className="mt-0.5 block text-xs text-muted-foreground">{t('keyUsage.rememberHint')}</span>
                </span>
              </label>
              {error ? (
                <div className="rounded-md border border-destructive/30 bg-destructive/10 px-3 py-2 text-sm text-destructive">
                  {error}
                </div>
              ) : null}
              <Button type="submit" disabled={loading} className="h-11 w-full">
                {loading ? <Loader2 className="size-4 animate-spin" /> : <LogIn className="size-4" />}
                {t('keyUsage.loginButton')}
              </Button>
            </form>
            <p className="mt-5 flex items-center justify-center gap-1.5 text-center text-xs text-muted-foreground">
              <ShieldCheck className="size-3.5" />
              {t('keyUsage.loginHint')}
            </p>
          </CardContent>
        </Card>
      </div>
    )
  }

  return (
    <div className="min-h-dvh bg-background text-foreground">
      <header className="sticky top-0 z-20 border-b border-border bg-card/80 backdrop-blur">
        <div className="mx-auto flex max-w-[1600px] flex-wrap items-center justify-between gap-3 px-4 py-3 sm:px-6">
          <div className="flex min-w-0 items-center gap-3">
            <img src={logoSrc} alt={siteName} className="size-9 rounded-lg object-cover shadow-sm" />
            <div className="min-w-0">
              <h1 className="truncate text-base font-semibold text-foreground">{t('keyUsage.title')}</h1>
              <div className="truncate text-xs text-muted-foreground">{siteName}</div>
            </div>
          </div>
          {toolbar}
        </div>
      </header>

      <main className="mx-auto max-w-[1600px] space-y-5 px-4 py-5 sm:px-6">
        {/* 工具条：当前 key + 时间范围 + 刷新 + 退出 */}
        <Card className="py-0">
          <CardContent className="flex flex-wrap items-center justify-between gap-3 p-3.5">
            <div className="flex min-w-0 items-center gap-2.5">
              <div className="flex size-9 shrink-0 items-center justify-center rounded-lg bg-primary/12 text-primary">
                <KeyRound className="size-4" />
              </div>
              <div className="min-w-0">
                <div className="truncate text-sm font-semibold text-foreground">{data?.key.name || t('keyUsage.keyCard')}</div>
                {data ? (
                  <div className="flex items-center gap-1.5">
                    <span className="truncate font-mono text-xs text-muted-foreground">{data.key.key}</span>
                    <StatusPill status={data.key.status} label={t(`apiKeys.status.${data.key.status}`)} />
                  </div>
                ) : null}
              </div>
            </div>
            <div className="flex flex-wrap items-center gap-2">
              <div className="inline-flex h-9 items-center rounded-lg border border-border bg-muted/50 p-0.5">
                {RANGE_OPTIONS.map((item) => (
                  <button
                    key={item}
                    type="button"
                    onClick={() => handleRangeChange(item)}
                    className={`h-8 rounded-md px-3 text-[13px] font-medium transition-all ${
                      range === item ? 'border border-border bg-background text-foreground shadow-sm' : 'text-muted-foreground hover:text-foreground'
                    }`}
                  >
                    {t(`keyUsage.range.${item}`)}
                  </button>
                ))}
              </div>
              <Button type="button" variant="outline" size="sm" disabled={loading} onClick={() => void reload()} className="h-9">
                {loading ? <Loader2 className="size-3.5 animate-spin" /> : <RefreshCw className="size-3.5" />}
                {t('common.refresh')}
              </Button>
              <Button type="button" variant="outline" size="sm" onClick={handleLogout} className="h-9">
                <LogOut className="size-3.5" />
                {t('common.logout')}
              </Button>
            </div>
          </CardContent>
        </Card>
        {error ? (
          <div className="rounded-md border border-destructive/30 bg-destructive/10 px-3 py-2 text-sm text-destructive">
            {error}
          </div>
        ) : null}

        {data && summary ? (
          <div className="space-y-5">
            <UsageTabs activeView={activeView} />
            <APIKeyModelRequestUsageCard items={data.model_request_usage ?? []} />

            {/* 概览：核心指标 + 模型/端点分布 */}
            {activeView === 'overview' ? (
              <div className="space-y-5">
                <div className="grid grid-cols-1 gap-3 min-[560px]:grid-cols-2 xl:grid-cols-4">
                  <StatCard
                    accent="emerald"
                    icon={<CircleDollarSign />}
                    label={t('keyUsage.cost')}
                    value={formatCostCardValue(summary.user_billed)}
                    hint={t('keyUsage.totalUsed', { amount: formatCostCardValue(data.key.total_used) })}
                  />
                  <StatCard
                    accent="blue"
                    icon={<Activity />}
                    label={t('keyUsage.requests')}
                    value={formatNumber(summary.requests)}
                    hint={t('keyUsage.rpmTpm', { rpm: formatNumber(summary.rpm), tpm: formatCompact(summary.tpm) })}
                  />
                  <StatCard
                    accent="violet"
                    icon={<Box />}
                    label={t('keyUsage.tokens')}
                    value={formatCompact(summary.tokens)}
                    hint={t('keyUsage.tokenSplit', { input: formatCompact(summary.input_tokens), output: formatCompact(summary.output_tokens) })}
                  />
                  <StatCard
                    accent="amber"
                    icon={<AlertTriangle />}
                    label={t('usage.errorRateCard')}
                    value={formatPercent(errorRate)}
                    hint={t('usage.avgLatencyInline', { value: Math.round(summary.avg_duration_ms) })}
                  />
                </div>
                <div className="grid gap-5 lg:grid-cols-2">
                  <ModelStatsPanel rows={data.usage.models} />
                  <DistributionPanel
                    accent="violet"
                    icon={<Route />}
                    title={t('keyUsage.endpoints')}
                    rows={data.usage.endpoints}
                    emptyText={t('usage.noEndpointStats')}
                  />
                </div>
              </div>
            ) : null}

            {/* 额度与限额：配额 + 窗口用量 + 限额 */}
            {activeView === 'quota' ? (
              <div className="grid gap-5 lg:grid-cols-2 xl:grid-cols-3">
                <QuotaCard data={data} quotaPercent={quotaPercent} />
                <WindowUsageCard limits={data.key.limits} windows={data.usage.windows} />
                <LimitsCard limits={data.key.limits} />
              </div>
            ) : null}

            {/* 请求日志：整页表格 */}
            {activeView === 'logs' ? (
              <RecentLogsTable
                logs={data.usage.recent_logs}
                page={recentLogsPage}
                totalPages={recentLogsTotalPages}
                totalItems={recentLogsTotal}
                pageSize={recentLogsPageSize}
                pageSizeOptions={LOG_PAGE_SIZE_OPTIONS}
                onPageChange={handleLogPageChange}
                onPageSizeChange={handleLogPageSizeChange}
              />
            ) : null}
          </div>
        ) : null}
      </main>
    </div>
  )
}

// ============================================================================
// UsageTabs —— 复刻生图工作台的分段标签，按 URL 切换概览/额度/日志
// ============================================================================
function UsageTabs({ activeView }: { activeView: UsageView }) {
  const { t } = useTranslation()
  const tabs: Array<{ view: UsageView; label: string }> = [
    { view: 'overview', label: t('keyUsage.views.overview') },
    { view: 'quota', label: t('keyUsage.views.quota') },
    { view: 'logs', label: t('keyUsage.views.logs') },
  ]
  const activeIndex = Math.max(0, tabs.findIndex((tab) => tab.view === activeView))

  return (
    <div className="flex justify-center">
      <div
        className="relative grid w-full max-w-[480px] grid-cols-3 rounded-2xl border border-border bg-card/80 p-1 shadow-sm backdrop-blur-lg"
        role="tablist"
        aria-label={t('keyUsage.title')}
      >
        <div
          className="pointer-events-none absolute left-1 top-1 h-[calc(100%-0.5rem)] rounded-xl border border-primary/15 bg-primary/8 transition-transform duration-300 ease-out"
          style={{ width: 'calc((100% - 0.5rem) / 3)', transform: `translateX(${activeIndex * 100}%)` }}
        />
        {tabs.map((tab) => (
          <NavLink
            key={tab.view}
            to={`/key-usage/${tab.view}`}
            role="tab"
            aria-selected={activeView === tab.view}
            className={`relative z-10 flex h-9 items-center justify-center rounded-xl px-3 text-sm font-semibold transition-colors ${
              activeView === tab.view ? 'text-primary' : 'text-muted-foreground hover:text-foreground'
            }`}
          >
            {tab.label}
          </NavLink>
        ))}
      </div>
    </div>
  )
}

// ============================================================================
// 强调色系统 —— 与管理端 Usage 面板保持一致：每个面板携带一种强调色，
// 贯穿图标底色、标题分隔线、进度条与排名徽标，亮/暗色与各 theme-* 调色板自适应。
// ============================================================================
type AccentKey = 'blue' | 'emerald' | 'violet' | 'amber' | 'cyan'

const ACCENTS: Record<AccentKey, { chip: string; ring: string; underline: string; bar: string; rank: string; value: string }> = {
  blue: {
    chip: 'bg-blue-500/12 text-blue-600 dark:bg-blue-500/20 dark:text-blue-300',
    ring: 'ring-1 ring-inset ring-blue-500/20 dark:ring-blue-500/30',
    underline: 'from-blue-500/45 via-blue-500/20 to-transparent dark:from-blue-400/45 dark:via-blue-400/20',
    bar: 'from-blue-500/85 to-blue-500/45 dark:from-blue-400/90 dark:to-blue-400/45',
    rank: 'bg-blue-500/14 text-blue-600 ring-1 ring-inset ring-blue-500/20 dark:bg-blue-500/22 dark:text-blue-300 dark:ring-blue-500/30',
    value: 'text-foreground',
  },
  emerald: {
    chip: 'bg-emerald-500/12 text-emerald-600 dark:bg-emerald-500/20 dark:text-emerald-300',
    ring: 'ring-1 ring-inset ring-emerald-500/20 dark:ring-emerald-500/30',
    underline: 'from-emerald-500/45 via-emerald-500/20 to-transparent dark:from-emerald-400/45 dark:via-emerald-400/20',
    bar: 'from-emerald-500/85 to-emerald-500/45 dark:from-emerald-400/90 dark:to-emerald-400/45',
    rank: 'bg-emerald-500/14 text-emerald-600 ring-1 ring-inset ring-emerald-500/20 dark:bg-emerald-500/22 dark:text-emerald-300 dark:ring-emerald-500/30',
    value: 'text-emerald-600 dark:text-emerald-400',
  },
  violet: {
    chip: 'bg-violet-500/12 text-violet-600 dark:bg-violet-500/20 dark:text-violet-300',
    ring: 'ring-1 ring-inset ring-violet-500/20 dark:ring-violet-500/30',
    underline: 'from-violet-500/45 via-violet-500/20 to-transparent dark:from-violet-400/45 dark:via-violet-400/20',
    bar: 'from-violet-500/85 to-violet-500/45 dark:from-violet-400/90 dark:to-violet-400/45',
    rank: 'bg-violet-500/14 text-violet-600 ring-1 ring-inset ring-violet-500/20 dark:bg-violet-500/22 dark:text-violet-300 dark:ring-violet-500/30',
    value: 'text-foreground',
  },
  amber: {
    chip: 'bg-amber-500/12 text-amber-600 dark:bg-amber-500/20 dark:text-amber-300',
    ring: 'ring-1 ring-inset ring-amber-500/20 dark:ring-amber-500/30',
    underline: 'from-amber-500/45 via-amber-500/20 to-transparent dark:from-amber-400/45 dark:via-amber-400/20',
    bar: 'from-amber-500/85 to-amber-500/45 dark:from-amber-400/90 dark:to-amber-400/45',
    rank: 'bg-amber-500/14 text-amber-600 ring-1 ring-inset ring-amber-500/20 dark:bg-amber-500/22 dark:text-amber-300 dark:ring-amber-500/30',
    value: 'text-foreground',
  },
  cyan: {
    chip: 'bg-cyan-500/12 text-cyan-600 dark:bg-cyan-500/20 dark:text-cyan-300',
    ring: 'ring-1 ring-inset ring-cyan-500/20 dark:ring-cyan-500/30',
    underline: 'from-cyan-500/45 via-cyan-500/20 to-transparent dark:from-cyan-400/45 dark:via-cyan-400/20',
    bar: 'from-cyan-500/85 to-cyan-500/45 dark:from-cyan-400/90 dark:to-cyan-400/45',
    rank: 'bg-cyan-500/14 text-cyan-600 ring-1 ring-inset ring-cyan-500/20 dark:bg-cyan-500/22 dark:text-cyan-300 dark:ring-cyan-500/30',
    value: 'text-foreground',
  },
}

function StatCard({ accent, icon, label, value, hint }: { accent: AccentKey; icon: ReactNode; label: string; value: string; hint: string }) {
  const a = ACCENTS[accent]
  return (
    <Card className="min-w-0 py-0 transition-all duration-200 hover:-translate-y-0.5 hover:shadow-md">
      <CardContent className="flex min-w-0 flex-col gap-1.5 p-3.5">
        <div className="flex items-center justify-between gap-2">
          <span className="truncate text-[11px] font-bold uppercase tracking-wide text-muted-foreground">{label}</span>
          <div className={`flex size-9 shrink-0 items-center justify-center rounded-lg ${a.chip} [&_svg]:size-4`}>{icon}</div>
        </div>
        <div className={`min-w-0 break-words text-[22px] font-bold leading-tight tabular-nums ${a.value}`}>{value}</div>
        <div className="truncate text-[11px] leading-snug text-muted-foreground" title={hint}>{hint}</div>
      </CardContent>
    </Card>
  )
}

function StatusPill({ status, label }: { status: string; label: string }) {
  const cls = status === 'active'
    ? 'bg-emerald-500/14 text-emerald-600 dark:bg-emerald-500/20 dark:text-emerald-300'
    : status === 'expired'
      ? 'bg-slate-500/14 text-slate-600 dark:bg-slate-500/20 dark:text-slate-300'
      : 'bg-red-500/14 text-red-600 dark:bg-red-500/20 dark:text-red-300'
  return (
    <span className={`inline-flex shrink-0 items-center rounded-full px-1.5 py-0.5 text-[10px] font-semibold ${cls}`}>{label}</span>
  )
}

// PanelShell / PanelHeader —— 复刻管理端面板外壳与表头
function PanelShell({ children }: { children: ReactNode }) {
  return (
    <Card className="group/panel h-full py-0 transition-all duration-200 hover:-translate-y-0.5 hover:shadow-md">
      <CardContent className="flex h-full flex-col p-5">{children}</CardContent>
    </Card>
  )
}

function PanelHeader({ accent, icon, title, trailing }: { accent: AccentKey; icon: ReactNode; title: string; trailing?: ReactNode }) {
  const a = ACCENTS[accent]
  return (
    <div className="mb-4">
      <div className="flex items-center justify-between gap-3">
        <div className="flex min-w-0 items-center gap-3">
          <div className={`flex size-10 shrink-0 items-center justify-center rounded-xl transition-transform duration-200 group-hover/panel:scale-[1.04] ${a.chip} ${a.ring} [&_svg]:size-[18px]`}>
            {icon}
          </div>
          <h3 className="truncate text-[15px] font-semibold tracking-tight text-foreground">{title}</h3>
        </div>
        {trailing ? <div className="shrink-0">{trailing}</div> : null}
      </div>
      <div className={`mt-3 h-px w-full rounded-full bg-gradient-to-r ${a.underline}`} />
    </div>
  )
}

function AccentBar({ accent, ratio, thickness = 'h-1.5', minWidth = 4 }: { accent: AccentKey; ratio: number; thickness?: string; minWidth?: number }) {
  const pct = Math.max(minWidth, Math.min(100, ratio * 100))
  return (
    <div className={`${thickness} overflow-hidden rounded-full bg-muted ring-1 ring-inset ring-border/50`}>
      <div className={`h-full rounded-full bg-gradient-to-r transition-[width] duration-500 ease-out ${ACCENTS[accent].bar}`} style={{ width: `${pct}%` }} />
    </div>
  )
}

function RankBadge({ accent, rank }: { accent: AccentKey; rank: number }) {
  const cls = rank <= 3 ? ACCENTS[accent].rank : 'bg-muted text-muted-foreground'
  return (
    <span className={`flex size-5 shrink-0 items-center justify-center rounded-md text-[11px] font-bold leading-none tabular-nums ${cls}`}>{rank}</span>
  )
}

function EmptyPanel({ accent, icon, text }: { accent: AccentKey; icon: ReactNode; text: string }) {
  return (
    <div className="flex min-h-[140px] flex-1 flex-col items-center justify-center gap-2.5 rounded-xl border border-dashed border-border/70 px-4 text-center">
      <div className={`flex size-9 items-center justify-center rounded-lg opacity-70 ${ACCENTS[accent].chip} [&_svg]:size-[16px]`}>{icon}</div>
      <p className="text-[13px] text-muted-foreground">{text}</p>
    </div>
  )
}

function QuotaCard({ data, quotaPercent }: { data: PublicAPIKeyUsageResponse; quotaPercent: number }) {
  const { t } = useTranslation()
  const hasQuota = data.key.quota_limit > 0
  return (
    <PanelShell>
      <PanelHeader accent="emerald" icon={<CircleDollarSign />} title={t('keyUsage.quota')} />
      <div className="space-y-4">
        {hasQuota ? (
          <div>
            <div className="mb-1.5 flex items-baseline justify-between gap-3 text-sm">
              <span className="font-semibold tabular-nums text-foreground">{formatUSD(data.key.quota_used)}</span>
              <span className="text-xs text-muted-foreground">/ {formatUSD(data.key.quota_limit)}</span>
            </div>
            <AccentBar accent={quotaPercent >= 90 ? 'amber' : 'emerald'} ratio={quotaPercent / 100} thickness="h-2" minWidth={2} />
            <div className="mt-1 text-right text-[11px] tabular-nums text-muted-foreground">{formatPercent(quotaPercent)}</div>
          </div>
        ) : (
          <div className="rounded-lg border border-border bg-muted/30 px-3 py-2 text-sm text-muted-foreground">{t('apiKeys.unlimited')}</div>
        )}
        <div className="grid grid-cols-2 gap-2">
          <InfoTile label={t('common.createdAt')} value={formatBeijingTime(data.key.created_at)} />
          <InfoTile label={t('apiKeys.expiresColumn')} value={data.key.expires_at ? formatBeijingTime(data.key.expires_at) : t('apiKeys.neverExpires')} />
          <InfoTile label={t('keyUsage.resetCount')} value={formatNumber(data.key.reset_count)} />
          <InfoTile label={t('keyUsage.lastReset')} value={data.key.last_reset_at ? formatRelativeTime(data.key.last_reset_at, { variant: 'compact' }) : '-'} />
        </div>
      </div>
    </PanelShell>
  )
}

function WindowUsageCard({ limits, windows }: { limits: APIKeyLimits; windows: PublicAPIKeyUsageResponse['usage']['windows'] }) {
  const { t } = useTranslation()
  const rows = [
    { label: t('keyUsage.windowToday'), usage: windows.today, costLimit: limits.cost_limit_daily ?? 0, tokenLimit: limits.token_limit_daily ?? 0 },
    { label: '5h', usage: windows.last_5h, costLimit: limits.cost_limit_5h ?? 0, tokenLimit: limits.token_limit_5h ?? 0 },
    { label: '7d', usage: windows.last_7d, costLimit: limits.cost_limit_7d ?? 0, tokenLimit: limits.token_limit_7d ?? 0 },
    { label: '30d', usage: windows.last_30d, costLimit: limits.cost_limit_30d ?? 0, tokenLimit: limits.token_limit_30d ?? 0 },
  ].filter((row) => row.usage)
  return (
    <PanelShell>
      <PanelHeader accent="cyan" icon={<Clock3 />} title={t('keyUsage.windows')} />
      <div className="space-y-3">
        {rows.map((row) => (
          <WindowRow key={row.label} {...row} />
        ))}
      </div>
    </PanelShell>
  )
}

function WindowRow({ label, usage, costLimit, tokenLimit }: { label: string; usage: PublicAPIKeyWindowUsage; costLimit: number; tokenLimit: number }) {
  const costPct = costLimit > 0 ? clampPercent((usage.user_billed / costLimit) * 100) : 0
  const tokenPct = tokenLimit > 0 ? clampPercent((usage.tokens / tokenLimit) * 100) : 0
  return (
    <div className="rounded-xl border border-border bg-muted/20 p-3">
      <div className="mb-2 flex items-center justify-between gap-3 text-sm">
        <span className="font-semibold text-foreground">{label}</span>
        <span className="text-xs tabular-nums text-muted-foreground">{formatNumber(usage.requests)} req</span>
      </div>
      <div className="space-y-2">
        <UsageLimitBar label="USD" used={formatUSD(usage.user_billed)} limit={costLimit > 0 ? formatUSD(costLimit) : '-'} percent={costPct} />
        <UsageLimitBar label="Tok" used={formatCompact(usage.tokens)} limit={tokenLimit > 0 ? formatCompact(tokenLimit) : '-'} percent={tokenPct} />
      </div>
      <WindowResetHint usage={usage} />
    </div>
  )
}

// WindowResetHint 展示服务端下发的窗口重置/回落时刻(issue #460):
// fixed(自然日)窗口有确定的清零时刻;sliding 窗口没有单一重置点,
// 只有最早一笔用量滚出窗口、额度开始回落的时间。
function WindowResetHint({ usage }: { usage: PublicAPIKeyWindowUsage }) {
  const { t } = useTranslation()
  if (usage.window_kind === 'fixed' && usage.reset_at) {
    return (
      <div className="mt-2 flex items-center justify-between gap-2 text-[11px] text-muted-foreground">
        <span>{t('keyUsage.resetsAt', { time: formatBeijingTime(usage.reset_at) })}</span>
        <span className="tabular-nums">{t('keyUsage.resetRemaining', { duration: formatDurationUntil(usage.reset_at) })}</span>
      </div>
    )
  }
  if (usage.window_kind === 'sliding') {
    return (
      <div className="mt-2 flex items-center justify-between gap-2 text-[11px] text-muted-foreground">
        <span>{t('keyUsage.slidingWindow')}</span>
        {usage.decay_at ? <span className="tabular-nums">{t('keyUsage.decayAt', { time: formatBeijingTime(usage.decay_at) })}</span> : null}
      </div>
    )
  }
  return null
}

// formatDurationUntil 计算距目标时刻的剩余时长(向上取整到分钟)。
function formatDurationUntil(dateStr: string): string {
  const diff = new Date(dateStr).getTime() - Date.now()
  if (!Number.isFinite(diff) || diff <= 0) return '0m'
  const totalMinutes = Math.ceil(diff / 60000)
  const days = Math.floor(totalMinutes / 1440)
  const hours = Math.floor((totalMinutes % 1440) / 60)
  const minutes = totalMinutes % 60
  if (days > 0) return `${days}d ${hours}h`
  if (hours > 0) return `${hours}h ${minutes}m`
  return `${minutes}m`
}

function LimitsCard({ limits }: { limits: APIKeyLimits }) {
  const { t } = useTranslation()
  const items: Array<[string, string | number]> = [
    ['RPM', limits.rpm ?? 0],
    ['RPD', limits.rpd ?? 0],
    [t('apiKeys.limits.maxConcurrency'), limits.max_concurrency ?? 0],
    [t('apiKeys.limits.cost5h'), limits.cost_limit_5h ? formatUSD(limits.cost_limit_5h) : ''],
    [t('apiKeys.limits.cost7d'), limits.cost_limit_7d ? formatUSD(limits.cost_limit_7d) : ''],
    [t('apiKeys.limits.cost30d'), limits.cost_limit_30d ? formatUSD(limits.cost_limit_30d) : ''],
    [t('apiKeys.limits.costDaily'), limits.cost_limit_daily ? formatUSD(limits.cost_limit_daily) : ''],
    [t('apiKeys.limits.tokens5h'), limits.token_limit_5h ? formatCompact(limits.token_limit_5h) : ''],
    [t('apiKeys.limits.tokens7d'), limits.token_limit_7d ? formatCompact(limits.token_limit_7d) : ''],
    [t('apiKeys.limits.tokens30d'), limits.token_limit_30d ? formatCompact(limits.token_limit_30d) : ''],
    [t('apiKeys.limits.tokensDaily'), limits.token_limit_daily ? formatCompact(limits.token_limit_daily) : ''],
  ]
  const visible = items.filter(([, value]) => value !== undefined && value !== '' && value !== 0)
  const hasModels = (limits.model_allow?.length ?? 0) > 0 || (limits.model_deny?.length ?? 0) > 0
  return (
    <PanelShell>
      <PanelHeader accent="amber" icon={<ShieldCheck />} title={t('keyUsage.limits')} />
      <div className="space-y-3">
        {visible.length === 0 && !hasModels ? (
          <div className="rounded-lg border border-border bg-muted/30 px-3 py-2 text-sm text-muted-foreground">{t('keyUsage.noLimits')}</div>
        ) : null}
        {visible.length > 0 ? (
          <div className="grid grid-cols-2 gap-2">
            {visible.map(([label, value]) => (
              <InfoTile key={String(label)} label={String(label)} value={String(value)} />
            ))}
          </div>
        ) : null}
        {hasModels ? (
          <div className="space-y-2">
            <ModelChips label={t('apiKeys.limits.modelAllow')} values={limits.model_allow ?? []} />
            <ModelChips label={t('apiKeys.limits.modelDeny')} values={limits.model_deny ?? []} />
          </div>
        ) : null}
      </div>
    </PanelShell>
  )
}

const PIE_COLORS = [
  'color-mix(in oklab, var(--color-primary) 92%, transparent)',
  'color-mix(in oklab, var(--color-primary) 70%, transparent)',
  'color-mix(in oklab, var(--color-primary) 50%, transparent)',
  'color-mix(in oklab, var(--color-primary) 34%, transparent)',
  'color-mix(in oklab, var(--color-primary) 22%, transparent)',
]

function ModelStatsPanel({ rows }: { rows: PublicAPIKeyUsageBreakdown[] }) {
  const { t } = useTranslation()
  const accent: AccentKey = 'blue'
  const totalRequests = rows.reduce((sum, item) => sum + safeNumber(item.requests), 0)
  const maxRequests = Math.max(1, ...rows.map((item) => safeNumber(item.requests)))
  return (
    <PanelShell>
      <PanelHeader accent={accent} icon={<BarChart3 />} title={t('keyUsage.models')} />
      {rows.length === 0 ? (
        <EmptyPanel accent={accent} icon={<BarChart3 />} text={t('usage.noModelStats')} />
      ) : (
        <div className="space-y-3.5">
          <ModelSharePie rows={rows} />
          {rows.slice(0, 5).map((item, index) => {
            const share = totalRequests > 0 ? (item.requests / totalRequests) * 100 : 0
            return (
              <div key={item.name} className="space-y-1.5">
                <div className="flex items-start justify-between gap-3">
                  <div className="flex min-w-0 items-start gap-2.5">
                    <RankBadge accent={accent} rank={index + 1} />
                    <div className="min-w-0">
                      <div className="truncate font-mono text-[13px] font-semibold leading-tight text-foreground" title={item.name}>{item.name}</div>
                      <div className="mt-0.5 flex flex-wrap items-center gap-x-2.5 gap-y-0.5 text-[11px] text-muted-foreground">
                        <span className="tabular-nums">{t('usage.modelStatsRequests')} {formatNumber(item.requests)}</span>
                        <span className="text-border">·</span>
                        <span className="tabular-nums">{t('usage.modelStatsTokens')} {formatCompact(item.tokens)}</span>
                        {item.error_count > 0 ? (
                          <>
                            <span className="text-border">·</span>
                            <span className="tabular-nums text-amber-600 dark:text-amber-400">{t('usage.modelStatsErrors')} {formatNumber(item.error_count)}</span>
                          </>
                        ) : null}
                      </div>
                    </div>
                  </div>
                  <div className="shrink-0 text-right">
                    <div className="font-mono text-[13px] font-semibold tabular-nums text-emerald-600 dark:text-emerald-400">{formatCostCardValue(item.user_billed)}</div>
                    <div className="mt-0.5 font-mono text-[11px] tabular-nums text-muted-foreground">{share.toFixed(1)}%</div>
                  </div>
                </div>
                <div className="pl-[30px]">
                  <AccentBar accent={accent} ratio={safeNumber(item.requests) / maxRequests} thickness="h-2" minWidth={5} />
                </div>
              </div>
            )
          })}
        </div>
      )}
    </PanelShell>
  )
}

function ModelSharePie({ rows }: { rows: PublicAPIKeyUsageBreakdown[] }) {
  const { t } = useTranslation()
  const totalAmount = rows.reduce((sum, item) => sum + safeNumber(item.user_billed), 0)
  const totalRequests = rows.reduce((sum, item) => sum + safeNumber(item.requests), 0)
  const useAmount = totalAmount > 0
  const base = rows
    .map((item) => ({ name: item.name || 'unknown', value: useAmount ? safeNumber(item.user_billed) : safeNumber(item.requests) }))
    .filter((item) => item.value > 0)
  const total = base.reduce((sum, item) => sum + item.value, 0)
  if (total <= 0) return null
  const visible = base.slice(0, 4)
  const overflow = base.slice(4)
  if (overflow.length > 0) {
    visible.push({ name: t('usage.modelStatsOther'), value: overflow.reduce((sum, item) => sum + item.value, 0) })
  }
  const pieData = visible.map((item) => ({ ...item, share: (item.value / total) * 100 }))
  const centerValue = useAmount ? formatCostCardValue(totalAmount) : formatNumber(totalRequests)
  const metricLabel = useAmount ? t('usage.modelPieAmount') : t('usage.modelPieRequests')
  return (
    <div className="rounded-xl border border-border bg-muted/20 p-3">
      <div className="mb-1.5 flex items-baseline justify-between gap-3">
        <div className="text-[11px] font-semibold uppercase tracking-wide text-muted-foreground">{t('usage.modelPieTitle')}</div>
        <div className="text-[11px] font-medium text-muted-foreground/80">{metricLabel}</div>
      </div>
      <div className="relative h-[150px]">
        <ResponsiveContainer width="100%" height="100%">
          <PieChart>
            <Pie data={pieData} dataKey="value" nameKey="name" cx="50%" cy="50%" innerRadius="54%" outerRadius="84%" paddingAngle={0} strokeWidth={0}>
              {pieData.map((_, index) => (
                <Cell key={index} fill={PIE_COLORS[index % PIE_COLORS.length]} />
              ))}
            </Pie>
            <RechartsTooltip
              cursor={false}
              formatter={(value, name) => [useAmount ? formatCostCardValue(Number(value ?? 0)) : formatNumber(Number(value ?? 0)), String(name ?? '')]}
              contentStyle={{ backgroundColor: 'var(--color-card)', border: '1px solid var(--color-border)', borderRadius: 12, boxShadow: '0 16px 36px rgba(15, 23, 42, 0.14)', fontSize: 12 }}
              itemStyle={{ color: 'var(--color-foreground)' }}
            />
          </PieChart>
        </ResponsiveContainer>
        <div className="pointer-events-none absolute inset-0 flex items-center justify-center">
          <div className="max-w-[112px] text-center">
            <div className="truncate font-mono text-[15px] font-semibold tabular-nums tracking-tight text-foreground">{centerValue}</div>
            <div className="mt-0.5 text-[10px] font-medium uppercase tracking-wide text-muted-foreground">{metricLabel}</div>
          </div>
        </div>
      </div>
      <div className="mt-3 grid grid-cols-2 gap-x-4 gap-y-1.5 max-sm:grid-cols-1">
        {pieData.map((item, index) => (
          <div key={`${item.name}-${index}`} className="flex items-center gap-2 text-xs">
            <span className="size-2.5 shrink-0 rounded-full" style={{ background: PIE_COLORS[index % PIE_COLORS.length] }} />
            <span className="min-w-0 flex-1 truncate text-muted-foreground" title={item.name}>{item.name}</span>
            <span className="shrink-0 font-mono text-[11px] font-medium tabular-nums text-foreground">{item.share.toFixed(1)}%</span>
          </div>
        ))}
      </div>
    </div>
  )
}

function DistributionPanel({ accent, icon, title, rows, emptyText }: { accent: AccentKey; icon: ReactNode; title: string; rows: PublicAPIKeyUsageBreakdown[]; emptyText: string }) {
  const { t } = useTranslation()
  const totalRequests = rows.reduce((sum, item) => sum + safeNumber(item.requests), 0)
  const maxRequests = Math.max(1, ...rows.map((item) => safeNumber(item.requests)))
  const visible = rows.slice(0, 6)
  return (
    <PanelShell>
      <PanelHeader accent={accent} icon={icon} title={title} />
      {visible.length === 0 ? (
        <EmptyPanel accent={accent} icon={icon} text={emptyText} />
      ) : (
        <div className="space-y-3.5">
          {visible.map((item, index) => (
            <div key={item.name} className="space-y-1.5">
              <div className="flex items-start justify-between gap-3">
                <div className="flex min-w-0 items-start gap-2.5">
                  <RankBadge accent={accent} rank={index + 1} />
                  <div className="min-w-0">
                    <div className="truncate font-mono text-[13px] font-semibold leading-tight text-foreground" title={item.name}>{item.name}</div>
                    <div className="mt-0.5 flex flex-wrap items-center gap-x-2.5 gap-y-0.5 text-[11px] text-muted-foreground">
                      <span className="tabular-nums">{t('usage.modelStatsRequests')} {formatNumber(item.requests)}</span>
                      <span className="text-border">·</span>
                      <span className="tabular-nums">{t('usage.modelStatsTokens')} {formatCompact(item.tokens)}</span>
                    </div>
                  </div>
                </div>
                <span className="ml-1 inline-block min-w-[3.25rem] shrink-0 text-right font-mono text-[13px] font-semibold tabular-nums text-foreground">{formatPercent(totalRequests > 0 ? (item.requests / totalRequests) * 100 : 0)}</span>
              </div>
              <div className="pl-[30px]">
                <AccentBar accent={accent} ratio={safeNumber(item.requests) / maxRequests} thickness="h-2" minWidth={5} />
              </div>
            </div>
          ))}
        </div>
      )}
    </PanelShell>
  )
}

function statusBadgeClass(code: number): string {
  if (code === 200) return 'border-transparent bg-emerald-500/14 text-emerald-600 dark:bg-emerald-500/20 dark:text-emerald-300'
  if (code === 401) return 'border-transparent bg-red-500/14 text-red-600 dark:bg-red-500/20 dark:text-red-300'
  if (code === 429) return 'border-transparent bg-amber-500/14 text-amber-600 dark:bg-amber-500/20 dark:text-amber-300'
  if (code >= 500) return 'border-transparent bg-red-500/14 text-red-600 dark:bg-red-500/20 dark:text-red-300'
  if (code >= 400) return 'border-transparent bg-amber-500/14 text-amber-600 dark:bg-amber-500/20 dark:text-amber-300'
  return 'border-transparent bg-slate-500/14 text-slate-600 dark:bg-slate-500/20 dark:text-slate-300'
}

function RecentLogsTable({
  logs,
  page,
  totalPages,
  totalItems,
  pageSize,
  pageSizeOptions,
  onPageChange,
  onPageSizeChange,
}: {
  logs: PublicAPIKeyUsageLog[]
  page: number
  totalPages: number
  totalItems: number
  pageSize: number
  pageSizeOptions: number[]
  onPageChange: (page: number) => void
  onPageSizeChange: (pageSize: number) => void
}) {
  const { t } = useTranslation()
  return (
    <Card className="py-0">
      <CardContent className="p-4">
        <div className="mb-3 flex items-center justify-between gap-3">
          <h3 className="text-base font-semibold text-foreground">{t('keyUsage.recent')}</h3>
          <span className="text-xs text-muted-foreground">{t('usage.recordsCount', { count: totalItems })}</span>
        </div>
        <div className="grid gap-3 lg:hidden">
          {logs.length > 0 ? logs.map((log) => (
            <Card key={log.id} className="p-3.5 space-y-2">
              <div className="flex items-start justify-between gap-2">
                <div className="flex flex-wrap items-center gap-1.5 min-w-0">
                  <Badge variant="outline" className={`text-[12px] ${statusBadgeClass(log.status_code)}`}>{log.status_code}</Badge>
                  <Badge variant="outline" className="text-[12px]">{log.model || '-'}</Badge>
                  {log.effective_model && log.effective_model !== log.model ? (
                    <Badge variant="outline" className="border-transparent bg-blue-500/10 text-[11px] font-medium text-blue-600 dark:bg-blue-500/20 dark:text-blue-400">→ {log.effective_model}</Badge>
                  ) : null}
                </div>
                <span className="shrink-0 font-mono text-[11px] text-muted-foreground">{formatBeijingTime(log.created_at)}</span>
              </div>
              <div className="flex flex-wrap items-center justify-between gap-2 text-xs">
                <span className="font-mono text-muted-foreground">{log.endpoint || '-'}</span>
                <Badge
                  variant="outline"
                  className="text-[11px]"
                  style={{ background: log.stream ? 'rgba(99, 102, 241, 0.12)' : 'rgba(107, 114, 128, 0.12)', color: log.stream ? '#6366f1' : '#6b7280', borderColor: 'transparent' }}
                >
                  {log.stream ? 'stream' : 'sync'}
                </Badge>
              </div>
              <div className="flex flex-wrap items-center justify-between gap-2 pt-1 border-t border-border/50 text-xs">
                {log.status_code < 400 && (log.input_tokens > 0 || log.output_tokens > 0) ? (
                  <div className="font-mono text-[12px] tabular-nums">
                    <span className="text-blue-500">↓{formatNumber(log.input_tokens)}</span>
                    <span className="mx-1 text-border">|</span>
                    <span className="text-emerald-500">↑{formatNumber(log.output_tokens)}</span>
                  </div>
                ) : <span className="font-mono text-[12px] text-muted-foreground">-</span>}
                <span className="font-mono text-[12px] text-muted-foreground">
                  {log.duration_ms > 1000 ? `${(log.duration_ms / 1000).toFixed(1)}s` : `${log.duration_ms}ms`}
                </span>
              </div>
            </Card>
          )) : (
            <div className="py-8 text-center text-sm text-muted-foreground">{t('keyUsage.noRows')}</div>
          )}
        </div>

        <div className="data-table-shell hidden lg:block">
          <TooltipProvider>
          <Table>
            <TableHeader>
              <TableRow>
                <TableHead className="text-[12px] font-semibold">{t('usage.tableStatus')}</TableHead>
                <TableHead className="text-[12px] font-semibold">{t('usage.tableModel')}</TableHead>
                <TableHead className="text-[12px] font-semibold">{t('keyUsage.endpoint')}</TableHead>
                <TableHead className="text-[12px] font-semibold">{t('usage.tableType')}</TableHead>
                <TableHead className="text-right text-[12px] font-semibold">{t('usage.tableToken')}</TableHead>
                <TableHead className="text-right text-[12px] font-semibold">{t('usage.tableCost')}</TableHead>
                <TableHead className="text-right text-[12px] font-semibold">
                  <span title={t('usage.tableFirstTokenHint')} className="cursor-help underline decoration-dotted underline-offset-2">{t('usage.tableFirstToken')}</span>
                </TableHead>
                <TableHead className="text-right text-[12px] font-semibold">{t('usage.tableDuration')}</TableHead>
                <TableHead className="text-right text-[12px] font-semibold">{t('usage.tableTime')}</TableHead>
              </TableRow>
            </TableHeader>
            <TableBody>
              {logs.length > 0 ? logs.map((log) => (
                <TableRow key={log.id}>
                  <TableCell>
                    <Badge variant="outline" className={`text-[13px] ${statusBadgeClass(log.status_code)}`}>{log.status_code}</Badge>
                  </TableCell>
                  <TableCell>
                    <div className="flex flex-wrap items-center gap-1.5">
                      {log.via_websocket ? (
                        <Badge variant="outline" className="border-transparent bg-cyan-500/12 text-[11px] font-semibold uppercase text-cyan-600 dark:bg-cyan-500/20 dark:text-cyan-400">ws</Badge>
                      ) : null}
                      <Badge variant="outline" className="text-[13px]">{log.model || '-'}</Badge>
                      {log.effective_model && log.effective_model !== log.model ? (
                        <Badge variant="outline" className="border-transparent bg-blue-500/10 text-[11px] font-medium text-blue-600 dark:bg-blue-500/20 dark:text-blue-400">→ {log.effective_model}</Badge>
                      ) : null}
                      {isFastTier(log.service_tier) ? (
                        <Badge variant="outline" className="gap-0.5 border-transparent bg-blue-500/12 text-[11px] font-semibold text-blue-600 dark:bg-blue-500/20 dark:text-blue-400"><Zap className="size-3" />Fast</Badge>
                      ) : null}
                    </div>
                  </TableCell>
                  <TableCell>
                    <span className="font-mono text-[13px] text-muted-foreground">{log.endpoint || '-'}</span>
                  </TableCell>
                  <TableCell>
                    <div className="flex flex-wrap items-center gap-1.5">
                      <Badge
                        variant="outline"
                        className="text-[13px]"
                        style={{ background: log.stream ? 'rgba(99, 102, 241, 0.12)' : 'rgba(107, 114, 128, 0.12)', color: log.stream ? '#6366f1' : '#6b7280', borderColor: 'transparent' }}
                      >
                        {log.stream ? 'stream' : 'sync'}
                      </Badge>
                      <CompactionBadges
                        compact={log.compact}
                        hasCompactionHistory={log.has_compaction_history}
                      />
                    </div>
                  </TableCell>
                  <TableCell className="text-right">
                    {log.status_code < 400 && (log.input_tokens > 0 || log.output_tokens > 0) ? (
                      <div className="whitespace-nowrap font-mono text-[13px] tabular-nums leading-relaxed">
                        <span className="text-blue-500">↓{formatNumber(log.input_tokens)}</span>
                        <span className="mx-1 text-border">|</span>
                        <span className="text-emerald-500">↑{formatNumber(log.output_tokens)}</span>
                        {log.cached_tokens > 0 ? (
                          <span className="ml-1 inline-flex items-center gap-0.5 text-indigo-500" title={t('usage.cacheReadCost')}><DatabaseZap className="size-3" />{formatNumber(log.cached_tokens)}</span>
                        ) : null}
                      </div>
                    ) : (
                      <span className="font-mono text-[13px] text-muted-foreground">-</span>
                    )}
                  </TableCell>
                  <TableCell className="text-right">
                    <LogCostCell log={log} />
                  </TableCell>
                  <TableCell className="text-right">
                    {log.first_token_ms > 0 ? (
                      <span className={`font-mono text-[13px] tabular-nums ${log.first_token_ms > 5000 ? 'text-red-500' : log.first_token_ms > 2000 ? 'text-amber-500' : 'text-emerald-500'}`}>
                        {log.first_token_ms > 1000 ? `${(log.first_token_ms / 1000).toFixed(1)}s` : `${log.first_token_ms}ms`}
                      </span>
                    ) : (
                      <span className="font-mono text-[13px] text-muted-foreground">-</span>
                    )}
                  </TableCell>
                  <TableCell className="text-right">
                    <span className={`font-mono text-[13px] tabular-nums ${log.duration_ms > 30000 ? 'text-red-500' : log.duration_ms > 10000 ? 'text-amber-500' : 'text-muted-foreground'}`}>
                      {log.duration_ms > 1000 ? `${(log.duration_ms / 1000).toFixed(1)}s` : `${log.duration_ms}ms`}
                    </span>
                  </TableCell>
                  <TableCell className="text-right">
                    <span className="whitespace-nowrap font-mono text-[13px] text-muted-foreground">{formatBeijingTime(log.created_at)}</span>
                  </TableCell>
                </TableRow>
              )) : (
                <TableRow>
                  <TableCell colSpan={9} className="py-8 text-center text-muted-foreground">{t('keyUsage.noRows')}</TableCell>
                </TableRow>
              )}
            </TableBody>
          </Table>
          </TooltipProvider>
        </div>
        <Pagination
          page={page}
          totalPages={totalPages}
          onPageChange={onPageChange}
          totalItems={totalItems}
          pageSize={pageSize}
          pageSizeOptions={pageSizeOptions}
          onPageSizeChange={onPageSizeChange}
        />
      </CardContent>
    </Card>
  )
}

// LogCostCell —— 价格列：悬停展示输入/输出/缓存的费用与单价明细（与管理端口径一致）
function LogCostCell({ log }: { log: PublicAPIKeyUsageLog }) {
  const { t } = useTranslation()
  const hasCostContext = log.status_code < 400 && (
    log.user_billed > 0 || log.total_cost > 0 || log.input_tokens > 0 || log.output_tokens > 0 || log.cached_tokens > 0
  )
  if (!hasCostContext) {
    return <span className="font-mono text-[13px] text-muted-foreground">-</span>
  }
  return (
    <Tooltip>
      <TooltipTrigger asChild>
        <button
          type="button"
          className="group ml-auto inline-flex cursor-help items-center gap-1.5 rounded-md px-1.5 py-1 text-right transition-colors hover:bg-muted/60 focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
        >
          <span className="font-mono text-[13px] font-semibold leading-none tabular-nums text-emerald-600 dark:text-emerald-400">{formatUSD(log.user_billed)}</span>
          <Info className="size-3.5 shrink-0 text-muted-foreground transition-colors group-hover:text-blue-500" />
        </button>
      </TooltipTrigger>
      <TooltipContent side="left" sideOffset={8} className="w-80 max-w-none whitespace-nowrap rounded-lg border border-slate-700 bg-slate-950 px-3 py-2.5 text-xs text-slate-50 shadow-xl">
        <div className="space-y-1.5">
          <div className="mb-1 text-xs font-semibold text-slate-300">{t('usage.costDetails')}</div>
          {log.input_cost > 0 ? <LogCostRow label={t('usage.inputCost')} value={formatUSD(log.input_cost)} /> : null}
          {log.output_cost > 0 ? <LogCostRow label={t('usage.outputCost')} value={formatUSD(log.output_cost)} /> : null}
          {log.cached_tokens > 0 ? <LogCostRow label={t('usage.cacheReadCost')} value={formatUSD(log.cache_read_cost)} /> : null}
          {log.input_tokens > 0 ? <LogCostRow label={t('usage.inputUnitPrice')} value={formatTokenPricePerMillion(log.input_price_per_mtoken)} valueClassName="text-sky-300" /> : null}
          {log.output_tokens > 0 ? <LogCostRow label={t('usage.outputUnitPrice')} value={formatTokenPricePerMillion(log.output_price_per_mtoken)} valueClassName="text-violet-300" /> : null}
          {log.cached_tokens > 0 && log.cache_read_price_per_mtoken > 0 ? <LogCostRow label={t('usage.cacheReadUnitPrice')} value={formatTokenPricePerMillion(log.cache_read_price_per_mtoken)} valueClassName="text-cyan-300" /> : null}
          <div className="my-1 h-px bg-slate-800" />
          <LogCostRow label={t('usage.tableCost')} value={formatUSD(log.user_billed)} valueClassName="text-emerald-300" />
        </div>
      </TooltipContent>
    </Tooltip>
  )
}

function LogCostRow({ label, value, valueClassName = 'font-medium text-white' }: { label: string; value: string; valueClassName?: string }) {
  return (
    <div className="flex items-center justify-between gap-6">
      <span className="text-slate-400">{label}</span>
      <span className={`font-mono tabular-nums ${valueClassName}`}>{value}</span>
    </div>
  )
}

function InfoTile({ label, value }: { label: string; value: string }) {
  return (
    <div className="min-w-0 rounded-lg border border-border bg-muted/20 px-3 py-2">
      <div className="truncate text-[11px] font-semibold uppercase text-muted-foreground">{label}</div>
      <div className="mt-1 truncate text-sm font-medium text-foreground" title={value}>{value}</div>
    </div>
  )
}

function UsageLimitBar({ label, used, limit, percent }: { label: string; used: string; limit: string; percent: number }) {
  const hasLimit = limit !== '-'
  return (
    <div>
      <div className="mb-1 flex items-center justify-between gap-2 text-[11px] text-muted-foreground">
        <span>{label}</span>
        <span className="tabular-nums">{used} / {limit}</span>
      </div>
      {hasLimit ? <AccentBar accent={percent >= 90 ? 'amber' : 'cyan'} ratio={percent / 100} minWidth={2} /> : <div className="h-1.5 rounded-full bg-muted ring-1 ring-inset ring-border/50" />}
    </div>
  )
}

function ModelChips({ label, values }: { label: string; values: string[] }) {
  if (values.length === 0) return null
  return (
    <div>
      <div className="mb-1 text-[11px] font-semibold uppercase text-muted-foreground">{label}</div>
      <div className="flex flex-wrap gap-1.5">
        {values.map((value) => (
          <Badge key={value} variant="secondary" className="max-w-full truncate">{value}</Badge>
        ))}
      </div>
    </div>
  )
}

function readStoredAPIKey(): { key: string; remember: boolean } {
  try {
    const persistent = localStorage.getItem(STORAGE_KEY)
    if (persistent) return { key: persistent, remember: true }
    const sessionOnly = sessionStorage.getItem(STORAGE_KEY)
    if (sessionOnly) return { key: sessionOnly, remember: false }
  } catch {
    // ignore
  }
  return { key: '', remember: false }
}

function writeStoredAPIKey(value: string, remember: boolean) {
  try {
    localStorage.removeItem(STORAGE_KEY)
    sessionStorage.removeItem(STORAGE_KEY)
    if (!value) return
    if (remember) localStorage.setItem(STORAGE_KEY, value)
    else sessionStorage.setItem(STORAGE_KEY, value)
  } catch {
    // ignore
  }
}

function clearStoredAPIKey() {
  try {
    localStorage.removeItem(STORAGE_KEY)
    sessionStorage.removeItem(STORAGE_KEY)
  } catch {
    // ignore
  }
}

function isFastTier(tier?: string | null): boolean {
  const normalized = (tier || '').trim().toLowerCase()
  return normalized === 'fast' || normalized === 'priority'
}

function safeNumber(value?: number | null): number {
  return typeof value === 'number' && Number.isFinite(value) ? value : 0
}

function clampPercent(value: number) {
  if (!Number.isFinite(value)) return 0
  return Math.min(100, Math.max(0, value))
}

function formatUSD(value?: number | null) {
  const numeric = safeNumber(value)
  return `$${numeric.toFixed(6)}`
}

function formatTokenPricePerMillion(value?: number | null) {
  return `$${safeNumber(value).toFixed(4)} / 1M Token`
}

// formatCostCardValue —— 与管理端一致的金额自适应精度（大数省小数，小数留 6 位）
function formatCostCardValue(value?: number | null) {
  const amount = safeNumber(value)
  if (amount >= 100) return `$${amount.toLocaleString(undefined, { maximumFractionDigits: 2 })}`
  if (amount >= 1) return `$${amount.toFixed(2)}`
  if (amount >= 0.01) return `$${amount.toFixed(4)}`
  return `$${amount.toFixed(6)}`
}

function formatNumber(value?: number | null) {
  return Math.round(safeNumber(value)).toLocaleString()
}

function formatCompact(value?: number | null) {
  return Intl.NumberFormat(undefined, { notation: 'compact', maximumFractionDigits: 2 }).format(safeNumber(value))
}

function formatPercent(value?: number | null) {
  return `${safeNumber(value).toFixed(1)}%`
}
