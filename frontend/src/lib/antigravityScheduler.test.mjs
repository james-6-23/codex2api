import assert from 'node:assert/strict'
import test from 'node:test'
import { buildSchedulerPatch, schedulerDraftFromAccount, soonestModelCooldown } from './antigravityScheduler.ts'

const account = {
  id: 72,
  score_bias_override: null,
  base_concurrency_override: null,
  scheduler_priority: null,
  dispatch_count_limit: null,
  skip_warm_tier: false,
  keep_concurrency_on_degrade: false,
  allowed_api_key_ids: [3, 1],
  tags: ['a'],
}

test('unchanged draft produces an empty patch', () => {
  assert.deepEqual(buildSchedulerPatch(account, schedulerDraftFromAccount(account)), { patch: {} })
  // 顺序不同的同一组 Key 不算修改
  const draft = { ...schedulerDraftFromAccount(account), allowedAPIKeyIDs: [1, 3] }
  assert.deepEqual(buildSchedulerPatch(account, draft), { patch: {} })
})

test('only changed fields are sent; 0 clears priority and dispatch limit', () => {
  const draft = {
    ...schedulerDraftFromAccount(account),
    concurrency: '8',
    priority: '0',
    dispatchLimit: '50',
    skipWarmTier: true,
    allowedAPIKeyIDs: [],
  }
  assert.deepEqual(buildSchedulerPatch(account, draft), {
    patch: { base_concurrency_override: 8, dispatch_count_limit: 50, skip_warm_tier: true, allowed_api_key_ids: [] },
  })
  const configured = { ...account, scheduler_priority: 20, dispatch_count_limit: 50, base_concurrency_override: 8 }
  const cleared = { ...schedulerDraftFromAccount(configured), priority: '0', dispatchLimit: '', concurrency: '' }
  assert.deepEqual(buildSchedulerPatch(configured, cleared), {
    patch: { base_concurrency_override: null, scheduler_priority: null, dispatch_count_limit: null },
  })
})

test('auto-pause thresholds are sent as ratios and cleared with null', () => {
  const draft = { ...schedulerDraftFromAccount(account), autoPause5h: '95', autoPause7dDisabled: true }
  assert.deepEqual(buildSchedulerPatch(account, draft), {
    patch: { auto_pause_5h_threshold: 0.95, auto_pause_7d_disabled: true },
  })
  const configured = { ...account, auto_pause_5h_threshold: 0.95, auto_pause_7d_threshold: 0.9 }
  assert.equal(schedulerDraftFromAccount(configured).autoPause5h, '95')
  const cleared = { ...schedulerDraftFromAccount(configured), autoPause5h: '' }
  assert.deepEqual(buildSchedulerPatch(configured, cleared), { patch: { auto_pause_5h_threshold: null } })
})

test('invalid inputs are rejected before any request', () => {
  const base = schedulerDraftFromAccount(account)
  assert.deepEqual(buildSchedulerPatch(account, { ...base, scoreBias: '201' }), { error: 'score' })
  assert.deepEqual(buildSchedulerPatch(account, { ...base, concurrency: '0' }), { error: 'concurrency' })
  assert.deepEqual(buildSchedulerPatch(account, { ...base, priority: '1.5' }), { error: 'priority' })
  assert.deepEqual(buildSchedulerPatch(account, { ...base, dispatchLimit: '-1' }), { error: 'dispatchLimit' })
  assert.deepEqual(buildSchedulerPatch(account, { ...base, autoPause7d: '101' }), { error: 'autoPause' })
})

test('soonest model cooldown ignores expired entries', () => {
  const now = Date.parse('2026-11-21T00:00:00Z')
  const row = {
    model_cooldowns: [
      { model: 'old', reason: 'rate_limited', reset_at: '2026-11-20T23:59:00Z', remaining_seconds: 0 },
      { model: 'late', reason: 'rate_limited', reset_at: '2026-11-21T00:10:00Z', remaining_seconds: 600 },
      { model: 'soon', reason: 'rate_limited', reset_at: '2026-11-21T00:01:00Z', remaining_seconds: 60 },
    ],
  }
  const result = soonestModelCooldown(row, now)
  assert.equal(result.first.model, 'soon')
  assert.equal(result.count, 2)
  assert.equal(soonestModelCooldown({ model_cooldowns: [] }, now), null)
})
