import { type PropsWithChildren, type ReactNode, useState } from 'react'
import { NavLink, useLocation } from 'react-router-dom'
import { LayoutDashboard, Users, Activity, Settings, Server, Workflow, Sun, Moon, Languages, Globe, BookOpen, FileCode2, KeyRound, Image as ImageIcon, ShieldAlert } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import logoImg from '../assets/logo.png'
import { useTheme } from '../hooks/useTheme'
import { useVersionCheck } from '../hooks/useVersionCheck'
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
  { to: '/ops', labelKey: 'nav.ops', icon: <Server className="size-[18px]" />, end: true },
  { to: '/ops/scheduler', labelKey: 'nav.scheduler', icon: <Workflow className="size-[18px]" />, end: true },
  { to: '/usage', labelKey: 'nav.usage', icon: <Activity className="size-[18px]" /> },
  { to: '/settings', labelKey: 'nav.settings', icon: <Settings className="size-[18px]" /> },
  { to: '/docs', labelKey: 'nav2.docs', icon: <BookOpen className="size-[18px]" /> },
  { to: '/api-reference', labelKey: 'nav2.apiRef', icon: <FileCode2 className="size-[18px]" /> },
]

export default function Layout({ children }: PropsWithChildren) {
  const location = useLocation()
  const { theme, toggle } = useTheme()
  const { t, i18n } = useTranslation()
  const { hasUpdate, latestVersion } = useVersionCheck()
  const [spinning, setSpinning] = useState(false)

  const handleThemeToggle = (e: React.MouseEvent) => {
    setSpinning(true)
    toggle(e)
    setTimeout(() => setSpinning(false), 500)
  }

  const toggleLang = () => {
    const next = i18n.language === 'zh' ? 'en' : 'zh'
    i18n.changeLanguage(next)
    localStorage.setItem('lang', next)
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

  return (
    <div className="min-h-dvh">
      <div className="grid grid-cols-[264px_minmax(0,1fr)] max-w-full max-lg:grid-cols-1 max-lg:px-3">
        {/* Sidebar - desktop */}
        <aside className="sticky top-0 self-start h-dvh border-r border-border bg-[hsl(var(--sidebar-background))] max-lg:hidden">
          <div className="flex flex-col h-full px-4 pt-5 pb-4">
            {/* Brand */}
            <div className="pb-4 border-b border-border">
              <div className="flex items-center gap-3">
                <img src={logoImg} alt="CodexProxy" className="size-10 rounded-lg object-cover shadow-sm shrink-0" />
                <div className="flex flex-col gap-1">
                  <h1 className="text-[20px] leading-tight font-bold text-foreground">
                    CodexProxy
                  </h1>
                  <span
                    className="relative inline-flex items-center rounded-md bg-primary/10 px-1.5 py-0.5 text-[10px] font-bold text-primary w-fit"
                    title={hasUpdate && latestVersion ? t('common.newVersionAvailable', { version: latestVersion }) : undefined}
                  >
                    {__APP_VERSION__}
                    {hasUpdate && (
                      <span className="absolute -top-0.5 -right-0.5 size-2 rounded-full bg-red-500 animate-pulse" />
                    )}
                  </span>
                </div>
              </div>
            </div>

            {/* Nav */}
            <nav className="flex-1 flex flex-col gap-1 pt-4" aria-label="Main navigation">
              <span className="mb-1 px-2 text-[11px] font-bold uppercase text-muted-foreground">
                {t('nav.console')}
              </span>
              {navDefs.map((item) => {
                const active = isNavActive(item)
                return (
                  <NavLink
                    key={item.to}
                    to={item.to}
                    end={item.end}
                    className={`flex items-center gap-2.5 min-h-10 px-3 py-2 border rounded-lg text-[14px] font-semibold transition-colors duration-150 ${
                      active
                        ? 'bg-primary/10 border-primary/20 text-primary'
                        : 'border-transparent text-muted-foreground hover:bg-muted/60 hover:text-foreground'
                    }`}
                  >
                    {item.icon}
                    <span>{t(item.labelKey)}</span>
                  </NavLink>
                )
              })}
            </nav>

            {/* Footer */}
            <div className="mt-auto flex items-center justify-between gap-2 border-t border-border pt-3">
              <span className="inline-flex items-center gap-1.5 rounded-md border border-emerald-500/16 bg-[hsl(var(--success-bg))] px-2 py-1 text-[11px] font-bold text-[hsl(var(--success))] shrink-0 whitespace-nowrap">
                <span className="size-2 rounded-full bg-emerald-500 shrink-0" />
                {t('common.online')}
              </span>
              <div className="flex items-center gap-0.5">
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
                >
                  <span className={`inline-flex transition-transform duration-500 ease-out ${spinning ? 'rotate-[360deg] scale-110' : 'rotate-0 scale-100'}`}>
                    {theme === 'dark' ? <Sun className="size-[18px]" /> : <Moon className="size-[18px]" />}
                  </span>
                </button>
              </div>
            </div>
          </div>
        </aside>

        {/* Main content */}
        <main className="min-w-0 p-5 max-lg:p-3 max-lg:pb-[92px]">
          {/* Mobile topbar */}
          <header className="hidden max-lg:flex items-center justify-between gap-4 mb-4 p-3 border border-border rounded-lg bg-card/95 shadow-sm">
            <div className="flex items-center gap-3">
              <img src={logoImg} alt="CodexProxy" className="w-8 h-8 rounded-[10px] object-cover" />
              <strong className="text-lg">CodexProxy</strong>
            </div>
            <div className="flex items-center gap-2">
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
              >
                <span className={`inline-flex transition-transform duration-500 ease-out ${spinning ? 'rotate-[360deg] scale-110' : 'rotate-0 scale-100'}`}>
                  {theme === 'dark' ? <Sun className="size-4" /> : <Moon className="size-4" />}
                </span>
              </button>
              <span className="inline-flex items-center justify-center min-h-[28px] px-2.5 rounded-full text-[12px] font-bold bg-[hsl(var(--success-bg))] text-[hsl(var(--success))] shrink-0 whitespace-nowrap">
                {t('common.online')}
              </span>
            </div>
          </header>

          <SecurityBanner />
          <div className="min-h-full">{children}</div>
        </main>

        {/* Mobile bottom nav */}
        <nav className="fixed left-3 right-3 bottom-3 z-40 hidden max-lg:flex gap-1 overflow-x-auto rounded-xl border border-border bg-card/95 p-1.5 shadow-lg backdrop-blur-[20px] [-ms-overflow-style:none] [scrollbar-width:none] [&::-webkit-scrollbar]:hidden" aria-label="Mobile navigation">
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
                <span>{t(item.labelKey)}</span>
              </NavLink>
            )
          })}
        </nav>
      </div>
    </div>
  )
}
