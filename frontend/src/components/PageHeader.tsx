import type { ReactNode } from 'react'

interface PageHeaderProps {
  title: string
  description?: string
  onRefresh?: () => void
  refreshLabel?: string
  actions?: ReactNode
}

function RefreshIcon() {
  return (
    <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2">
      <path d="M23 4v6h-6" />
      <path d="M1 20v-6h6" />
      <path d="M3.51 9a9 9 0 0 1 14.85-3.36L23 10M1 14l4.64 4.36A9 9 0 0 0 20.49 15" />
    </svg>
  )
}

export default function PageHeader({
  title,
  description,
  onRefresh,
  refreshLabel = '刷新',
  actions,
}: PageHeaderProps) {
  const hasActions = Boolean(onRefresh) || Boolean(actions)

  return (
    <div className="page-header">
      <div className="page-header-copy">
        <h2>{title}</h2>
        {description ? <p>{description}</p> : null}
      </div>
      {hasActions ? (
        <div className="page-header-actions">
          {onRefresh ? (
            <button className="btn btn-secondary" onClick={onRefresh}>
              <RefreshIcon />
              {refreshLabel}
            </button>
          ) : null}
          {actions}
        </div>
      ) : null}
    </div>
  )
}
