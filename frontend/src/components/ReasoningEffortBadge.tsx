import { Badge } from './ui/badge'

const EFFORT_STYLES: Record<string, string> = {
  none: 'border-transparent bg-slate-500/12 text-slate-500 dark:bg-slate-500/20 dark:text-slate-400',
  minimal: 'border-transparent bg-sky-500/12 text-sky-600 dark:bg-sky-500/20 dark:text-sky-400',
  low: 'border-transparent bg-emerald-500/12 text-emerald-600 dark:bg-emerald-500/20 dark:text-emerald-400',
  medium: 'border-transparent bg-amber-500/12 text-amber-600 dark:bg-amber-500/20 dark:text-amber-400',
  high: 'border-transparent bg-orange-500/14 text-orange-600 dark:bg-orange-500/22 dark:text-orange-400',
  xhigh: 'border-transparent bg-rose-500/14 text-rose-600 dark:bg-rose-500/22 dark:text-rose-400',
  ultra: 'border-transparent bg-violet-500/14 text-violet-600 dark:bg-violet-500/22 dark:text-violet-300',
}
const EFFORT_ALIASES: Record<string, string> = { off: 'none', min: 'minimal', med: 'medium', max: 'xhigh' }

export default function ReasoningEffortBadge({ effort }: { effort?: string }) {
  const label = effort?.trim()
  if (!label) return null
  const normalized = label.toLowerCase()
  const style = EFFORT_STYLES[EFFORT_ALIASES[normalized] || normalized] || 'border-transparent bg-muted text-muted-foreground'
  return (
    <Badge variant="outline" title={`reasoning: ${label}`} className={`text-[11px] font-semibold lowercase tracking-wide ${style}`}>
      {label}
    </Badge>
  )
}
