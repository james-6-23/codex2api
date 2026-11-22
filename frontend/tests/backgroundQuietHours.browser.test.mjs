import assert from 'node:assert/strict'
import { test } from 'node:test'
import { fileURLToPath } from 'node:url'
import { build } from 'vite'
import { chromium } from 'playwright'

test('quiet hours editor saves multiple and overnight periods, rejects invalid input, and clears all', async () => {
  const root = fileURLToPath(new URL('../', import.meta.url))
  const component = fileURLToPath(new URL('../src/components/BackgroundQuietHoursEditor.tsx', import.meta.url))
  const result = await build({
    configFile: false, root, logLevel: 'silent',
    resolve: { alias: { '@': root + 'src' } },
    define: { 'process.env.NODE_ENV': '"production"' },
    plugins: [{ name: 'quiet-hours-harness', enforce: 'pre',
      resolveId(id) { if (id === 'virtual:quiet') return '\0quiet'; if (id === 'react-i18next') return '\0translate' },
      load(id) {
        if (id === '\0translate') return 'export const useTranslation = () => ({t: key => key})'
        if (id === '\0quiet') return `
          import {createElement, useState} from 'react'; import {createRoot} from 'react-dom/client';
          import Editor from ${JSON.stringify(component)};
          window.saved=[];
          function App(){const [value,setValue]=useState([]);return createElement(Editor,{value,onSave:async periods=>{window.saved.push(periods);setValue(periods)}})}
          createRoot(document.getElementById('root')).render(createElement(App));`
      }
    }],
    build: { write: false, minify: false, rolldownOptions: { input: 'virtual:quiet', output: { format: 'iife', name: 'QuietHarness' } } }
  })
  const browser = await chromium.launch({ headless: true, ...(process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH } : {}) })
  try {
    const page = await browser.newPage({ viewport: { width: 480, height: 800 } })
    await page.setContent('<div id="root"></div>')
    await page.addScriptTag({ content: result.output.find(x => x.type === 'chunk').code })
    const add = page.getByRole('button', { name: 'settings.quietHoursAdd', exact: true })
    const save = page.getByRole('button', { name: 'settings.quietHoursSave', exact: true })
    await add.click(); await add.click()
    await page.getByLabel('settings.quietHoursStart 2', { exact: true }).fill('23:00')
    await page.getByLabel('settings.quietHoursEnd 2', { exact: true }).fill('01:00')
    await save.click()
    await page.waitForFunction(() => window.saved.length === 1)
    assert.deepEqual(await page.evaluate(() => window.saved[0]), [{ start: '00:00', end: '07:00' }, { start: '23:00', end: '01:00' }])
    await page.getByLabel('settings.quietHoursEnd 2', { exact: true }).fill('23:00')
    await save.click()
    assert.equal(await page.getByRole('alert').textContent(), 'settings.quietHoursInvalid')
    assert.equal(await page.evaluate(() => window.saved.length), 1)
    await page.getByRole('button', { name: 'settings.quietHoursRemove', exact: true }).first().click()
    await page.getByRole('button', { name: 'settings.quietHoursRemove', exact: true }).click()
    await save.click()
    await page.waitForFunction(() => window.saved.length === 2)
    assert.deepEqual(await page.evaluate(() => window.saved[1]), [])
  } finally { await browser.close() }
})
