import type { ReactNode } from 'react'
import { Card, CardContent } from '@/components/ui/card'
import { cn } from '@/lib/utils'

interface StatCardProps {
  icon: ReactNode
  iconClass: string
  label: string
  value: ReactNode
  sub?: string
  className?: string
}

const iconColors: Record<string, string> = {
  blue: 'bg-[hsl(var(--info-bg))] text-[hsl(var(--info))] ring-[hsl(var(--info))]/15',
  green: 'bg-[hsl(var(--success-bg))] text-[hsl(var(--success))] ring-[hsl(var(--success))]/15',
  amber: 'bg-amber-500/12 text-amber-600 ring-amber-500/15 dark:text-amber-400',
  red: 'bg-destructive/12 text-destructive ring-destructive/15',
  purple: 'bg-primary/12 text-primary ring-primary/15',
}

export default function StatCard({ icon, iconClass, label, value, sub, className }: StatCardProps) {
  return (
    <Card
      className={cn(
        'group relative overflow-hidden py-0 transition-all duration-200 hover:-translate-y-0.5 hover:shadow-md',
        className,
      )}
    >
      <CardContent className="relative flex flex-col justify-between gap-2 p-4 sm:p-5">
        <div className="flex items-center justify-between gap-3">
          <div className="min-w-0">
            <label className="block text-[11px] font-bold uppercase tracking-wide text-muted-foreground">
              {label}
            </label>
            <div className="mt-2 text-[26px] font-bold leading-none tabular-nums tracking-tight text-foreground sm:text-[28px]">
              {value}
            </div>
          </div>
          <div
            className={cn(
              'flex size-10 shrink-0 items-center justify-center rounded-xl ring-1 ring-inset sm:size-11',
              iconColors[iconClass] || iconColors.purple,
            )}
            aria-hidden="true"
          >
            <span className="[&_svg]:size-[18px]">{icon}</span>
          </div>
        </div>
        {sub ? (
          <div className="border-t border-border/80 pt-2 text-[12px] text-muted-foreground">
            {sub}
          </div>
        ) : null}
      </CardContent>
    </Card>
  )
}
