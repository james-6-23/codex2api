import type { PropsWithChildren, ReactNode } from 'react'
import { NavLink } from 'react-router-dom'

type NavItem = {
  to: string
  label: string
  icon: ReactNode
  end?: boolean
}

const icons: Record<string, ReactNode> = {
  dashboard: (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round">
      <rect x="3" y="3" width="7" height="7" rx="1" />
      <rect x="14" y="3" width="7" height="7" rx="1" />
      <rect x="3" y="14" width="7" height="7" rx="1" />
      <rect x="14" y="14" width="7" height="7" rx="1" />
    </svg>
  ),
  accounts: (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round">
      <path d="M16 21v-2a4 4 0 0 0-4-4H6a4 4 0 0 0-4 4v2" />
      <circle cx="9" cy="7" r="4" />
      <path d="M22 21v-2a4 4 0 0 0-3-3.87" />
      <path d="M16 3.13a4 4 0 0 1 0 7.75" />
    </svg>
  ),
  usage: (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round">
      <polyline points="22 12 18 12 15 21 9 3 6 12 2 12" />
    </svg>
  ),
  settings: (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round">
      <circle cx="12" cy="12" r="3" />
      <path d="M19.4 15a1.65 1.65 0 0 0 .33 1.82l.06.06a2 2 0 0 1 0 2.83 2 2 0 0 1-2.83 0l-.06-.06a1.65 1.65 0 0 0-1.82-.33 1.65 1.65 0 0 0-1 1.51V21a2 2 0 0 1-2 2 2 2 0 0 1-2-2v-.09A1.65 1.65 0 0 0 9 19.4a1.65 1.65 0 0 0-1.82.33l-.06.06a2 2 0 0 1-2.83 0 2 2 0 0 1 0-2.83l.06-.06A1.65 1.65 0 0 0 4.68 15a1.65 1.65 0 0 0-1.51-1H3a2 2 0 0 1-2-2 2 2 0 0 1 2-2h.09A1.65 1.65 0 0 0 4.6 9a1.65 1.65 0 0 0-.33-1.82l-.06-.06a2 2 0 0 1 0-2.83 2 2 0 0 1 2.83 0l.06.06A1.65 1.65 0 0 0 9 4.68a1.65 1.65 0 0 0 1-1.51V3a2 2 0 0 1 2-2 2 2 0 0 1 2 2v.09a1.65 1.65 0 0 0 1 1.51 1.65 1.65 0 0 0 1.82-.33l.06-.06a2 2 0 0 1 2.83 0 2 2 0 0 1 0 2.83l-.06.06A1.65 1.65 0 0 0 19.4 9a1.65 1.65 0 0 0 1.51 1H21a2 2 0 0 1 2 2 2 2 0 0 1-2 2h-.09a1.65 1.65 0 0 0-1.51 1z" />
    </svg>
  ),
}

const navItems: NavItem[] = [
  { to: '/', label: '仪表盘', icon: icons.dashboard, end: true },
  { to: '/accounts', label: '账号管理', icon: icons.accounts },
  { to: '/usage', label: '使用统计', icon: icons.usage },
  { to: '/settings', label: '系统设置', icon: icons.settings },
]

export default function Layout({ children }: PropsWithChildren) {
  return (
    <div className="app-shell">
      <div className="app-layout">
        <aside className="sidebar">
          <div className="sidebar-panel">
            <div className="sidebar-brand">
              <span className="brand-kicker">Codex Workspace</span>
              <h1>Codex2API</h1>
              <p>用更柔和、更清晰的方式管理账号池与请求流量。</p>
              <div className="version">v2.0 管理后台</div>
            </div>

            <nav className="sidebar-nav" aria-label="主导航">
              <span className="nav-section-title">控制台</span>
              {navItems.map((item) => (
                <NavLink
                  key={item.to}
                  to={item.to}
                  end={item.end}
                  className={({ isActive }) => `nav-link ${isActive ? 'active' : ''}`}
                >
                  {item.icon}
                  <span>{item.label}</span>
                </NavLink>
              ))}
            </nav>

            <div className="sidebar-footer">
              <div className="workspace-card">
                <span className="workspace-chip">在线</span>
                <strong>OpenAI 兼容代理</strong>
                <p>在一个工作台里查看账号、流量与系统健康度，不再在页面间来回切换。</p>
              </div>
            </div>
          </div>
        </aside>

        <main className="main-content">
          <header className="mobile-topbar">
            <div>
              <span className="brand-kicker">Codex Workspace</span>
              <strong>Codex2API</strong>
            </div>
            <span className="workspace-chip">在线</span>
          </header>

          <div className="content-shell">{children}</div>
        </main>

        <nav className="mobile-nav" aria-label="移动导航">
          {navItems.map((item) => (
            <NavLink
              key={item.to}
              to={item.to}
              end={item.end}
              className={({ isActive }) => `mobile-nav-link ${isActive ? 'active' : ''}`}
            >
              {item.icon}
              <span>{item.label}</span>
            </NavLink>
          ))}
        </nav>
      </div>
    </div>
  )
}
