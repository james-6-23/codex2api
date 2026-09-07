import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { test } from 'node:test'
import { formatSessionUsageDuration } from './sessionUsageStats.ts'

test('window usage duration distinguishes missing data from zero and formats long periods', () => {
  for (const [seconds, expected] of [[undefined, '-'], [null, '-'], [NaN, '-'], [-1, '-'], [0, '0s'], [0.2, '<1s'], [65, '1m 5s'], [3661, '1h 1m 1s'], [90000, '25h']]) {
    assert.equal(formatSessionUsageDuration(seconds), expected)
  }
})

test('account table uses the server summary before account rows and user details use recorded periods', () => {
  const source = readFileSync(new URL('../pages/PromptFilter.tsx', import.meta.url), 'utf8')
  const table = source.slice(source.indexOf('function RiskProfilesTable'), source.indexOf('function PromptRiskProfileDetailButton'))
  assert.ok(table.indexOf('accountSummary.average_duration_seconds') < table.indexOf('profiles.map'))
  assert.match(source, /setAccountSummary\(result\.account_summary \?\? null\)/)
  assert.match(table, /profile\.session_average_duration_seconds/)
  assert.match(source, /detail\.session_usage\?\.average_duration_seconds/)
  for (const locale of ['zh', 'zh-TW', 'en']) {
    const messages = JSON.parse(readFileSync(new URL(`../locales/${locale}.json`, import.meta.url), 'utf8'))
    assert.ok(messages.promptFilter.risk.averageWindowDurationHint)
    assert.ok(messages.promptFilter.risk.accountAverageHint)
  }
})
