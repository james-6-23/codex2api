import assert from 'node:assert/strict'
import { after, before, test } from 'node:test'
import path from 'node:path'
import { fileURLToPath } from 'node:url'
import { build } from 'vite'
import react from '@vitejs/plugin-react'
import tailwindcss from '@tailwindcss/vite'
import { chromium } from 'playwright'

const root = fileURLToPath(new URL('../', import.meta.url))
let browser, script, css

before(async () => {
  const result = await build({
    configFile: false, root, logLevel: 'silent',
    plugins: [react(), tailwindcss(), {
      name: 'version-popover-harness', enforce: 'pre',
      resolveId(id) { if (id === 'virtual:version-popover-harness') return '\0version-popover-harness' },
      load(id) {
        if (id !== '\0version-popover-harness') return
        return `import React from 'react'; import {createRoot} from 'react-dom/client';
          import Layout from ${JSON.stringify(path.join(root, 'src/components/Layout.tsx'))};
          import {ToastProvider} from ${JSON.stringify(path.join(root, 'src/components/ToastProvider.tsx'))};
          import {BrandingProvider} from ${JSON.stringify(path.join(root, 'src/branding.tsx'))};
          import {ThemeProvider} from ${JSON.stringify(path.join(root, 'src/hooks/useTheme.tsx'))};
          import {MemoryRouter} from 'react-router-dom';
          import ${JSON.stringify(path.join(root, 'src/i18n.ts'))};
          import ${JSON.stringify(path.join(root, 'src/index.css'))};
          createRoot(document.getElementById('root')).render(
            React.createElement(MemoryRouter,null,React.createElement(ThemeProvider,null,React.createElement(BrandingProvider,null,
              React.createElement(ToastProvider,null,React.createElement(Layout,null,'Synthetic content'))))));`
      },
    }],
    resolve: { alias: { '@': path.join(root, 'src') } },
    define: { __APP_VERSION__: 'window.__buildVersion', 'process.env.NODE_ENV': '"production"' },
    build: {
      write: false, minify: false, rollupOptions: { input: 'virtual:version-popover-harness' },
      lib: { entry: 'virtual:version-popover-harness', formats: ['iife'], name: 'VersionPopoverHarness' },
    },
  })
  const bundle = Array.isArray(result) ? result[0] : result
  script = bundle.output.find(item => item.type === 'chunk').code
  css = bundle.output.filter(item => item.type === 'asset' && item.fileName.endsWith('.css')).map(item => item.source).join('\n')
  browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || undefined, args: ['--no-sandbox'] })
})

after(async () => { await browser?.close() })

async function fixture(t, version, { mobile = false, cached = false, backend = version, deploymentMode = 'docker' } = {}) {
  const page = await browser.newPage({ viewport: mobile ? { width: 390, height: 844 } : { width: 1440, height: 1000 } })
  page.setDefaultTimeout(5_000)
  const errors = []
  page.on('pageerror', error => errors.push(error.message))
  t.after(() => assert.deepEqual(errors, []))
  t.after(() => page.close())
  await page.route('http://version-popover.test/**', route => route.fulfill({ contentType: 'text/html', body: '<div id="root"></div>' }))
  await page.goto('http://version-popover.test/')
  await page.evaluate(({ version, cached, backend, deploymentMode }) => {
    window.__buildVersion = version
    window.__updateRequests = []
    localStorage.setItem('lang', 'zh')
    if (cached) localStorage.setItem('codex2api_latest_release', JSON.stringify({ latest_version: '3.1.6', checkedAt: Date.now() }))
    window.fetch = async (url, options = {}) => {
      const parsed = new URL(String(url), location.origin)
      const pathname = parsed.pathname
      const method = options.method || 'GET'
      if (pathname === '/api/admin/system/update') {
        window.__updateRequests.push({ method, search: parsed.search })
      }
      if (method !== 'GET') throw new Error('Unexpected write in version check')
      let body = {}
      if (pathname === '/api/admin/health') body = { status: 'ok', build_version: backend }
      else if (pathname === '/api/admin/system/update') body = {
        current_version: version,
        latest_version: '3.1.6',
        has_update: true,
        supported: version.startsWith('v'),
        runtime_os: 'linux',
        runtime_arch: 'amd64',
        mode: 'binary',
        build_type: version === 'dev' ? 'dev' : version.startsWith('v') ? 'release' : 'custom',
        deployment_mode: deploymentMode,
        release_name: 'v3.1.6',
        release_notes: '## 新增功能\n- 支持系统更新弹窗\n- 查看完整发布说明',
        release_url: 'https://example.test/releases/v3.1.6',
      }
      else if (pathname === '/api/admin/keys') body = { keys: [{ id: 1 }] }
      return new Response(JSON.stringify(body), { headers: { 'Content-Type': 'application/json' } })
    }
  }, { version, cached, backend, deploymentMode })
  await page.addStyleTag({ content: css })
  await page.addScriptTag({ content: script })
  return page
}

test('custom build modal has no update dot or action on desktop, including a cached release', async t => {
  for (const cached of [false, true]) {
    const page = await fixture(t, 'custom.abcdef1', { cached })
    const badge = page.getByRole('button', { name: 'custom.abcdef1', exact: true }).filter({ visible: true })
    await badge.waitFor()
    await badge.click()
    const modal = page.locator('#version-details')
    await modal.getByText('系统更新', { exact: true }).waitFor()
    await modal.getByText('自定义构建', { exact: true }).first().waitFor()
    await modal.getByText('v3.1.6', { exact: true }).first().waitFor()
    await modal.getByText('支持系统更新弹窗', { exact: true }).waitFor()
    assert.match(await modal.innerText(), /无法与上游发布版本比较/)
    assert.equal(await modal.getByRole('button', { name: '立即更新', exact: true }).count(), 0)
    assert.equal(await modal.getByText('已是最新版本', { exact: true }).count(), 0)
    assert.equal(await modal.getByRole('link', { name: '发布页' }).getAttribute('href'), 'https://example.test/releases/v3.1.6')
    await page.keyboard.press('Escape')
    await modal.waitFor({ state: 'detached' })
    assert.equal(await badge.locator('span[aria-hidden="true"]').count(), 0)
  }
})

test('old official frontend with custom backend has no update dot or action with or without cache', async t => {
  for (const cached of [false, true]) {
    const page = await fixture(t, 'v3.1.5', { cached, backend: 'custom.abcdef1' })
    const badge = page.getByRole('button', { name: 'custom.abcdef1', exact: true }).filter({ visible: true })
    await badge.waitFor()
    await badge.click()
    const modal = page.locator('#version-details')
    await modal.getByText('自定义构建', { exact: true }).first().waitFor()
    await modal.getByText('v3.1.6', { exact: true }).first().waitFor()
    assert.equal(await modal.getByRole('button', { name: '立即更新', exact: true }).count(), 0)
    assert.equal(await modal.getByRole('link', { name: '发布页' }).getAttribute('href'), 'https://example.test/releases/v3.1.6')
    await page.keyboard.press('Escape')
    await modal.waitFor({ state: 'detached' })
    assert.equal(await badge.locator('span[aria-hidden="true"]').count(), 0)
  }
})

test('custom mobile build has no update dot and opens the update modal', async t => {
  const page = await fixture(t, 'custom.abcdef1', { mobile: true, cached: true })
  const badge = page.getByRole('button', { name: 'custom.abcdef1', exact: true }).filter({ visible: true })
  await badge.waitFor()
  assert.equal(await badge.locator('span[aria-hidden="true"]').count(), 0)
  await badge.click()
  await page.locator('#version-details').getByText('系统更新', { exact: true }).waitFor()
})

test('older official release opens update modal with release notes, metadata, force check, and enabled update action', async t => {
  const page = await fixture(t, 'v3.1.5')
  const badge = page.getByRole('button', { name: 'v3.1.5', exact: true }).filter({ visible: true })
  await badge.locator('span[aria-hidden="true"]').waitFor()
  assert.equal(await badge.locator('span[aria-hidden="true"]').count(), 1)
  await badge.click()
  const modal = page.locator('#version-details')
  await modal.getByText('系统更新', { exact: true }).waitFor()
  await modal.getByText('有可用更新', { exact: true }).waitFor()
  await modal.getByText('正式构建', { exact: true }).waitFor()
  await modal.getByText('Docker', { exact: true }).waitFor()
  await modal.getByText('支持系统更新弹窗', { exact: true }).waitFor()
  assert.equal(await modal.getByRole('button', { name: '立即更新', exact: true }).isEnabled(), true)
  assert.equal(await modal.getByText('自定义构建', { exact: true }).count(), 0)

  await modal.getByRole('button', { name: '检查更新', exact: true }).click()
  await page.waitForFunction(() => window.__updateRequests.some(req => req.search === '?force=1'))
})

test('dev build stays idle until modal opens, then loads release notes on demand', async t => {
  const page = await fixture(t, 'dev', { deploymentMode: 'binary' })
  const badge = page.getByRole('button', { name: 'dev', exact: true }).filter({ visible: true })
  await badge.waitFor()
  assert.deepEqual(await page.evaluate(() => window.__updateRequests), [])
  await badge.click()
  const modal = page.locator('#version-details')
  await modal.getByText('系统更新', { exact: true }).waitFor()
  await modal.getByText('开发构建', { exact: true }).first().waitFor()
  await modal.getByText('二进制', { exact: true }).waitFor()
  await modal.getByText('支持系统更新弹窗', { exact: true }).waitFor()
  assert.equal(await modal.getByRole('button', { name: '立即更新', exact: true }).count(), 0)
})
