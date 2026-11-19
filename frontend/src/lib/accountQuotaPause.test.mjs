import assert from 'node:assert/strict'
import test from 'node:test'
import { getAccountQuotaPausePresentation } from './accountQuotaPause.ts'
import { mergeAccountLiveState } from './accountLiveState.ts'

const account = {
  id: 1, status: 'quota_paused',
  // Inherited thresholds are not account overrides and are absent from the row.
  reset_5h_at: '2030-01-01T05:00:00Z', reset_7d_at: '2030-01-07T00:00:00Z',
}

for (const window of ['5h', '7d']) {
  test(`${window} pause uses the matching upstream reset`, () => {
    assert.deepEqual(getAccountQuotaPausePresentation({ ...account, quota_pause_window: window }), {
      window, resetAt: account[window === '5h' ? 'reset_5h_at' : 'reset_7d_at'],
    })
  })
}

test('missing reset never fabricates a recovery time or uses the other window', () => {
  assert.deepEqual(getAccountQuotaPausePresentation({ ...account, quota_pause_window: '7d', reset_7d_at: undefined }), {
    window: '7d', resetAt: undefined,
  })
})

test('older backend and non-auto-pause states retain existing presentation', () => {
  assert.equal(getAccountQuotaPausePresentation(account), null)
  assert.equal(getAccountQuotaPausePresentation({ ...account, status: 'error', quota_pause_window: '7d' }), null)
})

test('live updates replace and clear the pause window without a list reload', () => {
  const rows = [{ ...account, quota_pause_window: '5h' }]
  const response = (fields) => ({ accounts: { '1': { active_requests: 0, ...fields } } })
  const weekly = mergeAccountLiveState(rows, response({ status: 'quota_paused', quota_pause_window: '7d' }))
  assert.equal(weekly[0].quota_pause_window, '7d')
  assert.equal(getAccountQuotaPausePresentation(weekly[0]).resetAt, account.reset_7d_at)
  const cleared = mergeAccountLiveState(weekly, response({ status: 'active', quota_pause_window: '' }))
  assert.equal(cleared[0].quota_pause_window, '')
  assert.equal(getAccountQuotaPausePresentation(cleared[0]), null)
  const oldBackend = mergeAccountLiveState(weekly, response({}))
  assert.equal(oldBackend[0].quota_pause_window, '7d')
})
