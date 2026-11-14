import assert from 'node:assert/strict'
import test from 'node:test'
import { KEY_USAGE_VIEWS, normalizeKeyUsageView } from './keyUsageViews.ts'

test('account usage is a distinct route from key limits and unknown views fall back', () => {
  for (const view of KEY_USAGE_VIEWS) assert.equal(normalizeKeyUsageView(view), view)
  assert.equal(normalizeKeyUsageView('accounts'), 'accounts')
  assert.equal(normalizeKeyUsageView('quota'), 'quota')
  assert.equal(normalizeKeyUsageView('unknown'), 'overview')
  assert.equal(normalizeKeyUsageView(), 'overview')
})
