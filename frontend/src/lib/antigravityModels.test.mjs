import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'
import { ANTIGRAVITY_DEFAULT_MODELS } from './antigravityModels.ts'

test('frontend fallbacks match public backend IDs including Flash 3.8', () => {
  const source = readFileSync(new URL('../../../proxy/antigravity_models.go', import.meta.url), 'utf8')
  const catalog = source.split('var antigravityPublicModelCatalog =')[1].split('var antigravityLogicalCompatibilityCatalog')[0]
  const ids = Array.from(catalog.matchAll(/id: "([^"]+)"/g), match => match[1])
  assert.deepEqual([...ANTIGRAVITY_DEFAULT_MODELS].sort(), ids.sort())
  assert.equal(ANTIGRAVITY_DEFAULT_MODELS[0], 'gemini-3.8-flash-low')
})
