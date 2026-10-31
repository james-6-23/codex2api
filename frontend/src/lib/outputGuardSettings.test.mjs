import test from 'node:test'
import assert from 'node:assert/strict'

import {
  KIB,
  MIB,
  DEFAULT_OUTPUT_GUARD_SETTINGS,
  OUTPUT_GUARD_KEYS,
  isOutputGuardKey,
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
