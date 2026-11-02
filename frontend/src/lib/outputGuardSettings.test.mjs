import test from 'node:test'
import assert from 'node:assert/strict'

import {
  KIB,
  MIB,
  DEFAULT_OUTPUT_GUARD_SETTINGS,
  OUTPUT_GUARD_KEYS,
  isOutputGuardKey,
  omitOutputGuardFields,
  preserveConfirmedGuardFields,
  preserveOutputGuardFields,
  validateOutputGuardHierarchy,
  validateOutputGuardRanges,
  bytesToKiB,
  kibToBytes,
  bytesToMiB,
  mibToBytes,
} from './outputGuardSettings.ts'

test('default output guard settings match Spec §5.1 / §7', () => {
  assert.equal(DEFAULT_OUTPUT_GUARD_SETTINGS.output_guard_enabled, false)
  assert.equal(DEFAULT_OUTPUT_GUARD_SETTINGS.output_guard_max_tool_argument_bytes, 256 * KIB)
  assert.equal(DEFAULT_OUTPUT_GUARD_SETTINGS.output_guard_max_tool_argument_deltas, 4096)
  assert.equal(DEFAULT_OUTPUT_GUARD_SETTINGS.output_guard_max_channel_bytes, 2 * MIB)
  assert.equal(DEFAULT_OUTPUT_GUARD_SETTINGS.output_guard_max_request_bytes, 4 * MIB)
})

test('OUTPUT_GUARD_KEYS covers exactly the 5 writable settings', () => {
  assert.deepEqual(Array.from(OUTPUT_GUARD_KEYS), [
    'output_guard_enabled',
    'output_guard_max_tool_argument_bytes',
    'output_guard_max_tool_argument_deltas',
    'output_guard_max_channel_bytes',
    'output_guard_max_request_bytes',
  ])
  assert.equal(isOutputGuardKey('output_guard_enabled'), true)
  assert.equal(isOutputGuardKey('output_guard_config_generation'), false)
  assert.equal(isOutputGuardKey('unknown_key'), false)
})

test('full-form saves omit every writable output guard field', () => {
  const settings = {
    site_name: 'CodexProxy',
    output_guard_enabled: true,
    output_guard_max_tool_argument_bytes: 256 * KIB,
    output_guard_max_tool_argument_deltas: 4096,
    output_guard_max_channel_bytes: 2 * MIB,
    output_guard_max_request_bytes: 4 * MIB,
    output_guard_config_generation: 17,
  }

  const payload = omitOutputGuardFields(settings)

  for (const key of OUTPUT_GUARD_KEYS) {
    assert.equal(Object.hasOwn(payload, key), false, `${key} must not be part of a full-form save`)
  }
  assert.deepEqual(payload, {
    site_name: 'CodexProxy',
    // 生成号由 buildWritableSettingsPayload 单独剔除，这里只验证不误删其他字段。
    output_guard_config_generation: 17,
  })
  assert.equal(settings.output_guard_enabled, true, 'the source settings object must not be mutated')
})

test('validateOutputGuardHierarchy validates tool <= channel <= request', () => {
  assert.equal(validateOutputGuardHierarchy({
    output_guard_max_tool_argument_bytes: 256 * KIB,
    output_guard_max_channel_bytes: 2 * MIB,
    output_guard_max_request_bytes: 4 * MIB,
  }), null)

  assert.equal(validateOutputGuardHierarchy({
    output_guard_max_tool_argument_bytes: 2 * MIB,
    output_guard_max_channel_bytes: 2 * MIB,
    output_guard_max_request_bytes: 2 * MIB,
  }), null)

  assert.ok(validateOutputGuardHierarchy({
    output_guard_max_tool_argument_bytes: 3 * MIB,
    output_guard_max_channel_bytes: 2 * MIB,
    output_guard_max_request_bytes: 4 * MIB,
  })?.includes('tool <= channel'))

  assert.ok(validateOutputGuardHierarchy({
    output_guard_max_tool_argument_bytes: 128 * KIB,
    output_guard_max_channel_bytes: 8 * MIB,
    output_guard_max_request_bytes: 4 * MIB,
  })?.includes('channel <= request'))
})

test('validateOutputGuardRanges validates individual ranges', () => {
  assert.equal(validateOutputGuardRanges({
    output_guard_max_tool_argument_bytes: 256 * KIB,
    output_guard_max_tool_argument_deltas: 4096,
    output_guard_max_channel_bytes: 2 * MIB,
    output_guard_max_request_bytes: 4 * MIB,
  }), null)

  assert.ok(validateOutputGuardRanges({
    output_guard_max_tool_argument_bytes: 10 * KIB,
  })?.includes('工具参数字节范围'))

  assert.ok(validateOutputGuardRanges({
    output_guard_max_tool_argument_deltas: 70000,
  })?.includes('工具 Delta 次数'))

  assert.equal(validateOutputGuardRanges({
    output_guard_max_channel_bytes: 128 * KIB,
  }), null)
  assert.ok(validateOutputGuardRanges({
    output_guard_max_channel_bytes: 64 * KIB,
  })?.includes('单通道字节范围'))
})

test('unit conversions work accurately', () => {
  assert.equal(bytesToKiB(128 * 1024), 128)
  assert.equal(kibToBytes(128), 128 * 1024)
  assert.equal(bytesToKiB(262144), 256)
  assert.equal(kibToBytes(256), 262144)
  assert.equal(bytesToKiB(4194304), 4096)
  assert.equal(kibToBytes(4096), 4194304)
  assert.equal(bytesToMiB(2 * 1024 * 1024), 2)
  assert.equal(mibToBytes(2), 2 * 1024 * 1024)
})

test('preserveOutputGuardFields keeps current guard fields and config generation when server generation is not newer', () => {
  const updatedFromServer = {
    site_name: 'NewServerSiteName',
    output_guard_enabled: false,
    output_guard_max_tool_argument_bytes: 256 * KIB,
    output_guard_max_tool_argument_deltas: 4096,
    output_guard_max_channel_bytes: 2 * MIB,
    output_guard_max_request_bytes: 4 * MIB,
    output_guard_config_generation: 1,
  }

  const currentLocalState = {
    site_name: 'OldSiteName',
    output_guard_enabled: true,
    output_guard_max_tool_argument_bytes: 512 * KIB,
    output_guard_max_tool_argument_deltas: 8192,
    output_guard_max_channel_bytes: 4 * MIB,
    output_guard_max_request_bytes: 8 * MIB,
    output_guard_config_generation: 5,
  }

  const result = preserveOutputGuardFields(updatedFromServer, currentLocalState)

  assert.equal(result.site_name, 'NewServerSiteName', 'non-guard fields must take the server updated values')
  assert.equal(result.output_guard_enabled, true)
  assert.equal(result.output_guard_max_tool_argument_bytes, 512 * KIB)
  assert.equal(result.output_guard_max_tool_argument_deltas, 8192)
  assert.equal(result.output_guard_max_channel_bytes, 4 * MIB)
  assert.equal(result.output_guard_max_request_bytes, 8 * MIB)
  assert.equal(result.output_guard_config_generation, 5)

  // Ensure immutability
  assert.equal(updatedFromServer.site_name, 'NewServerSiteName')
  assert.equal(updatedFromServer.output_guard_enabled, false)
})

test('preserveOutputGuardFields adopts server guard fields when server generation is newer', () => {
  const updatedFromServer = {
    site_name: 'NewServerSiteName',
    output_guard_enabled: true,
    output_guard_max_tool_argument_bytes: 1024 * KIB,
    output_guard_max_tool_argument_deltas: 16384,
    output_guard_max_channel_bytes: 8 * MIB,
    output_guard_max_request_bytes: 16 * MIB,
    output_guard_config_generation: 10,
  }

  const currentLocalState = {
    site_name: 'OldSiteName',
    output_guard_enabled: false,
    output_guard_max_tool_argument_bytes: 256 * KIB,
    output_guard_max_tool_argument_deltas: 4096,
    output_guard_max_channel_bytes: 2 * MIB,
    output_guard_max_request_bytes: 4 * MIB,
    output_guard_config_generation: 5,
  }

  const result = preserveOutputGuardFields(updatedFromServer, currentLocalState)

  assert.equal(result.site_name, 'NewServerSiteName')
  assert.equal(result.output_guard_enabled, true)
  assert.equal(result.output_guard_max_tool_argument_bytes, 1024 * KIB)
  assert.equal(result.output_guard_max_tool_argument_deltas, 16384)
  assert.equal(result.output_guard_max_channel_bytes, 8 * MIB)
  assert.equal(result.output_guard_max_request_bytes, 16 * MIB)
  assert.equal(result.output_guard_config_generation, 10)
})

test('preserveConfirmedGuardFields copies only non-guard fields and keeps confirmed guard fields when server generation is not newer', () => {
  const currentForm = {
    site_name: 'UpdatedSiteName',
    output_guard_enabled: true, // in-flight draft
    output_guard_max_tool_argument_bytes: 512 * KIB,
    output_guard_max_tool_argument_deltas: 8192,
    output_guard_max_channel_bytes: 4 * MIB,
    output_guard_max_request_bytes: 8 * MIB,
    output_guard_config_generation: 5,
  }

  const confirmedSnapshot = {
    site_name: 'OldSiteName',
    output_guard_enabled: false,
    output_guard_max_tool_argument_bytes: 256 * KIB,
    output_guard_max_tool_argument_deltas: 4096,
    output_guard_max_channel_bytes: 2 * MIB,
    output_guard_max_request_bytes: 4 * MIB,
    output_guard_config_generation: 4,
  }

  const result = preserveConfirmedGuardFields(currentForm, confirmedSnapshot)

  // Non-guard field updated from currentForm
  assert.equal(result.site_name, 'UpdatedSiteName')
  // Guard fields kept from confirmedSnapshot
  assert.equal(result.output_guard_enabled, false)
  assert.equal(result.output_guard_max_tool_argument_bytes, 256 * KIB)
  assert.equal(result.output_guard_max_tool_argument_deltas, 4096)
  assert.equal(result.output_guard_max_channel_bytes, 2 * MIB)
  assert.equal(result.output_guard_max_request_bytes, 4 * MIB)
  assert.equal(result.output_guard_config_generation, 4)

  // When serverResponse generation is equal or lower, confirmed guard fields are kept
  const serverOlderResponse = {
    site_name: 'UpdatedSiteName',
    output_guard_enabled: true,
    output_guard_max_tool_argument_bytes: 1024 * KIB,
    output_guard_max_tool_argument_deltas: 16384,
    output_guard_max_channel_bytes: 8 * MIB,
    output_guard_max_request_bytes: 16 * MIB,
    output_guard_config_generation: 4, // not newer than confirmedSnapshot (4)
  }
  const resultEqual = preserveConfirmedGuardFields(currentForm, confirmedSnapshot, serverOlderResponse)
  assert.equal(resultEqual.site_name, 'UpdatedSiteName')
  assert.equal(resultEqual.output_guard_enabled, false)
  assert.equal(resultEqual.output_guard_max_tool_argument_bytes, 256 * KIB)
  assert.equal(resultEqual.output_guard_max_tool_argument_deltas, 4096)
  assert.equal(resultEqual.output_guard_max_channel_bytes, 2 * MIB)
  assert.equal(resultEqual.output_guard_max_request_bytes, 4 * MIB)
  assert.equal(resultEqual.output_guard_config_generation, 4)

  // Handles null confirmed gracefully
  const nullConfirmedResult = preserveConfirmedGuardFields(currentForm, null)
  assert.deepEqual(nullConfirmedResult, currentForm)
})

test('preserveConfirmedGuardFields adopts server guard fields and generation when server generation is newer', () => {
  const currentForm = {
    site_name: 'FormSiteName',
    output_guard_enabled: false,
    output_guard_max_tool_argument_bytes: 256 * KIB,
    output_guard_max_tool_argument_deltas: 4096,
    output_guard_max_channel_bytes: 2 * MIB,
    output_guard_max_request_bytes: 4 * MIB,
    output_guard_config_generation: 4,
  }

  const confirmedSnapshot = {
    site_name: 'OldSiteName',
    output_guard_enabled: false,
    output_guard_max_tool_argument_bytes: 256 * KIB,
    output_guard_max_tool_argument_deltas: 4096,
    output_guard_max_channel_bytes: 2 * MIB,
    output_guard_max_request_bytes: 4 * MIB,
    output_guard_config_generation: 4,
  }

  const serverResponse = {
    site_name: 'ServerSiteName',
    output_guard_enabled: true,
    output_guard_max_tool_argument_bytes: 1024 * KIB,
    output_guard_max_tool_argument_deltas: 16384,
    output_guard_max_channel_bytes: 8 * MIB,
    output_guard_max_request_bytes: 16 * MIB,
    output_guard_config_generation: 8,
  }

  const result = preserveConfirmedGuardFields(currentForm, confirmedSnapshot, serverResponse)

  // Non-guard field updated from currentForm
  assert.equal(result.site_name, 'FormSiteName')
  // Guard fields adopted from serverResponse
  assert.equal(result.output_guard_enabled, true)
  assert.equal(result.output_guard_max_tool_argument_bytes, 1024 * KIB)
  assert.equal(result.output_guard_max_tool_argument_deltas, 16384)
  assert.equal(result.output_guard_max_channel_bytes, 8 * MIB)
  assert.equal(result.output_guard_max_request_bytes, 16 * MIB)
  assert.equal(result.output_guard_config_generation, 8)
})



