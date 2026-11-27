import assert from 'node:assert/strict'
import { after, before, test } from 'node:test'
import { fileURLToPath } from 'node:url'
import { build } from 'vite'
import { chromium } from 'playwright'

const root = fileURLToPath(new URL('../', import.meta.url))
const hookPath = fileURLToPath(new URL('../src/hooks/useVersionCheck.ts', import.meta.url))
const CACHE_KEY = 'codex2api_latest_release'
const LEGACY_KEY = 'codex2api_latest_version'
let browser
let harnessScript

before(async () => {
  // Compile the real hook and React rather than mirroring their async behavior.
  const result = await build({
    configFile: false,
    root,
    logLevel: 'silent',
    define: { __APP_VERSION__: 'window.__frontendVersion', 'process.env.NODE_ENV': '"production"' },
    plugins: [{
      name: 'version-check-harness',
      enforce: 'pre',
      resolveId(id, importer) {
        if (id === 'virtual:version-check-harness') return '\0version-check-harness'
        if (id === '../api' && importer?.replaceAll('\\', '/') === hookPath.replaceAll('\\', '/')) return '\0version-check-api'
      },
      load(id) {
        if (id === '\0version-check-api') {
          return `export const api = {
            getHealth: options => window.__request('health', options),
            getSystemUpdate: () => window.__request('update'),
          }`
        }
        if (id !== '\0version-check-harness') return
        return `
          import { createElement } from 'react'
          import { createRoot } from 'react-dom/client'
          import { useVersionCheck } from ${JSON.stringify(hookPath)}
          let root
          function Harness({ triggerKey }) {
            const { refreshVersion, ...state } = useVersionCheck(triggerKey)
            window.__versionState = state
            window.__refresh = refreshVersion
            return createElement('output', null, JSON.stringify(state))
          }
          window.__render = triggerKey => root.render(createElement(Harness, { triggerKey }))
          window.__mount = triggerKey => {
            root = createRoot(document.getElementById('root'))
            window.__render(triggerKey)
          }
          window.__unmount = () => root.unmount()
          window.__mount('/dashboard')
        `
      },
    }],
    build: {
      write: false,
      emptyOutDir: false,
      minify: false,
      lib: { entry: 'virtual:version-check-harness', formats: ['iife'], name: 'VersionCheckHarness' },
      rollupOptions: { input: 'virtual:version-check-harness' },
    },
  })
  const bundle = Array.isArray(result) ? result[0] : result
  harnessScript = bundle.output.find((item) => item.type === 'chunk').code
  browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || undefined, args: ['--no-sandbox'] })
})

after(async () => { await browser?.close() })

async function fixture(t, frontend = 'v3.0.1', cached = null, legacy = null) {
  const page = await browser.newPage()
  page.setDefaultTimeout(5_000)
  t.after(async () => { await page.close() })
  await page.route('http://version-check.test/**', (route) => route.fulfill({
    contentType: 'text/html', body: '<!doctype html><div id="root"></div>',
  }))
  await page.goto('http://version-check.test/')
  await page.evaluate(({ frontend, cached, legacy, CACHE_KEY, LEGACY_KEY }) => {
    window.__frontendVersion = frontend
    window.__requests = []
    window.__request = (kind, options) => new Promise((resolve, reject) => window.__requests.push({ kind, options, resolve, reject }))
    window.__intervals = []
    const originalSetInterval = window.setInterval.bind(window)
    window.setInterval = (callback, delay) => {
      window.__intervals.push({ callback, delay })
      return originalSetInterval(callback, delay)
    }
    if (cached) localStorage.setItem(CACHE_KEY, JSON.stringify({ checkedAt: Date.now(), ...cached }))
    if (legacy) localStorage.setItem(LEGACY_KEY, JSON.stringify(legacy))
  }, { frontend, cached, legacy, CACHE_KEY, LEGACY_KEY })
  await page.addScriptTag({ content: harnessScript })
  await page.waitForFunction(() => Boolean(window.__versionState))
  return page
}

async function waitRequests(page, count) {
  await page.waitForFunction(count => window.__requests.length === count, count)
  return page.evaluate(() => window.__requests.map(({ kind }) => kind))
}

async function settle(page, index, response, failed = false) {
  await page.evaluate(({ index, response, failed }) => {
    const request = window.__requests[index]
    if (failed) request.reject(new Error('controlled request failure'))
    else request.resolve(response)
  }, { index, response, failed })
}

async function waitState(page, expected) {
  await page.waitForFunction(expected => Object.entries(expected).every(([key, value]) => window.__versionState[key] === value), expected)
  return page.evaluate(() => window.__versionState)
}

function health(buildVersion) {
  return { status: 'ok', available: 1, total: 1, ...(buildVersion === undefined ? {} : { build_version: buildVersion }) }
}

function update(latest = '3.0.5', overrides = {}) {
  return {
    current_version: '3.0.5', latest_version: latest, has_update: false, supported: true,
    runtime_os: 'linux', runtime_arch: 'amd64', mode: 'binary',
    release_url: `https://example.test/releases/v${latest}`, ...overrides,
  }
}

test('cached latest does not hide runtime identity or create an update after a Go-only rebuild', async t => {
  const page = await fixture(t, 'v3.0.1', { latest_version: '3.0.5' })
  assert.deepEqual(await waitRequests(page, 1), ['health'])
  assert.deepEqual(await page.evaluate(() => window.__requests[0].options), { timeoutMs: 5_000 })
  await settle(page, 0, health('3.0.5'))
  const state = await waitState(page, { currentVersion: 'v3.0.5', latestVersion: 'v3.0.5', hasUpdate: false, versionMismatch: true })
  assert.equal(state.frontendVersion, 'v3.0.1')
  assert.equal(state.updateInfo, null)
  assert.deepEqual(await page.evaluate(key => Object.keys(JSON.parse(localStorage.getItem(key))).sort(), CACHE_KEY), ['checkedAt', 'latest_version'])
})

test('cached latest still detects a real backend update and equivalent version prefixes preserve the label', async t => {
  const page = await fixture(t, 'v3.0.1', { latest_version: '3.0.5' })
  await waitRequests(page, 1)
  await settle(page, 0, health('3.0.4'))
  await waitState(page, { currentVersion: 'v3.0.4', hasUpdate: true, versionMismatch: true })
  await page.evaluate(() => { void window.__refresh() })
  await waitRequests(page, 2)
  await settle(page, 1, health('refs/tags/3.0.1'))
  await waitState(page, { currentVersion: 'v3.0.1', hasUpdate: true, versionMismatch: false })
})

test('health resolves while GitHub is pending and remote metadata excludes runtime identity', async t => {
  const page = await fixture(t)
  assert.deepEqual(await waitRequests(page, 2), ['health', 'update'])
  await settle(page, 0, health('3.0.5'))
  await waitState(page, { currentVersion: 'v3.0.5', latestVersion: null, hasUpdate: false, versionMismatch: true })
  await settle(page, 1, update('3.0.5', { supported: false, unsupported_reason: 'No release asset for ARMv6' }))
  const state = await waitState(page, { latestVersion: 'v3.0.5', hasUpdate: false })
  assert.equal(state.updateInfo.supported, false)
  assert.equal(state.updateInfo.unsupported_reason, 'No release asset for ARMv6')
  const cached = await page.evaluate(key => JSON.parse(localStorage.getItem(key)), CACHE_KEY)
  assert.equal(cached.latest_version, '3.0.5')
  assert.equal(cached.release_url, 'https://example.test/releases/v3.0.5')
  assert.equal('current_version' in cached, false)
  assert.equal('build_version' in cached, false)
  assert.equal('supported' in cached, false)
})

test('failed, legacy and malformed numeric health responses fall back without retaining an old runtime version', async t => {
  const page = await fixture(t, 'v3.0.1', { latest_version: '3.0.5' })
  await waitRequests(page, 1)
  await settle(page, 0, health('3.0.5'))
  await waitState(page, { currentVersion: 'v3.0.5', hasUpdate: false })
  for (const [index, version] of [undefined, '', '   ', '3.0.5oops', 'failed'].entries()) {
    await page.evaluate(() => { void window.__refresh() })
    await waitRequests(page, index + 2)
    await settle(page, index + 1, health(version), version === 'failed')
    await waitState(page, { currentVersion: 'v3.0.1', hasUpdate: true, versionMismatch: false })
  }
})

test('cached release waits for health and follows explicit custom backend identities', async t => {
  const page = await fixture(t, 'v3.0.1', { latest_version: '3.0.5', release_url: 'https://example.test/release' })
  await waitRequests(page, 1)
  await waitState(page, { latestVersion: 'v3.0.5', hasUpdate: false })
  for (const [index, version] of ['custom.abcdef1', 'local-20261002-1234-abc', 'main', 'dev', 'bad-version', '758d5a8d', '_nightly'].entries()) {
    if (index) {
      await page.evaluate(() => { void window.__refresh() })
      await waitRequests(page, index + 1)
    }
    await settle(page, index, health(version))
    await waitState(page, { currentVersion: version, hasUpdate: false, versionMismatch: false, isCustomBuild: true })
  }
  assert.equal(await page.evaluate(key => JSON.parse(localStorage.getItem(key)).release_url, CACHE_KEY), 'https://example.test/release')
})

test('dev skips update probes and local frontend builds do not claim an upstream update', async t => {
  const devPage = await fixture(t, 'dev', { latest_version: '3.0.5' })
  await waitState(devPage, { currentVersion: 'dev', latestVersion: null, hasUpdate: false, versionMismatch: false, isCustomBuild: true })
  assert.deepEqual(await waitRequests(devPage, 0), [])

  const localPage = await fixture(t, 'local-20261002-1234-abc')
  await waitRequests(localPage, 2)
  await settle(localPage, 0, health('3.0.5'))
  await settle(localPage, 1, update('3.0.5', { supported: false, current_version: 'dev', unsupported_reason: 'Development build' }))
  const state = await waitState(localPage, { currentVersion: 'local-20261002-1234-abc', latestVersion: 'v3.0.5', hasUpdate: false, versionMismatch: false, isCustomBuild: true })
  assert.equal(state.updateInfo.supported, false)
})

test('custom deployment builds ignore cached release ordering while keeping release metadata', async t => {
  const page = await fixture(t, 'custom.abcdef1', { latest_version: '3.1.6' })
  await waitRequests(page, 1)
  await settle(page, 0, health('custom.abcdef1'))
  await waitState(page, { currentVersion: 'custom.abcdef1', latestVersion: 'v3.1.6', hasUpdate: false, isCustomBuild: true })
  await page.evaluate(() => { void window.__refresh(true) })
  await waitRequests(page, 3)
  await settle(page, 1, health('custom.abcdef1'))
  await settle(page, 2, update('3.1.6', { current_version: 'custom.abcdef1', supported: false, has_update: true }))
  await page.waitForFunction(() => window.__versionState.updateInfo !== null)
  const state = await waitState(page, { latestVersion: 'v3.1.6', hasUpdate: false, isCustomBuild: true })
  assert.equal(state.updateInfo.release_url, 'https://example.test/releases/v3.1.6')
  assert.equal(state.updateInfo.supported, false)
})

test('a newer route check owns both runtime state and remote cache when older responses arrive last', async t => {
  const page = await fixture(t)
  await waitRequests(page, 2)
  await page.evaluate(() => window.__render('/settings'))
  assert.deepEqual(await waitRequests(page, 4), ['health', 'update', 'health', 'update'])
  await settle(page, 2, health('3.0.5'))
  await settle(page, 3, update('3.0.5'))
  await waitState(page, { currentVersion: 'v3.0.5', latestVersion: 'v3.0.5', hasUpdate: false })
  await settle(page, 0, health('3.0.4'))
  await settle(page, 1, update('3.0.6'))
  await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 0)))
  await waitState(page, { currentVersion: 'v3.0.5', latestVersion: 'v3.0.5', hasUpdate: false })
  assert.equal(await page.evaluate(key => JSON.parse(localStorage.getItem(key)).latest_version, CACHE_KEY), '3.0.5')
})

test('responses from an unmounted instance cannot change a later mount or its cache', async t => {
  const page = await fixture(t)
  await waitRequests(page, 2)
  await page.evaluate(() => { window.__unmount(); window.__mount('/settings') })
  await waitRequests(page, 4)
  await settle(page, 2, health('3.0.5'))
  await settle(page, 3, update('3.0.5'))
  await waitState(page, { currentVersion: 'v3.0.5', latestVersion: 'v3.0.5', hasUpdate: false })
  await settle(page, 0, health('3.0.4'))
  await settle(page, 1, update('9.9.9'))
  await page.evaluate(() => new Promise(resolve => setTimeout(resolve, 0)))
  await waitState(page, { currentVersion: 'v3.0.5', latestVersion: 'v3.0.5', hasUpdate: false })
  assert.equal(await page.evaluate(key => JSON.parse(localStorage.getItem(key)).latest_version, CACHE_KEY), '3.0.5')
})

test('expired release cache survives network failure, removes legacy cache and still uses fresh runtime identity', async t => {
  const page = await fixture(t, 'v3.0.1', { latest_version: '3.0.5', checkedAt: Date.now() - 11 * 60_000 }, { current_version: '3.0.1' })
  await waitRequests(page, 2)
  await settle(page, 0, health('3.0.5'))
  await settle(page, 1, null, true)
  await waitState(page, { currentVersion: 'v3.0.5', latestVersion: 'v3.0.5', hasUpdate: false })
  assert.equal(await page.evaluate(key => localStorage.getItem(key), LEGACY_KEY), null)
})

test('periodic checks retain their interval and remote cache while route refreshes bypass it', async t => {
  const page = await fixture(t, 'v3.0.1', { latest_version: '3.0.5' })
  await waitRequests(page, 1)
  await settle(page, 0, health('3.0.5'))
  await waitState(page, { currentVersion: 'v3.0.5', hasUpdate: false })
  assert.deepEqual(await page.evaluate(() => window.__intervals.map(({ delay }) => delay)), [30 * 60_000])
  await page.evaluate(() => window.__intervals[0].callback())
  assert.deepEqual(await waitRequests(page, 2), ['health', 'health'])
  await settle(page, 1, health('3.0.5'))
  await page.evaluate(() => window.__render('/settings'))
  assert.deepEqual(await waitRequests(page, 4), ['health', 'health', 'health', 'update'])
  await settle(page, 2, health('3.0.5'))
  await settle(page, 3, update('3.0.6'))
  await waitState(page, { currentVersion: 'v3.0.5', latestVersion: 'v3.0.6', hasUpdate: true })
})
