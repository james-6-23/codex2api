import assert from 'node:assert/strict'
import test from 'node:test'
import { hasRemoteUpdate, isComparableVersion, normalizeVersion, resolveBuildVersions } from './versionCheck.ts'

test('an old frontend uses the running release for cached latest-version comparison', () => {
  const current = resolveBuildVersions('v3.0.1', '3.0.5')
  assert.deepEqual(current, {
    frontendVersion: 'v3.0.1', currentVersion: 'v3.0.5', versionMismatch: true,
  })
  assert.equal(hasRemoteUpdate(current.currentVersion, 'v3.0.5'), false)
  assert.equal(hasRemoteUpdate(resolveBuildVersions('v3.0.1', '3.0.4').currentVersion, '3.0.5'), true)
})

test('matching release prefixes preserve the original frontend label', () => {
  for (const frontend of ['v3.0.5', '3.0.5', 'V3.0.5', 'refs/tags/v3.0.5']) {
    for (const backend of ['v3.0.5', '3.0.5', 'V3.0.5', ' refs/tags/v3.0.5 ']) {
      assert.equal(normalizeVersion(frontend), normalizeVersion(backend))
      assert.deepEqual(resolveBuildVersions(frontend, backend), {
        frontendVersion: frontend, currentVersion: frontend, versionMismatch: false,
      })
    }
  }
})

test('missing and malformed numeric backend identities keep the frontend behavior', () => {
  for (const backend of [undefined, null, '', '   ', '3.0', '3.0.5oops', '01.0.5', '3.0.5-01', '3.0.5+']) {
    const versions = resolveBuildVersions('v3.0.1', backend)
    assert.equal(versions.currentVersion, 'v3.0.1', String(backend))
    assert.equal(versions.versionMismatch, false, String(backend))
    assert.equal(hasRemoteUpdate(versions.currentVersion, '3.0.5'), true)
  }
})

test('explicit custom backend identities override an official frontend without claiming an update', () => {
  for (const backend of ['custom.abcdef1', 'local-20261002-1234-abc', 'main', 'dev', 'bad-version', '758d5a8d', '_nightly']) {
    const versions = resolveBuildVersions('v3.0.1', backend)
    assert.equal(versions.currentVersion, backend)
    assert.equal(versions.versionMismatch, false)
    assert.equal(isComparableVersion(versions.currentVersion), false)
    assert.equal(hasRemoteUpdate(versions.currentVersion, '3.0.5'), false)
  }
})

test('development and local frontend builds do not claim an upstream update', () => {
  const dev = resolveBuildVersions('dev', 'v3.0.5')
  assert.equal(dev.currentVersion, 'dev')
  assert.equal(dev.versionMismatch, false)
  assert.equal(hasRemoteUpdate(dev.currentVersion, '3.0.5'), false)

  const local = resolveBuildVersions('local-20261002-1234-abc', 'v3.0.5')
  assert.equal(local.currentVersion, 'local-20261002-1234-abc')
  assert.equal(local.versionMismatch, false)
  assert.equal(hasRemoteUpdate(local.currentVersion, '3.0.5'), false)
  assert.equal(resolveBuildVersions('branch-preview', 'v3.0.5').currentVersion, 'branch-preview')
})

test('release build metadata remains diagnostic while numeric update comparison stays compatible', () => {
  const current = resolveBuildVersions('v3.0.1', 'refs/tags/v3.0.5-rc.1+build.7')
  assert.equal(current.currentVersion, 'v3.0.5-rc.1+build.7')
  assert.equal(current.versionMismatch, true)
  assert.equal(hasRemoteUpdate(current.currentVersion, 'v3.0.5'), false)
  assert.equal(hasRemoteUpdate(current.currentVersion, 'v3.0.6'), true)
  assert.equal(hasRemoteUpdate('3.0', '3.0.1'), true)
  assert.equal(hasRemoteUpdate('v3.0.6', '3.0.5'), false)
  assert.equal(hasRemoteUpdate('v3.0.1', null), false)
})

test('custom build identities cannot be ordered against releases or cached build labels', () => {
  for (const custom of ['custom.abcdef1', 'local-20261002-1234-abc', 'main', 'dev', '', '3.0.5oops']) {
    assert.equal(isComparableVersion(custom), false, custom)
    assert.equal(hasRemoteUpdate(custom, 'v3.1.6'), false, custom)
    assert.equal(hasRemoteUpdate('v3.1.6', custom), false, custom)
    assert.equal(hasRemoteUpdate(custom, 'other-preview'), false, custom)
  }
  for (const release of ['v3.1.6', '3.1', 'v3.1.6+build.7']) {
    assert.equal(isComparableVersion(release), true, release)
  }
  assert.equal(hasRemoteUpdate('v3.1.5+build.7', 'v3.1.6'), true)
  assert.equal(hasRemoteUpdate('v3.1.6+build.7', 'v3.1.6'), false)
})
