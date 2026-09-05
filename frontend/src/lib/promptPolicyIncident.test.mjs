import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

const usageSource = readFileSync(new URL('../pages/Usage.tsx', import.meta.url), 'utf8')
const promptFilterSource = readFileSync(new URL('../pages/PromptFilter.tsx', import.meta.url), 'utf8')
const apiSource = readFileSync(new URL('../api.ts', import.meta.url), 'utf8')
const zh = JSON.parse(readFileSync(new URL('../locales/zh.json', import.meta.url), 'utf8'))

test('new CY usage rows resolve the immutable incident ID before legacy inference', () => {
  const exactLookup = usageSource.indexOf('if (log.prompt_policy_incident_id)')
  const exactRequest = usageSource.indexOf('api.getPromptPolicyIncident(log.prompt_policy_incident_id)')
  const legacyRequest = usageSource.indexOf('api.matchPromptFilterLog({')

  assert.ok(exactLookup >= 0, 'missing incident ID guard')
  assert.ok(exactRequest > exactLookup, 'incident detail must use the immutable ID')
  assert.ok(legacyRequest > exactRequest, 'timestamp inference must remain a legacy-only fallback')
  assert.ok(apiSource.includes('`/prompt-policy/incidents/${encodeURIComponent(incidentId)}`'))
  assert.ok(apiSource.includes('`/prompt-policy/incidents?${search.toString()}`'))
  assert.ok(apiSource.includes("deletePromptPolicyIncident"))
  assert.ok(apiSource.includes("method: 'DELETE'"))
})

test('CY detail keeps null distinct from a real zero score', () => {
  assert.ok(usageSource.includes("value === null || value === undefined ? t('usage.cyberPolicyUnscored') : String(value)"))
  assert.ok(promptFilterSource.includes('value === null || value === undefined ? unscored : String(value)'))
  assert.equal(String(0), '0')
  assert.equal(zh.usage.cyberPolicyUnscored, '未评分')
  assert.equal(zh.promptFilter.cyberUnscored, '未评分')
})

test('Chinese CY states and historical inference warnings are user-facing', () => {
  assert.deepEqual(zh.usage.cyberPolicyState, {
    completed: '已完成',
    not_run: '未运行',
    unavailable: '无法提取',
    legacy_unknown: '历史未知',
  })
  assert.deepEqual(zh.usage.cyberPolicyOutcome, {
    no_hit: '未命中',
    audit_hit: '审计命中',
    warn: '警告',
    block: '拦截',
  })
  assert.equal(zh.usage.cyberPolicyLegacyUnknown, '历史记录：本地判定不可还原')
  assert.match(zh.usage.cyberPolicyLegacyInferred, /时间窗口推断/)
  assert.ok(usageSource.includes("t('usage.cyberPolicyLegacyInferred')"))
  assert.ok(promptFilterSource.includes("t('promptFilter.cyberLegacyUnknown')"))
})

test('audit log retention controls are wired to the retention API', () => {
  assert.match(promptFilterSource, /api\.getPromptLogRetention\(\)/)
  assert.match(promptFilterSource, /api\.updatePromptLogRetention\(/)
  assert.match(promptFilterSource, /api\.runPromptLogRetention\(\)/)
  assert.match(promptFilterSource, /DraftNumberInput min=\{0\} max=\{365\}/)
  assert.match(apiSource, /\/prompt-filter\/retention/)
  assert.equal(typeof zh.promptFilter.retention.title, 'string')
  assert.equal(typeof zh.promptFilter.retention.lastRun, 'string')
})
