interface BillingWindowAccount {
  id: number
  reset_7d_at?: string
  usage_window_7d_seconds?: number
}

interface BillingWindowBoundary {
  resetAt: number | null
  seconds: number
}

export type AccountBillingWindowBoundaries = Map<number, BillingWindowBoundary>

export function updateAccountBillingWindowBoundaries(accounts: BillingWindowAccount[], previous: AccountBillingWindowBoundaries) {
  const boundaries: AccountBillingWindowBoundaries = new Map()
  let changed = false
  for (const account of accounts) {
    const rawReset = account.reset_7d_at ? Date.parse(account.reset_7d_at) : NaN
    const resetAt = Number.isFinite(rawReset) && rawReset > 0 ? rawReset : null
    const rawSeconds = account.usage_window_7d_seconds
    const seconds = typeof rawSeconds === 'number' && Number.isFinite(rawSeconds) && rawSeconds > 0 ? Math.trunc(rawSeconds) : 604800
    const current = { resetAt, seconds }
    const old = previous.get(account.id)
    if (!old) {
      boundaries.set(account.id, current)
      continue
    }
    if (resetAt === null) {
      boundaries.set(account.id, old)
      continue
    }
    if (old.resetAt === null) {
      changed = true
      boundaries.set(account.id, current)
      continue
    }
    const resetDelta = resetAt - old.resetAt
    const tolerance = Math.min(Math.floor(seconds / 4), 300) * 1000
    const sameReset = Math.abs(resetDelta) <= 300000
    const durationChanged = seconds !== old.seconds && !(sameReset && seconds < old.seconds)
    if (durationChanged || resetDelta > tolerance) {
      changed = true
      boundaries.set(account.id, current)
    } else {
      boundaries.set(account.id, old)
    }
  }
  return { boundaries, changed }
}
