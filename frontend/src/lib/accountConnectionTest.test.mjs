import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

const connectionModal = readFileSync(new URL('../components/TestConnectionModal.tsx', import.meta.url), 'utf8')

test('connection test exposes a selectable model and keeps account models on catalog fallback', () => {
  assert.match(connectionModal, /<Select[\s\S]*onValueChange=\{setSelectedModel\}/)
  assert.match(connectionModal, /\[\.\.\.accountModels, \.\.\.upstreamModels\]/)
  assert.match(connectionModal, /\(account\.models \?\? \[\]\)\.filter\(isConnectionTestModel\)/)
})
