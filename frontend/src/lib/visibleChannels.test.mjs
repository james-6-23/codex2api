import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { normalizeVisibleChannels, toggleVisibleChannel } from './visibleChannels.ts'

test('normalizeVisibleChannels mirrors the backend rules', () => {
  assert.deepEqual(normalizeVisibleChannels(null), ['codex', 'claude', 'antigravity', 'grok'])
  assert.deepEqual(normalizeVisibleChannels(undefined), ['codex', 'claude', 'antigravity', 'grok'])
  assert.deepEqual(normalizeVisibleChannels([]), ['codex'])
  assert.deepEqual(normalizeVisibleChannels(['grok']), ['codex', 'grok'])
  assert.deepEqual(normalizeVisibleChannels([' Grok ', '', 'claude', 'grok', 'openai', 42]), ['codex', 'claude', 'grok'])
  assert.deepEqual(normalizeVisibleChannels('codex'), ['codex', 'claude', 'antigravity', 'grok'])
})

test('toggleVisibleChannel never drops the fallback channel', () => {
  assert.deepEqual(toggleVisibleChannel(['codex', 'claude'], 'claude'), ['codex'])
  assert.deepEqual(toggleVisibleChannel(['codex'], 'grok'), ['codex', 'grok'])
  assert.deepEqual(toggleVisibleChannel(['codex', 'grok'], 'codex'), ['codex', 'grok'])
})

test('dashboard, accounts and the usage channel filter consume the visibility setting', () => {
  const read = (p) => readFileSync(new URL(p, import.meta.url), 'utf8')
  assert.match(read('../App.tsx'), /<VisibleChannelsProvider>/)
  assert.match(read('../pages/Dashboard.tsx'), /useVisibleChannels\(\)/)
  assert.match(read('../pages/Accounts.tsx'), /useVisibleChannels\(\)/)
  assert.match(read('../components/ChannelFilter.tsx'), /useVisibleChannels\(\)/)
  // 直接打开被隐藏渠道的账号路由要回落到 Codex，而不是渲染一个切换器里不存在的视图。
  assert.match(read('../pages/Accounts.tsx'), /isChannelVisible\(providerView\)/)
})
