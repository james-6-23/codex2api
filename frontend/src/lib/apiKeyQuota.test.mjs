import assert from 'node:assert/strict'
import test from 'node:test'

import { apiKeyQuotaCost, canResetAPIKeyWeeklyQuota } from './apiKeyQuota.ts'

test('window quota uses the exemption while preserving actual billing', () => {
  const usage = { user_billed: 480, cost_exempted: 120 }
  assert.equal(apiKeyQuotaCost(usage), 360)
  assert.equal(usage.user_billed, 480)
  assert.equal(apiKeyQuotaCost({ user_billed: 600, cost_exempted: 120 }), 480)
  assert.equal(apiKeyQuotaCost({ user_billed: 100 }), 100)
  assert.equal(apiKeyQuotaCost({ user_billed: 0, cost_exempted: 0 }), 0)
  assert.equal(apiKeyQuotaCost({ user_billed: 80, cost_exempted: 120 }), 0)
})

test('weekly reset requires both bounded weekly and monthly cost quotas', () => {
  assert.equal(canResetAPIKeyWeeklyQuota({ cost_limit_7d: 120, cost_limit_30d: 480 }), true)
  for (const limits of [
    undefined,
    {},
    { cost_limit_7d: 120 },
    { cost_limit_30d: 480 },
    { cost_limit_7d: 0, cost_limit_30d: 480 },
    { cost_limit_7d: 120, cost_limit_30d: -1 },
    { cost_limit_7d: Number.POSITIVE_INFINITY, cost_limit_30d: 480 },
  ]) {
    assert.equal(canResetAPIKeyWeeklyQuota(limits), false)
  }
})
