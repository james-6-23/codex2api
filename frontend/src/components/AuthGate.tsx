import type { PropsWithChildren, ReactNode } from 'react'
import { useCallback, useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import {
  ArrowRight,
  CheckCircle2,
  CircleAlert,
  Database,
  KeyRound,
  MonitorCheck,
  RefreshCw,
  Server,
  ShieldCheck,
} from 'lucide-react'
import { ADMIN_AUTH_REQUIRED_EVENT, api, getAdminKey, setAdminKey } from '../api'
import { DEFAULT_SITE_LOGO, useBranding } from '../branding'
import type { SetupHintsResponse, SystemSettings } from '../types'

type AuthStatus =
  | 'checking'
  | 'need_bootstrap'
  | 'setup_review'
  | 'setup_complete'
  | 'need_login'
  | 'authenticated'
type SetupCheckStatus = 'success' | 'warning' | 'error'

type BootstrapSetupInfo = SetupHintsResponse

interface BootstrapStatusResponse {
  needs_bootstrap?: boolean
  source?: string
  setup?: BootstrapSetupInfo
}

interface SetupCheck {
  id: string
  label: string
  detail: string
  status: SetupCheckStatus
}

const MIN_SECRET_LEN = 8
const MAX_SECRET_LEN = 256
const SETUP_SERVICE_URL_KEY = 'codex2api:first_setup_service_url'
const SETUP_REVIEW_DONE_KEY = 'codex2api:first_setup_review_done_v1'

const COPY = {
  zh: {
    bootstrapTitle: '首次配置',
    bootstrapSubtitle: '完成入口、管理密钥、数据与请求监控配置后再进入后台。',
    reviewSubtitle: '管理密钥已配置，确认入口、数据与请求监控设置后进入后台。',
    serviceTitle: '服务地址',
    serviceLabel: '客户端 Base URL',
    serviceDesc: '用于 OpenAI / Anthropic 兼容客户端连接。',
    adminEntry: '后台入口',
    apiEntry: 'API 入口',
    secretTitle: '管理密钥',
    configuredSecret: '已通过当前管理密钥登录',
    bootstrapHint: `至少 ${MIN_SECRET_LEN} 位，保存后用于登录后台和调用 /api/admin/*。`,
    secretLabel: '管理密钥',
    confirmLabel: '再次输入确认',
    dataTitle: '数据目录',
    databaseLabel: '数据库',
    cacheLabel: '缓存',
    imageDirLabel: '图片目录',
    monitorTitle: '请求监控',
    monitorEnabled: '记录全部请求',
    monitorDisabled: '关闭请求日志',
    monitorDesc: '开启后写入 usage_logs，用于趋势、成本、API Key 和错误分析。',
    submit: '保存并检测',
    reviewAction: '确认并检测',
    submitting: '正在保存…',
    completeTitle: '检测结果',
    completeSubtitle: '以下结果来自刚刚保存后的运行状态。',
    enterAdmin: '进入后台',
    checkAgain: '重新检测',
    checkService: '服务地址',
    checkSecret: '管理密钥',
    checkSettings: '系统设置',
    checkHealth: '服务健康',
    checkData: '数据目录',
    checkMonitoring: '请求监控',
    saved: '已保存',
    healthOk: '服务可用，账号池 {{available}}/{{total}} 可用',
    healthWarn: '健康接口返回异常',
    settingsSaved: '系统设置已应用',
    dataReady: '数据位置已识别',
    monitoringFull: '已开启完整请求日志',
    monitoringOff: '已关闭请求日志',
    monitoringErrors: '仅记录错误请求',
    unknown: '未检测到',
    errEmpty: '管理密钥不能为空',
    errTooShort: `管理密钥至少 ${MIN_SECRET_LEN} 位`,
    errTooLong: `管理密钥不可超过 ${MAX_SECRET_LEN} 个字符`,
    errMismatch: '两次输入不一致',
    errServer: '初始化失败，请稍后再试',
    loginSubtitle: '请输入管理密钥登录',
    loginPlaceholder: '请输入 ADMIN_SECRET',
    loginError: '密钥错误，请重新输入',
    loginButton: '登录',
    loadingText: '加载中…',
  },
  en: {
    bootstrapTitle: 'First-run setup',
    bootstrapSubtitle: 'Set the entry URL, admin secret, data paths, and request monitoring before opening the dashboard.',
    reviewSubtitle: 'The admin secret is already configured. Confirm the entry URL, data paths, and request monitoring before opening the dashboard.',
    serviceTitle: 'Service URL',
    serviceLabel: 'Client base URL',
    serviceDesc: 'Used by OpenAI / Anthropic compatible clients.',
    adminEntry: 'Admin entry',
    apiEntry: 'API entry',
    secretTitle: 'Admin secret',
    configuredSecret: 'Signed in with the current admin secret',
    bootstrapHint: `At least ${MIN_SECRET_LEN} characters. Used for dashboard login and /api/admin/* calls.`,
    secretLabel: 'Admin secret',
    confirmLabel: 'Confirm secret',
    dataTitle: 'Data paths',
    databaseLabel: 'Database',
    cacheLabel: 'Cache',
    imageDirLabel: 'Image directory',
    monitorTitle: 'Request monitoring',
    monitorEnabled: 'Record all requests',
    monitorDisabled: 'Disable request logs',
    monitorDesc: 'When enabled, usage_logs powers trends, cost, API key, and error analysis.',
    submit: 'Save and check',
    reviewAction: 'Confirm and check',
    submitting: 'Saving…',
    completeTitle: 'Check results',
    completeSubtitle: 'These results came from the saved runtime state.',
    enterAdmin: 'Open dashboard',
    checkAgain: 'Run checks again',
    checkService: 'Service URL',
    checkSecret: 'Admin secret',
    checkSettings: 'System settings',
    checkHealth: 'Service health',
    checkData: 'Data paths',
    checkMonitoring: 'Request monitoring',
    saved: 'Saved',
    healthOk: 'Service reachable, {{available}}/{{total}} accounts available',
    healthWarn: 'Health endpoint returned an unexpected state',
    settingsSaved: 'System settings applied',
    dataReady: 'Data locations detected',
    monitoringFull: 'Full request logging is enabled',
    monitoringOff: 'Request logging is disabled',
    monitoringErrors: 'Only failed requests are recorded',
    unknown: 'Not detected',
    errEmpty: 'Admin secret cannot be empty',
    errTooShort: `Admin secret must be at least ${MIN_SECRET_LEN} characters`,
    errTooLong: `Admin secret must not exceed ${MAX_SECRET_LEN} characters`,
    errMismatch: 'The two entries do not match',
    errServer: 'Initialization failed, please retry',
    loginSubtitle: 'Enter your admin secret to sign in',
    loginPlaceholder: 'Enter ADMIN_SECRET',
    loginError: 'Invalid secret, please try again',
    loginButton: 'Sign in',
    loadingText: 'Loading…',
  },
} as const

function browserOrigin() {
  return typeof window === 'undefined' ? '' : window.location.origin
}

function initialServiceURL() {
  if (typeof window === 'undefined') return ''
  return window.localStorage.getItem(SETUP_SERVICE_URL_KEY) || browserOrigin()
}

function shouldShowSetupReview() {
  if (typeof window === 'undefined') return false
  return window.localStorage.getItem(SETUP_REVIEW_DONE_KEY) !== '1'
}

function markSetupReviewDone() {
  if (typeof window === 'undefined') return
  window.localStorage.setItem(SETUP_REVIEW_DONE_KEY, '1')
}

function cleanServiceURL(value: string) {
  return value.trim().replace(/\/+$/, '')
}

function errorMessage(error: unknown) {
  return error instanceof Error ? error.message : String(error || '')
}

function interpolate(template: string, values: Record<string, string | number>) {
  return Object.entries(values).reduce(
    (result, [key, value]) => result.replace(`{{${key}}}`, String(value)),
    template
  )
}

function setupDataDetail(settings: SystemSettings | null, setup: BootstrapSetupInfo | null, unknown: string) {
  const databaseLabel = settings?.database_label || setup?.database?.label || setup?.database?.driver || unknown
  const databaseLocation = setup?.database?.location
  const imageDir = setup?.data?.image_local_dir || unknown
  const imageBackend = settings?.image_storage_backend || setup?.data?.image_storage_backend || 'local'
  const parts = [
    databaseLocation ? `${databaseLabel}: ${databaseLocation}` : databaseLabel,
    `Images: ${imageBackend}${imageDir ? ` (${imageDir})` : ''}`,
  ]
  return parts.join(' · ')
}

function setupMonitoringDetail(
  mode: string,
  copy: { monitoringOff: string; monitoringErrors: string; monitoringFull: string }
) {
  if (mode === 'off') return copy.monitoringOff
  if (mode === 'errors') return copy.monitoringErrors
  return copy.monitoringFull
}

function AuthBackdrop() {
  return (
    <div aria-hidden className="pointer-events-none absolute inset-0 overflow-hidden">
      <div className="absolute inset-0 bg-[radial-gradient(ellipse_at_top,color-mix(in_oklab,var(--color-primary)_14%,transparent),transparent_55%)]" />
      <div className="absolute -left-24 top-16 size-72 rounded-full bg-primary/10 blur-3xl" />
      <div className="absolute -right-16 bottom-10 size-80 rounded-full bg-[hsl(var(--info))]/10 blur-3xl" />
      <div className="absolute inset-0 opacity-[0.035] [background-image:linear-gradient(var(--color-foreground)_1px,transparent_1px),linear-gradient(90deg,var(--color-foreground)_1px,transparent_1px)] [background-size:28px_28px]" />
    </div>
  )
}

function PanelShell({
  children,
  siteName,
  logoSrc,
  subtitle,
}: {
  children: ReactNode
  siteName: string
  logoSrc: string
  subtitle: string
}) {
  return (
    <div className="relative flex min-h-dvh items-center justify-center bg-background px-4 py-10">
      <AuthBackdrop />
      <div className="relative z-10 w-full max-w-[920px]">
        <div className="mb-7 text-center">
          <div className="mx-auto mb-4 inline-flex rounded-2xl bg-card/80 p-1.5 shadow-sm ring-1 ring-border/80 backdrop-blur-sm">
            <img src={logoSrc} alt={siteName} className="size-14 rounded-xl object-cover" />
          </div>
          <h1 className="text-[28px] font-bold tracking-tight text-foreground">{siteName}</h1>
          <p className="mx-auto mt-1.5 max-w-lg text-sm leading-relaxed text-muted-foreground">{subtitle}</p>
        </div>
        {children}
      </div>
    </div>
  )
}

function SetupCard({
  icon,
  title,
  children,
}: {
  icon: ReactNode
  title: string
  children: ReactNode
}) {
  return (
    <section className="rounded-2xl border border-border/80 bg-card/90 p-5 shadow-sm backdrop-blur-sm">
      <div className="mb-4 flex items-center gap-2.5">
        <div className="flex size-9 items-center justify-center rounded-xl bg-primary/10 text-primary ring-1 ring-primary/15">
          {icon}
        </div>
        <h2 className="text-base font-bold text-foreground">{title}</h2>
      </div>
      {children}
    </section>
  )
}

function InfoTile({ label, value }: { label: string; value: string }) {
  return (
    <div className="min-w-0 rounded-md border border-border bg-muted/25 px-3 py-2.5">
      <div className="text-[11px] font-bold uppercase text-muted-foreground">{label}</div>
      <div className="mt-1 break-words font-mono text-[13px] text-foreground">{value}</div>
    </div>
  )
}

function CheckRow({ check }: { check: SetupCheck }) {
  const tone =
    check.status === 'success'
      ? 'text-emerald-600 dark:text-emerald-400'
      : check.status === 'warning'
        ? 'text-amber-600 dark:text-amber-400'
        : 'text-red-600 dark:text-red-400'
  return (
    <div className="flex items-start gap-3 rounded-md border border-border bg-muted/20 p-3">
      {check.status === 'success' ? (
        <CheckCircle2 className={`mt-0.5 size-4 shrink-0 ${tone}`} />
      ) : (
        <CircleAlert className={`mt-0.5 size-4 shrink-0 ${tone}`} />
      )}
      <div className="min-w-0">
        <div className="text-sm font-semibold text-foreground">{check.label}</div>
        <div className="mt-0.5 break-words text-xs leading-relaxed text-muted-foreground">{check.detail}</div>
      </div>
    </div>
  )
}

export default function AuthGate({ children }: PropsWithChildren) {
  const { t, i18n } = useTranslation()
  const lang = (i18n.language || 'zh').startsWith('zh') ? 'zh' : 'en'
  const copy = COPY[lang]
  const { siteName, siteLogo } = useBranding()
  const logoSrc = siteLogo || DEFAULT_SITE_LOGO

  const [status, setStatus] = useState<AuthStatus>('checking')
  const [inputKey, setInputKey] = useState('')
  const [error, setError] = useState('')
  const [submitting, setSubmitting] = useState(false)

  const [bootstrapSetup, setBootstrapSetup] = useState<BootstrapSetupInfo | null>(null)
  const [bsServiceUrl, setBsServiceUrl] = useState(initialServiceURL)
  const [bsMonitoringEnabled, setBsMonitoringEnabled] = useState(true)
  const [bsSecret, setBsSecret] = useState('')
  const [bsConfirm, setBsConfirm] = useState('')
  const [bsError, setBsError] = useState('')
  const [bsSubmitting, setBsSubmitting] = useState(false)
  const [setupChecks, setSetupChecks] = useState<SetupCheck[]>([])

  const loadSetupReview = useCallback(async () => {
    try {
      const setup = await api.getSetupHints()
      setBootstrapSetup(setup)
      if (setup.service_url) {
        setBsServiceUrl((current) => current || setup.service_url || browserOrigin())
      }
      if (setup.usage?.log_mode) {
        setBsMonitoringEnabled(setup.usage.log_mode !== 'off')
      }
    } catch {
      setBootstrapSetup(null)
    }
    setStatus('setup_review')
  }, [])

  const checkAuth = useCallback(async () => {
    try {
      const bsRes = await fetch('/api/admin/bootstrap-status')
      if (bsRes.ok) {
        const bs = (await bsRes.json()) as BootstrapStatusResponse
        if (bs.needs_bootstrap) {
          setBootstrapSetup(bs.setup ?? null)
          if (bs.setup?.service_url) {
            setBsServiceUrl((current) => current || bs.setup?.service_url || browserOrigin())
          }
          if (bs.setup?.usage?.log_mode) {
            setBsMonitoringEnabled(bs.setup.usage.log_mode !== 'off')
          }
          setStatus('need_bootstrap')
          return
        }
      }

      const headers: Record<string, string> = {}
      const key = getAdminKey()
      if (key) headers['X-Admin-Key'] = key
      const res = await fetch('/api/admin/health', { headers })
      if (res.status === 401) {
        // 仅 401 才代表密钥确实无效，此时才清除并要求重新登录
        setAdminKey('')
        setStatus('need_login')
      } else if (res.status === 503) {
        setStatus('need_bootstrap')
      } else if (!res.ok) {
        // 5xx/网关错误等多为服务重启或反代瞬时故障，保留密钥不强制登出；
        // 有密钥时乐观地维持登录态（下一轮轮询若返回 401 会纠正），
        // 无密钥则停留在登录页。
        if (key) {
          setStatus('authenticated')
        } else {
          setStatus('need_login')
        }
      } else {
        if (key && shouldShowSetupReview()) {
          await loadSetupReview()
        } else {
          setStatus('authenticated')
        }
      }
    } catch {
      // 网络异常（断网、连接中断、标签页休眠唤醒等）不应清除密钥；
      // 有密钥时乐观维持登录态，无密钥则停留登录页。
      if (getAdminKey()) {
        setStatus('authenticated')
      } else {
        setStatus('need_login')
      }
    }
  }, [loadSetupReview])

  useEffect(() => {
    void checkAuth()
  }, [checkAuth])

  useEffect(() => {
    const timer = window.setInterval(() => {
      void checkAuth()
    }, 30000)

    const handleAuthRequired = () => {
      setError('')
      setInputKey('')
      void checkAuth()
    }

    const handleStorage = (event: StorageEvent) => {
      if (event.key === 'admin_auth_reset_at') {
        handleAuthRequired()
      }
    }

    window.addEventListener(ADMIN_AUTH_REQUIRED_EVENT, handleAuthRequired)
    window.addEventListener('storage', handleStorage)
    return () => {
      window.clearInterval(timer)
      window.removeEventListener(ADMIN_AUTH_REQUIRED_EVENT, handleAuthRequired)
      window.removeEventListener('storage', handleStorage)
    }
  }, [checkAuth])

  const buildSetupChecks = useCallback(async (serviceUrl: string, monitoringEnabled: boolean) => {
    const checks: SetupCheck[] = [
      {
        id: 'service',
        label: copy.checkService,
        status: serviceUrl ? 'success' : 'warning',
        detail: serviceUrl || copy.unknown,
      },
      {
        id: 'secret',
        label: copy.checkSecret,
        status: 'success',
        detail: copy.saved,
      },
    ]

    let settings: SystemSettings | null = null
    try {
      settings = await api.updateSettings({
        usage_log_mode: monitoringEnabled ? 'full' : 'off',
      })
      checks.push({
        id: 'settings',
        label: copy.checkSettings,
        status: 'success',
        detail: copy.settingsSaved,
      })
    } catch (err) {
      checks.push({
        id: 'settings',
        label: copy.checkSettings,
        status: 'warning',
        detail: errorMessage(err) || copy.healthWarn,
      })
    }

    try {
      const health = await api.getHealth()
      const ok = health.status === 'ok'
      checks.push({
        id: 'health',
        label: copy.checkHealth,
        status: ok ? 'success' : 'warning',
        detail: ok
          ? interpolate(copy.healthOk, { available: health.available, total: health.total })
          : copy.healthWarn,
      })
    } catch (err) {
      checks.push({
        id: 'health',
        label: copy.checkHealth,
        status: 'error',
        detail: errorMessage(err) || copy.healthWarn,
      })
    }

    if (!settings) {
      try {
        settings = await api.getSettings()
      } catch {
        settings = null
      }
    }

    checks.push({
      id: 'data',
      label: copy.checkData,
      status: 'success',
      detail: setupDataDetail(settings, bootstrapSetup, copy.unknown),
    })

    const usageMode = settings?.usage_log_mode || (monitoringEnabled ? 'full' : 'off')
    checks.push({
      id: 'monitoring',
      label: copy.checkMonitoring,
      status: usageMode === 'off' ? 'warning' : 'success',
      detail: setupMonitoringDetail(usageMode, copy),
    })

    setSetupChecks(checks)
  }, [bootstrapSetup, copy])

  const handleLogin = async () => {
    if (!inputKey.trim()) {
      setError(t('auth.error'))
      return
    }
    setSubmitting(true)
    setError('')
    try {
      const res = await fetch('/api/admin/health', {
        headers: { 'X-Admin-Key': inputKey.trim() },
      })
      if (res.status === 401) {
        setError(copy.loginError)
      } else if (res.status === 503) {
        setStatus('need_bootstrap')
      } else if (!res.ok) {
        setError(copy.loginError)
      } else {
        setAdminKey(inputKey.trim())
        if (shouldShowSetupReview()) {
          await loadSetupReview()
        } else {
          setStatus('authenticated')
        }
      }
    } catch {
      setError(copy.loginError)
    } finally {
      setSubmitting(false)
    }
  }

  const handleBootstrap = async () => {
    setBsError('')
    const secret = bsSecret.trim()
    const confirm = bsConfirm.trim()
    const serviceUrl = cleanServiceURL(bsServiceUrl || bootstrapSetup?.service_url || browserOrigin())
    if (!secret) {
      setBsError(copy.errEmpty)
      return
    }
    if (secret.length < MIN_SECRET_LEN) {
      setBsError(copy.errTooShort)
      return
    }
    if (secret.length > MAX_SECRET_LEN) {
      setBsError(copy.errTooLong)
      return
    }
    if (secret !== confirm) {
      setBsError(copy.errMismatch)
      return
    }
    setBsSubmitting(true)
    setSetupChecks([])
    try {
      const res = await fetch('/api/admin/bootstrap', {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ admin_secret: secret }),
      })
      if (!res.ok) {
        const body = (await res.json().catch(() => ({}))) as { error?: string }
        setBsError(body.error || copy.errServer)
        return
      }
      setAdminKey(secret)
      if (typeof window !== 'undefined' && serviceUrl) {
        window.localStorage.setItem(SETUP_SERVICE_URL_KEY, serviceUrl)
      }
      setBsServiceUrl(serviceUrl)
      setBsSecret('')
      setBsConfirm('')
      await buildSetupChecks(serviceUrl, bsMonitoringEnabled)
      markSetupReviewDone()
      setStatus('setup_complete')
    } catch {
      setBsError(copy.errServer)
    } finally {
      setBsSubmitting(false)
    }
  }

  const handleSetupReview = async () => {
    const serviceUrl = cleanServiceURL(bsServiceUrl || bootstrapSetup?.service_url || browserOrigin())
    setBsSubmitting(true)
    setBsError('')
    setSetupChecks([])
    try {
      if (typeof window !== 'undefined' && serviceUrl) {
        window.localStorage.setItem(SETUP_SERVICE_URL_KEY, serviceUrl)
      }
      setBsServiceUrl(serviceUrl)
      await buildSetupChecks(serviceUrl, bsMonitoringEnabled)
      markSetupReviewDone()
      setStatus('setup_complete')
    } catch {
      setBsError(copy.errServer)
    } finally {
      setBsSubmitting(false)
    }
  }

  if (status === 'checking') {
    return (
      <div className="relative flex min-h-dvh items-center justify-center bg-background">
        <AuthBackdrop />
        <div className="relative z-10 text-center">
          <div className="mx-auto mb-3 flex size-12 items-center justify-center rounded-2xl bg-card/80 shadow-sm ring-1 ring-border/80">
            <div className="size-6 animate-spin rounded-full border-[2.5px] border-primary/25 border-t-primary" />
          </div>
          <p className="text-sm text-muted-foreground">{copy.loadingText}</p>
        </div>
      </div>
    )
  }

  if (status === 'need_bootstrap') {
    const serviceUrl = cleanServiceURL(bsServiceUrl || bootstrapSetup?.service_url || browserOrigin())
    const adminUrl = serviceUrl ? `${serviceUrl}/admin/` : bootstrapSetup?.admin_url || copy.unknown
    const apiUrl = serviceUrl ? `${serviceUrl}/v1` : bootstrapSetup?.api_base_url || copy.unknown
    const databaseValue = [
      bootstrapSetup?.database?.label || bootstrapSetup?.database?.driver || copy.unknown,
      bootstrapSetup?.database?.location,
    ].filter(Boolean).join(' · ')
    const cacheValue = bootstrapSetup?.cache?.label || bootstrapSetup?.cache?.driver || copy.unknown
    const imageValue = [
      bootstrapSetup?.data?.image_storage_backend || 'local',
      bootstrapSetup?.data?.image_local_dir || copy.unknown,
    ].filter(Boolean).join(' · ')

    return (
      <PanelShell siteName={siteName} logoSrc={logoSrc} subtitle={copy.bootstrapSubtitle}>
        <div className="grid gap-4 lg:grid-cols-[minmax(0,1.08fr)_minmax(320px,0.92fr)]">
          <div className="space-y-4">
            <SetupCard icon={<Server className="size-4" />} title={copy.serviceTitle}>
              <label className="block text-sm font-semibold text-foreground">{copy.serviceLabel}</label>
              <input
                type="url"
                value={bsServiceUrl}
                onChange={(e) => setBsServiceUrl(e.target.value)}
                className="mt-2 h-10 w-full rounded-md border border-input bg-background px-3.5 font-mono text-[14px] outline-none transition-all focus:border-primary/40 focus:ring-2 focus:ring-primary/10"
              />
              <p className="mt-2 text-xs leading-relaxed text-muted-foreground">{copy.serviceDesc}</p>
              <div className="mt-3 grid gap-2 sm:grid-cols-2">
                <InfoTile label={copy.adminEntry} value={adminUrl} />
                <InfoTile label={copy.apiEntry} value={apiUrl} />
              </div>
            </SetupCard>

            <SetupCard icon={<Database className="size-4" />} title={copy.dataTitle}>
              <div className="grid gap-2 sm:grid-cols-3">
                <InfoTile label={copy.databaseLabel} value={databaseValue || copy.unknown} />
                <InfoTile label={copy.cacheLabel} value={cacheValue} />
                <InfoTile label={copy.imageDirLabel} value={imageValue} />
              </div>
            </SetupCard>
          </div>

          <div className="space-y-4">
            <SetupCard icon={<KeyRound className="size-4" />} title={copy.secretTitle}>
              <p className="text-xs leading-relaxed text-muted-foreground">{copy.bootstrapHint}</p>
              <div className="mt-4 space-y-4">
                <div>
                  <label className="mb-2 block text-sm font-semibold text-foreground">{copy.secretLabel}</label>
                  <input
                    type="password"
                    value={bsSecret}
                    onChange={(e) => { setBsSecret(e.target.value); setBsError('') }}
                    className="h-10 w-full rounded-md border border-input bg-background px-3.5 text-[15px] outline-none transition-all focus:border-primary/40 focus:ring-2 focus:ring-primary/10"
                    autoFocus
                  />
                </div>

                <div>
                  <label className="mb-2 block text-sm font-semibold text-foreground">{copy.confirmLabel}</label>
                  <input
                    type="password"
                    value={bsConfirm}
                    onChange={(e) => { setBsConfirm(e.target.value); setBsError('') }}
                    onKeyDown={(e) => { if (e.key === 'Enter') void handleBootstrap() }}
                    className="h-10 w-full rounded-md border border-input bg-background px-3.5 text-[15px] outline-none transition-all focus:border-primary/40 focus:ring-2 focus:ring-primary/10"
                  />
                </div>
              </div>
            </SetupCard>

            <SetupCard icon={<MonitorCheck className="size-4" />} title={copy.monitorTitle}>
              <button
                type="button"
                onClick={() => setBsMonitoringEnabled((enabled) => !enabled)}
                className="flex w-full items-center justify-between gap-3 rounded-md border border-border bg-muted/25 px-3 py-3 text-left transition-colors hover:bg-muted/40"
              >
                <span>
                  <span className="block text-sm font-semibold text-foreground">
                    {bsMonitoringEnabled ? copy.monitorEnabled : copy.monitorDisabled}
                  </span>
                  <span className="mt-1 block text-xs leading-relaxed text-muted-foreground">{copy.monitorDesc}</span>
                </span>
                <span
                  className={`relative h-6 w-11 shrink-0 rounded-full transition-colors ${bsMonitoringEnabled ? 'bg-primary' : 'bg-muted-foreground/30'}`}
                  aria-hidden="true"
                >
                  <span
                    className={`absolute top-1 size-4 rounded-full bg-white shadow-sm transition-transform ${bsMonitoringEnabled ? 'translate-x-6' : 'translate-x-1'}`}
                  />
                </span>
              </button>

              {bsError && (
                <div className="mt-4 rounded-md border border-red-500/20 bg-red-500/5 px-3 py-2 text-sm font-medium text-red-500">
                  {bsError}
                </div>
              )}

              <button
                onClick={() => void handleBootstrap()}
                disabled={bsSubmitting}
                className="mt-4 flex h-10 w-full items-center justify-center gap-2 rounded-md bg-primary px-4 text-[15px] font-semibold text-primary-foreground shadow-sm transition-colors hover:bg-primary/90 disabled:opacity-50"
              >
                {bsSubmitting ? <RefreshCw className="size-4 animate-spin" /> : <ShieldCheck className="size-4" />}
                {bsSubmitting ? copy.submitting : copy.submit}
              </button>
            </SetupCard>
          </div>
        </div>
      </PanelShell>
    )
  }

  if (status === 'setup_review') {
    const serviceUrl = cleanServiceURL(bsServiceUrl || bootstrapSetup?.service_url || browserOrigin())
    const adminUrl = serviceUrl ? `${serviceUrl}/admin/` : bootstrapSetup?.admin_url || copy.unknown
    const apiUrl = serviceUrl ? `${serviceUrl}/v1` : bootstrapSetup?.api_base_url || copy.unknown
    const databaseValue = [
      bootstrapSetup?.database?.label || bootstrapSetup?.database?.driver || copy.unknown,
      bootstrapSetup?.database?.location,
    ].filter(Boolean).join(' · ')
    const cacheValue = bootstrapSetup?.cache?.label || bootstrapSetup?.cache?.driver || copy.unknown
    const imageValue = [
      bootstrapSetup?.data?.image_storage_backend || 'local',
      bootstrapSetup?.data?.image_local_dir || copy.unknown,
    ].filter(Boolean).join(' · ')

    return (
      <PanelShell siteName={siteName} logoSrc={logoSrc} subtitle={copy.reviewSubtitle}>
        <div className="grid gap-4 lg:grid-cols-[minmax(0,1.08fr)_minmax(320px,0.92fr)]">
          <div className="space-y-4">
            <SetupCard icon={<Server className="size-4" />} title={copy.serviceTitle}>
              <label className="block text-sm font-semibold text-foreground">{copy.serviceLabel}</label>
              <input
                type="url"
                value={bsServiceUrl}
                onChange={(e) => setBsServiceUrl(e.target.value)}
                className="mt-2 h-10 w-full rounded-md border border-input bg-background px-3.5 font-mono text-[14px] outline-none transition-all focus:border-primary/40 focus:ring-2 focus:ring-primary/10"
              />
              <p className="mt-2 text-xs leading-relaxed text-muted-foreground">{copy.serviceDesc}</p>
              <div className="mt-3 grid gap-2 sm:grid-cols-2">
                <InfoTile label={copy.adminEntry} value={adminUrl} />
                <InfoTile label={copy.apiEntry} value={apiUrl} />
              </div>
            </SetupCard>

            <SetupCard icon={<Database className="size-4" />} title={copy.dataTitle}>
              <div className="grid gap-2 sm:grid-cols-3">
                <InfoTile label={copy.databaseLabel} value={databaseValue || copy.unknown} />
                <InfoTile label={copy.cacheLabel} value={cacheValue} />
                <InfoTile label={copy.imageDirLabel} value={imageValue} />
              </div>
            </SetupCard>
          </div>

          <div className="space-y-4">
            <SetupCard icon={<KeyRound className="size-4" />} title={copy.secretTitle}>
              <div className="flex items-start gap-3 rounded-md border border-emerald-500/20 bg-emerald-500/5 p-3">
                <CheckCircle2 className="mt-0.5 size-4 shrink-0 text-emerald-600 dark:text-emerald-400" />
                <div className="min-w-0">
                  <div className="text-sm font-semibold text-foreground">{copy.saved}</div>
                  <div className="mt-0.5 text-xs leading-relaxed text-muted-foreground">{copy.configuredSecret}</div>
                </div>
              </div>
            </SetupCard>

            <SetupCard icon={<MonitorCheck className="size-4" />} title={copy.monitorTitle}>
              <button
                type="button"
                onClick={() => setBsMonitoringEnabled((enabled) => !enabled)}
                className="flex w-full items-center justify-between gap-3 rounded-md border border-border bg-muted/25 px-3 py-3 text-left transition-colors hover:bg-muted/40"
              >
                <span>
                  <span className="block text-sm font-semibold text-foreground">
                    {bsMonitoringEnabled ? copy.monitorEnabled : copy.monitorDisabled}
                  </span>
                  <span className="mt-1 block text-xs leading-relaxed text-muted-foreground">{copy.monitorDesc}</span>
                </span>
                <span
                  className={`relative h-6 w-11 shrink-0 rounded-full transition-colors ${bsMonitoringEnabled ? 'bg-primary' : 'bg-muted-foreground/30'}`}
                  aria-hidden="true"
                >
                  <span
                    className={`absolute top-1 size-4 rounded-full bg-white shadow-sm transition-transform ${bsMonitoringEnabled ? 'translate-x-6' : 'translate-x-1'}`}
                  />
                </span>
              </button>

              {bsError && (
                <div className="mt-4 rounded-md border border-red-500/20 bg-red-500/5 px-3 py-2 text-sm font-medium text-red-500">
                  {bsError}
                </div>
              )}

              <button
                onClick={() => void handleSetupReview()}
                disabled={bsSubmitting}
                className="mt-4 flex h-10 w-full items-center justify-center gap-2 rounded-md bg-primary px-4 text-[15px] font-semibold text-primary-foreground shadow-sm transition-colors hover:bg-primary/90 disabled:opacity-50"
              >
                {bsSubmitting ? <RefreshCw className="size-4 animate-spin" /> : <ShieldCheck className="size-4" />}
                {bsSubmitting ? copy.submitting : copy.reviewAction}
              </button>
            </SetupCard>
          </div>
        </div>
      </PanelShell>
    )
  }

  if (status === 'setup_complete') {
    return (
      <PanelShell siteName={siteName} logoSrc={logoSrc} subtitle={copy.completeSubtitle}>
        <div className="mx-auto max-w-[640px] rounded-2xl border border-border/80 bg-card/90 p-5 shadow-lg backdrop-blur-sm sm:p-6">
          <div className="mb-4 flex items-center gap-2.5">
            <div className="flex size-9 items-center justify-center rounded-xl bg-emerald-500/10 text-emerald-600 ring-1 ring-emerald-500/20 dark:text-emerald-400">
              <CheckCircle2 className="size-4" />
            </div>
            <h2 className="text-base font-bold text-foreground">{copy.completeTitle}</h2>
          </div>
          <div className="space-y-2">
            {setupChecks.map((check) => (
              <CheckRow key={check.id} check={check} />
            ))}
          </div>
          <button
            onClick={() => {
              markSetupReviewDone()
              setStatus('authenticated')
            }}
            className="mt-5 flex h-11 w-full items-center justify-center gap-2 rounded-xl bg-primary px-4 text-[15px] font-semibold text-primary-foreground shadow-sm transition-colors hover:bg-primary/90"
          >
            {copy.enterAdmin}
            <ArrowRight className="size-4" />
          </button>
        </div>
      </PanelShell>
    )
  }

  if (status === 'need_login') {
    return (
      <div className="relative flex min-h-dvh items-center justify-center bg-background px-4 py-10">
        <AuthBackdrop />
        <div className="relative z-10 mx-auto w-full max-w-[420px]">
          <div className="mb-7 text-center">
            <div className="mx-auto mb-4 inline-flex rounded-2xl bg-card/80 p-1.5 shadow-sm ring-1 ring-border/80 backdrop-blur-sm">
              <img src={logoSrc} alt={siteName} className="size-14 rounded-xl object-cover" />
            </div>
            <h1 className="text-[28px] font-bold tracking-tight text-foreground">{siteName}</h1>
            <p className="mt-1.5 text-sm text-muted-foreground">{copy.loginSubtitle}</p>
          </div>

          <div className="rounded-2xl border border-border/80 bg-card/90 p-5 shadow-lg backdrop-blur-sm sm:p-6">
            <div className="mb-5 flex items-center gap-2.5">
              <div className="flex size-9 items-center justify-center rounded-xl bg-primary/10 text-primary ring-1 ring-primary/15">
                <KeyRound className="size-4" />
              </div>
              <div>
                <div className="text-sm font-semibold text-foreground">{t('settings.adminSecret')}</div>
                <div className="text-[12px] text-muted-foreground">{copy.loginSubtitle}</div>
              </div>
            </div>
            <div className="space-y-4">
              <div>
                <input
                  type="password"
                  value={inputKey}
                  onChange={(e) => { setInputKey(e.target.value); setError('') }}
                  onKeyDown={(e) => { if (e.key === 'Enter') void handleLogin() }}
                  placeholder={copy.loginPlaceholder}
                  autoFocus
                  className="h-11 w-full rounded-xl border border-input bg-background/80 px-3.5 text-[15px] outline-none transition-all focus:border-primary/40 focus:ring-2 focus:ring-primary/15"
                />
              </div>

              {error && (
                <div className="rounded-lg border border-red-500/20 bg-red-500/8 px-3 py-2 text-sm font-medium text-red-600 dark:text-red-400">
                  {error}
                </div>
              )}

              <button
                onClick={() => void handleLogin()}
                disabled={submitting}
                className="flex h-11 w-full items-center justify-center gap-2 rounded-xl bg-primary text-[15px] font-semibold text-primary-foreground shadow-sm transition-all hover:bg-primary/90 hover:shadow-md disabled:opacity-50"
              >
                {submitting ? <RefreshCw className="size-4 animate-spin" /> : <ArrowRight className="size-4" />}
                {submitting ? copy.loadingText : copy.loginButton}
              </button>
            </div>
          </div>
        </div>
      </div>
    )
  }

  return <>{children}</>
}
