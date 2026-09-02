import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import { extractBalancedBody } from './sourceBoundary.mjs'

const componentSource = readFileSync(new URL('../components/PromptFilterNewAPIBindings.tsx', import.meta.url), 'utf8')
const pageSource = readFileSync(new URL('../pages/PromptFilter.tsx', import.meta.url), 'utf8')
const apiSource = readFileSync(new URL('../api.ts', import.meta.url), 'utf8')
const typesSource = readFileSync(new URL('../types.ts', import.meta.url), 'utf8')

test('identity binding only narrows prompt scope without overriding GuardPipeline thresholds', () => {
  assert.equal(componentSource.includes('requireSignedIdentity: false'), true)
  assert.equal(componentSource.includes('全局阈值和审核档位仍由 GuardPipeline 管理'), true)
  for (const fragment of ['promptFilterScope', "'inherit'", "'local_only'", "'off'", '仅本地检测：跳过远程模型复核']) {
    assert.equal(componentSource.includes(fragment), true, `binding editor missing prompt scope control: ${fragment}`)
  }
  for (const fragment of ['policyMode', 'policyProfile', 'policy_mode', 'policy_profile', '策略模式']) {
    assert.equal(componentSource.includes(fragment), false, `binding editor still exposes policy control: ${fragment}`)
  }
  const bindingStart = typesSource.indexOf('export interface PromptFilterNewAPIBinding')
  const bindingEnd = typesSource.indexOf('export interface CreateAPIKeyRequest')
  assert.ok(bindingStart >= 0, 'missing PromptFilterNewAPIBinding interface')
  assert.ok(bindingEnd > bindingStart, 'missing CreateAPIKeyRequest boundary after PromptFilterNewAPIBinding')
  const bindingTypes = typesSource.slice(bindingStart, bindingEnd)
  assert.equal(bindingTypes.includes('prompt_filter_scope'), true)
  assert.equal(bindingTypes.includes('policy_mode'), false)
  assert.equal(bindingTypes.includes('policy_profile'), false)
})

test('platform binding list never reads plaintext secret from a persisted binding', () => {
  assert.equal(componentSource.includes('binding.secret_masked'), true)
  assert.equal(componentSource.includes('binding.secret}'), false)
  assert.equal(componentSource.includes('binding.secret ??'), false)
  assert.equal(componentSource.includes('getPromptFilterNewAPISecret'), false)
})

test('key binding UI documents one-to-one isolation and one-time secret reveal', () => {
  for (const fragment of ['一个 Key 只能绑定一个调用方', '未绑定 Key 不接受 NewAPI 签名身份', '最长 32 字符且全局唯一', 'maxLength={32}', '明文只在本次响应中展示', '确认关闭密钥窗口', 'min={60}', '旧密钥仍可用于恢复']) {
    assert.equal(componentSource.includes(fragment), true, `missing isolation warning: ${fragment}`)
  }
  for (const deploymentSpecificCopy of ['凡人', 'buycodekey', '例如 fanren']) {
    assert.equal(componentSource.includes(deploymentSpecificCopy), false, `deployment-specific copy leaked into generic UI: ${deploymentSpecificCopy}`)
  }
})

test('key bindings are embedded in an optional NewAPI adapter panel', () => {
  const panelIndex = pageSource.indexOf("t('promptFilter.newapiAdapterSummary')")
  const bindingIndex = pageSource.indexOf('<PromptFilterNewAPIBindings />')
  const expertIndex = pageSource.indexOf("t('promptFilter.expertSettingsSummary')")

  assert.ok(panelIndex >= 0, 'missing optional NewAPI adapter panel')
  assert.ok(bindingIndex > panelIndex, 'Key bindings must be rendered inside the NewAPI integration panel')
  assert.ok(expertIndex > bindingIndex, 'Key bindings must remain inside the NewAPI panel before expert settings')
  assert.equal(pageSource.match(/<PromptFilterNewAPIBindings \/>/g)?.length, 1)
  assert.equal(componentSource.includes('<Card'), false, 'embedded binding editor must not create a second top-level card')
  assert.equal(pageSource.includes('getPromptFilterNewAPISecret'), false)
  assert.equal(pageSource.includes("setBool('newapi', 'enabled'"), false)
  const recommendedPreset = extractBalancedBody(
    pageSource,
    'const applyRecommendedProtection',
    'applyRecommendedProtection body',
  )
  for (const fragment of [
    "recommendedStrength === 'penalty'",
    "t('promptFilter.penaltyRequiresNewAPI')",
    'prompt_filter_review_enabled: true',
  ]) {
    assert.equal(recommendedPreset.includes(fragment), false, `generic local preset still contains a deployment-specific dependency: ${fragment}`)
  }
})

test('enabling required signatures is protected by a 401 safety confirmation', () => {
  for (const fragment of [
    '!editing.require_signed_identity && editForm.requireSignedIdentity',
    '确认开启强制签名身份',
    '请求会立即返回 401',
    '不会记为 Prompt 违规，也不会触发违规处罚',
    'if (!approved) return',
  ]) {
    assert.equal(componentSource.includes(fragment), true, `missing required-signature safety guard: ${fragment}`)
  }
})

test('platform binding API client covers CRUD and secret rotation endpoints', () => {
  for (const fragment of [
    "'/prompt-filter/newapi-bindings'",
    '/secret/generate',
    "method: 'PATCH'",
    "method: 'DELETE'",
  ]) {
    assert.equal(apiSource.includes(fragment), true, `missing binding API operation: ${fragment}`)
  }
})
