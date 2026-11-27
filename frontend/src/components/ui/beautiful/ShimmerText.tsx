import type { ReactNode } from 'react'
import { cn } from '@/lib/utils'

export function ShimmerText({
  children,
  active = true,
  className,
}: {
  children: ReactNode
  active?: boolean
  className?: string
}) {
  return (
    <span className={cn(active ? 'bui-shimmer-text' : 'text-foreground', className)}>
      {children}
    </span>
  )
}
