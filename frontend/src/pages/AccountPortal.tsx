import { type FormEvent, useMemo, useState } from 'react'
import { useTranslation } from 'react-i18next'
import {
  ArrowRight,
  CheckCircle2,
  ClipboardPaste,
  Copy,
  ExternalLink,
  Languages,
  Link2,
  Loader2,
  Lock,
  Mail,
  PartyPopper,
  ShieldCheck,
} from 'lucide-react'
import { api } from '../api'
import { useBranding } from '../branding'
import { getErrorMessage } from '../utils/error'
import { CinematicThemeSwitcher } from '@/components/ui/cinematic-theme-switcher'
import { Button } from '@/components/ui/button'
import { Card, CardContent } from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { cn } from '@/lib/utils'

const EMAIL_RE = /^[^\s@]+@[^\s@]+\.[^\s@]+$/

// 从用户粘贴的重定向地址(通常是打不开的 localhost:1455/auth/callback?code=...&state=...)
// 中解析出 code 与 state。兼容整段 URL、仅 query 串、带 # 片段等多种粘贴形态。
function parseCodeState(input: string): { code: string; state: string } {
  const trimmed = input.trim()
  if (!trimmed) return { code: '', state: '' }
  let query = ''
  const qIndex = trimmed.indexOf('?')
  if (qIndex >= 0) {
    query = trimmed.slice(qIndex + 1)
  } else if (trimmed.includes('=')) {
    // 粘贴的可能是不带 ? 的裸 query 串
    query = trimmed
  }
  const hashIndex = query.indexOf('#')
  if (hashIndex >= 0) query = query.slice(0, hashIndex)
  try {
    const params = new URLSearchParams(query)
    return {
      code: (params.get('code') ?? '').trim(),
      state: (params.get('state') ?? '').trim(),
    }
  } catch {
    return { code: '', state: '' }
  }
}

function StepBadge({ index, active, done }: { index: number; active: boolean; done: boolean }) {
  return (
    <div
      className={cn(
        'flex size-7 shrink-0 items-center justify-center rounded-full border text-xs font-semibold transition-colors',
        done
          ? 'border-transparent bg-primary text-primary-foreground'
          : active
            ? 'border-primary/40 bg-primary/10 text-primary'
            : 'border-border/70 bg-muted/40 text-muted-foreground',
      )}
    >
      {done ? <CheckCircle2 className="size-4" /> : index}
    </div>
  )
}

export default function AccountPortal() {
  const { t, i18n } = useTranslation()
  const { siteName, siteLogo, defaultLogo } = useBranding()
  const logoSrc = siteLogo || defaultLogo

  const [contactEmail, setContactEmail] = useState('')
  const [generating, setGenerating] = useState(false)
  const [authUrl, setAuthUrl] = useState('')
  const [sessionId, setSessionId] = useState('')

  const [pastedUrl, setPastedUrl] = useState('')
  const [code, setCode] = useState('')
  const [state, setState] = useState('')
  const [showFallback, setShowFallback] = useState(false)

  const [submitting, setSubmitting] = useState(false)
  const [successMessage, setSuccessMessage] = useState('')
  const [error, setError] = useState('')
  const [copied, setCopied] = useState(false)
  const [portalDisabled, setPortalDisabled] = useState(false)

  const emailValid = EMAIL_RE.test(contactEmail.trim())
  const hasAuthUrl = Boolean(authUrl && sessionId)
  const canSubmit = hasAuthUrl && Boolean(code.trim()) && Boolean(state.trim())

  const currentStep = useMemo(() => {
    if (successMessage) return 3
    if (hasAuthUrl) return 2
    return 1
  }, [hasAuthUrl, successMessage])

  const handlePastedUrlChange = (value: string) => {
    setPastedUrl(value)
    const parsed = parseCodeState(value)
    if (parsed.code) setCode(parsed.code)
    if (parsed.state) setState(parsed.state)
  }

  const handleGenerate = async (event: FormEvent<HTMLFormElement>) => {
    event.preventDefault()
    setError('')
    if (!emailValid) {
      setError(t('accountPortal.invalidEmail'))
      return
    }
    setGenerating(true)
    try {
      const res = await api.generateAccountPortalAuthURL({ contact_email: contactEmail.trim() })
      setAuthUrl(res.auth_url)
      setSessionId(res.session_id)
      // 换邮箱重新生成时清空上一轮授权码,避免误提交。
      setPastedUrl('')
      setCode('')
      setState('')
      setSuccessMessage('')
    } catch (err) {
      const status = (err as { status?: number }).status
      if (status === 404) {
        setPortalDisabled(true)
        return
      }
      setError(getErrorMessage(err))
    } finally {
      setGenerating(false)
    }
  }

  const handleSubmit = async () => {
    setError('')
    if (!canSubmit) {
      setError(t('accountPortal.codeStateRequired'))
      return
    }
    setSubmitting(true)
    try {
      const res = await api.submitAccountPortalCode({
        session_id: sessionId,
        code: code.trim(),
        state: state.trim(),
      })
      setSuccessMessage(res.message || t('accountPortal.submitSuccess'))
    } catch (err) {
      const status = (err as { status?: number }).status
      if (status === 404) {
        setPortalDisabled(true)
        return
      }
      setError(getErrorMessage(err))
    } finally {
      setSubmitting(false)
    }
  }

  const handleCopyAuthUrl = async () => {
    if (!authUrl) return
    try {
      await navigator.clipboard.writeText(authUrl)
      setCopied(true)
      window.setTimeout(() => setCopied(false), 2000)
    } catch {
      // 忽略剪贴板失败
    }
  }

  const handleReset = () => {
    setContactEmail('')
    setAuthUrl('')
    setSessionId('')
    setPastedUrl('')
    setCode('')
    setState('')
    setShowFallback(false)
    setSuccessMessage('')
    setError('')
  }

  const toolbar = (
    <div className="flex items-center gap-2">
      <Button
        variant="outline"
        size="icon-sm"
        onClick={() => i18n.changeLanguage(i18n.language === 'zh' ? 'en' : 'zh')}
        title={i18n.language === 'zh' ? 'English' : '中文'}
      >
        <Languages className="size-4" />
      </Button>
      <CinematicThemeSwitcher size="compact" />
    </div>
  )

  if (portalDisabled) {
    return (
      <div className="relative flex min-h-dvh items-center justify-center overflow-hidden bg-background px-4 py-10 text-foreground">
        <div className="absolute right-4 top-4 z-10">{toolbar}</div>
        <Card className="relative w-full max-w-md border-border/70 shadow-2xl shadow-primary/5">
          <CardContent className="flex flex-col items-center p-8 text-center">
            <div className="flex size-14 items-center justify-center rounded-2xl border border-border/70 bg-muted/40 text-muted-foreground">
              <Lock className="size-6" />
            </div>
            <h1 className="mt-5 text-xl font-semibold tracking-tight">{t('accountPortal.disabledTitle')}</h1>
            <p className="mt-2 max-w-sm text-sm leading-relaxed text-muted-foreground">
              {t('accountPortal.disabledDesc')}
            </p>
          </CardContent>
        </Card>
      </div>
    )
  }

  return (
    <div className="relative min-h-dvh bg-background text-foreground">
      <div
        aria-hidden="true"
        className="pointer-events-none fixed inset-0 -z-10 opacity-100 [background:radial-gradient(ellipse_70%_50%_at_15%_-10%,color-mix(in_oklab,var(--color-primary)_12%,transparent),transparent_55%),radial-gradient(ellipse_55%_45%_at_90%_0%,color-mix(in_oklab,var(--color-primary)_8%,transparent),transparent_50%),linear-gradient(180deg,color-mix(in_oklab,var(--color-muted)_55%,var(--color-background)),var(--color-background)_42%)]"
      />

      <header className="sticky top-0 z-20 border-b border-border/60 bg-card/75 shadow-sm backdrop-blur-xl supports-[backdrop-filter]:bg-card/65">
        <div className="mx-auto flex h-14 max-w-3xl items-center gap-3 px-3 sm:px-5">
          <div className="flex min-w-0 shrink-0 items-center gap-2.5">
            <img src={logoSrc} alt={siteName} className="size-8 rounded-lg object-cover shadow-sm ring-1 ring-border/60" />
            <div className="min-w-0 hidden min-[400px]:block">
              <h1 className="truncate text-sm font-semibold tracking-tight leading-tight">{t('accountPortal.title')}</h1>
              <div className="truncate text-[11px] text-muted-foreground leading-tight">{siteName}</div>
            </div>
          </div>
          <div className="flex flex-1 justify-end">{toolbar}</div>
        </div>
      </header>

      <main className="mx-auto w-full max-w-3xl px-3 py-6 sm:px-5 sm:py-8">
        <div className="mb-6 text-center">
          <div className="mx-auto flex size-14 items-center justify-center rounded-2xl border border-border/70 bg-background/80 text-primary shadow-sm">
            <ShieldCheck className="size-7" />
          </div>
          <h2 className="mt-4 text-xl font-semibold tracking-tight sm:text-2xl">{t('accountPortal.heading')}</h2>
          <p className="mx-auto mt-2 max-w-xl text-sm leading-relaxed text-muted-foreground">
            {t('accountPortal.subheading')}
          </p>
        </div>

        {successMessage ? (
          <Card className="border-border/70 shadow-md">
            <CardContent className="flex flex-col items-center p-8 text-center">
              <div className="flex size-14 items-center justify-center rounded-2xl bg-emerald-500/14 text-emerald-600 dark:bg-emerald-500/20 dark:text-emerald-300">
                <PartyPopper className="size-7" />
              </div>
              <h3 className="mt-5 text-lg font-semibold tracking-tight">{t('accountPortal.doneTitle')}</h3>
              <p className="mt-2 max-w-md text-sm leading-relaxed text-muted-foreground">{successMessage}</p>
              <Button variant="outline" className="mt-6" onClick={handleReset}>
                {t('accountPortal.contributeAnother')}
              </Button>
            </CardContent>
          </Card>
        ) : (
          <div className="space-y-4">
            {/* Step 1 — contact email */}
            <Card className="border-border/70 shadow-sm">
              <CardContent className="p-4 sm:p-5">
                <div className="flex items-start gap-3">
                  <StepBadge index={1} active={currentStep === 1} done={hasAuthUrl} />
                  <div className="min-w-0 flex-1">
                    <div className="text-sm font-semibold tracking-tight">{t('accountPortal.step1Title')}</div>
                    <p className="mt-1 text-xs leading-relaxed text-muted-foreground">{t('accountPortal.step1Desc')}</p>
                    <form onSubmit={handleGenerate} className="mt-3 flex flex-col gap-2 sm:flex-row">
                      <div className="relative flex-1">
                        <Mail className="pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2 text-muted-foreground" />
                        <Input
                          type="email"
                          value={contactEmail}
                          onChange={(e) => setContactEmail(e.target.value)}
                          placeholder={t('accountPortal.emailPlaceholder')}
                          className="h-10 pl-9"
                          autoComplete="email"
                        />
                      </div>
                      <Button type="submit" className="h-10 shrink-0" disabled={generating || !emailValid}>
                        {generating ? <Loader2 className="size-4 animate-spin" /> : <Link2 className="size-4" />}
                        {t('accountPortal.generateButton')}
                      </Button>
                    </form>
                  </div>
                </div>
              </CardContent>
            </Card>

            {/* Step 2 — open auth url, log in, paste redirected url */}
            <Card className={cn('border-border/70 shadow-sm transition-opacity', !hasAuthUrl && 'opacity-55')}>
              <CardContent className="p-4 sm:p-5">
                <div className="flex items-start gap-3">
                  <StepBadge index={2} active={currentStep === 2} done={Boolean(successMessage)} />
                  <div className="min-w-0 flex-1">
                    <div className="text-sm font-semibold tracking-tight">{t('accountPortal.step2Title')}</div>
                    <p className="mt-1 text-xs leading-relaxed text-muted-foreground">{t('accountPortal.step2Desc')}</p>

                    {hasAuthUrl ? (
                      <>
                        <div className="mt-3 flex flex-wrap items-center gap-2">
                          <Button asChild className="h-9">
                            <a href={authUrl} target="_blank" rel="noopener noreferrer">
                              <ExternalLink className="size-3.5" />
                              {t('accountPortal.openAuthUrl')}
                            </a>
                          </Button>
                          <Button variant="outline" size="sm" className="h-9" onClick={() => void handleCopyAuthUrl()}>
                            {copied ? <CheckCircle2 className="size-3.5" /> : <Copy className="size-3.5" />}
                            {copied ? t('accountPortal.copied') : t('accountPortal.copyAuthUrl')}
                          </Button>
                        </div>

                        <div className="mt-4 rounded-lg border border-amber-500/30 bg-amber-500/10 px-3 py-2 text-xs leading-relaxed text-amber-700 dark:text-amber-300">
                          {t('accountPortal.localhostHint')}
                        </div>

                        <label className="mt-3 flex flex-col gap-1.5">
                          <span className="text-xs font-semibold text-muted-foreground">
                            {t('accountPortal.pasteLabel')}
                          </span>
                          <textarea
                            value={pastedUrl}
                            onChange={(e) => handlePastedUrlChange(e.target.value)}
                            rows={3}
                            placeholder="http://localhost:1455/auth/callback?code=...&state=..."
                            className="w-full resize-y rounded-xl border border-input bg-background/80 px-3 py-2.5 font-mono text-xs leading-6 shadow-xs outline-none transition-[border-color,box-shadow] placeholder:text-muted-foreground focus-visible:border-ring focus-visible:ring-[3px] focus-visible:ring-ring/50 dark:bg-input/30"
                          />
                        </label>

                        {code && state ? (
                          <div className="mt-2 inline-flex items-center gap-1.5 rounded-md bg-emerald-500/12 px-2.5 py-1 text-xs font-medium text-emerald-600 dark:text-emerald-300">
                            <CheckCircle2 className="size-3.5" />
                            {t('accountPortal.parsedOk')}
                          </div>
                        ) : null}

                        <button
                          type="button"
                          onClick={() => setShowFallback((v) => !v)}
                          className="mt-3 flex items-center gap-1 text-xs font-medium text-muted-foreground hover:text-foreground"
                        >
                          <ClipboardPaste className="size-3.5" />
                          {t('accountPortal.fallbackToggle')}
                        </button>
                        {showFallback ? (
                          <div className="mt-2 grid gap-2 sm:grid-cols-2">
                            <label className="flex flex-col gap-1.5">
                              <span className="text-xs font-semibold text-muted-foreground">code</span>
                              <Input
                                value={code}
                                onChange={(e) => setCode(e.target.value)}
                                placeholder="code"
                                className="h-9 font-mono text-xs"
                              />
                            </label>
                            <label className="flex flex-col gap-1.5">
                              <span className="text-xs font-semibold text-muted-foreground">state</span>
                              <Input
                                value={state}
                                onChange={(e) => setState(e.target.value)}
                                placeholder="state"
                                className="h-9 font-mono text-xs"
                              />
                            </label>
                          </div>
                        ) : null}
                      </>
                    ) : (
                      <p className="mt-3 text-xs italic text-muted-foreground">{t('accountPortal.step2Locked')}</p>
                    )}
                  </div>
                </div>
              </CardContent>
            </Card>

            {/* Step 3 — submit */}
            <Card className={cn('border-border/70 shadow-sm transition-opacity', !hasAuthUrl && 'opacity-55')}>
              <CardContent className="p-4 sm:p-5">
                <div className="flex items-start gap-3">
                  <StepBadge index={3} active={currentStep === 2 && canSubmit} done={Boolean(successMessage)} />
                  <div className="min-w-0 flex-1">
                    <div className="text-sm font-semibold tracking-tight">{t('accountPortal.step3Title')}</div>
                    <p className="mt-1 text-xs leading-relaxed text-muted-foreground">{t('accountPortal.step3Desc')}</p>
                    <Button className="mt-3 h-10" disabled={!canSubmit || submitting} onClick={() => void handleSubmit()}>
                      {submitting ? <Loader2 className="size-4 animate-spin" /> : <ArrowRight className="size-4" />}
                      {t('accountPortal.submitButton')}
                    </Button>
                  </div>
                </div>
              </CardContent>
            </Card>

            {error ? (
              <div className="rounded-lg border border-destructive/30 bg-destructive/10 px-3 py-2 text-sm text-destructive">
                {error}
              </div>
            ) : null}

            <p className="flex items-center justify-center gap-1.5 pt-1 text-center text-xs text-muted-foreground">
              <ShieldCheck className="size-3.5" />
              {t('accountPortal.privacyHint')}
            </p>
          </div>
        )}
      </main>
    </div>
  )
}
