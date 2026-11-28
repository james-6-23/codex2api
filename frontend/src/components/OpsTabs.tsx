import { NavLink } from 'react-router-dom'
import { Activity, AlertCircle, ServerCog, Workflow } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { cn } from '@/lib/utils'

const tabs = [
  { to: '/ops/overview', labelKey: 'ops.tabs.overview', icon: <Activity className="size-3.5" /> },
  { to: '/ops/runtime', labelKey: 'ops.tabs.runtime', icon: <ServerCog className="size-3.5" /> },
  { to: '/ops/errors', labelKey: 'ops.tabs.errors', icon: <AlertCircle className="size-3.5" /> },
  { to: '/ops/scheduler', labelKey: 'ops.tabs.scheduler', icon: <Workflow className="size-3.5" /> },
]

export default function OpsTabs() {
  const { t } = useTranslation()

  return (
    <div className="mb-5 flex max-w-full items-center overflow-x-auto pb-1 [-ms-overflow-style:none] [scrollbar-width:none] [&::-webkit-scrollbar]:hidden">
      <nav
        aria-label={t('ops.title')}
        className="inline-flex items-center gap-1 rounded-xl border border-border/75 bg-muted/45 p-1 shadow-2xs"
      >
        {tabs.map((tab) => (
          <NavLink
            key={tab.to}
            to={tab.to}
            className={({ isActive }) =>
              cn(
                'inline-flex h-8 shrink-0 items-center gap-1.5 rounded-lg px-3.5 text-[12.5px] font-medium whitespace-nowrap transition-all duration-150',
                isActive
                  ? 'bg-background text-foreground shadow-2xs ring-1 ring-border/70'
                  : 'text-muted-foreground hover:bg-background/50 hover:text-foreground',
              )
            }
          >
            {tab.icon}
            {t(tab.labelKey)}
          </NavLink>
        ))}
      </nav>
    </div>
  )
}
