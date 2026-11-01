import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import test from 'node:test'

import { extractBalancedBody } from './sourceBoundary.mjs'

const settingsSource = readFileSync(new URL('../pages/Settings.tsx', import.meta.url), 'utf8')

test('full-form save strips output guard fields before updating settings', () => {
  const saveHandler = extractBalancedBody(
    settingsSource,
    'const handleSaveSettings = async () =>',
    'Settings save handler',
  )

  assert.equal(
    saveHandler.includes('omitOutputGuardFields(buildWritableSettingsPayload(normalized))'),
    true,
    'full-form save must not carry the stale guard snapshot loaded with the page',
  )
  assert.equal(saveHandler.includes('api.updateSettings(payload)'), true)
})

test('every guard change is persisted through the auto-save patch path', () => {
  assert.equal(
    settingsSource.includes('autoSaveSettingsPatch({ output_guard_enabled: checked })'),
    true,
  )
  assert.equal(
    settingsSource.includes('autoSaveSettingsPatch({ output_guard_max_tool_argument_deltas: val })'),
    true,
  )
})

test('blur commits of unit-displayed byte limits go through the rounding-safe helper', () => {
  for (const field of [
    'output_guard_max_tool_argument_bytes',
    'output_guard_max_channel_bytes',
    'output_guard_max_request_bytes',
    'models_list_read_max_bytes',
  ]) {
    assert.equal(
      settingsSource.includes(`commitByteLimitField('${field}',`),
      true,
      `${field} must not write the rounded display value back directly`,
    )
  }
})
