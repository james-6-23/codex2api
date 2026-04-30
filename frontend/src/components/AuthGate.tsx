import type { PropsWithChildren } from 'react'
import { useCallback, useEffect, useState } from 'react'
import { useTranslation } from 'react-i18next'
import { ADMIN_AUTH_REQUIRED_EVENT, getAdminKey, setAdminKey } from '../api'
import logoImg from '../assets/logo.png'

type AuthStatus = 'checking' | 'need_bootstrap' | 'need_login' | 'authenticated'

const MIN_SECRET_LEN = 8
const MAX_SECRET_LEN = 256

const COPY = {
  zh: {
    bootstrapTitle: '首次使用：设置管理密钥',
    bootstrapSubtitle: '该密钥用于登录管理后台与调用 /api/admin/* 接口，请妥善保管。',
    bootstrapHint: `至少 ${MIN_SECRET_LEN} 位`,
    secretLabel: '管理密钥',
    confirmLabel: '再次输入确认',
    submit: '完成初始化并登录',
    submitting: '正在保存…',
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
    bootstrapTitle: 'First-run setup: choose an admin secret',
    bootstrapSubtitle: 'This secret is required for both web login and /api/admin/* API calls. Store it safely.',
    bootstrapHint: `At least ${MIN_SECRET_LEN} characters.`,
    secretLabel: 'Admin secret',
    confirmLabel: 'Confirm secret',
    submit: 'Initialize and sign in',
    submitting: 'Saving…',
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

export default function AuthGate({ children }: PropsWithChildren) {
  const { t, i18n } = useTranslation()
  const lang = (i18n.language || 'zh').startsWith('zh') ? 'zh' : 'en'
  const copy = COPY[lang]

  const [status, setStatus] = useState<AuthStatus>('checking')
  const [inputKey, setInputKey] = useState('')
  const [error, setError] = useState('')
  const [submitting, setSubmitting] = useState(false)

  const [bsSecret, setBsSecret] = useState('')
  const [bsConfirm, setBsConfirm] = useState('')
  const [bsError, setBsError] = useState('')
  const [bsSubmitting, setBsSubmitting] = useState(false)

  const checkAuth = useCallback(async () => {
    try {
      const bsRes = await fetch('/api/admin/bootstrap-status')
      if (bsRes.ok) {
        const bs = (await bsRes.json()) as { needs_bootstrap?: boolean }
        if (bs.needs_bootstrap) {
          setStatus('need_bootstrap')
          return
        }
      }

      const headers: Record<string, string> = {}
      const key = getAdminKey()
      if (key) headers['X-Admin-Key'] = key
      const res = await fetch('/api/admin/health', { headers })
      if (res.status === 401) {
        setAdminKey('')
        setStatus('need_login')
      } else if (res.status === 503) {
        setStatus('need_bootstrap')
      } else {
        setStatus('authenticated')
      }
    } catch {
      setStatus('authenticated')
    }
  }, [])

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
      } else {
        setAdminKey(inputKey.trim())
        setStatus('authenticated')
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
      setBsSecret('')
      setBsConfirm('')
      setStatus('authenticated')
    } catch {
      setBsError(copy.errServer)
    } finally {
      setBsSubmitting(false)
    }
  }

  if (status === 'checking') {
    return (
      <div className="flex items-center justify-center min-h-dvh">
        <div className="text-center">
          <div className="size-8 mx-auto mb-3 rounded-full border-3 border-primary/30 border-t-primary animate-spin" />
          <p className="text-sm text-muted-foreground">{copy.loadingText}</p>
        </div>
      </div>
    )
  }

  if (status === 'need_bootstrap') {
    return (
      <div className="flex items-center justify-center min-h-dvh bg-background">
        <div className="w-full max-w-[460px] mx-4">
          <div className="text-center mb-6">
            <img src={logoImg} alt="Codex2API" className="size-14 rounded-lg object-cover shadow-sm mx-auto mb-4" />
            <h1 className="text-[26px] font-bold text-foreground">Codex2API</h1>
            <p className="mt-1 text-sm text-muted-foreground">{copy.bootstrapSubtitle}</p>
          </div>

          <div className="rounded-lg border border-border bg-card p-5 shadow-sm">
            <h2 className="text-base font-bold text-foreground">{copy.bootstrapTitle}</h2>
            <p className="mt-1 text-xs text-muted-foreground">{copy.bootstrapHint}</p>

            <div className="mt-4 space-y-4">
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">{copy.secretLabel}</label>
                <input
                  type="password"
                  value={bsSecret}
                  onChange={(e) => { setBsSecret(e.target.value); setBsError('') }}
                  className="w-full h-10 px-3.5 rounded-md border border-input bg-background text-[15px] outline-none transition-all focus:border-primary/40 focus:ring-2 focus:ring-primary/10"
                  autoFocus
                />
              </div>

              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">{copy.confirmLabel}</label>
                <input
                  type="password"
                  value={bsConfirm}
                  onChange={(e) => { setBsConfirm(e.target.value); setBsError('') }}
                  onKeyDown={(e) => { if (e.key === 'Enter') void handleBootstrap() }}
                  className="w-full h-10 px-3.5 rounded-md border border-input bg-background text-[15px] outline-none transition-all focus:border-primary/40 focus:ring-2 focus:ring-primary/10"
                />
              </div>

              {bsError && (
                <div className="text-sm text-red-500 font-medium px-1">{bsError}</div>
              )}

              <button
                onClick={() => void handleBootstrap()}
                disabled={bsSubmitting}
                className="w-full h-10 rounded-md bg-primary text-primary-foreground font-semibold text-[15px] shadow-sm transition-colors hover:bg-primary/90 disabled:opacity-50"
              >
                {bsSubmitting ? copy.submitting : copy.submit}
              </button>
            </div>
          </div>
        </div>
      </div>
    )
  }

  if (status === 'need_login') {
    return (
      <div className="flex items-center justify-center min-h-dvh bg-background">
        <div className="w-full max-w-[400px] mx-4">
          <div className="text-center mb-6">
            <img src={logoImg} alt="Codex2API" className="size-14 rounded-lg object-cover shadow-sm mx-auto mb-4" />
            <h1 className="text-[26px] font-bold text-foreground">Codex2API</h1>
            <p className="text-sm text-muted-foreground mt-1">{copy.loginSubtitle}</p>
          </div>

          <div className="rounded-lg border border-border bg-card p-5 shadow-sm">
            <div className="space-y-4">
              <div>
                <label className="block mb-2 text-sm font-semibold text-muted-foreground">{t('settings.adminSecret')}</label>
                <input
                  type="password"
                  value={inputKey}
                  onChange={(e) => { setInputKey(e.target.value); setError('') }}
                  onKeyDown={(e) => { if (e.key === 'Enter') void handleLogin() }}
                  placeholder={copy.loginPlaceholder}
                  autoFocus
                  className="w-full h-10 px-3.5 rounded-md border border-input bg-background text-[15px] outline-none transition-all focus:border-primary/40 focus:ring-2 focus:ring-primary/10"
                />
              </div>

              {error && (
                <div className="text-sm text-red-500 font-medium px-1">{error}</div>
              )}

              <button
                onClick={() => void handleLogin()}
                disabled={submitting}
                className="w-full h-10 rounded-md bg-primary text-primary-foreground font-semibold text-[15px] shadow-sm transition-colors hover:bg-primary/90 disabled:opacity-50"
              >
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
