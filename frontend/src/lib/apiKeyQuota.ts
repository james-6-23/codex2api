import type { APIKeyLimits, PublicAPIKeyWindowUsage } from '../types'

/** Cost counted against a window's quota; historical billing stays unchanged. */
export function apiKeyQuotaCost(usage: Pick<PublicAPIKeyWindowUsage, 'user_billed' | 'cost_exempted'>): number {
  return Math.max(0, usage.user_billed - (usage.cost_exempted ?? 0))
}

export function canResetAPIKeyWeeklyQuota(limits?: APIKeyLimits): boolean {
  const weekly = limits?.cost_limit_7d ?? 0
  const monthly = limits?.cost_limit_30d ?? 0
  return Number.isFinite(weekly) && weekly > 0 && Number.isFinite(monthly) && monthly > 0
}
