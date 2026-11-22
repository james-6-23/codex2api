import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import {
  ANTIGRAVITY_DEFAULT_MODELS,
  isAntigravityDisabledModel,
  orderAntigravityTestModels,
  resolveAntigravityTestModelSelection,
} from './antigravityModels.ts'

test('frontend fallbacks match public backend IDs including Flash 3.8', () => {
  const source = readFileSync(new URL('../../../proxy/antigravity_models.go', import.meta.url), 'utf8')
  const catalog = source.split('var antigravityPublicModelCatalog =')[1].split('var antigravityLogicalCompatibilityCatalog')[0]
  // Claude and Argon require account discovery; neither belongs in the fallback.
  const ids = Array.from(catalog.matchAll(/id: "([^"]+)"/g), match => match[1])
    .filter(id => !id.startsWith('claude-') && !id.startsWith('gemini-4-argon-'))
  assert.deepEqual([...ANTIGRAVITY_DEFAULT_MODELS].sort(), ids.sort())
  assert.equal(ANTIGRAVITY_DEFAULT_MODELS[0], 'gemini-3.8-flash-low')
})

test('test model ordering prefers configured model, then newest flash low tier', () => {
  const catalog = ['gemini-3.5-flash-low', 'gemini-3.5-flash-high', 'gemini-3.8-flash-low', 'gemini-3.7-flash-low', 'claude-sonnet-4-6', 'imagen-4']
  assert.deepEqual(orderAntigravityTestModels(catalog, '')[0], 'gemini-3.8-flash-low')
  assert.deepEqual(orderAntigravityTestModels(catalog, 'claude-sonnet-4-6')[0], 'claude-sonnet-4-6')
  assert.deepEqual(orderAntigravityTestModels(catalog, 'gemini-9-flash-low')[0], 'gemini-3.8-flash-low')
  assert.ok(!orderAntigravityTestModels(catalog, '').includes('imagen-4'))
  assert.deepEqual(orderAntigravityTestModels([], ''), [])
})

test('connection test choices match quota models instead of the broader catalog', () => {
  const available = ['claude-opus-4-6-thinking', 'gemini-3.8-flash-low', 'gemini-3.8-flash-high']
  const extra = ['gemini-2.5-pro', 'gemini-3-flash', 'gemini-3.1-flash-lite', 'gemini-3.5-flash-lite']
  const quota = { models: available.map(model_id => ({ model_id, remaining_fraction: 0 })) }
  assert.deepEqual(
    orderAntigravityTestModels([...available, ...extra], extra[0], quota),
    [available[1], available[0], available[2]],
  )
  assert.equal(orderAntigravityTestModels([], available[0], quota)[0], available[0])
  assert.deepEqual(orderAntigravityTestModels(extra, '', { models: [] }), [])
  assert.deepEqual(orderAntigravityTestModels(extra, '', { models: {} }), [])
})

test('legacy quota maps and model ID fields use quota IDs without display labels or static fallbacks', () => {
  assert.deepEqual(orderAntigravityTestModels(['extra'], '', {
    models: { 'claude-sonnet-4-6': { display_name: 'Sonnet' }, 'imagen-4': {} },
  }), ['claude-sonnet-4-6'])
  assert.deepEqual(orderAntigravityTestModels(['extra'], '', {
    models: [{ model: ' claude-a ' }, { model_id: 'claude-a' }, { modelId: 'claude-b' }, { name: 'claude-c' }, { id: 'claude-d' }, { display_name: 'Missing ID' }],
  }), ['claude-a', 'claude-b', 'claude-c', 'claude-d'])
  assert.deepEqual(orderAntigravityTestModels(['imagen-4']), [])
  assert.deepEqual(orderAntigravityTestModels(['custom-model']), ['custom-model'])
})

test('disabled state follows an exact model ID segment and clears on a refreshed ID', () => {
  for (const model of ['gemini-4-argon-disabled', 'gemini-4-argon-disabled-high', ' CLAUDE_DISABLED_HIGH ']) {
    assert.equal(isAntigravityDisabledModel(model), true)
  }
  for (const model of ['gemini-4-argon-high', 'gemini-4-argon-nondisabled', 'disabledness', '']) {
    assert.equal(isAntigravityDisabledModel(model), false)
  }
})

test('test choices include current models without quota and exclude disabled placeholders', () => {
  const disabled = 'gemini-4-argon-disabled'
  const active = 'gemini-3.8-flash-low'
  const historical = 'gemini-2.5-pro'
  const current = 'gemini-4-current-high'
  const quota = {
    models: [{ model_id: active, remaining_fraction: 0.75 }],
    catalog_model_ids: [active, disabled, current],
  }
  assert.deepEqual(orderAntigravityTestModels([historical], disabled, quota), [active, current])
  assert.deepEqual(orderAntigravityTestModels([disabled, historical], '', { models: [] }), [])
  assert.deepEqual(orderAntigravityTestModels([disabled, historical], '', { models: {}, catalog_model_ids: [] }), [])
})

test('disabled models never appear or remain selected even when configured or the only choice', () => {
  const disabled = 'gemini-9-flash-disabled-low'
  const active = 'claude-sonnet-5-5-high'
  const ordered = orderAntigravityTestModels([disabled, active], disabled)
  assert.deepEqual(ordered, [active])
  assert.equal(resolveAntigravityTestModelSelection(ordered), active)
  assert.equal(resolveAntigravityTestModelSelection([disabled]), '')
  assert.equal(resolveAntigravityTestModelSelection(ordered, disabled), active)
  assert.equal(resolveAntigravityTestModelSelection([disabled], disabled), '')
  assert.deepEqual(orderAntigravityTestModels([disabled]), [])
  assert.deepEqual(orderAntigravityTestModels([], disabled, { models: { [disabled]: {} } }), [])
  assert.deepEqual(orderAntigravityTestModels([], disabled, { models: [{ model_id: disabled }], catalog_model_ids: [disabled] }), [])
})

test('new catalogs remove disabled test choices and allow the restored model normally', () => {
  const disabled = 'gemini-4-argon-disabled'
  const restored = 'gemini-4-argon-high'
  const quota = {
    models: [{ model_id: disabled }, { model_id: restored, remaining_fraction: 1 }],
    catalog_model_ids: [restored],
  }
  const ordered = orderAntigravityTestModels([disabled], '', quota)
  assert.deepEqual(ordered, [restored])
  assert.equal(resolveAntigravityTestModelSelection(ordered, disabled), restored)
})

test('catalog recovery enables testing immediately without waiting for a quota row', () => {
  const disabled = 'gemini-4-argon-disabled'
  const restored = 'gemini-4-argon-high'
  const ordered = orderAntigravityTestModels([disabled], '', {
    models: [],
    catalog_model_ids: [restored],
  })
  assert.deepEqual(ordered, [restored])
  assert.equal(resolveAntigravityTestModelSelection(ordered, disabled), restored)
  assert.equal(isAntigravityDisabledModel(ordered[0]), false)
})

test('authoritative catalogs remove stale quota choices including ordinary models', () => {
  assert.deepEqual(orderAntigravityTestModels(['stale-model'], '', {
    models: [{ model_id: 'stale-model' }],
    catalog_model_ids: ['current-model'],
  }), ['current-model'])
})
