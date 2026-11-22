import test from 'node:test'
import assert from 'node:assert/strict'
import { IMAGE_MODELS, imageQualityOptions, normalizeImageQualityForModel } from './imageStudioModels.ts'

test('2.5 snapshots and resolution aliases retain their quality when reused', () => {
  for (const model of ['gpt-image-2.5-flare', 'gpt-image-2.5-sunburst-4k', 'gpt-image-2.5-flare-2026-09-08-2k']) {
    assert.equal(normalizeImageQualityForModel('max', model), 'max')
    assert.ok(imageQualityOptions(model).some(option => option.value === 'xhigh'))
  }
  assert.ok(IMAGE_MODELS.some(option => option.value === 'gpt-image-2.5-sunburst-4k'))
})

test('switching back to an older or unknown model normalizes only the new qualities', () => {
  for (const model of ['gpt-image-2', 'gpt-image-2-4k', 'gpt-image-2.50-flare', 'gpt-image-2.5-unknown']) {
    assert.equal(normalizeImageQualityForModel('max', model), 'high')
    assert.equal(normalizeImageQualityForModel('xhigh', model), 'high')
    assert.equal(normalizeImageQualityForModel('auto', model), 'auto')
    assert.ok(!imageQualityOptions(model).some(option => option.value === 'max'))
  }
})
