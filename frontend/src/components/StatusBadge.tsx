import { Tooltip, TooltipContent, TooltipProvider, TooltipTrigger } from '@/components/ui/tooltip'
import { useTranslation } from 'react-i18next'
import { cn } from '@/lib/utils'

interface StatusBadgeProps {
  status?: string | null
  detail?: string | null
  errorMessage?: string | null
}

const TONE_CLASS = {
  success: 'bg-emerald-500/10 text-emerald-700 ring-emerald-500/20 dark:bg-emerald-500/15 dark:text-emerald-300 dark:ring-emerald-400/25',
  warning: 'bg-amber-500/10 text-amber-700 ring-amber-500/25 dark:bg-amber-500/15 dark:text-amber-300 dark:ring-amber-400/25',
  danger: 'bg-destructive/10 text-destructive ring-destructive/20 dark:bg-destructive/15 dark:text-red-300 dark:ring-red-400/25',
  info: 'bg-sky-500/10 text-sky-700 ring-sky-500/20 dark:bg-sky-500/15 dark:text-sky-300 dark:ring-sky-400/25',
  neutral: 'bg-muted/70 text-muted-foreground ring-border/80 dark:bg-white/[0.06] dark:ring-white/10',
} as const

const statusConfig: Record<string, { tone: keyof typeof TONE_CLASS; dotColor: string }> = {
  active: { tone: 'success', dotColor: 'bg-emerald-500' },
  ready: { tone: 'success', dotColor: 'bg-emerald-500' },
  cooldown: { tone: 'warning', dotColor: 'bg-amber-500' },
  rate_limited: { tone: 'warning', dotColor: 'bg-amber-500' },
  responses_rate_limited: { tone: 'warning', dotColor: 'bg-amber-500' },
  rate_limited_5h: { tone: 'warning', dotColor: 'bg-amber-500' },
  rate_limited_7d: { tone: 'warning', dotColor: 'bg-amber-500' },
  usage_exhausted: { tone: 'warning', dotColor: 'bg-amber-500' },
  usage_limited: { tone: 'warning', dotColor: 'bg-amber-500' },
  quota_paused: { tone: 'warning', dotColor: 'bg-amber-500' },
  overload_paused: { tone: 'warning', dotColor: 'bg-orange-500' },
  forbidden: { tone: 'danger', dotColor: 'bg-red-500' },
  unauthorized: { tone: 'danger', dotColor: 'bg-red-500' },
  error: { tone: 'danger', dotColor: 'bg-red-400' },
  refreshing: { tone: 'info', dotColor: 'bg-sky-500 animate-pulse' },
  paused: { tone: 'info', dotColor: 'bg-sky-500' },
  unsampled: { tone: 'neutral', dotColor: 'bg-slate-400' },
}

export default function StatusBadge({ status, detail, errorMessage }: StatusBadgeProps) {
  const { t } = useTranslation()
  const key = status ?? 'unknown'
  const config = statusConfig[key] ?? { tone: 'neutral' as const, dotColor: 'bg-muted-foreground/60' }
  const trimmedError = errorMessage?.trim() ?? ''
  const showErrorTooltip = key === 'unauthorized' || key === 'error'

  const badge = (
    <span
      data-slot="status-badge"
      className={cn(
        'inline-flex w-fit shrink-0 items-center justify-center gap-1.5 rounded-full px-2.5 py-0.5 text-[12px] font-medium whitespace-nowrap ring-1 ring-inset transition-colors',
        TONE_CLASS[config.tone],
        showErrorTooltip && 'cursor-help',
      )}
    >
      <span className={cn('size-1.5 shrink-0 rounded-full', config.dotColor)} />
      <span>{t(`status.${key}`, { defaultValue: t('status.unknown', { defaultValue: key }) })}</span>
      {detail && (
        <>
          <span className="h-3 w-px bg-current/20" />
          <span className="font-mono text-[11px] font-semibold leading-none tabular-nums">{detail}</span>
        </>
      )}
    </span>
  )

  if (!showErrorTooltip) {
    return badge
  }

  const message = trimmedError || t('usage.statusErrorEmpty')

  return (
    <TooltipProvider>
      <Tooltip>
        <TooltipTrigger asChild>
          <span
            tabIndex={0}
            aria-label={message}
            className="inline-flex focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring"
          >
            {badge}
          </span>
        </TooltipTrigger>
        <TooltipContent
          side="right"
          sideOffset={8}
          className="max-w-[360px] rounded-lg border border-slate-700 bg-slate-950 px-3 py-2.5 text-xs text-slate-50 shadow-xl"
        >
          <div className="space-y-1.5">
            <div className="font-semibold text-slate-300">{t('usage.statusErrorDetails')}</div>
            <div className="whitespace-pre-wrap break-words leading-relaxed text-slate-50">{message}</div>
          </div>
        </TooltipContent>
      </Tooltip>
    </TooltipProvider>
  )
}
