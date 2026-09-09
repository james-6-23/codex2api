import assert from 'node:assert/strict'
import { test } from 'node:test'
import { diagnosticEntries, diagnosticRecord, diagnosticValueText, usageRequestTypeLabelKey } from './usageRequestDiagnostics.ts'

test('historical and unknown request types are not inferred', () => {
  assert.equal(usageRequestTypeLabelKey(), 'usage.diagnostics.types.not_recorded')
  assert.equal(usageRequestTypeLabelKey(''), 'usage.diagnostics.types.not_recorded')
  assert.equal(usageRequestTypeLabelKey('future'), 'usage.diagnostics.types.unknown')
  assert.equal(usageRequestTypeLabelKey('related_internal'), 'usage.diagnostics.types.related_internal')
})

test('diagnostic values distinguish missing from false and zero', () => {
  for (const value of [null, undefined, '', '0001-01-01T00:00:00Z', []]) assert.equal(diagnosticValueText(value), null)
  assert.equal(diagnosticValueText(false), 'false')
  assert.equal(diagnosticValueText(0), '0')
  assert.equal(diagnosticValueText(['concurrency', 'model']), 'concurrency, model')
})

test('diagnostic fields show missing source values and omit private state', () => {
  const entries = Object.fromEntries(diagnosticEntries({ thread_source: 'guardian_review', _internal: 'hidden', passive_authorized: false }, true))
  assert.equal(entries.thread_source, 'guardian_review')
  assert.equal(entries.request_kind, '')
  assert.equal(entries.passive_authorized, false)
  assert.equal('_internal' in entries, false)
  for (const value of [null, [], 'text', 17]) assert.deepEqual(diagnosticRecord(value), {})
})
