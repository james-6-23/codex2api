import assert from 'node:assert/strict'
import { after, before, test } from 'node:test'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { build } from 'vite'
import react from '@vitejs/plugin-react'
import { chromium } from 'playwright'

const root = fileURLToPath(new URL('../', import.meta.url))
let browser, script

before(async () => {
  const result = await build({ configFile: false, root, logLevel: 'silent', plugins: [react(), {
    name: 'portal-harness', enforce: 'pre',
    resolveId(id) { if (id === 'virtual:portal-harness') return '\0portal-harness' },
    load(id) {
      if (id !== '\0portal-harness') return
      return `import React from 'react'; import {createRoot} from 'react-dom/client';
        import Accounts from ${JSON.stringify(path.join(root, 'src/components/KeyUsageAccounts.tsx'))};
        import Permissions from ${JSON.stringify(path.join(root, 'src/components/KeyUsageAccountPermissions.tsx'))};
        import ${JSON.stringify(path.join(root, 'src/i18n.ts'))};
        createRoot(document.getElementById('root')).render(React.createElement(React.Fragment,null,
          React.createElement(Permissions,{keys:[{id:10,name:'test-key'}]}), React.createElement(Accounts,{apiKey:'fixture-key'})));`
    },
  }], resolve: { alias: { '@': path.join(root, 'src') } }, define: { 'process.env.NODE_ENV': '"production"' }, build: { write: false, minify: false, rollupOptions: { input: 'virtual:portal-harness' }, lib: { entry: 'virtual:portal-harness', formats: ['iife'], name: 'PortalHarness' } } })
  const bundle = Array.isArray(result) ? result[0] : result
  script = bundle.output.find(item => item.type === 'chunk').code
  browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || undefined, args: ['--no-sandbox'] })
})
after(async () => { await browser?.close() })

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
        { id: 1, name: '账号甲', email: 'a@example.test', plan_type: 'plus', status: 'active', group_ids: [1], usage_percent_5h: 25, usage_percent_7d: 10, long_window_seconds: 2592000, reset_credits: 2, reset_allowed: true, reset_uncertain: false },
        { id: 2, name: '账号乙', email: '', plan_type: 'plus', status: 'active', group_ids: [2], usage_percent_5h: 60, usage_percent_7d: null, reset_credits: 0, reset_allowed: true, reset_uncertain: false },
        { id: 3, name: '账号丙', email: '', plan_type: 'plus', status: 'active', group_ids: [1], usage_percent_5h: null, usage_percent_7d: null, reset_credits: 1, reset_allowed: false, reset_uncertain: true },
      ], groups: [{ id: 1, name: '一组' }, { id: 2, name: '二组' }], reset_enabled: true }
      if (path.endsWith('/reset-credits')) body = { message: 'ok', usage_refreshed: true }
      return new Response(JSON.stringify(body), { status: 200, headers: { 'Content-Type': 'application/json' } })
    }
  })
  await page.addScriptTag({ content: script })
  await page.getByText('账号甲').waitFor()
  await page.getByText('30d: 10% 已用', { exact: false }).waitFor()
  await page.getByText('上次重置结果待管理员核实，暂不能再次使用重置卡。').waitFor()
  await page.getByRole('switch').first().click()
  await page.getByRole('button', { name: '保存', exact: true }).click()
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
})
