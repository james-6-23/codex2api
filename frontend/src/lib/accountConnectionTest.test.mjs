import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

const accountsPage = readFileSync(new URL('../pages/Accounts.tsx', import.meta.url), 'utf8')

test('connection test exposes a selectable model and keeps account models on catalog fallback', () => {
  assert.match(accountsPage, /<Select[\s\S]*onValueChange=\{setSelectedModel\}/)
  assert.match(accountsPage, /\[\.\.\.accountModels, \.\.\.upstreamModels\]/)
  assert.match(accountsPage, /\(account\.models \?\? \[\]\)\.filter\(isConnectionTestModel\)/)
})
