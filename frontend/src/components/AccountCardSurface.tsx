import type { ComponentProps } from 'react'
import { cn } from '@/lib/utils'
import './account-cards.css'

// Shared by management cards and the read-only API key account portal. Actions
// are supplied by each caller; this surface never invokes a management API.
export function AccountCardSurface({ className, ...props }: ComponentProps<'article'>) {
  return <article className={cn('codex-account-card', className)} {...props} />
}
