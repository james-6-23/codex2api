import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

import {
  CLAUDE_TIMEZONE_CUSTOM,
  claudeTimezoneLabel,
  findClaudeTimezoneOption,
} from './claudeAccountOptions.ts'

const apiSource = readFileSync(new URL('../api.ts', import.meta.url), 'utf8')

const claudeAccountsSource = readFileSync(
  new URL('../pages/ClaudeAccounts.tsx', import.meta.url),
  'utf8',
)
const settingsSource = readFileSync(
  new URL('../pages/Settings.tsx', import.meta.url),
  'utf8',
)
const detailSheetSource = readFileSync(
  new URL('../components/AccountDetailSheet.tsx', import.meta.url),
  'utf8',
)

test('Claude timezone options expose a readable UTC offset and IANA name', () => {
  const option = findClaudeTimezoneOption('Asia/Shanghai')
  assert.equal(option?.value, 'Asia/Shanghai')
  assert.match(option?.label ?? '', /UTC\+08:00/)
  assert.match(option?.label ?? '', /Asia\/Shanghai/)
})

test('unknown IANA zones use the custom editor instead of being silently changed', () => {
  assert.equal(findClaudeTimezoneOption('Pacific/Chatham'), undefined)
  assert.equal(claudeTimezoneLabel('Pacific/Chatham'), 'Pacific/Chatham')
  assert.equal(CLAUDE_TIMEZONE_CUSTOM, '__custom__')
})

test('Claude account forms use the shared readable timezone picker', () => {
  assert.equal(claudeAccountsSource.includes('CLAUDE_TIMEZONE_OPTIONS'), true)
  assert.equal(claudeAccountsSource.includes('CLAUDE_TIMEZONE_CUSTOM'), true)
  assert.equal(settingsSource.includes('CLAUDE_TIMEZONE_OPTIONS'), true)
  assert.equal(settingsSource.includes('claudeTimezoneLabel'), true)
})

test('Claude account management exposes the dedicated secret export and bundle import API', () => {
  assert.equal(apiSource.includes("/accounts/claude/export"), true)
  assert.equal(apiSource.includes('importClaudeCredentialBundle'), true)
  assert.equal(claudeAccountsSource.includes('exportClaudeAccounts'), true)
  assert.equal(claudeAccountsSource.includes('importClaudeCredentialBundle'), true)
})

test('shared account detail actions allow the Claude credential export action', () => {
  assert.equal(detailSheetSource.includes('showAuthJson = account && !isGrok && !isClaude'), false)
  assert.equal(detailSheetSource.includes('onGenerateAuthJson'), true)
})

test('Claude import keeps bundle metadata and asks before inheriting a file proxy', () => {
  assert.equal(claudeAccountsSource.includes('hasImportedProxy'), true)
  assert.equal(claudeAccountsSource.includes('importProxyConfirmTitle'), true)
  assert.equal(claudeAccountsSource.includes('Array.isArray(parsed)'), true)
  assert.equal(claudeAccountsSource.includes('selectedGroupRefs'), true)
})
