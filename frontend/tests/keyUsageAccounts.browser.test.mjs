import assert from 'node:assert/strict'
import { after, before, test } from 'node:test'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { build } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { mkdir, mkdtemp } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import { chromium } from 'playwright'

const root = fileURLToPath(new URL('../', import.meta.url))
let browser, script, css, artifactDir

before(async () => {
  artifactDir = process.env.KEY_USAGE_TEST_ARTIFACT_DIR || await mkdtemp(path.join(tmpdir(), 'codex2api-card-review-'))
  await mkdir(artifactDir, { recursive: true })
  const result = await build({ configFile: false, root, logLevel: 'silent', plugins: [react(), tailwindcss(), {
    name: 'portal-harness', enforce: 'pre',
    resolveId(id) { if (id === 'virtual:portal-harness') return '\0portal-harness' },
    load(id) {
      if (id !== '\0portal-harness') return
      return `import React from 'react'; import {createRoot} from 'react-dom/client';
        import Accounts from ${JSON.stringify(path.join(root, 'src/components/KeyUsageAccounts.tsx'))};
        import Permissions from ${JSON.stringify(path.join(root, 'src/components/KeyUsageAccountPermissions.tsx'))};
        import APIKeys from ${JSON.stringify(path.join(root, 'src/pages/APIKeys.tsx'))};
        import {ToastProvider} from ${JSON.stringify(path.join(root, 'src/components/ToastProvider.tsx'))};
        import {MemoryRouter} from 'react-router-dom';
        import ${JSON.stringify(path.join(root, 'src/i18n.ts'))}; import ${JSON.stringify(path.join(root, 'src/index.css'))};
        createRoot(document.getElementById('root')).render(window.__keyEditor
          ? React.createElement(MemoryRouter,null,React.createElement(ToastProvider,null,React.createElement(APIKeys)))
          : React.createElement(React.Fragment,null,
          React.createElement(Permissions), React.createElement(Accounts,{apiKey:'fixture-key'})));`
    },
  }], resolve: { alias: { '@': path.join(root, 'src') } }, define: { 'process.env.NODE_ENV': '"production"' }, build: { write: false, minify: false, rollupOptions: { input: 'virtual:portal-harness' }, lib: { entry: 'virtual:portal-harness', formats: ['iife'], name: 'PortalHarness' } } })
  const bundle = Array.isArray(result) ? result[0] : result
  script = bundle.output.find(item => item.type === 'chunk').code
  css = bundle.output.filter(item => item.type === 'asset' && item.fileName.endsWith('.css')).map(item => item.source).join('\n')
  browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || undefined, args: ['--no-sandbox'] })
})
after(async () => { await browser?.close() })

test('key permissions retain an in-flight save across edit tabs', async t => {
  const page = await browser.newPage()
  page.setDefaultTimeout(5000)
  const errors = []
  page.on('pageerror', error => errors.push(error.message))
  t.after(() => assert.deepEqual(errors, []))
  t.after(() => page.close())
  await page.route('http://portal.test/**', route => route.fulfill({ contentType: 'text/html', body: '<div id="root"></div>' }))
  await page.goto('http://portal.test/')
  await page.evaluate(() => {
    localStorage.setItem('lang', 'zh')
    window.__keyEditor = true
    window.__policyReads = 0
    window.__policyWrites = []
    let policy = { view_enabled: true, reset_enabled: true }
    const pendingSave = new Promise(resolve => { window.__completeSave = resolve })
    window.fetch = async (url, options = {}) => {
      const path = String(url)
      let body = {}
      if (path === '/api/admin/keys') body = { keys: [{
        id: 10, name: 'fixture-key', key: 'sk-fixture', status: 'active',
        quota_limit: 0, quota_used: 0, total_used: 0, allowed_group_ids: [], limits: {},
        created_at: '2026-01-01T00:00:00Z',
      }] }
      else if (path === '/api/admin/account-groups') body = { groups: [] }
      else if (path === '/api/admin/models') body = { models: ['gpt-5.5'] }
      else if (path === '/api/admin/settings') body = { public_key_usage_page_enabled: true }
      else if (path === '/api/admin/prompt-filter/newapi-bindings') body = { bindings: [] }
      else if (path === '/api/admin/keys-concurrency') body = { concurrency: {} }
      else if (path === '/api/admin/keys/10/model-request-usage') body = { model_request_usage: [] }
      else if (path === '/api/admin/key-usage-account-permissions/10') {
        if (options.method === 'PUT') {
          const next = JSON.parse(options.body)
          window.__policyWrites.push(next)
          if (window.__policyWrites.length === 1) await pendingSave
          policy = next
        } else window.__policyReads += 1
        body = policy
      } else throw new Error(`Unexpected request: ${options.method || 'GET'} ${path}`)
      return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } })
    }
  })
  await page.addStyleTag({ content: css })
  await page.addScriptTag({ content: script })
  await page.getByRole('button', { name: '编辑', exact: true }).click()
  const dialog = page.getByRole('dialog')
  const view = dialog.getByRole('switch', { name: '继承全局账号查看权限', exact: true })
  const reset = dialog.getByRole('switch', { name: '继承全局重置卡权限', exact: true })
  await view.click()
  await page.waitForFunction(() => window.__policyWrites.length === 1)
  await dialog.getByRole('tab', { name: '高级限额', exact: true }).click()
  assert.equal(await view.isVisible(), false)
  await dialog.getByRole('tab', { name: '基本设置', exact: true }).click()
  await view.waitFor()
  assert.equal(await view.isDisabled(), true, 'tab changes must preserve the pending save')
  assert.equal(await reset.isDisabled(), true)
  assert.equal(await page.evaluate(() => window.__policyReads), 1, 'tab changes must not reload stale permissions')
  await page.evaluate(() => window.__completeSave())
  await dialog.getByText('已保存', { exact: true }).waitFor()
  assert.equal(await view.isChecked(), false)
  await reset.click()
  await page.waitForFunction(() => window.__policyWrites.length === 2)
  assert.deepEqual(await page.evaluate(() => window.__policyWrites), [
    { view_enabled: false, reset_enabled: true },
    { view_enabled: false, reset_enabled: false },
  ])
})

test('portal filters groups, confirms redemption and uses a UUID on HTTP', async t => {
  const page = await browser.newPage()
  page.setDefaultTimeout(5000)
  const errors = []
  page.on('pageerror', error => errors.push(error.message))
  t.after(() => assert.deepEqual(errors, []))
  t.after(() => page.close())
  await page.route('http://portal.test/**', route => route.fulfill({ contentType: 'text/html', body: '<div id="root"></div>' }))
  await page.goto('http://portal.test/')
  await page.evaluate(() => {
    localStorage.setItem('lang', 'zh')
    Object.defineProperty(crypto, 'randomUUID', { value: undefined, configurable: true })
    window.__calls = []
    window.fetch = async (url, options = {}) => {
      const path = String(url)
      const headers = new Headers(options.headers)
      window.__calls.push({ path, method: options.method || 'GET', auth: headers.get('Authorization'), idempotency: headers.get('Idempotency-Key'), body: options.body })
      let body = { view_enabled: true, reset_enabled: true }
      if (path === '/api/key-usage/accounts') body = { accounts: [
        { id: 1, name: '账号甲', email: 'a@example.test', plan_type: 'plus', status: 'active', group_ids: [1], usage_percent_5h: 25, usage_percent_7d: 10, long_window_seconds: 2592000, reset_credits: 2, reset_allowed: true },
        { id: 2, name: '账号乙', email: '', plan_type: 'plus', status: 'active', group_ids: [2], usage_percent_5h: 60, usage_percent_7d: null, reset_credits: 0, reset_allowed: true },
        { id: 3, name: '账号丙', email: '', plan_type: 'plus', status: 'active', group_ids: [1], usage_percent_5h: null, usage_percent_7d: null, reset_credits: 1, reset_allowed: false },
      ], groups: [{ id: 1, name: '一组' }, { id: 2, name: '二组' }], reset_enabled: true }
      if (options.method === 'PUT') body = JSON.parse(options.body)
      if (path.endsWith('/reset-credits')) body = { message: 'ok', usage_refreshed: true }
      return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } })
    }
  })
  await page.addStyleTag({ content: css })
  await page.addScriptTag({ content: script })
  await page.getByText('账号甲').waitFor()
  await page.getByRole('progressbar', { name: '30d' }).waitFor()
  assert.equal(await page.getByRole('progressbar', { name: '30d' }).getAttribute('aria-valuenow'), '10')
  assert.equal(await page.locator('.codex-account-card').count(), 3)
  assert.equal(await page.getByText('解除待核实').count(), 0)
  await page.getByRole('switch').first().click()
  await page.getByText('已保存', { exact: true }).waitFor()
  const saved = (await page.evaluate(() => window.__calls)).find(call => call.method === 'PUT')
  assert.deepEqual(JSON.parse(saved.body), { view_enabled: false, reset_enabled: true })
  await page.getByLabel('分组', { exact: true }).click()
  await page.getByRole('option', { name: '二组' }).click()
  assert.equal(await page.getByText('账号甲').count(), 0)
  assert.equal(await page.getByText('账号乙').count(), 1)
  await page.getByLabel('分组', { exact: true }).click()
  await page.getByRole('option', { name: '一组' }).click()
  await page.getByRole('button', { name: '使用重置卡' }).click()
  await page.getByRole('dialog').getByRole('button', { name: '使用重置卡' }).click()
  await page.getByText('重置请求已完成').waitFor()
  const calls = await page.evaluate(() => window.__calls)
  const reset = calls.find(call => call.method === 'POST')
  assert.equal(reset.path, '/api/key-usage/accounts/1/reset-credits')
  assert.equal(reset.auth, 'Bearer fixture-key')
  assert.match(reset.idempotency, /^[0-9a-f-]{36}$/)
  await page.screenshot({ path: path.join(artifactDir, 'account-cards-desktop.png'), fullPage: true })
  await page.setViewportSize({ width: 390, height: 844 })
  await page.screenshot({ path: path.join(artifactDir, 'account-cards-mobile.png'), fullPage: true })
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true)
  await page.evaluate(() => document.documentElement.classList.add('dark'))
  await page.screenshot({ path: path.join(artifactDir, 'account-cards-dark.png'), fullPage: true })
  console.log('Card screenshots:', artifactDir)
})
