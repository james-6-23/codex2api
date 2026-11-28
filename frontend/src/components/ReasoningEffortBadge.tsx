const EFFORT_STYLES: Record<string, string> = {
  none: 'bg-slate-500/[0.08] text-slate-500 ring-slate-500/15 dark:bg-slate-500/15 dark:text-slate-400',
  minimal: 'bg-sky-500/[0.08] text-sky-700 ring-sky-500/15 dark:bg-sky-500/15 dark:text-sky-300',
  low: 'bg-emerald-500/[0.08] text-emerald-700 ring-emerald-500/15 dark:bg-emerald-500/15 dark:text-emerald-300',
  medium: 'bg-amber-500/[0.09] text-amber-700 ring-amber-500/20 dark:bg-amber-500/15 dark:text-amber-300',
  high: 'bg-orange-500/[0.09] text-orange-700 ring-orange-500/20 dark:bg-orange-500/15 dark:text-orange-300',
  xhigh: 'bg-rose-500/[0.09] text-rose-700 ring-rose-500/20 dark:bg-rose-500/15 dark:text-rose-300',
  ultra: 'bg-violet-500/[0.10] text-violet-700 ring-violet-500/20 dark:bg-violet-500/18 dark:text-violet-300',
}
const EFFORT_ALIASES: Record<string, string> = { off: 'none', min: 'minimal', med: 'medium', max: 'xhigh' }

export default function ReasoningEffortBadge({ effort }: { effort?: string }) {
  const label = effort?.trim()
  if (!label) return null
  const normalized = label.toLowerCase()
  const style = EFFORT_STYLES[EFFORT_ALIASES[normalized] || normalized] || 'bg-muted/70 text-muted-foreground ring-border/60'
  return (
    <span
      title={`reasoning: ${label}`}
      className={`inline-flex h-[19px] shrink-0 items-center rounded-md px-1.5 font-sans text-[10.5px] font-medium lowercase leading-none ring-1 ring-inset ${style}`}
    >
      {label}
    </span>
  )
}
