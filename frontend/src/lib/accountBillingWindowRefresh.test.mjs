import assert from 'node:assert/strict'
import { test } from 'node:test'
import { updateAccountBillingWindowBoundaries } from './accountBillingWindowRefresh.ts'

const resetAt = Date.parse('2026-09-15T01:00:00Z')
const account = (offset = 0, seconds = 604800, id = 1) => ({ id, reset_7d_at: new Date(resetAt + offset).toISOString(), usage_window_7d_seconds: seconds })

test('initial page and page changes do not duplicate page-stats fetching', () => {
  const initial = updateAccountBillingWindowBoundaries([account()], new Map())
  assert.equal(initial.changed, false)
  const nextPage = updateAccountBillingWindowBoundaries([account(0, 604800, 2)], initial.boundaries)
  assert.equal(nextPage.changed, false)
  assert.deepEqual([...nextPage.boundaries.keys()], [2])
})

test('long reset changes refresh once and not on percentages or small jitter', () => {
  const initial = updateAccountBillingWindowBoundaries([account()], new Map())
  for (const offset of [0, 1000, 180000, 300000, -3600000]) {
    const result = updateAccountBillingWindowBoundaries([{ ...account(offset), usage_percent_7d: 0 }], initial.boundaries)
    assert.equal(result.changed, false)
    assert.equal(result.boundaries.get(1).resetAt, resetAt)
  }
  const reset = updateAccountBillingWindowBoundaries([account(301000)], initial.boundaries)
  assert.equal(reset.changed, true)
  assert.equal(reset.boundaries.get(1).resetAt, resetAt + 301000)
  assert.equal(updateAccountBillingWindowBoundaries([account(301000)], reset.boundaries).changed, false)
})

test('missing or invalid metadata does not erase the comparison anchor', () => {
  const initial = updateAccountBillingWindowBoundaries([account()], new Map())
  for (const reset of [undefined, '', 'invalid']) {
    const missing = updateAccountBillingWindowBoundaries([{ id: 1, reset_7d_at: reset }], initial.boundaries)
    assert.equal(missing.changed, false)
    assert.equal(missing.boundaries.get(1).resetAt, resetAt)
  }
  const unsampled = updateAccountBillingWindowBoundaries([{ id: 1 }], new Map())
  assert.equal(updateAccountBillingWindowBoundaries([account()], unsampled.boundaries).changed, true)
})

test('duration corrections refresh but stale monthly-to-weekly shrink does not', () => {
  const initial = updateAccountBillingWindowBoundaries([account()], new Map())
  const monthly = updateAccountBillingWindowBoundaries([account(0, 2592000)], initial.boundaries)
  assert.equal(monthly.changed, true)
  const stale = updateAccountBillingWindowBoundaries([account()], monthly.boundaries)
  assert.equal(stale.changed, false)
  assert.equal(stale.boundaries.get(1).seconds, 2592000)
})
