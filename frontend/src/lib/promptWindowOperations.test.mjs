import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { test } from 'node:test'

const source = readFileSync(new URL('../pages/PromptFilter.tsx', import.meta.url), 'utf8')
const windows = source.slice(source.indexOf('function PromptRiskSessionWindows'), source.indexOf('function RiskProfilesTable'))

test('window controls confirm exact-window locks and keep expired-window unlocks accessible', () => {
  assert.match(windows, /disabled=\{busy\} onClick=\{\(\) => void \(manualLock \? onUnlock\(session\.session_hash\) : onLock\(session\)\)/)
  assert.match(windows, /inactiveLockedWindows\.map/)
  assert.match(windows, /onUnlock\(lock\.session_hash\)/)
  assert.match(source, /windowActionBusy \|\| !window\.confirm\(t\('promptFilter\.risk\.sessionLimit\.lockWindowConfirm'\)\)/)
  assert.match(source, /api\.lockPromptUserWindow\(item\.subject_key, session\.session_hash, session\.expires_at\)/)
  const api = readFileSync(new URL('../api.ts', import.meta.url), 'utf8')
  const methods = api.slice(api.indexOf('lockPromptUserWindow:'), api.indexOf('testPromptFilter:'))
  assert.match(methods, /encodeURIComponent\(subjectKey\)/)
  assert.match(methods, /encodeURIComponent\(sessionHash\)/)
  assert.match(methods, /window_expires_at: windowExpiresAt/)
})

test('red 500 badge belongs to the bound account and only exposes the latest timestamp', () => {
  const account = windows.split('\n').find((line) => line.includes("label={t('promptFilter.risk.sessionLimit.account')}"))
  assert.match(account, /session\.last_500_at \? <Badge variant="destructive"/)
  assert.match(account, /<AlertTriangle[^>]*\/>500<\/Badge>/)
  assert.match(account, /formatBeijingTime\(session\.last_500_at\)/)
  assert.match(account, /aria-label=/)
  for (const locale of ['zh', 'zh-TW', 'en']) {
    const messages = JSON.parse(readFileSync(new URL(`../locales/${locale}.json`, import.meta.url), 'utf8')).promptFilter.risk.sessionLimit
    for (const key of ['lockWindow', 'unlockWindow', 'manuallyLocked', 'manualLockUntil', 'lockWindowConfirm', 'unlockWindowConfirm', 'windowLocked', 'windowUnlocked', 'otherLockedWindows', 'account500Hint']) {
      assert.ok(messages[key], `${locale}.${key}`)
    }
  }
})
