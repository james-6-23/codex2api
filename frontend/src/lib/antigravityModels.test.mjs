import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import { ANTIGRAVITY_DEFAULT_MODELS, orderAntigravityTestModels } from './antigravityModels.ts'

test('frontend fallbacks match public backend IDs including Flash 3.8', () => {
  const source = readFileSync(new URL('../../../proxy/antigravity_models.go', import.meta.url), 'utf8')
  const catalog = source.split('var antigravityPublicModelCatalog =')[1].split('var antigravityLogicalCompatibilityCatalog')[0]
  const ids = Array.from(catalog.matchAll(/id: "([^"]+)"/g), match => match[1])
  assert.deepEqual([...ANTIGRAVITY_DEFAULT_MODELS].sort(), ids.sort())
  assert.equal(ANTIGRAVITY_DEFAULT_MODELS[0], 'gemini-3.8-flash-low')
})

test('test model ordering prefers configured model, then newest flash low tier', () => {
  const catalog = ['gemini-3.5-flash-low', 'gemini-3.5-flash-high', 'gemini-3.8-flash-low', 'gemini-3.7-flash-low', 'claude-sonnet-4-6', 'imagen-4']
  assert.deepEqual(orderAntigravityTestModels(catalog, '')[0], 'gemini-3.8-flash-low')
  assert.deepEqual(orderAntigravityTestModels(catalog, 'claude-sonnet-4-6')[0], 'claude-sonnet-4-6')
  assert.deepEqual(orderAntigravityTestModels(catalog, 'gemini-9-flash-low')[0], 'gemini-3.8-flash-low')
  assert.ok(!orderAntigravityTestModels(catalog, '').includes('imagen-4'))
  assert.equal(orderAntigravityTestModels([], '')[0], ANTIGRAVITY_DEFAULT_MODELS[0])
})
