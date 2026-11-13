import assert from 'node:assert/strict'
import test from 'node:test'
import { createUUID } from './uuid.ts'

test('UUID fallback supports non-secure HTTP contexts', () => {
  const value = createUUID({ getRandomValues: bytes => bytes.fill(0xff) })
  assert.match(value, /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/)
})
