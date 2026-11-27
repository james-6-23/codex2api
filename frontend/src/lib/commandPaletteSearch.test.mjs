import test from 'node:test'
import assert from 'node:assert/strict'
import { filterCommandItems } from './commandPaletteSearch.ts'

const SAMPLE_ITEMS = [
  {
    id: 'nav-dashboard',
    label: '仪表盘',
    group: '页面导航',
    keywords: ['dashboard', 'overview'],
  },
  {
    id: 'channel-opencode',
    label: 'OpenCode',
    description: 'OpenCode Go / Zen 账号池',
    group: '账号渠道直达',
    keywords: ['opencode', 'sub2api'],
  },
  {
    id: 'action-studio',
    label: '切换为 Beautiful Studio 主题',
    group: '快捷操作',
    keywords: ['studio', 'theme', '美化'],
  },
]

test('empty query returns all command items', () => {
  const result = filterCommandItems(SAMPLE_ITEMS, '   ')
  assert.equal(result.length, SAMPLE_ITEMS.length)
})

test('multi-token query matches across label, description, group, and keywords case-insensitively', () => {
  const byMixedTokens = filterCommandItems(SAMPLE_ITEMS, 'opencode sub2api')
  assert.deepEqual(byMixedTokens.map((i) => i.id), ['channel-opencode'])

  const byChineseAndEnglish = filterCommandItems(SAMPLE_ITEMS, 'studio 主题')
  assert.deepEqual(byChineseAndEnglish.map((i) => i.id), ['action-studio'])
})

test('unmatched query returns an empty list', () => {
  const result = filterCommandItems(SAMPLE_ITEMS, 'nonexistent-route-xyz')
  assert.deepEqual(result, [])
})
