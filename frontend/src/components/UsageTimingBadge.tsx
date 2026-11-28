import type { TimingTone } from '../lib/usageTiming'
import { cn } from '../lib/utils'

const VALUE_TONE: Record<TimingTone, string> = {
  good: 'text-emerald-600 dark:text-emerald-400',
  warn: 'text-amber-600 dark:text-amber-400',
  bad: 'text-rose-600 dark:text-rose-400',
  muted: 'text-muted-foreground/70',
}

function shellToneClass(kind: 'first' | 'duration', tone: TimingTone): string {
  if (tone === 'warn') return 'bg-amber-500/[0.08] ring-1 ring-inset ring-amber-500/20 dark:bg-amber-500/12'
  if (tone === 'bad') return 'bg-rose-500/[0.08] ring-1 ring-inset ring-rose-500/20 dark:bg-rose-500/12'
  if (tone === 'muted') return 'bg-muted/35 ring-1 ring-inset ring-border/40'
  return kind === 'first'
    ? 'bg-emerald-500/[0.08] ring-1 ring-inset ring-emerald-500/20 dark:bg-emerald-500/12'
    : 'bg-muted/55 ring-1 ring-inset ring-border/55'
}

export default function UsageTimingBadge({ kind, label, value, tone }: {
  kind: 'first' | 'duration'
  label: string
  value: string
  tone: TimingTone
}) {
  return (
    <span
      className={cn(
        'inline-flex h-[22px] items-center gap-1 rounded-md px-1.5 whitespace-nowrap transition-colors',
        shellToneClass(kind, tone),
      )}
    >
      <span
        className={cn(
          'text-[10.5px] font-normal leading-none',
          kind === 'first' && tone === 'good'
            ? 'text-emerald-700/75 dark:text-emerald-300/80'
            : 'text-muted-foreground',
        )}
      >
        {label}
      </span>
      <span className={cn('font-sans text-[11.5px] font-medium tabular-nums leading-none tracking-[-0.01em]', VALUE_TONE[tone])}>
        {value}
      </span>
    </span>
  )
}
