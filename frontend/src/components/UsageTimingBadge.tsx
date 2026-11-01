import type { TimingTone } from '../lib/usageTiming'
import { cn } from '../lib/utils'

const VALUE_TONE: Record<TimingTone, string> = {
  good: 'text-emerald-700 dark:text-emerald-300',
  warn: 'text-amber-700 dark:text-amber-300',
  bad: 'text-red-600 dark:text-red-400',
  muted: 'text-muted-foreground',
}
const PILL_SHELL = {
  first: 'bg-sky-500/12 ring-1 ring-inset ring-sky-500/25 dark:bg-sky-500/16 dark:ring-sky-400/30',
  duration: 'bg-violet-500/12 ring-1 ring-inset ring-violet-500/25 dark:bg-violet-500/16 dark:ring-violet-400/30',
  muted: 'bg-muted/60 ring-1 ring-inset ring-border',
}
const PILL_LABEL = {
  first: 'text-sky-700/85 dark:text-sky-300/90',
  duration: 'text-violet-700/85 dark:text-violet-300/90',
  muted: 'text-muted-foreground',
}

export default function UsageTimingBadge({ kind, label, value, tone }: {
  kind: 'first' | 'duration'
  label: string
  value: string
  tone: TimingTone
}) {
  const style = tone === 'muted' ? 'muted' : kind
  return (
    <span className={cn('inline-flex h-5 items-center gap-0.5 rounded-full px-1.5 whitespace-nowrap', PILL_SHELL[style])}>
      <span className={cn('text-[10px] font-semibold leading-none tracking-tight', PILL_LABEL[style])}>{label}</span>
      <span className={cn('font-mono text-[11px] font-bold tabular-nums leading-none', VALUE_TONE[tone])}>{value}</span>
    </span>
  )
}
