import assert from 'node:assert/strict'
import { after, before, test } from 'node:test'
import { fileURLToPath } from 'node:url'
import { mkdtemp } from 'node:fs/promises'
import { tmpdir } from 'node:os'
import path from 'node:path'
import { build } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { chromium } from 'playwright'

const root = fileURLToPath(new URL('../', import.meta.url))
let browser, script, css, artifacts

before(async () => {
  artifacts = await mkdtemp(path.join(tmpdir(), 'kiro-account-review-'))
  const result = await build({
    configFile: false, root, logLevel: 'silent',
    plugins: [react(), tailwindcss(), {
      name: 'kiro-account-review', enforce: 'pre',
      resolveId(id) {
        if (id === 'virtual:kiro-account-review') return '\0kiro-account-review'
        if (/^(?:\.\.\/|\.\/)+api$/.test(id)) return '\0kiro-mock-api'
      },
      load(id) {
        if (id === '\0kiro-mock-api') return 'export const api = new Proxy({}, {get: (_, name) => (...args) => window.__api(name, args)}); export const getAdminKey = () => "fixture"; export class AdminAPIError extends Error {}'
        if (id !== '\0kiro-account-review') return
        return `import React from 'react'; import {createRoot} from 'react-dom/client'; import {MemoryRouter} from 'react-router-dom';
          import KiroAccounts from ${JSON.stringify(path.join(root, 'src/pages/KiroAccounts.tsx'))};
          import {ToastProvider} from ${JSON.stringify(path.join(root, 'src/components/ToastProvider.tsx'))};
          import ${JSON.stringify(path.join(root, 'src/i18n.ts'))}; import ${JSON.stringify(path.join(root, 'src/index.css'))};
          createRoot(document.getElementById('root')).render(React.createElement(MemoryRouter, null, React.createElement(ToastProvider, null, React.createElement('main', {style:{padding:24}}, React.createElement(KiroAccounts)))));`
      },
    }],
    resolve: { alias: { '@': path.join(root, 'src') } },
    define: { 'process.env.NODE_ENV': '"production"' },
    build: { write: false, minify: false, rollupOptions: { input: 'virtual:kiro-account-review' }, lib: { entry: 'virtual:kiro-account-review', formats: ['iife'], name: 'KiroReview' } },
  })
  const bundle = Array.isArray(result) ? result[0] : result
  script = bundle.output.find(item => item.type === 'chunk').code
  css = bundle.output.filter(item => item.type === 'asset' && item.fileName.endsWith('.css')).map(item => item.source).join('\n')
  browser = await chromium.launch({ headless: true, ...(process.env.KIRO_TEST_CHROMIUM ? {executablePath: process.env.KIRO_TEST_CHROMIUM} : {}) })
})
after(async () => { await browser?.close(); console.log(`Screenshots: ${artifacts}`) })

async function fixture(testContext, options = {}) {
  const page = await browser.newPage({ viewport: { width: 1500, height: 1000 } })
  page.setDefaultTimeout(7000)
  testContext.after(() => page.close())
  const errors = []
  page.on('pageerror', error => errors.push(error.message))
  testContext.after(() => assert.deepEqual(errors, []))
  await page.route('http://kiro.test/**', route => route.fulfill({contentType:'text/html', body:'<!doctype html><html><body><div id="root"></div></body></html>'}))
  await page.goto('http://kiro.test/')
  await page.evaluate(options => {
    localStorage.setItem('lang', 'zh')
    window.__calls = []
    window.__saved = null
    window.__detail = {id:1, name:'Kiro fixture', email:'kiro@example.test', status:'active', enabled:true, kiro_api:true, kiro_auth_kind:'social', kiro_plan:'kiro student', plan_type:'KIRO STUDENT', health_tier:'healthy', models:['claude-haiku-4.5'], group_ids:[], tags:[], allowed_api_key_ids:[7], detail_loaded:true, created_at:'2026-11-17T00:00:00Z'}
    window.__api = async (name, args) => {
      window.__calls.push({name, args})
      if (name === 'getAccountsPage') {
        const {allowed_api_key_ids, detail_loaded, ...row} = window.__detail
        return {accounts:[row], total:1, page:1, page_size:20, stats_state:'ready', summary:{total:1, normal:1, healthy:1, kiro_auth_kinds:{social:1}}, facets:{tags:[],email_domains:[]}}
      }
      if (name === 'getAccount') {
        if (options.detailError) throw new Error('fixture detail unavailable')
        return {...window.__detail}
      }
      if (name === 'updateAccountScheduler') { window.__saved=args[1]; Object.assign(window.__detail, args[1]); return {} }
      if (name === 'getAccountHealthBars') return {buckets:{'1':[{success:9,failed:1}]}}
      if (name === 'getIntelligenceBars') return {bars:{}}
      if (name === 'getAccountPageStats') return {stats:{}}
      if (name === 'getAccountLiveState') return {accounts:{}}
      if (name === 'getAPIKeys') {
        if (options.keyError) throw new Error('fixture keys unavailable')
        return {keys:options.omitKey ? [{id:8,name:'Other key',key:'masked'}] : [{id:7,name:'Restricted key',key:'masked'}]}
      }
      if (name === 'listProxies') return {proxies:[]}
      if (name === 'listAccountGroups') return {groups:[]}
      if (name === 'getSettings') return {}
      throw new Error(`Unexpected API: ${name}`)
    }
  }, options)
  await page.addStyleTag({content:css})
  await page.addScriptTag({content:script})
  await page.getByRole('button', {name:'kiro@example.test',exact:true}).waitFor()
  return page
}

test('editing another field preserves existing API-key restrictions', async testContext => {
  for (const options of [{}, {keyError:true}, {omitKey:true}]) {
    const page = await fixture(testContext, options)
    await page.getByRole('button', {name:'编辑账号配置',exact:true}).click()
    const dialog = page.getByRole('dialog')
    await dialog.waitFor()
    assert.equal(await page.evaluate(() => window.__calls.filter(call => call.name === 'getAccount').length), 1)
    await dialog.locator('input[inputmode="numeric"]').first().fill('8')
    await dialog.getByRole('button', {name:'保存',exact:true}).click()
    await dialog.waitFor({state:'hidden'})
    const result = await page.evaluate(() => ({saved:window.__saved, allowed:window.__detail.allowed_api_key_ids}))
    assert.equal(result.saved.score_bias_override, 8)
    assert.equal(Object.hasOwn(result.saved,'allowed_api_key_ids'), false)
    assert.deepEqual(result.allowed, [7])
  }
})

test('intentional restriction changes are saved, detail failures cannot open editor', async testContext => {
  const page = await fixture(testContext)
  await page.getByRole('button', {name:'编辑账号配置',exact:true}).click()
  const dialog = page.getByRole('dialog')
  await dialog.getByRole('button', {name:/已选.*1|已選.*1/}).click()
  const option = dialog.getByRole('button', {name:/Restricted key/})
  assert.equal(await option.getAttribute('aria-pressed'), 'true')
  await page.screenshot({path:path.join(artifacts,'restricted-key-editor.png')})
  await option.click()
  await dialog.getByRole('button', {name:'保存',exact:true}).click()
  await dialog.waitFor({state:'hidden'})
  assert.deepEqual(await page.evaluate(() => window.__saved.allowed_api_key_ids), [])
  const unavailable = await fixture(testContext, {detailError:true})
  await unavailable.getByRole('button', {name:'编辑账号配置',exact:true}).click()
  await unavailable.getByText('fixture detail unavailable').waitFor()
  assert.equal(await unavailable.getByRole('dialog').count(), 0)
  assert.equal(await unavailable.evaluate(() => window.__saved), null)
})

test('explicit refresh reloads health history for the same visible IDs', async testContext => {
  const page = await fixture(testContext)
  await page.waitForFunction(() => window.__calls.some(call => call.name === 'getAccountHealthBars'))
  const before = await page.evaluate(() => window.__calls.filter(call => call.name === 'getAccountHealthBars').length)
  await page.getByRole('button', {name:'刷新',exact:true}).click()
  await page.waitForFunction(before => window.__calls.filter(call => call.name === 'getAccountHealthBars').length > before, before)
  await page.screenshot({path:path.join(artifacts,'health-refreshed.png')})
})
