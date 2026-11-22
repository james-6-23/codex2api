import test from 'node:test'
import assert from 'node:assert/strict'
import { intelligenceWindow, intelligenceColor, intelligenceQuery, latestRequestGuard, intelligenceTargetConflict } from './intelligenceCheck.ts'
test('last 20 completed checks remain chronological and errors do not dilute pass rate', () => {
  const records = Array.from({ length: 23 }, (_, i) => ({ id: i, status: i % 3 === 0 ? 'normal' : i % 3 === 1 ? 'degraded' : 'error', created_at: new Date(i * 1000).toISOString() }))
  records.push({ id: 24, status: 'running' })
  const result = intelligenceWindow(records.toReversed())
  assert.equal(result.records.length, 20)
  assert.equal(result.records[0].id, 3)
  assert.equal(result.records.at(-1).id, 22)
  assert.equal(result.rate, 50)
  assert.equal(result.errors, 6)
})
test('no scored samples never display 100 percent, and specified colors stay distinct', () => {
  assert.equal(intelligenceWindow([]).rate, null)
  assert.equal(intelligenceWindow([{ id: 1, status: 'error' }]).rate, null)
  assert.equal(intelligenceColor('normal'), '#22c55e')
  assert.equal(intelligenceColor('degraded'), '#ef4444')
  assert.equal(intelligenceColor('error'), '#991b1b')
  assert.equal(intelligenceColor('interrupted'), undefined)
})
test('queries escape filters and stale async responses are rejected after newer load or disposal', () => {
  assert.equal(intelligenceQuery(2, { search: 'a&b', channel: '', account_id: 42 }), 'page=2&page_size=20&search=a%26b&account_id=42')
  const guard = latestRequestGuard()
  const first = guard.next(), second = guard.next()
  assert.equal(guard.isCurrent(first), false)
  assert.equal(guard.isCurrent(second), true)
  guard.next()
  assert.equal(guard.isCurrent(second), false)
})
test('conflict actions require structured account details and never infer IDs from an error message', () => {
  const details = { code: 'intelligence_check_target_conflict', target_group_id: 8, target_group_channel: 'codex', conflict_count: 1, conflicts: [{ account_id: 68876, account_name: 'Claude', channel: 'claude' }] }
  assert.deepEqual(intelligenceTargetConflict({ details }), details)
  for (const error of [null, new Error('1 enabled accounts belong to another channel'), { details: null }, { details: {...details, code: 'revision_conflict'} }, { details: {...details, conflicts: [{account_id: -1}]} }, { details: {...details, conflicts: []} }]) {
    assert.equal(intelligenceTargetConflict(error), null)
  }
})
