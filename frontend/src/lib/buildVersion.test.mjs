import assert from 'node:assert/strict'
import test from 'node:test'
import { buildVersionLabel } from './buildVersion.ts'

test('local version uses its build date and preserves normal releases', () => {
  assert.equal(buildVersionLabel('local-20260906-0310-fb4426b2'), '本地版本 2026-09-06 03:10')
  assert.equal(buildVersionLabel('v2.9.0'), 'v2.9.0')
  assert.equal(buildVersionLabel('dev'), 'dev')
})
