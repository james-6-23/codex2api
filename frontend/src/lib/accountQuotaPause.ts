import type { AccountRow } from '../types'

// Account overrides alone cannot identify pauses inherited from a group/global
// threshold. Use the scheduler's window and the corresponding upstream reset.
export function getAccountQuotaPausePresentation(account: Pick<AccountRow,
  'status' | 'quota_pause_window' | 'reset_5h_at' | 'reset_7d_at'
>) {
  const window = account.quota_pause_window
  if (account.status !== 'quota_paused' || (window !== '5h' && window !== '7d')) return null
  return {
    window,
    resetAt: window === '7d' ? account.reset_7d_at : account.reset_5h_at,
  }
}
