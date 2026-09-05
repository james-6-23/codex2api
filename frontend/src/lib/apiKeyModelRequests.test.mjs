import assert from 'node:assert/strict'
import test from 'node:test'
import { MAX_MODEL_REQUEST_RULES, modelRequestLimitsFromAPIKey, modelRequestLimitsToPayload, newModelRequestLimitRow } from './apiKeyModelRequests.ts'

const savedRules = [
  { id: 'rule-series', model: 'gpt-6*', window: 'week', max_requests: 50, timezone: 'Asia/Shanghai', reset_weekday: 1, reset_time: '00:00' },
  { id: 'rule-exact', model: 'gpt-6', window: 'week', max_requests: 20, timezone: 'America/New_York', reset_weekday: 7, reset_time: '09:30' },
]

test('editing caps and reordering rules preserves their ledger identity and calendar settings', () => {
  const form = modelRequestLimitsFromAPIKey(savedRules).reverse()
  form[1].maxRequests = '75'
  assert.deepEqual(modelRequestLimitsToPayload(form), [savedRules[1], { ...savedRules[0], max_requests: 75 }])
  assert.equal(savedRules[0].max_requests, 50)
})

test('new rules omit IDs and retain overlapping wildcard rules; removing all rules sends an empty list', () => {
  const form = { ...newModelRequestLimitRow(), model: ' gpt-6* ', maxRequests: ' 50 ', timezone: ' UTC ' }
  const [rule] = modelRequestLimitsToPayload([form])
  assert.equal(Object.hasOwn(rule, 'id'), false)
  assert.equal(rule.model, 'gpt-6*')
  assert.equal(rule.timezone, 'UTC')
  assert.equal(modelRequestLimitsToPayload([form, { ...form, model: 'gpt-6' }]).length, 2)
  assert.deepEqual(modelRequestLimitsToPayload([]), [])
  assert.deepEqual(modelRequestLimitsFromAPIKey(undefined), [])
})

test('invalid budgets cannot silently become an unlimited rule', () => {
  const row = modelRequestLimitsFromAPIKey(savedRules)[0]
  for (const maxRequests of ['', '0', '-1', '1.5', 'Infinity', '9007199254740992']) {
    assert.throws(() => modelRequestLimitsToPayload([{ ...row, maxRequests }]), /invalidLimit/)
  }
  assert.throws(() => modelRequestLimitsToPayload([{ ...row, model: ' ' }]), /invalidModel/)
  assert.throws(() => modelRequestLimitsToPayload([{ ...row, timezone: '' }]), /invalidTimezone/)
  assert.throws(() => modelRequestLimitsToPayload([{ ...row, timezone: 'Local' }]), /invalidTimezone/)
  for (const resetTime of ['24:00', '12:60', '9:00', '']) {
    assert.throws(() => modelRequestLimitsToPayload([{ ...row, resetTime }]), /invalidReset/)
  }
  for (const resetWeekday of [0, 8, 1.5]) {
    assert.throws(() => modelRequestLimitsToPayload([{ ...row, resetWeekday }]), /invalidReset/)
  }
})

test('model patterns follow the database syntax and length boundary', () => {
  const row = modelRequestLimitsFromAPIKey(savedRules)[0]
  for (const model of ['*', 'gpt-6*', 'gpt-6.1', 'provider/model_name:variant+test', 'x'.repeat(200)]) {
    assert.equal(modelRequestLimitsToPayload([{ ...row, model }])[0].model, model)
  }
  for (const model of ['gpt-6?', 'gpt-[67]', 'gpt 6', '模型', 'x'.repeat(201)]) {
    assert.throws(() => modelRequestLimitsToPayload([{ ...row, model }]), /invalidModel/)
  }
})

test('the rule limit includes every configured rule and rejects 65 instead of truncating', () => {
  const row = { ...newModelRequestLimitRow(), model: '*', maxRequests: '1' }
  assert.equal(MAX_MODEL_REQUEST_RULES, 64)
  assert.equal(modelRequestLimitsToPayload(Array.from({ length: 64 }, () => ({ ...row }))).length, 64)
  assert.throws(() => modelRequestLimitsToPayload(Array.from({ length: 65 }, () => ({ ...row }))), /tooManyRules/)
})
