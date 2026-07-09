import type { ReactNode } from 'react'
import { Button } from '@/components/ui/button'
import { AlertCircle, Inbox, Loader2 } from 'lucide-react'
import { useTranslation } from 'react-i18next'
import { cn } from '@/lib/utils'

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

function ShellFrame({
  variant,
  children,
  className,
  role,
}: {
  variant: 'page' | 'section'
  children: ReactNode
  className?: string
  role?: string
}) {
  const minH = variant === 'page' ? 'min-h-[320px]' : 'min-h-[220px]'
  return (
    <div
      role={role}
      className={cn(
        'relative flex flex-col items-center justify-center gap-3 overflow-hidden rounded-2xl border border-border/80 bg-card/85 p-8 text-center shadow-sm',
        minH,
        className,
      )}
    >
      <div
        aria-hidden
        className="pointer-events-none absolute inset-0 bg-[radial-gradient(ellipse_at_top,color-mix(in_oklab,var(--color-primary)_8%,transparent),transparent_60%)]"
      />
      <div className="relative z-10 flex w-full max-w-md flex-col items-center gap-3">
        {children}
      </div>
    </div>
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
  loadingTitle,
  loadingDescription,
  errorTitle,
  emptyTitle,
  emptyDescription,
}: StateShellProps) {
  const { t } = useTranslation()
  const resolvedLoadingTitle = loadingTitle ?? t('common.loading')
  const resolvedLoadingDescription = loadingDescription ?? t('common.syncingData')
  const resolvedErrorTitle = errorTitle ?? t('common.loadFailed')
  const resolvedEmptyTitle = emptyTitle ?? t('common.noData')
  const resolvedEmptyDescription = emptyDescription ?? t('common.noContentYet')

  if (loading) {
    return (
      <ShellFrame variant={variant} role="status" aria-live="polite">
        <div className="flex size-14 items-center justify-center rounded-2xl bg-primary/10 text-primary ring-1 ring-primary/15">
          <Loader2 className="size-6 animate-spin" />
        </div>
        <strong className="text-lg font-bold tracking-tight text-foreground">{resolvedLoadingTitle}</strong>
        <p className="text-sm leading-relaxed text-muted-foreground">{resolvedLoadingDescription}</p>
        <div className="mt-1 grid w-full max-w-xs grid-cols-3 gap-2">
          {[0, 1, 2].map((i) => (
            <div
              key={i}
              className="h-1.5 rounded-full bg-muted"
              style={{ opacity: 1 - i * 0.22 }}
            >
              <div className="h-full w-2/3 animate-pulse rounded-full bg-primary/40" />
            </div>
          ))}
        </div>
      </ShellFrame>
    )
  }

  if (error) {
    return (
      <ShellFrame variant={variant} role="alert">
        <div className="flex size-14 items-center justify-center rounded-2xl bg-destructive/12 text-destructive ring-1 ring-destructive/15">
          <AlertCircle className="size-6" />
        </div>
        <strong className="text-lg font-bold tracking-tight text-foreground">{resolvedErrorTitle}</strong>
        <p className="text-sm leading-relaxed text-muted-foreground">{error}</p>
        {(onRetry || action) ? (
          <div className="mt-1 flex flex-wrap items-center justify-center gap-2.5">
            {onRetry ? <Button variant="outline" onClick={onRetry}>{t('common.retry')}</Button> : null}
            {action}
          </div>
        ) : null}
      </ShellFrame>
    )
  }

  if (isEmpty) {
    return (
      <ShellFrame variant={variant}>
        <div className="relative">
          <div className="flex size-16 items-center justify-center rounded-2xl bg-[hsl(var(--info-bg))] text-[hsl(var(--info))] ring-1 ring-[hsl(var(--info))]/15">
            <Inbox className="size-7" />
          </div>
          <span className="absolute -right-1 -top-1 size-3 rounded-full bg-primary/70 ring-2 ring-card" />
        </div>
        <strong className="text-lg font-bold tracking-tight text-foreground">{resolvedEmptyTitle}</strong>
        <p className="text-sm leading-relaxed text-muted-foreground">{resolvedEmptyDescription}</p>
        {action ? (
          <div className="mt-1 flex flex-wrap items-center justify-center gap-2.5">{action}</div>
        ) : null}
      </ShellFrame>
    )
  }

  return <>{children}</>
}
