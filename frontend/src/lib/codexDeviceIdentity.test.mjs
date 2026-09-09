import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

test('account details display and copy the retained device ID independently of convergence mode', () => {
  const sheet = readFileSync(new URL('../components/AccountDetailSheet.tsx', import.meta.url), 'utf8')
  assert.match(sheet, /account\.codex_installation_id && \(/)
  assert.match(sheet, /<CopyValueButton\s+value=\{account\.codex_installation_id\}/)
  for (const locale of ['zh', 'zh-TW', 'en']) {
    const messages = JSON.parse(readFileSync(new URL(`../locales/${locale}.json`, import.meta.url), 'utf8'))
    assert.ok(messages.accounts.codexInstallationIdLabel)
    assert.ok(messages.accounts.codexInstallationIdHint)
  }
})
