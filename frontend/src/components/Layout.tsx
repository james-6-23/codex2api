import { type CSSProperties, type PropsWithChildren, type ReactNode, useCallback, useEffect, useRef, useState } from 'react'
import { createPortal } from 'react-dom'
import { NavLink, useLocation } from 'react-router-dom'
import { LayoutDashboard, Users, Activity, Settings, Server, Languages, Globe, BookOpen, KeyRound, Image as ImageIcon, ShieldAlert, ShieldCheck, ExternalLink, ChevronLeft, Palette, Sun, Moon, LogOut, Download, Loader2, RefreshCw } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { api, resetAdminAuthState } from '../api'
import { DEFAULT_SITE_LOGO, isBrandingVideo, useBranding } from '../branding'
import { useVersionCheck } from '../hooks/useVersionCheck'
import { useTheme } from '../hooks/useTheme'
import { useToast } from '../hooks/useToast'
import { getErrorMessage } from '../utils/error'
import SecurityBanner from './SecurityBanner'

type NavDef = {
  to: string
  labelKey: string
  icon: ReactNode
  end?: boolean
  activePrefix?: string
}

const navDefs: NavDef[] = [
  { to: '/', labelKey: 'nav.dashboard', icon: <LayoutDashboard className="size-[18px]" />, end: true },
  { to: '/accounts', labelKey: 'nav.accounts', icon: <Users className="size-[18px]" /> },
  { to: '/api-keys', labelKey: 'nav.apiKeys', icon: <KeyRound className="size-[18px]" /> },
  { to: '/proxies', labelKey: 'nav.proxies', icon: <Globe className="size-[18px]" /> },
  { to: '/images/studio', labelKey: 'nav.images', icon: <ImageIcon className="size-[18px]" />, activePrefix: '/images' },
  { to: '/prompt-filter/overview', labelKey: 'nav.promptFilter', icon: <ShieldAlert className="size-[18px]" />, activePrefix: '/prompt-filter' },
  { to: '/codex-audit', labelKey: 'nav.codexAudit', icon: <ShieldCheck className="size-[18px]" /> },
  { to: '/ops/overview', labelKey: 'nav.ops', icon: <Server className="size-[18px]" />, activePrefix: '/ops' },
  { to: '/usage', labelKey: 'nav.usage', icon: <Activity className="size-[18px]" /> },
  { to: '/theme', labelKey: 'nav.theme', icon: <Palette className="size-[18px]" /> },
  { to: '/settings', labelKey: 'nav.settings', icon: <Settings className="size-[18px]" /> },
  { to: '/docs', labelKey: 'nav2.docs', icon: <BookOpen className="size-[18px]" /> },
]

export default function Layout({ children }: PropsWithChildren) {
  const location = useLocation()
  const { t, i18n } = useTranslation()
  const { hasUpdate, latestVersion, updateInfo, refreshVersion } = useVersionCheck(location.pathname)
  const { siteName, siteLogo, backgroundImage, backgroundOpacity, backgroundBlur, backgroundGlassOpacity, backgroundGlassBlur } = useBranding()
  const { theme, toggle } = useTheme()
  const { showToast } = useToast()
  const [spinning, setSpinning] = useState(false)
  const logoSrc = siteLogo || DEFAULT_SITE_LOGO
  const [showVersionPopover, setShowVersionPopover] = useState(false)
  const [updatingVersion, setUpdatingVersion] = useState(false)
  const [restartingAfterUpdate, setRestartingAfterUpdate] = useState(false)
  const restartPollRef = useRef<number | null>(null)
  const restartPollActiveRef = useRef(false)
  // 侧栏折叠状态。lg+ 屏才生效;collapsed=true 时只显示 icon,列宽从 264 → 64。
  // localStorage 持久化跨刷新保留选择。
  const [sidebarCollapsed, setSidebarCollapsed] = useState<boolean>(() => {
    if (typeof window === 'undefined') return false
    try {
      return window.localStorage.getItem('sidebar_collapsed') === '1'
    } catch {
      return false
    }
  })
  const toggleSidebarCollapsed = () => {
    setSidebarCollapsed((prev) => {
      const next = !prev
      try {
        window.localStorage.setItem('sidebar_collapsed', next ? '1' : '0')
      } catch {
        /* localStorage 不可用时静默忽略 */
      }
      return next
    })
  }
  const versionPopoverRef = useRef<HTMLDivElement | null>(null)
  const versionButtonRef = useRef<HTMLButtonElement | null>(null)
  const [versionPopoverPos, setVersionPopoverPos] = useState<{ top: number; left: number } | null>(null)
  const releaseURL = updateInfo?.release_url || (latestVersion
    ? `https://github.com/james-6-23/codex2api/releases/tag/${encodeURIComponent(latestVersion)}`
    : undefined)
  const canApplyUpdate = hasUpdate && Boolean(updateInfo) && updateInfo?.supported !== false
  const updateUnavailableReason = updateInfo?.unsupported_reason

  const stopRestartPolling = useCallback(() => {
    restartPollActiveRef.current = false
    if (restartPollRef.current !== null) {
      window.clearTimeout(restartPollRef.current)
      restartPollRef.current = null
    }
  }, [])

  const pollForUpdatedVersion = useCallback((targetVersion: string) => {
    stopRestartPolling()
    const normalizedTarget = targetVersion.replace(/^v/i, '')
    let attempts = 0
    restartPollActiveRef.current = true

    const scheduleNext = (delayMs: number) => {
      restartPollRef.current = window.setTimeout(async () => {
        if (!restartPollActiveRef.current) return
        attempts += 1
        try {
          const info = await api.getSystemUpdate()
          if (info.current_version.replace(/^v/i, '') === normalizedTarget) {
            stopRestartPolling()
            window.location.reload()
            return
          }
        } catch {
          // service may be restarting
        }
        if (!restartPollActiveRef.current) return
        if (attempts >= 60) {
          stopRestartPolling()
          setRestartingAfterUpdate(false)
          // 90s 内未观察到版本切换:服务可能仍在重启(容器/守护进程拉起较慢),
          // 提示用户稍后手动刷新,而不是静默恢复按钮让人以为“没反应”。
          showToast(t('common.restartTimeout'), 'error')
          return
        }
        scheduleNext(1500)
      }, delayMs)
    }

    scheduleNext(2500)
  }, [stopRestartPolling, showToast, t])

  const handleApplyUpdate = async () => {
    if (!canApplyUpdate || updatingVersion || restartingAfterUpdate) return
    setUpdatingVersion(true)
    try {
      const result = await api.performSystemUpdate()
      showToast(result.message || t('common.updateApplied'), 'success')
      setRestartingAfterUpdate(Boolean(result.restarting))
      if (result.restarting) {
        pollForUpdatedVersion(result.latest_version)
      } else {
        void refreshVersion(true)
      }
    } catch (error) {
      showToast(getErrorMessage(error, t('common.updateFailed')), 'error')
    } finally {
      setUpdatingVersion(false)
    }
  }

  useEffect(() => {
    if (showVersionPopover && hasUpdate && !updateInfo) {
      void refreshVersion(true)
    }
  }, [showVersionPopover, hasUpdate, updateInfo, refreshVersion])

  useEffect(() => {
    if (!showVersionPopover) return

    const updatePosition = () => {
      const rect = versionButtonRef.current?.getBoundingClientRect()
      if (!rect) return
      setVersionPopoverPos({ top: rect.bottom + 8, left: rect.left })
    }
    updatePosition()

    const handlePointerDown = (event: PointerEvent) => {
      const target = event.target instanceof Node ? event.target : null
      if (target && versionPopoverRef.current?.contains(target)) return
      if (target && versionButtonRef.current?.contains(target)) return
      setShowVersionPopover(false)
    }
    const handleKeyDown = (event: KeyboardEvent) => {
      if (event.key === 'Escape') setShowVersionPopover(false)
    }

    document.addEventListener('pointerdown', handlePointerDown)
    document.addEventListener('keydown', handleKeyDown)
    window.addEventListener('resize', updatePosition)
    window.addEventListener('scroll', updatePosition, true)
    return () => {
      document.removeEventListener('pointerdown', handlePointerDown)
      document.removeEventListener('keydown', handleKeyDown)
      window.removeEventListener('resize', updatePosition)
      window.removeEventListener('scroll', updatePosition, true)
    }
  }, [showVersionPopover])

  useEffect(() => stopRestartPolling, [stopRestartPolling])

  useEffect(() => {
    const root = document.documentElement
    const glassVars = [
      '--admin-glass-card-opacity',
      '--admin-glass-card-strong-opacity',
      '--admin-glass-control-opacity',
      '--admin-glass-sidebar-opacity',
      '--admin-glass-border-opacity',
      '--admin-glass-card',
      '--admin-glass-card-strong',
      '--admin-glass-control',
      '--admin-glass-sidebar',
      '--admin-glass-border',
      '--admin-glass-panel-blur',
      '--admin-glass-control-blur',
      '--admin-glass-shadow-alpha',
    ]
    if (backgroundImage) {
      root.dataset.adminBackground = 'true'
      const glassOpacity = Math.min(100, Math.max(0, Math.round(backgroundGlassOpacity)))
      const panelBlur = Math.min(20, Math.max(0, Math.round(backgroundGlassBlur)))
      const strongOpacity = glassOpacity === 0 ? 0 : Math.min(100, glassOpacity + 8)
      const controlOpacity = Math.max(0, glassOpacity - 12)
      const sidebarOpacity = glassOpacity === 0 ? 0 : Math.min(100, glassOpacity + 6)
      const borderOpacity = glassOpacity === 0 ? 0 : Math.min(100, Math.max(38, glassOpacity + 14))
      root.style.setProperty('--admin-glass-card-opacity', `${glassOpacity}%`)
      root.style.setProperty('--admin-glass-card-strong-opacity', `${strongOpacity}%`)
      root.style.setProperty('--admin-glass-control-opacity', `${controlOpacity}%`)
      root.style.setProperty('--admin-glass-sidebar-opacity', `${sidebarOpacity}%`)
      root.style.setProperty('--admin-glass-border-opacity', `${borderOpacity}%`)
      root.style.setProperty('--admin-glass-card', glassOpacity === 0 ? 'transparent' : `color-mix(in oklab, var(--color-card) ${glassOpacity}%, transparent)`)
      root.style.setProperty('--admin-glass-card-strong', strongOpacity === 0 ? 'transparent' : `color-mix(in oklab, var(--color-card) ${strongOpacity}%, transparent)`)
      root.style.setProperty('--admin-glass-control', controlOpacity === 0 ? 'transparent' : `color-mix(in oklab, var(--color-background) ${controlOpacity}%, transparent)`)
      root.style.setProperty('--admin-glass-sidebar', sidebarOpacity === 0 ? 'transparent' : `color-mix(in oklab, hsl(var(--sidebar-background)) ${sidebarOpacity}%, transparent)`)
      root.style.setProperty('--admin-glass-border', borderOpacity === 0 ? 'transparent' : `color-mix(in oklab, var(--color-border) ${borderOpacity}%, transparent)`)
      root.style.setProperty('--admin-glass-panel-blur', `${panelBlur}px`)
      root.style.setProperty('--admin-glass-control-blur', `${Math.max(0, panelBlur - 2)}px`)
      root.style.setProperty('--admin-glass-shadow-alpha', `${Math.min(0.24, (glassOpacity / 58) * 0.14).toFixed(3)}`)
    } else {
      delete root.dataset.adminBackground
      glassVars.forEach((name) => root.style.removeProperty(name))
    }

    return () => {
      delete root.dataset.adminBackground
      glassVars.forEach((name) => root.style.removeProperty(name))
    }
  }, [backgroundGlassBlur, backgroundGlassOpacity, backgroundImage])

  const toggleLang = () => {
    const next = i18n.language === 'zh' ? 'en' : 'zh'
    i18n.changeLanguage(next)
    localStorage.setItem('lang', next)
  }

  const handleThemeToggle = (e: React.MouseEvent) => {
    setSpinning(true)
    toggle(e)
    setTimeout(() => setSpinning(false), 500)
  }

  const isNavActive = (item: NavDef) => {
    if (item.activePrefix) {
      return location.pathname === item.activePrefix || location.pathname.startsWith(`${item.activePrefix}/`)
    }
    if (item.end) {
      return location.pathname === item.to
    }
    return location.pathname === item.to || location.pathname.startsWith(`${item.to}/`)
  }

  // Apple HIG-style easing for sidebar choreography:
  //  - container width/padding/gap use 380ms (long, slow ease-out)
  //  - text opacity/translate use 200ms (snappy)
  //  - when expanding, text waits 150ms so the container opens up first;
  //    when collapsing, text fades out immediately
  const containerEase = 'duration-[380ms] ease-[cubic-bezier(0.32,0.72,0,1)]'
  const textEase = 'duration-[200ms] ease-[cubic-bezier(0.32,0.72,0,1)]'
  const textRevealDelay = sidebarCollapsed ? 'delay-0' : 'delay-150'

  const isBackgroundVideo = isBrandingVideo(backgroundImage)
  const backgroundMediaStyle: CSSProperties | undefined = backgroundImage
    ? {
        opacity: backgroundOpacity / 100,
        filter: backgroundBlur > 0 ? `blur(${backgroundBlur}px)` : undefined,
        transform: backgroundBlur > 0 ? 'scale(1.04)' : undefined,
      }
    : undefined
  const backgroundLayerStyle: CSSProperties | undefined = backgroundImage && !isBackgroundVideo
    ? {
        ...backgroundMediaStyle,
        backgroundImage: `url(${JSON.stringify(backgroundImage)})`,
      }
    : undefined
  const mobileNav = (
    <nav
      data-slot="admin-mobile-nav"
      className="fixed left-3 right-3 bottom-[calc(env(safe-area-inset-bottom)+0.75rem)] z-40 hidden max-lg:flex gap-1 overflow-x-auto rounded-xl border border-border bg-card/95 p-1.5 shadow-lg backdrop-blur-[20px] [-ms-overflow-style:none] [scrollbar-width:none] [&::-webkit-scrollbar]:hidden"
      aria-label="Mobile navigation"
    >
      {navDefs.map((item) => {
        const active = isNavActive(item)
        return (
          <NavLink
            key={item.to}
            to={item.to}
            end={item.end}
            className={`flex min-w-[74px] flex-col items-center justify-center gap-1 min-h-[54px] px-2 py-1.5 border rounded-lg text-center text-[10px] font-bold transition-colors duration-150 ${
              active
                ? 'bg-primary/10 border-primary/20 text-primary'
                : 'border-transparent text-muted-foreground'
            }`}
          >
            {item.icon}
            <span className="w-full truncate leading-tight">{t(item.labelKey)}</span>
          </NavLink>
        )
      })}
    </nav>
  )

  return (
    <div className="relative min-h-dvh">
      {backgroundImage ? (
        <div aria-hidden="true" className="pointer-events-none fixed inset-0 z-0 overflow-hidden">
          {isBackgroundVideo ? (
            <video
              src={backgroundImage}
              className="absolute inset-0 size-full object-cover transition-[opacity,filter,transform] duration-300"
              style={backgroundMediaStyle}
              autoPlay
              muted
              loop
              playsInline
            />
          ) : (
            <div
              className="absolute inset-0 bg-cover bg-center bg-no-repeat transition-[opacity,filter,transform] duration-300"
              style={backgroundLayerStyle}
            />
          )}
        </div>
      ) : null}
      <div
        className={`relative z-10 grid w-full min-w-0 max-w-full grid-cols-1 max-lg:px-3 lg:grid-cols-[var(--admin-layout-columns)] lg:overflow-x-clip transition-[grid-template-columns] ${containerEase}`}
        style={{
          '--admin-layout-columns': sidebarCollapsed ? '64px minmax(0,1fr)' : '264px minmax(0,1fr)',
        } as CSSProperties}
      >
        {/* Sidebar - desktop */}
        <aside data-slot="admin-sidebar" className="sticky top-0 z-40 self-start h-dvh overflow-hidden border-r border-border bg-[hsl(var(--sidebar-background))] max-lg:hidden">
          <div className={`flex flex-col h-full ${sidebarCollapsed ? 'px-2' : 'px-4'} pt-5 pb-4 transition-[padding] ${containerEase}`}>
            {/* Brand */}
            <div className={`pb-4 border-b border-border ${sidebarCollapsed ? 'flex justify-center' : ''}`}>
              <div className={`flex items-center gap-3 ${sidebarCollapsed ? 'justify-center' : ''}`}>
                <img src={logoSrc} alt={siteName} className="size-10 rounded-lg object-cover shadow-sm shrink-0" />
                <div
                  aria-hidden={sidebarCollapsed}
                  className={`min-w-0 overflow-hidden transition-[max-width] ${containerEase} ${
                    sidebarCollapsed ? 'pointer-events-none max-w-0' : 'max-w-[160px]'
                  }`}
                >
                  <div
                    className={`flex min-w-0 flex-col gap-1 whitespace-nowrap transition-[opacity,transform] ${textEase} ${textRevealDelay} ${
                      sidebarCollapsed ? '-translate-x-1 opacity-0' : 'translate-x-0 opacity-100'
                    }`}
                  >
                    <h1 className="max-w-[160px] truncate text-[20px] leading-tight font-bold text-foreground" title={siteName}>
                      {siteName}
                    </h1>
                    <div ref={versionPopoverRef} className="relative w-fit">
                      <button
                        ref={versionButtonRef}
                        type="button"
                        className="relative inline-flex cursor-pointer items-center rounded-md bg-primary/10 px-1.5 py-0.5 text-[10px] font-bold text-primary ring-1 ring-primary/10 transition-colors hover:bg-primary/15"
                        title={hasUpdate && latestVersion ? t('common.newVersionAvailable', { version: latestVersion }) : undefined}
                        tabIndex={sidebarCollapsed ? -1 : 0}
                        onClick={() => setShowVersionPopover((current) => !current)}
                      >
                        {__APP_VERSION__}
                        {hasUpdate && (
                          <span className="absolute -top-1.5 left-1/2 size-2.5 -translate-x-1/2 rounded-full bg-red-500 shadow-sm ring-2 ring-[hsl(var(--sidebar-background))] animate-pulse" />
                        )}
                      </button>
                      {showVersionPopover && versionPopoverPos && createPortal(
                        <div
                          ref={versionPopoverRef}
                          style={{ position: 'fixed', top: versionPopoverPos.top, left: versionPopoverPos.left }}
                          className="z-[100] w-[240px] rounded-lg border border-border bg-popover p-3 text-left shadow-xl"
                        >
                          <div className="text-[13px] font-semibold text-foreground">
                            {latestVersion
                              ? hasUpdate
                                ? t('common.newVersionAvailable', { version: latestVersion })
                                : t('common.versionLatest')
                              : t('common.versionChecking')}
                          </div>
                          <div className="mt-1 text-[11px] text-muted-foreground">
                            {t('common.currentVersion', { version: __APP_VERSION__ })}
                          </div>
                          {latestVersion && (
                            <div className="mt-1 text-[11px] text-muted-foreground">
                              {t('common.latestVersion', { version: latestVersion })}
                            </div>
                          )}
                          {hasUpdate && updateUnavailableReason && (
                            <div className="mt-3 rounded-md border border-amber-200 bg-amber-50 px-2.5 py-2 text-[11px] font-medium leading-relaxed text-amber-700">
                              {updateUnavailableReason}
                            </div>
                          )}
                          {hasUpdate && updateInfo?.warning && (
                            <div className="mt-3 rounded-md border border-amber-200 bg-amber-50 px-2.5 py-2 text-[11px] font-medium leading-relaxed text-amber-700">
                              {updateInfo.warning}
                            </div>
                          )}
                          {hasUpdate && (
                            <button
                              type="button"
                              className="mt-3 inline-flex w-full items-center justify-center gap-1.5 rounded-md bg-primary px-2.5 py-1.5 text-[12px] font-semibold text-primary-foreground transition-colors hover:bg-primary/90 disabled:cursor-not-allowed disabled:opacity-60"
                              disabled={!canApplyUpdate || updatingVersion || restartingAfterUpdate}
                              title={!canApplyUpdate ? updateUnavailableReason : undefined}
                              onClick={handleApplyUpdate}
                            >
                              {restartingAfterUpdate ? (
                                <RefreshCw className="size-3.5 animate-spin" />
                              ) : updatingVersion ? (
                                <Loader2 className="size-3.5 animate-spin" />
                              ) : (
                                <Download className="size-3.5" />
                              )}
                              {restartingAfterUpdate
                                ? t('common.restarting')
                                : updatingVersion
                                  ? t('common.updating')
                                  : t('common.updateNow')}
                            </button>
                          )}
                          {releaseURL && (
                            <a
                              href={releaseURL}
                              target="_blank"
                              rel="noopener noreferrer"
                              className="mt-3 inline-flex w-full items-center justify-center gap-1.5 rounded-md border border-primary/20 bg-primary/10 px-2.5 py-1.5 text-[12px] font-semibold text-primary transition-colors hover:bg-primary/15"
                              onClick={() => setShowVersionPopover(false)}
                            >
                              {t('common.viewReleaseNotes')}
                              <ExternalLink className="size-3.5" />
                            </a>
                          )}
                        </div>,
                        document.body,
                      )}
                    </div>
                  </div>
                </div>
              </div>
            </div>

            {/* Collapse toggle: 嵌入 brand 与 nav 之间,展开态显示文字,收起态只显示箭头 */}
            <button
              type="button"
              onClick={toggleSidebarCollapsed}
              title={sidebarCollapsed ? t('common.expandSidebar') : t('common.collapseSidebar')}
              aria-label={sidebarCollapsed ? t('common.expandSidebar') : t('common.collapseSidebar')}
              className={`mt-3 flex items-center min-h-9 rounded-lg text-[12px] font-semibold text-muted-foreground hover:bg-muted/60 hover:text-foreground transition-[background-color,color,padding] ${containerEase} ${
                sidebarCollapsed ? 'justify-center px-2 py-2' : 'gap-2 px-3 py-2'
              }`}
            >
              <ChevronLeft
                className={`size-4 transition-transform ${containerEase} ${
                  sidebarCollapsed ? 'rotate-180' : 'rotate-0'
                }`}
              />
              <span
                className={`overflow-hidden whitespace-nowrap transition-[max-width,opacity] ${textEase} ${textRevealDelay} ${
                  sidebarCollapsed ? 'max-w-0 opacity-0' : 'max-w-[160px] opacity-100'
                }`}
              >
                {t('common.collapseSidebar')}
              </span>
            </button>

            {/* Nav */}
            <nav className="flex min-h-0 flex-1 flex-col gap-1 overflow-y-auto pt-3 [scrollbar-width:none] [&::-webkit-scrollbar]:hidden" aria-label="Main navigation">
              <span
                className={`mb-1 overflow-hidden whitespace-nowrap px-2 text-[11px] font-bold uppercase text-muted-foreground transition-[max-height,opacity,margin] ${textEase} ${textRevealDelay} ${
                  sidebarCollapsed ? 'mb-0 max-h-0 opacity-0' : 'max-h-5 opacity-100'
                }`}
                aria-hidden={sidebarCollapsed}
              >
                {t('nav.console')}
              </span>
              {navDefs.map((item) => {
                const active = isNavActive(item)
                const label = t(item.labelKey)
                return (
                  <NavLink
                    key={item.to}
                    to={item.to}
                    end={item.end}
                    title={sidebarCollapsed ? label : undefined}
                    className={`flex items-center min-h-10 border rounded-lg text-[14px] font-semibold transition-[background-color,color,border-color,padding,gap] ${containerEase} ${
                      sidebarCollapsed ? 'justify-center px-2 py-2' : 'gap-2.5 px-3 py-2'
                    } ${
                      active
                        ? 'bg-primary/10 border-primary/20 text-primary'
                        : 'border-transparent text-muted-foreground hover:bg-muted/60 hover:text-foreground'
                    }`}
                  >
                    {item.icon}
                    <span
                      className={`overflow-hidden whitespace-nowrap transition-[max-width,opacity] ${textEase} ${textRevealDelay} ${
                        sidebarCollapsed ? 'max-w-0 opacity-0' : 'max-w-[160px] opacity-100'
                      }`}
                    >
                      {label}
                    </span>
                  </NavLink>
                )
              })}
            </nav>

            {/* Footer */}
            <div
              className={`mt-auto border-t border-border pt-3 transition-[gap] ${containerEase} ${
                sidebarCollapsed
                  ? 'flex flex-col items-center gap-1'
                  : 'flex items-center justify-between gap-2'
              }`}
            >
              <span
                aria-hidden={sidebarCollapsed}
                className={`inline-flex items-center gap-1.5 overflow-hidden rounded-md border border-emerald-500/16 bg-[hsl(var(--success-bg))] text-[11px] font-bold text-[hsl(var(--success))] shrink-0 whitespace-nowrap transition-[max-width,opacity,padding] ${textEase} ${textRevealDelay} ${
                  sidebarCollapsed ? 'pointer-events-none max-w-0 px-0 opacity-0' : 'max-w-[120px] px-2 py-1 opacity-100'
                }`}
              >
                <span className="size-2 rounded-full bg-emerald-500 shrink-0" />
                {t('common.online')}
              </span>
              <div className={`flex items-center gap-0.5 ${sidebarCollapsed ? 'flex-col' : ''}`}>
                <button
                  onClick={toggleLang}
                  className="flex items-center justify-center size-9 rounded-lg text-muted-foreground hover:text-foreground hover:bg-muted/70 transition-colors duration-150 text-[12px] font-bold"
                  title={i18n.language === 'zh' ? 'English' : '中文'}
                >
                  <Languages className="size-[18px]" />
                </button>
                <a
                  href="https://github.com/james-6-23/codex2api"
                  target="_blank"
                  rel="noopener noreferrer"
                  className="flex items-center justify-center size-9 rounded-lg text-muted-foreground hover:text-foreground hover:bg-muted/70 transition-colors duration-150"
                  title="GitHub"
                >
                  <svg className="size-[18px]" viewBox="0 0 24 24" fill="currentColor"><path d="M12 0C5.37 0 0 5.37 0 12c0 5.31 3.435 9.795 8.205 11.385.6.105.825-.255.825-.57 0-.285-.015-1.23-.015-2.235-3.015.555-3.795-.735-4.035-1.41-.135-.345-.72-1.41-1.23-1.695-.42-.225-1.02-.78-.015-.795.945-.015 1.62.87 1.845 1.23 1.08 1.815 2.805 1.305 3.495.99.105-.78.42-1.305.765-1.605-2.67-.3-5.46-1.335-5.46-5.925 0-1.305.465-2.385 1.23-3.225-.12-.3-.54-1.53.12-3.18 0 0 1.005-.315 3.3 1.23.96-.27 1.98-.405 3-.405s2.04.135 3 .405c2.295-1.56 3.3-1.23 3.3-1.23.66 1.65.24 2.88.12 3.18.765.84 1.23 1.905 1.23 3.225 0 4.605-2.805 5.625-5.475 5.925.435.375.81 1.095.81 2.22 0 1.605-.015 2.895-.015 3.3 0 .315.225.69.825.57A12.02 12.02 0 0 0 24 12c0-6.63-5.37-12-12-12z"/></svg>
                </a>
                <button
                  onClick={handleThemeToggle}
                  className="flex items-center justify-center size-9 rounded-lg text-muted-foreground hover:text-foreground hover:bg-muted/70 transition-colors duration-150"
                  title={theme === 'dark' ? t('common.switchToLight') : t('common.switchToDark')}
                  aria-label={theme === 'dark' ? t('common.switchToLight') : t('common.switchToDark')}
                >
                  <span className={`inline-flex transition-transform duration-500 ease-out ${spinning ? 'rotate-[360deg] scale-110' : 'rotate-0 scale-100'}`}>
                    {theme === 'dark' ? <Sun className="size-[18px]" /> : <Moon className="size-[18px]" />}
                  </span>
                </button>
                <button
                  onClick={resetAdminAuthState}
                  className="flex items-center justify-center size-9 rounded-lg text-muted-foreground hover:text-foreground hover:bg-muted/70 transition-colors duration-150"
                  title={t('common.logout')}
                  aria-label={t('common.logout')}
                >
                  <LogOut className="size-[18px]" />
                </button>
              </div>
            </div>
          </div>
        </aside>

        {/* Main content */}
        <main className="relative z-0 w-full min-w-0 max-w-full p-5 max-lg:p-3 max-lg:pb-[calc(92px+env(safe-area-inset-bottom))]">
          {/* Mobile topbar */}
          <header data-slot="admin-mobile-topbar" className="hidden max-lg:flex min-w-0 w-full max-w-full items-center justify-between gap-2 overflow-hidden mb-4 p-3 border border-border rounded-lg bg-card/95 shadow-sm">
            <div className="flex min-w-0 flex-1 items-center gap-3">
              <img src={logoSrc} alt={siteName} className="w-8 h-8 rounded-[10px] object-cover" />
              <strong className="min-w-0 flex-1 truncate text-lg" title={siteName}>{siteName}</strong>
            </div>
            <div className="flex shrink-0 items-center gap-1.5">
              <button
                onClick={toggleLang}
                className="flex items-center justify-center size-8 rounded-lg text-muted-foreground hover:text-foreground transition-colors text-[11px] font-bold"
                title={i18n.language === 'zh' ? 'English' : '中文'}
              >
                <Languages className="size-4" />
              </button>
              <button
                onClick={handleThemeToggle}
                className="flex items-center justify-center size-8 rounded-lg text-muted-foreground hover:text-foreground transition-colors"
                title={theme === 'dark' ? t('common.switchToLight') : t('common.switchToDark')}
                aria-label={theme === 'dark' ? t('common.switchToLight') : t('common.switchToDark')}
              >
                <span className={`inline-flex transition-transform duration-500 ease-out ${spinning ? 'rotate-[360deg] scale-110' : 'rotate-0 scale-100'}`}>
                  {theme === 'dark' ? <Sun className="size-4" /> : <Moon className="size-4" />}
                </span>
              </button>
              <button
                onClick={resetAdminAuthState}
                className="flex items-center justify-center size-8 rounded-lg text-muted-foreground hover:text-foreground transition-colors"
                title={t('common.logout')}
                aria-label={t('common.logout')}
              >
                <LogOut className="size-4" />
              </button>
              <span className="inline-flex items-center justify-center min-h-[28px] px-2.5 rounded-full text-[12px] font-bold bg-[hsl(var(--success-bg))] text-[hsl(var(--success))] shrink-0 whitespace-nowrap max-[420px]:hidden">
                {t('common.online')}
              </span>
            </div>
          </header>

          <SecurityBanner />
          <div className="min-h-full w-full min-w-0 max-w-full">{children}</div>
        </main>
      </div>
      {typeof document !== 'undefined' ? createPortal(mobileNav, document.body) : mobileNav}
    </div>
  )
}
