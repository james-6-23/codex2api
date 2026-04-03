import { type PropsWithChildren, type ReactNode, useState } from 'react'
import { NavLink } from 'react-router-dom'
import { LayoutDashboard, Users, Activity, Settings, Server, Workflow, Sun, Moon, Languages, Globe, BookOpen, FileCode2 } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import logoImg from '../assets/logo.png'
import { useTheme } from '../hooks/useTheme'
import { useVersionCheck } from '../hooks/useVersionCheck'

type NavDef = {
  to: string
  labelKey: string
  icon: ReactNode
  end?: boolean
}

const navDefs: NavDef[] = [
  { to: '/', labelKey: 'nav.dashboard', icon: <LayoutDashboard className="size-[18px]" />, end: true },
  { to: '/accounts', labelKey: 'nav.accounts', icon: <Users className="size-[18px]" /> },
  { to: '/proxies', labelKey: 'nav.proxies', icon: <Globe className="size-[18px]" /> },
  { to: '/ops', labelKey: 'nav.ops', icon: <Server className="size-[18px]" />, end: true },
  { to: '/ops/scheduler', labelKey: 'nav.scheduler', icon: <Workflow className="size-[18px]" />, end: true },
  { to: '/usage', labelKey: 'nav.usage', icon: <Activity className="size-[18px]" /> },
  { to: '/settings', labelKey: 'nav.settings', icon: <Settings className="size-[18px]" /> },
  { to: '/guide', labelKey: 'nav2.guide', icon: <BookOpen className="size-[18px]" /> },
  { to: '/api-reference', labelKey: 'nav2.apiRef', icon: <FileCode2 className="size-[18px]" /> },
]

export default function Layout({ children }: PropsWithChildren) {
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

  return (
    <div className="min-h-dvh">
      <div className="grid grid-cols-[296px_minmax(0,1fr)] max-w-full max-lg:grid-cols-1 max-lg:px-4">
        {/* Sidebar - desktop */}
        <aside className="sticky top-0 self-start h-dvh border-r border-border bg-[hsl(var(--sidebar-background))] max-lg:hidden">
          <div className="flex flex-col h-full px-6 pt-8 pb-6">
            {/* Brand */}
            <div className="pb-6 border-b border-border">
              <div className="flex items-center gap-3.5">
                <img src={logoImg} alt="CodexProxy" className="w-[52px] h-[52px] rounded-2xl object-cover shadow-[0_4px_16px_hsl(258_60%_63%/0.2)] shrink-0" />
                <div className="flex flex-col gap-1">
                  <h1 className="text-[26px] leading-tight font-bold bg-gradient-to-br from-[hsl(258,60%,63%)] to-[hsl(210,80%,60%)] bg-clip-text text-transparent">
                    CodexProxy
                  </h1>
                  <span
                    className="relative inline-flex items-center px-2 py-0.5 rounded-full bg-primary/10 text-primary text-[11px] font-bold w-fit"
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
            <nav className="flex-1 flex flex-col gap-2 pt-5" aria-label="Main navigation">
              <span className="text-[12px] font-bold tracking-[0.16em] uppercase text-primary/70 mb-1">
                {t('nav.console')}
              </span>
              {navDefs.map((item) => (
                <NavLink
                  key={item.to}
                  to={item.to}
                  end={item.end}
                  className={({ isActive }) =>
                    `flex items-center gap-3 min-h-[50px] px-3.5 py-3 border rounded-2xl text-[20px] font-semibold transition-all duration-150 ${
                      isActive
                        ? 'bg-gradient-to-br from-primary/8 to-blue-500/6 border-primary/20 text-primary shadow-[inset_0_1px_0_rgba(255,255,255,0.8)]'
                        : 'border-transparent text-muted-foreground hover:-translate-y-px hover:bg-white/50 hover:border-border hover:text-foreground'
                    }`
                  }
                >
                  {item.icon}
                  <span>{t(item.labelKey)}</span>
                </NavLink>
              ))}
            </nav>

            {/* Footer */}
            <div className="mt-auto flex items-center justify-between">
              <span className="inline-flex items-center gap-1.5 rounded-full border border-emerald-500/16 bg-[hsl(var(--success-bg))] px-3 py-1.5 text-[11px] font-bold text-[hsl(var(--success))] shadow-[inset_0_1px_0_rgba(255,255,255,0.55)] shrink-0 whitespace-nowrap">
                <span className="size-2 rounded-full bg-emerald-500 shrink-0" />
                {t('common.online')}
              </span>
              <div className="flex items-center gap-0.5">
                <button
                  onClick={toggleLang}
                  className="flex items-center justify-center size-9 rounded-xl text-muted-foreground hover:text-foreground hover:bg-white/60 dark:hover:bg-white/10 transition-all duration-150 text-[12px] font-bold"
                  title={i18n.language === 'zh' ? 'English' : '中文'}
                >
                  <Languages className="size-[18px]" />
                </button>
                <a
                  href="https://github.com/james-6-23/codex2api"
                  target="_blank"
                  rel="noopener noreferrer"
                  className="flex items-center justify-center size-9 rounded-xl text-muted-foreground hover:text-foreground hover:bg-white/60 dark:hover:bg-white/10 transition-all duration-150"
                  title="GitHub"
                >
                  <svg className="size-[18px]" viewBox="0 0 24 24" fill="currentColor"><path d="M12 0C5.37 0 0 5.37 0 12c0 5.31 3.435 9.795 8.205 11.385.6.105.825-.255.825-.57 0-.285-.015-1.23-.015-2.235-3.015.555-3.795-.735-4.035-1.41-.135-.345-.72-1.41-1.23-1.695-.42-.225-1.02-.78-.015-.795.945-.015 1.62.87 1.845 1.23 1.08 1.815 2.805 1.305 3.495.99.105-.78.42-1.305.765-1.605-2.67-.3-5.46-1.335-5.46-5.925 0-1.305.465-2.385 1.23-3.225-.12-.3-.54-1.53.12-3.18 0 0 1.005-.315 3.3 1.23.96-.27 1.98-.405 3-.405s2.04.135 3 .405c2.295-1.56 3.3-1.23 3.3-1.23.66 1.65.24 2.88.12 3.18.765.84 1.23 1.905 1.23 3.225 0 4.605-2.805 5.625-5.475 5.925.435.375.81 1.095.81 2.22 0 1.605-.015 2.895-.015 3.3 0 .315.225.69.825.57A12.02 12.02 0 0 0 24 12c0-6.63-5.37-12-12-12z"/></svg>
                </a>
                <button
                  onClick={handleThemeToggle}
                  className="flex items-center justify-center size-9 rounded-xl text-muted-foreground hover:text-foreground hover:bg-white/60 dark:hover:bg-white/10 transition-all duration-150"
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
        <main className="min-w-0 p-6 max-lg:pb-[104px]">
          {/* Mobile topbar */}
          <header className="hidden max-lg:flex items-center justify-between gap-4 mb-4 p-3.5 border border-border rounded-[22px] bg-white/70 dark:bg-[hsl(220_13%_15%/0.7)]">
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

          <div className="min-h-full">{children}</div>
        </main>

        {/* Mobile bottom nav */}
        <nav className="fixed left-4 right-4 bottom-4 z-40 hidden max-lg:grid grid-cols-6 gap-2 p-2 border border-border rounded-3xl bg-white/90 shadow-lg backdrop-blur-[20px]" aria-label="Mobile navigation">
          {navDefs.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              end={item.end}
              className={({ isActive }) =>
                `flex flex-col items-center justify-center gap-1.5 min-h-[64px] p-2 border rounded-2xl text-center text-[11px] font-bold transition-all duration-150 ${
                  isActive
                    ? 'bg-white/80 border-primary/20 text-foreground'
                    : 'border-transparent text-muted-foreground'
                }`
              }
            >
              {item.icon}
              <span>{t(item.labelKey)}</span>
            </NavLink>
          ))}
        </nav>
      </div>
    </div>
  )
}
