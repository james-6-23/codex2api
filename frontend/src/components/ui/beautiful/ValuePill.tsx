import type { ReactNode } from 'react'
import { cn } from '@/lib/utils'

export type ValuePillTone = 'neutral' | 'primary' | 'success' | 'warning' | 'danger' | 'info'

const PILL_TONES: Record<ValuePillTone, { badge: string; dot: string }> = {
  neutral: {
    badge: 'bg-muted/70 text-foreground/85 ring-border/80 dark:bg-white/[0.06] dark:ring-white/10',
    dot: 'bg-muted-foreground/70',
  },
  primary: {
    badge: 'bg-primary/10 text-primary ring-primary/20 dark:bg-primary/15 dark:ring-primary/25',
    dot: 'bg-primary',
  },
  success: {
    badge: 'bg-emerald-500/10 text-emerald-700 ring-emerald-500/20 dark:bg-emerald-500/15 dark:text-emerald-300 dark:ring-emerald-400/25',
    dot: 'bg-emerald-500',
  },
  warning: {
    badge: 'bg-amber-500/10 text-amber-700 ring-amber-500/25 dark:bg-amber-500/15 dark:text-amber-300 dark:ring-amber-400/25',
    dot: 'bg-amber-500',
  },
  danger: {
    badge: 'bg-destructive/10 text-destructive ring-destructive/20 dark:bg-destructive/15 dark:text-red-300 dark:ring-red-400/25',
    dot: 'bg-destructive',
  },
  info: {
    badge: 'bg-sky-500/10 text-sky-700 ring-sky-500/20 dark:bg-sky-500/15 dark:text-sky-300 dark:ring-sky-400/25',
    dot: 'bg-sky-500',
  },
}

/**
 * Hairline-ringed semantic value pill inspired by beautifului.dev ValuePill.
 */
export function ValuePill({
  tone = 'neutral',
  dot = false,
  pulse = false,
  count,
  children,
  className,
}: {
  tone?: ValuePillTone
  dot?: boolean
  pulse?: boolean
  count?: number | string
  children: ReactNode
  className?: string
}) {
  const style = PILL_TONES[tone]
  return (
    <span
      className={cn(
        'inline-flex items-center gap-1.5 rounded-full px-2.5 py-0.5 text-xs font-medium ring-1 ring-inset whitespace-nowrap',
        style.badge,
        className,
      )}
    >
      {dot ? (
        <span className={cn('size-1.5 shrink-0 rounded-full', style.dot, pulse && 'animate-pulse')} />
      ) : null}
      <span>{children}</span>
      {count !== undefined ? (
        <span className="font-mono text-[11px] tabular-nums opacity-80">{count}</span>
      ) : null}
    </span>
  )
}

export interface FilterChipOption<T extends string = string> {
  value: T
  label: string
  count?: number
  dotClassName?: string
}

/**
 * Colored-dot status/mode filter chip bar inspired by beautifului.dev #13 Filter Table.
 */
export function FilterChipBar<T extends string = string>({
  options,
  value,
  onChange,
  className,
}: {
  options: readonly FilterChipOption<T>[]
  value: T
  onChange: (next: T) => void
  className?: string
}) {
  return (
    <div
      role="tablist"
      className={cn(
        'inline-flex flex-wrap items-center gap-1 rounded-lg border border-border/80 bg-muted/35 p-1 dark:bg-white/[0.03]',
        className,
      )}
    >
      {options.map((opt) => {
        const active = opt.value === value
        return (
          <button
            key={opt.value}
            type="button"
            role="tab"
            aria-selected={active}
            onClick={() => onChange(opt.value)}
            className={cn(
              'inline-flex h-7 items-center gap-1.5 rounded-md px-2.5 text-xs font-medium transition-all duration-150 outline-none focus-visible:ring-2 focus-visible:ring-ring/50',
              active
                ? 'bg-background text-foreground shadow-2xs ring-1 ring-border/80 dark:bg-card dark:ring-white/12'
                : 'text-muted-foreground hover:bg-background/50 hover:text-foreground',
            )}
          >
            {opt.dotClassName ? (
              <span className={cn('size-1.5 shrink-0 rounded-full', opt.dotClassName)} />
            ) : null}
            <span>{opt.label}</span>
            {opt.count !== undefined ? (
              <span
                className={cn(
                  'rounded px-1 py-0.2 font-mono text-[10px] tabular-nums',
                  active
                    ? 'bg-muted text-foreground/80 dark:bg-white/10'
                    : 'text-muted-foreground/80',
                )}
              >
                {opt.count}
              </span>
            ) : null}
          </button>
        )
      })}
    </div>
  )
}
