import { cn } from '../lib/utils'

export type CompactStatTone = 'neutral' | 'success' | 'warning' | 'danger'

const TONE_STYLE: Record<CompactStatTone, { chip: string; dot: string }> = {
  neutral: {
    chip: 'bg-muted/75 text-muted-foreground ring-1 ring-inset ring-border/70 dark:bg-white/[0.06] dark:ring-white/10',
    dot: 'bg-muted-foreground/80',
  },
  success: {
    chip: 'bg-emerald-500/10 text-emerald-700 ring-1 ring-inset ring-emerald-500/20 dark:bg-emerald-500/15 dark:text-emerald-300 dark:ring-emerald-400/25',
    dot: 'bg-emerald-500',
  },
  warning: {
    chip: 'bg-amber-500/10 text-amber-700 ring-1 ring-inset ring-amber-500/25 dark:bg-amber-500/15 dark:text-amber-300 dark:ring-amber-400/25',
    dot: 'bg-amber-500',
  },
  danger: {
    chip: 'bg-destructive/10 text-destructive ring-1 ring-inset ring-destructive/20 dark:bg-destructive/15 dark:text-red-300 dark:ring-red-400/25',
    dot: 'bg-destructive',
  },
}

/**
 * 可点击的账号筛选统计磁贴（数值 + 状态芯片 + 可选明细列），
 * 从 Accounts/GrokAccounts 的两份本地实现合并而来。
 * 标签排版跟随 house 磁贴规格（uppercase tracking-wider 小标签）。
 */
export function CompactStat({
  label,
  chipLabel,
  value,
  tone,
  details,
  active = false,
  onClick,
}: {
  label: string
  chipLabel?: string
  value: number
  tone: CompactStatTone
  details?: Array<{ label: string; value: number }>
  active?: boolean
  onClick?: () => void
}) {
  const toneStyle = TONE_STYLE[tone]

  const className = cn(
    'flex min-h-[72px] w-full items-center justify-between gap-2 rounded-xl border px-3 py-2 text-left shadow-xs transition-[border-color,box-shadow,background-color,transform] duration-200 ease-[cubic-bezier(0.23,1,0.32,1)] sm:min-h-[84px] sm:gap-3 sm:px-3.5 sm:py-2.5 dark:shadow-[inset_0_1px_0_rgba(255,255,255,0.04)]',
    active
      ? 'border-primary/45 bg-primary/[0.06] ring-1 ring-primary/25 shadow-sm'
      : 'border-border/80 bg-card hover:border-border dark:border-white/10 dark:hover:border-white/16',
    onClick &&
      'cursor-pointer hover:-translate-y-[1px] hover:shadow-sm active:scale-[0.99] focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-ring/50',
  )

  const content = (
    <>
      <div className="min-w-0">
        <div className="truncate text-[11px] font-semibold uppercase tracking-wider text-muted-foreground">
          {label}
        </div>
        <div className="mt-1.5 text-[22px] font-semibold leading-none tabular-nums tracking-tight text-foreground sm:text-[26px]">
          {value}
        </div>
      </div>
      <div className="flex min-h-[48px] shrink-0 flex-col items-end gap-1 sm:min-h-[54px] sm:gap-1.5">
        <div
          className={cn(
            'inline-flex items-center gap-1.5 rounded-full px-1.5 py-0.5 text-[11px] font-medium sm:px-2 sm:py-1 sm:text-[12px]',
            toneStyle.chip,
          )}
        >
          <span className={cn('size-1.5 rounded-full sm:size-1.5', toneStyle.dot)} />
          <span className="max-w-[4.5rem] truncate sm:max-w-none">{chipLabel ?? label}</span>
        </div>
        {details && details.length > 0 && (
          <div className="flex flex-col items-end gap-0.5 text-[11px] font-medium leading-4 text-muted-foreground">
            {details.map((item) => (
              <div
                key={item.label}
                className="grid grid-cols-[max-content_auto_max-content] items-center gap-x-0.5 whitespace-nowrap tabular-nums"
              >
                <span className="justify-self-start">{item.label}</span>
                <span className="justify-self-center">：</span>
                <span className="justify-self-end text-foreground">{item.value}</span>
              </div>
            ))}
          </div>
        )}
      </div>
    </>
  )

  if (onClick) {
    return (
      <button type="button" onClick={onClick} aria-pressed={active} className={className}>
        {content}
      </button>
    )
  }

  return <div className={className}>{content}</div>
}
