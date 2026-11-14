import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

test('account card labels do not replace quality test task state translations', () => {
  for (const locale of ['zh', 'en', 'zh-TW']) {
    const data = JSON.parse(readFileSync(new URL(`../locales/${locale}.json`, import.meta.url), 'utf8'))
    for (const state of ['idle', 'running', 'completed', 'cancelling', 'interrupted', 'stopped', 'streamInterrupted']) {
      assert.equal(typeof data.qualityTest.status[state], 'string', `${locale}: ${state}`)
    }
    assert.equal(typeof data.status.unavailable, 'string')
    assert.equal(typeof data.keyUsage.views.accounts, 'string')
  }
})
