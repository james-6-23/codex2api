import type { ReactNode } from 'react'

interface StateShellProps {
  children: ReactNode
  loading?: boolean
  error?: string | null
  isEmpty?: boolean
  onRetry?: () => void
  action?: ReactNode
  variant?: 'page' | 'section'
  loadingTitle?: string
  loadingDescription?: string
  errorTitle?: string
  emptyTitle?: string
  emptyDescription?: string
}

function LoadingIcon() {
  return <div className="spinner" aria-hidden="true" />
}

function EmptyIcon() {
  return (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7">
      <rect x="3" y="5" width="18" height="14" rx="2" />
      <path d="M7 9h10M7 13h6" />
    </svg>
  )
}

function ErrorIcon() {
  return (
    <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.7">
      <circle cx="12" cy="12" r="9" />
      <path d="M12 8v5" />
      <path d="M12 16h.01" />
    </svg>
  )
}

export default function StateShell({
  children,
  loading = false,
  error,
  isEmpty = false,
  onRetry,
  action,
  variant = 'section',
  loadingTitle = '正在加载',
  loadingDescription = '请稍候，数据正在同步。',
  errorTitle = '加载失败',
  emptyTitle = '暂无数据',
  emptyDescription = '当前还没有可展示的内容。',
}: StateShellProps) {
  if (loading) {
    return (
      <div className={`state-shell state-shell-${variant}`} role="status" aria-live="polite">
        <div className="state-shell-icon state-shell-icon-loading">
          <LoadingIcon />
        </div>
        <strong className="state-shell-title">{loadingTitle}</strong>
        <p className="state-shell-description">{loadingDescription}</p>
      </div>
    )
  }

  if (error) {
    return (
      <div className={`state-shell state-shell-${variant} state-shell-error`} role="alert">
        <div className="state-shell-icon state-shell-icon-error">
          <ErrorIcon />
        </div>
        <strong className="state-shell-title">{errorTitle}</strong>
        <p className="state-shell-description">{error}</p>
        {(onRetry || action) ? (
          <div className="state-shell-actions">
            {onRetry ? <button className="btn btn-secondary" onClick={onRetry}>重试</button> : null}
            {action}
          </div>
        ) : null}
      </div>
    )
  }

  if (isEmpty) {
    return (
      <div className={`state-shell state-shell-${variant} state-shell-empty`}>
        <div className="state-shell-icon state-shell-icon-empty">
          <EmptyIcon />
        </div>
        <strong className="state-shell-title">{emptyTitle}</strong>
        <p className="state-shell-description">{emptyDescription}</p>
        {action ? <div className="state-shell-actions">{action}</div> : null}
      </div>
    )
  }

  return <>{children}</>
}
