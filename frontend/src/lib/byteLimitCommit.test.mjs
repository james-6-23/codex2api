import assert from 'node:assert/strict'
import test from 'node:test'

import { resolveByteLimitCommit } from './byteLimitCommit.ts'

const KIB = 1024
const MIB = 1024 * 1024

// 复刻 Settings 页 commitByteLimitField 的决策：只有 submit=true 才算真正写入。
function makeCommitHandler(persistedBytes, bytesPerUnit) {
  const writes = []
  const commit = (submittedUnits) => {
    const decision = resolveByteLimitCommit(persistedBytes, submittedUnits, bytesPerUnit)
    if (decision.submit) writes.push(decision.bytes)
  }
  return { commit, writes }
}

test('unedited blur keeps the persisted non-multiple byte value', () => {
  // 131073 字节在界面上显示为 128 KiB；blur 时把展示值写回会静默改成 131072。
  const decision = resolveByteLimitCommit(131073, 128, KIB)
  assert.deepEqual(decision, { submit: false, bytes: 131073 })
})

test('unedited blur never submits and edited blur submits exactly once', () => {
  const { commit, writes } = makeCommitHandler(131073, KIB)

  // 未编辑：展示值仍是 128 KiB，即使表单在 onValueChange 时已被改成 131072。
  commit(128)
  assert.deepEqual(writes, [], 'unedited blur must not persist a rounded value')

  // 用户实际改成 129 KiB：只提交一次，提交值是准确的 129 KiB。
  commit(129)
  assert.deepEqual(writes, [129 * KIB])
})

test('an exactly aligned byte value also skips the redundant write', () => {
  assert.deepEqual(resolveByteLimitCommit(131072, 128, KIB), {
    submit: false,
    bytes: 131072,
  })
})

test('MiB byte limits keep their raw persisted value on unedited blur', () => {
  const persisted = 5 * MIB + 1

  assert.deepEqual(resolveByteLimitCommit(persisted, 5, MIB), {
    submit: false,
    bytes: persisted,
  })
  assert.deepEqual(resolveByteLimitCommit(persisted, 6, MIB), {
    submit: true,
    bytes: 6 * MIB,
  })
})

test('no persisted baseline yet still submits the typed value', () => {
  assert.deepEqual(resolveByteLimitCommit(null, 128, KIB), {
    submit: true,
    bytes: 128 * KIB,
  })
  assert.deepEqual(resolveByteLimitCommit(undefined, 2, MIB), {
    submit: true,
    bytes: 2 * MIB,
  })
})
