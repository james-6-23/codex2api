import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

const payloadRulesSource = readFileSync(new URL('../pages/PayloadRules.tsx', import.meta.url), 'utf8')
const settingsSource = readFileSync(new URL('../pages/Settings.tsx', import.meta.url), 'utf8')

test('payload rule effort mapping offers the same levels as settings, including max', () => {
  const payloadMatch = payloadRulesSource.match(/const EFFORT_LEVELS = \[([^\]]+)\]/)
  const settingsMatch = settingsSource.match(/const REASONING_EFFORT_OPTIONS = \[([^\]]+)\]/)
  assert.ok(payloadMatch, 'missing EFFORT_LEVELS in PayloadRules.tsx')
  assert.ok(settingsMatch, 'missing REASONING_EFFORT_OPTIONS in Settings.tsx')

  const parseLevels = (raw) =>
    [...raw.matchAll(/'([^']+)'/g)].map((item) => item[1])

  const payloadLevels = parseLevels(payloadMatch[1])
  const settingsLevels = parseLevels(settingsMatch[1])
  assert.deepEqual(payloadLevels, settingsLevels)
  assert.equal(payloadLevels.includes('max'), true)
  assert.equal(payloadRulesSource.includes("form.template === 'effortMap'"), true)
  assert.equal(payloadRulesSource.includes('options={EFFORT_LEVELS}'), true)
})
