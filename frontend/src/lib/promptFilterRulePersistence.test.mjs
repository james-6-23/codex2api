import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import { extractBalancedBody } from './sourceBoundary.mjs'

const promptFilterSource = readFileSync(new URL('../pages/PromptFilter.tsx', import.meta.url), 'utf8')
const settingsSource = readFileSync(new URL('../pages/Settings.tsx', import.meta.url), 'utf8')
const apiSource = readFileSync(new URL('../api.ts', import.meta.url), 'utf8')
const typesSource = readFileSync(new URL('../types.ts', import.meta.url), 'utf8')

test('ordinary settings saves never submit a stale custom rule snapshot', () => {
  const promptFilterPayload = extractBalancedBody(
    promptFilterSource,
    'function promptFilterSavePayload',
    'Prompt Filter save payload builder',
  )
  assert.equal(promptFilterPayload.includes('delete payload.prompt_filter_custom_patterns'), true)

  const settingsSaveStart = settingsSource.indexOf('const handleSaveSettings = async () =>')
  const settingsSaveEnd = settingsSource.indexOf('\n  const handleTestImageStorage', settingsSaveStart)
  assert.ok(settingsSaveStart >= 0, 'missing Settings save handler')
  assert.ok(settingsSaveEnd > settingsSaveStart, 'missing Settings save handler boundary')
  const settingsSave = settingsSource.slice(settingsSaveStart, settingsSaveEnd)
  assert.equal(settingsSave.includes('delete payload.prompt_filter_custom_patterns'), true)
  assert.equal(settingsSave.includes('api.updateSettings(payload)'), true)
})

test('explicit custom rule saves carry the raw expected snapshot', () => {
  const rulesViewStart = promptFilterSource.indexOf('function RulesView(')
  const saveStart = promptFilterSource.indexOf('const saveCustomPatterns = async')
  const saveEnd = promptFilterSource.indexOf('\n  const startCreateCustomRule', saveStart)
  assert.ok(rulesViewStart >= 0, 'missing rules view')
  assert.ok(saveStart >= 0, 'missing custom rule save handler')
  assert.ok(saveEnd > saveStart, 'missing custom rule save handler boundary')
  const rulesViewBaseline = promptFilterSource.slice(rulesViewStart, saveStart)
  const saveCustomPatterns = promptFilterSource.slice(saveStart, saveEnd)
  assert.equal(rulesViewBaseline.includes('rules?.custom_patterns'), false)
  assert.equal(
    rulesViewBaseline.includes('parseJSONList<PromptFilterRule>(form.prompt_filter_custom_patterns)'),
    true,
  )
  assert.equal(saveCustomPatterns.includes('prompt_filter_custom_patterns: JSON.stringify(next)'), true)
  assert.equal(
    saveCustomPatterns.includes("prompt_filter_custom_patterns_expected: form.prompt_filter_custom_patterns || '[]'"),
    true,
  )
  assert.equal(typesSource.includes('prompt_filter_custom_patterns_expected?: string'), true)
})

test('rule save conflicts refresh settings and rules without leaking a rejected promise', () => {
  const saveStart = promptFilterSource.indexOf('const savePartialAndReload = async')
  const saveEnd = promptFilterSource.indexOf('\n  const toggleBuiltin', saveStart)
  assert.ok(saveStart >= 0, 'missing partial rule save handler')
  assert.ok(saveEnd > saveStart, 'missing partial rule save handler boundary')
  const saveHandler = promptFilterSource.slice(saveStart, saveEnd)
  assert.equal(saveHandler.includes('catch (error)'), true)
  assert.equal(saveHandler.includes('error instanceof AdminAPIError && error.status === 409'), true)
  assert.equal(saveHandler.includes('api.getSettings()'), true)
  assert.equal(saveHandler.includes('api.getPromptFilterRules()'), true)
  assert.equal(saveHandler.includes("showToast(t('promptFilter.ruleSaveConflict'), 'warning')"), true)
  assert.equal(apiSource.includes('export class AdminAPIError extends Error'), true)
  assert.equal(apiSource.includes('throw new AdminAPIError(res.status,'), true)
})

test('an edit retained across a conflict re-finds the exact original rule by stable fingerprint', () => {
  const rulesViewStart = promptFilterSource.indexOf('function RulesView(')
  const rulesViewEnd = promptFilterSource.indexOf('\nfunction RulePatternTester', rulesViewStart)
  assert.ok(rulesViewStart >= 0 && rulesViewEnd > rulesViewStart, 'missing rules view boundary')
  const rulesView = promptFilterSource.slice(rulesViewStart, rulesViewEnd)
  assert.equal(rulesView.includes('editingCustomOriginalFingerprint'), true)
  assert.equal(rulesView.includes('setEditingCustomOriginalFingerprint(customRuleIdentity(rule))'), true)
  assert.equal(rulesView.includes('customPatterns.findIndex((rule) => customRuleIdentity(rule) === editingCustomOriginalFingerprint)'), true)
  assert.equal(rulesView.includes("showToast(t('promptFilter.ruleEditTargetMissing'), 'warning')"), true)
  assert.equal(rulesView.includes('const [editingCustomIndex, setEditingCustomIndex]'), false)
})
